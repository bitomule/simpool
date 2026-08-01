"""simpool_ios_test_runner: an AppleTestRunnerInfo provider that hands the
whole test action, not just a hook, to a simpool slot.

An earlier version of this rule wrapped
`@build_bazel_rules_apple//apple/testing/default_runner:ios_xctestrun_runner`
and swapped its `create_simulator_action` exec tool. That could not hold a
flock across a test: `test_runner_template` is an *output* of that rule
(`"%{name}.sh"`), not an attribute — there is no way to hand it a script of
our own — and its only two hooks, `pre_action`/`post_action`, are short,
separate steps that finish (and, so, release anything they held) before the
test itself ever starts.

This rule owns its own `test_runner_template`
(`simpool_ios_test_runner.template.sh`, adapted from rules_apple's
`ios_xctestrun_runner.template.sh` under the Apache License 2.0 — see that
file's header for the exact diff) instead, which puts `simpool with` in
charge of the *entire* rest of the script the moment it starts: simulator
resolution, the xctestrun run, and cleanup all happen as children of the
process holding the pool slot's flock. That is simpool's own `with` model
— the parent holds the lock and execs the consumer as a child with CLOEXEC
intact — applied to a single test action instead of a whole `bazel test`
invocation, which is what makes two test targets running concurrently land
on two different slots instead of serializing behind one shared lock.

Zero configuration required: the template resolves the `simpool` binary
and the real `$HOME` for the pool from inside the test action itself (see
the template's header for why neither can be assumed present in a
sanitized test action's environment), and falls back to stock
`ios_xctestrun_runner`-equivalent behavior — reuse-or-create a simulator by
a fixed name — when `simpool` isn't installed at all, so this rule always
builds and runs whether or not the host has simpool. That fallback is not
equally *safe*, though: it reuses one fixed name across every concurrent
test action missing simpool, which is exactly the collision this rule
exists to prevent — the template warns on stderr when it takes that path.

`_get_template_substitutions` and `_get_execution_environment` below, and
most of the attributes' names/docstrings, are adapted near-verbatim from
@build_bazel_rules_apple//apple/testing/default_runner:ios_xctestrun_runner.bzl
(Apache License 2.0, https://github.com/bazelbuild/rules_apple). Beyond the
substitutions this rule adds for its own template (`max_slots`, `wait`),
the two functions are otherwise unchanged; the differences are in
`_simpool_ios_test_runner_impl` (no `_simulator_creator` exec tool — this
rule's own template resolves the simulator itself, see its header) and in
the `rule()` attrs (this rule's own `_test_template` default, added
`max_slots`/`wait`, dropped `_simulator_creator`).
"""

load(
    "@build_bazel_rules_apple//apple:providers.bzl",
    "AppleDeviceTestRunnerInfo",
    "apple_provider",
)

def _get_template_substitutions(
        *,
        create_xcresult_bundle,
        device_type,
        os_version,
        random,
        xcodebuild_args,
        command_line_args,
        xctestrun_template,
        attachment_lifetime,
        destination_timeout,
        reuse_simulator,
        xctrunner_entitlements_template,
        pre_action_binary,
        post_action_binary,
        post_action_determines_exit_code,
        max_slots,
        wait):
    substitutions = {
        "device_type": device_type,
        "os_version": os_version,
        "create_xcresult_bundle": create_xcresult_bundle,
        "xcodebuild_args": xcodebuild_args,
        "command_line_args": command_line_args,
        # "ordered" isn't a special string, but anything besides "random" for this field runs in order
        "test_order": "random" if random else "ordered",
        "xctestrun_template": xctestrun_template,
        "attachment_lifetime": attachment_lifetime,
        "reuse_simulator": reuse_simulator,
        "destination_timeout": destination_timeout,
        "xctrunner_entitlements_template": xctrunner_entitlements_template,
        "pre_action_binary": pre_action_binary,
        "post_action_binary": post_action_binary,
        "post_action_determines_exit_code": post_action_determines_exit_code,
        "max_slots": max_slots,
        "wait": wait,
    }

    return {"%({})s".format(key): value for key, value in substitutions.items()}

def _get_execution_environment(ctx):
    xcode_version = str(ctx.attr._xcode_config[apple_common.XcodeVersionConfig].xcode_version())
    if not xcode_version:
        fail("error: No xcode_version in _xcode_config")

    return {"XCODE_VERSION_OVERRIDE": xcode_version}

