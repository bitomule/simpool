"""simpool_ios_test_runner: an ios_xctestrun_runner wired to a simpool slot.

Wraps `@build_bazel_rules_apple//apple/testing/default_runner:ios_xctestrun_runner`
— NOT the default `ios_test_runner`, which shells out to Google's
xctestrunner. That tool creates a brand-new `New-<device>-<os>` simulator
per invocation and deletes it in a Python `finally` that a SIGKILL (e.g. a
Bazel test timeout) skips entirely, leaking 1-3GB per orphaned run.

`ios_xctestrun_runner` delegates simulator lifecycle to two overridable exec
tools, `create_simulator_action` / `clean_up_simulator_action`. This rule
swaps only the former for `:simulator_locator`, which looks up the UDID of
the simulator named `$SIMPOOL_NAME_0` — the slot simpool already provisioned
and booted — instead of creating a new one. `reuse_simulator = True` makes
the stock `clean_up_simulator_action` (simulator_cleanup.sh) skip deletion
on the way out, so the slot's simulator survives for the next run.

The test itself must run under `simpool with --device <device_type> --os
<os_version> -- bazel test //target`, which is what exports SIMPOOL_NAME_0
naming that exact simulator — this rule does not invoke simpool itself, it
only consumes the environment contract simpool sets up.
"""

load(
    "@build_bazel_rules_apple//apple/testing/default_runner:ios_xctestrun_runner.bzl",
    "ios_xctestrun_runner",
)

def simpool_ios_test_runner(name, device_type, os_version, **kwargs):
    """Declares an ios_xctestrun_runner that targets a simpool slot by name.

    Args:
        name: name of the runner target.
        device_type: simulator device type, e.g. "iPhone 17 Pro" — must
            match the `--device` passed to `simpool with`.
        os_version: simulator OS version, e.g. "26.3" — must match the
            `--os` passed to `simpool with`.
        **kwargs: forwarded to `ios_xctestrun_runner` (e.g. `random`,
            `command_line_args`). `reuse_simulator` and
            `create_simulator_action` are owned by this macro and must not
            be passed.
    """
    ios_xctestrun_runner(
        name = name,
        device_type = device_type,
        os_version = os_version,
        reuse_simulator = True,
        create_simulator_action = Label("//:simulator_locator"),
        **kwargs
    )