def _simpool_ios_test_runner_impl(ctx):
    os_version = str(ctx.attr.os_version or ctx.fragments.objc.ios_simulator_version or
                     ctx.attr._xcode_config[apple_common.XcodeProperties].default_ios_sdk_version)

    device_type = ctx.attr.device_type or ctx.fragments.objc.ios_simulator_device or "iPhone 15"

    if not os_version:
        fail("error: os_version must be set on simpool_ios_test_runner, or passed with --ios_simulator_version")
    if not device_type:
        fail("error: device_type must be set on simpool_ios_test_runner, or passed with --ios_simulator_device")

    runfiles = ctx.runfiles(files = [
        ctx.file._xctestrun_template,
        ctx.file._xctrunner_entitlements_template,
    ])

    default_action_binary = "/usr/bin/true"

    pre_action_binary = default_action_binary
    post_action_binary = default_action_binary

    if ctx.executable.pre_action:
        pre_action_binary = ctx.executable.pre_action.short_path
        runfiles = runfiles.merge(ctx.attr.pre_action[DefaultInfo].default_runfiles)

    post_action_determines_exit_code = False
    if ctx.executable.post_action:
        post_action_binary = ctx.executable.post_action.short_path
        post_action_determines_exit_code = ctx.attr.post_action_determines_exit_code
        runfiles = runfiles.merge(ctx.attr.post_action[DefaultInfo].default_runfiles)

    ctx.actions.expand_template(
        template = ctx.file._test_template,
        output = ctx.outputs.test_runner_template,
        substitutions = _get_template_substitutions(
            create_xcresult_bundle = "true" if ctx.attr.create_xcresult_bundle else "false",
            device_type = device_type,
            os_version = os_version,
            random = ctx.attr.random,
            xcodebuild_args = " ".join(ctx.attr.xcodebuild_args) if ctx.attr.xcodebuild_args else "",
            command_line_args = " ".join(ctx.attr.command_line_args) if ctx.attr.command_line_args else "",
            xctestrun_template = ctx.file._xctestrun_template.short_path,
            attachment_lifetime = ctx.attr.attachment_lifetime,
            destination_timeout = "" if ctx.attr.destination_timeout == 0 else str(ctx.attr.destination_timeout),
            reuse_simulator = "true" if ctx.attr.reuse_simulator else "false",
            xctrunner_entitlements_template = ctx.file._xctrunner_entitlements_template.short_path,
            pre_action_binary = pre_action_binary,
            post_action_binary = post_action_binary,
            post_action_determines_exit_code = "true" if post_action_determines_exit_code else "false",
            max_slots = str(ctx.attr.max_slots) if ctx.attr.max_slots > 0 else "",
            wait = ctx.attr.wait,
        ),
    )

    return [
        apple_provider.make_apple_test_runner_info(
            execution_environment = _get_execution_environment(ctx),
            execution_requirements = {"requires-darwin": ""},
            test_runner_template = ctx.outputs.test_runner_template,
        ),
        AppleDeviceTestRunnerInfo(
            device_type = device_type,
            os_version = os_version,
        ),
        DefaultInfo(runfiles = runfiles),
    ]

simpool_ios_test_runner = rule(
    _simpool_ios_test_runner_impl,
    attrs = {
        "device_type": attr.string(
            default = "",
            doc = """
The device type of the iOS simulator to run tests on — must match the
`--device` a `simpool` pool for it was provisioned with. The supported
types correspond to the output of `xcrun simctl list devicetypes`, e.g.
"iPhone 17 Pro". By default, reads from --ios_simulator_device or falls
back to some device.
""",
        ),
        "random": attr.bool(
            default = False,
            doc = """
Whether to run the tests in random order to identify unintended state
dependencies.
""",
        ),
        "os_version": attr.string(
            default = "",
            doc = """
The os version of the iOS simulator to run tests on — must match the
`--os` a `simpool` pool for it was provisioned with. The supported os
versions correspond to the output of `xcrun simctl list runtimes`, e.g.
"26.3". By default, reads --ios_simulator_version and then falls back to
the latest supported version.
""",
        ),
        "create_xcresult_bundle": attr.bool(
            default = False,
            doc = """
Force the test runner to always create an XCResult bundle. This means it will
always use `xcodebuild test-without-building` to run the test bundle.
""",
        ),
        "xcodebuild_args": attr.string_list(
            doc = """
Arguments to pass to `xcodebuild` when running the test bundle. This means it
will always use `xcodebuild test-without-building` to run the test bundle.
""",
        ),
        "command_line_args": attr.string_list(
            doc = """
CommandLineArguments to pass to xctestrun file when running the test bundle. This means it
will always use `xcodebuild test-without-building` to run the test bundle.
""",
        ),
        "attachment_lifetime": attr.string(
            default = "keepNever",
            doc = """
Attachment lifetime to set in the xctestrun file when running the test bundle - `"keepNever"` (default), `"keepAlways"`
or `"deleteOnSuccess"`. This affects presence of attachments in the XCResult output. This does not force using
`xcodebuild` or an XCTestRun file but the value will be used in that case.
""",
        ),
        "destination_timeout": attr.int(
            doc = "Use the specified timeout when searching for a destination device. The default is 30 seconds.",
        ),
        "reuse_simulator": attr.bool(
            default = True,
            doc = """
Toggle simulator reuse for the fallback path (no `simpool` installed) only.
When a `simpool` slot is in play its simulator is always reused regardless
of this attribute — it belongs to the pool, not to this test run, so this
rule never deletes it.
""",
        ),
        "max_slots": attr.int(
            default = 0,
            doc = """
Forwarded to `simpool with --max` for this rule's device+OS group. 0 (the
default) omits `--max` entirely, so `simpool` falls back to its own
default resolution (`SIMPOOL_MAX_SLOTS` if set in `simpool`'s own
environment, else its built-in default of 3).

This is the only way to raise the cap from a Bazel test action:
`SIMPOOL_MAX_SLOTS` is read from `simpool`'s environment, which a
sanitized test action cannot set via `--test_env` any more than it can set
`$HOME` (see the template's header) — this attribute is a compile-time
substitution instead, exactly like `device_type`/`os_version`.

Mind `--local_test_jobs` (default: number of local cores) when raising
this: with more concurrently-scheduled iOS test actions than `max_slots`,
the excess actions block inside `simpool with`'s own `--wait` instead of
running, and a blocked action that outlives its test's `size`/`timeout`
(the Bazel default for `size = "medium"` is 300s; `simpool`'s own
`--wait` default is 600s) is reported as a plain TIMEOUT — nothing in that
report says it was pool contention rather than the test itself hanging.
Raise `max_slots` to cover your real peak concurrency, or cap
`--local_test_jobs` to it, rather than debugging a phantom timeout later.
""",
        ),
        "wait": attr.string(
            default = "",
            doc = """
Forwarded to `simpool with --wait` for this rule's device+OS group — how
long a test action blocks for a free slot once `max_slots` is reached,
as a Go `time.Duration` string (e.g. `"2m"`, `"0"` to fail immediately
instead of waiting). Empty (the default) omits `--wait` entirely, so
`simpool` uses its own default (10 minutes). See `max_slots`'s docstring
for why this can't just be set via the environment from a test action,
and for the interaction with `--local_test_jobs` and Bazel's own test
timeout.
""",
        ),
        "pre_action": attr.label(
            executable = True,
            cfg = "exec",
            doc = """
A binary to run prior to test execution. Runs after simulator resolution. Sets the `$SIMULATOR_UDID` environment variable, in addition to any other variables available to the test runner.
""",
        ),
        "post_action": attr.label(
            executable = True,
            cfg = "exec",
            doc = """
A binary to run following test execution. Runs after testing but before test result handling and coverage processing. Sets the `$TEST_EXIT_CODE`, `$TEST_LOG_FILE`, and `$SIMULATOR_UDID` environment variables, the `$TEST_XCRESULT_BUNDLE_PATH` environment variable if the test run produces an XCResult bundle, and any other variables available to the test runner.
""",
        ),
        "post_action_determines_exit_code": attr.bool(
            default = False,
            doc = """
When true, the exit code of the test run will be set to the exit code of the post action. This is useful for tests that need to fail the test run based on their own criteria.
""",
        ),
        "_test_template": attr.label(
            default = Label(
                "//:simpool_ios_test_runner.template.sh",
            ),
            allow_single_file = True,
        ),
        "_xcode_config": attr.label(
            default = configuration_field(
                name = "xcode_config_label",
                fragment = "apple",
            ),
        ),
        "_xctestrun_template": attr.label(
            default = Label(
                "@build_bazel_rules_apple//apple/testing/default_runner:ios_xctestrun_runner.template.xctestrun",
            ),
            allow_single_file = True,
        ),
        "_xctrunner_entitlements_template": attr.label(
            default = Label(
                "@build_bazel_rules_apple//apple/testing/default_runner:xctrunner_entitlements.template.plist",
            ),
            allow_single_file = True,
        ),
    },
    outputs = {
        "test_runner_template": "%{name}.sh",
    },
    fragments = ["apple", "objc"],
    doc = """
An iOS test runner that pulls its simulator from a `simpool` pool with no
extra configuration: no `.bazelrc` prefix, no `--test_env`. Each test
action resolves `simpool` and the pool's `$HOME` for itself, acquires a
slot, and holds its flock for the whole run — creation through cleanup —
so concurrent test targets land on different slots instead of serializing
behind one lock. Falls back to stock `ios_xctestrun_runner`-equivalent
behavior when `simpool` isn't installed.

```bzl
load("@simpool//:simpool_ios_test_runner.bzl", "simpool_ios_test_runner")

simpool_ios_test_runner(
    name = "iphone_17_pro_test_runner",
    device_type = "iPhone 17 Pro",
    os_version = "26.3",
)

ios_unit_test(
    name = "Tests",
    minimum_os_version = "17.0",
    runner = ":iphone_17_pro_test_runner",
    deps = [":TestsLib"],
)
```
""",
)
