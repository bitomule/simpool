#!/bin/bash
# This script replaces the variables in the templated xctestrun file with the
# the specific paths to the test bundle, and the optionally test host
#
# Adapted from @build_bazel_rules_apple//apple/testing/default_runner:ios_xctestrun_runner.template.sh
# (Apache License 2.0, https://github.com/bazelbuild/rules_apple). Changes
# from upstream are confined to the spots marked "simpool:" below:
#   1. A re-exec at the very top that hands the rest of this script to
#      `simpool with`, so the flock it holds on a pool slot covers
#      simulator creation, the xctestrun run, AND cleanup — not just a
#      short pre/post hook, which is the whole reason this isn't just
#      ios_xctestrun_runner with pre_action/post_action set. `--max`/`--wait`
#      are forwarded to that `simpool with` call from the rule's own
#      `max_slots`/`wait` attributes when set (see simpool_ios_test_runner.bzl)
#      — otherwise Bazel test actions have no way to reach them at all, since
#      the sanitized test-action environment this rule exists to route
#      around is also what `SIMPOOL_MAX_SLOTS`/`--test_env` would otherwise
#      require.
#   2. Simulator resolution is reimplemented inline (~20 lines of `xcrun
#      simctl`) instead of shelling out to rules_apple's own
#      simulator_creator.py, whose CLI is not a stable contract across
#      rules_apple releases (see the comment at its call site). It matches
#      simulator_creator.py's own reuse-or-create-by-name behavior exactly
#      (see the resolver's comments below), with one deliberate addition:
#      when the name in play is a specific pool slot (SIMPOOL_NAME_0), not
#      finding it is a hard error, never a cue to create a same-named
#      simulator the pool doesn't know about — this rule is never allowed
#      to create or delete a pool slot's simulator out from under it.
#   3. After a failed test run against a pool-owned slot, a cheap read-only
#      probe checks whether the simulator itself is still responsive; if
#      not, it's shut down so the pool re-verifies it on the next
#      acquisition instead of handing a wedged-but-nominally-"Booted"
#      device straight to the next consumer (see the comment at its call
#      site, right after the test execution block).
# Everything else, including the fallback when simpool isn't installed
# (this script then behaves like a stock ios_xctestrun_runner), is
# unmodified upstream behavior.

set -euo pipefail

# simpool: run "$@" with a wall-clock bound, killing it if it overruns.
#
# macOS ships no `timeout(1)`, so this backgrounds the command and polls
# `kill -0` on its pid instead — the same background+poll shape every
# bounded `xcrun simctl` call already gets on the Go side (see
# SIMPOOL_SIMCTL_TIMEOUT in internal/simctl), applied here because this
# script's own wedge probe below calls `xcrun simctl` directly against a
# simulator that is, by definition, already suspected of being wedged —
# exactly the moment an unbounded call is most likely to hang. Returns the
# command's own exit status, or 124 (matching GNU timeout's convention) if
# it had to be killed.
simpool_run_bounded() {
  local timeout_s="$1"
  shift
  "$@" &
  local pid=$!
  local waited=0
  local interval=1
  while kill -0 "$pid" 2>/dev/null; do
    if (( waited >= timeout_s )); then
      kill -9 "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep "$interval"
    waited=$(( waited + interval ))
  done
  wait "$pid"
}

# simpool: hold a pool slot's flock for the life of this whole test action.
#
# Bazel test actions run with a sanitized environment — no inherited PATH,
# no $HOME — so this resolves both itself instead of depending on a
# .bazelrc or --test_env anyone has to remember (and that nothing enforces
# for a new checkout or a new agent). `simpool with` is the parent-holds-
# the-lock model: it execs *this same script* as its child with CLOEXEC
# intact on the lock fd, so nothing this script — or anything it spawns —
# can ever inherit or fumble the lock. A SIGKILL to `simpool` itself (not
# its whole process group) is the one case that loses it: the kernel
# reclaims the flock immediately with no cleanup step of its own required,
# but this script (and anything it spawned) survives that as an orphan.
# `simpool with` fingerprints this script's process (its own start time,
# rendered under a fixed, locale/timezone-independent environment) right
# after launching it, so the next `simpool with`/`acquire`/`lease` to try
# this slot — or a `simpool reap` — can verify that fingerprint and reclaim
# it (kill the orphan, shut down the simulator) on its own, no manual step
# required. It only stays quarantined if that identity can no longer be
# verified (see `internal/pool/poison.go`'s AttemptRecovery). Recovery only
# ever shuts down the reclaimed slot's OWN device, never a sibling's — even
# a stale/corrupt meta.json pointing at a different slot's live simulator is
# refused (see `deviceBelongsToSlot` in poison.go).
#
# There is no automatic idle-simulator sweep anywhere in simpool. An
# earlier version of this feature shut down OTHER, idle sibling slots as a
# side effect of this same acquisition, on the assumption that an expired
# lease reliably proves nobody's using that slot. That assumption was false
# exactly where it mattered: the TTL keepalive it relied on only runs on
# MAV's `run` path, never on the one-shot tap/swipe/ui-tree commands the
# sweep actually had to reason about, so an expired lease only proves no
# command has run in the last few minutes — routine in an agent's tool
# loop, not evidence of absence. It was removed. `simpool reap --cold N`,
# run by a human, cron, or CI, is the only path that shuts down an
# otherwise-idle slot's simulator — never a side effect of this script's
# own acquisition.
if [[ -z "${_SIMPOOL_WRAPPED:-}" ]]; then
  simpool_bin=""
  for candidate in "${SIMPOOL_BIN:-}" /opt/homebrew/bin/simpool /usr/local/bin/simpool; do
    if [[ -n "$candidate" && -f "$candidate" && -x "$candidate" ]]; then
      simpool_bin="$candidate"
      break
    fi
  done
  if [[ -z "$simpool_bin" ]]; then
    simpool_bin="$(command -v simpool 2>/dev/null || true)"
  fi

  if [[ -n "$simpool_bin" ]]; then
    # simpool (the Go binary) resolves its pool root via os.UserHomeDir(),
    # which on Unix only ever reads $HOME — it does not fall back to the
    # password database the way `~` in a shell does. A test action's $HOME
    # is not trustworthy either way: it observably comes through unset on
    # this repo's setup ("$HOME is not defined"), and other Bazel spawn
    # strategies are known to point it at a private per-action tmp dir
    # instead — which would silently give every single test action its own
    # empty "pool" rather than sharing the real one. Ignore whatever $HOME
    # is (or isn't) and always resolve the actual user's home directory
    # from the password database, the same source `~` falls back to.
    #
    # This intentionally does NOT shell out to `dscl . -read`: that only
    # ever consults the *local* directory node, so it returns exit 56 for
    # any account resolved through a network/domain directory (LDAP, a
    # directory-bound CI runner account, ...) — silently, since stderr is
    # discarded. Under `set -euo pipefail` that assignment's failure (and
    # `pipefail` propagating it through the `awk` stage) kills this whole
    # script before it prints anything: `bazel test` fails in ~0s with no
    # message. Python's `pwd.getpwuid` calls the same libc getpwuid() that
    # backs `id -un`/`~` expansion, which walks the *full* directory
    # services search path instead of just the local node, so it resolves
    # exactly the accounts `dscl` can miss. `|| true` on the substitution
    # itself (not just the fallback `if`) is what actually saves this
    # script if that ever fails anyway: with `set -e`, a bare `var=$(cmd)`
    # still aborts the script on `cmd`'s failure unless the failure is
    # absorbed inside the substitution.
    real_home="$(/usr/bin/python3 -c 'import pwd, os; print(pwd.getpwuid(os.getuid()).pw_dir)' 2>/dev/null || true)"
    if [[ -z "$real_home" || "$real_home" == "/var/empty" ]]; then
      real_home="/Users/$(id -un)"
    fi

    export _SIMPOOL_WRAPPED=1
    export HOME="$real_home"
    simpool_with_args=(--device "%(device_type)s" --os "%(os_version)s")
    if [[ -n "%(max_slots)s" ]]; then
      simpool_with_args+=(--max "%(max_slots)s")
    fi
    if [[ -n "%(wait)s" ]]; then
      simpool_with_args+=(--wait "%(wait)s")
    fi
    exec "$simpool_bin" with "${simpool_with_args[@]}" -- "$0" "$@"
  fi
  # simpool: no simpool binary found anywhere on this machine (not at
  # SIMPOOL_BIN, not at either Homebrew prefix, not on $PATH) — fall
  # through to the stock ios_xctestrun_runner-equivalent behavior below, so
  # this rule stays a correct drop-in for anyone who hasn't installed it
  # yet. That fallback is *not* equally safe, though: it reuses one fixed
  # BAZEL_TEST_<type>_<os> simulator by name, so every test action
  # concurrently missing simpool shares that one simulator — precisely the
  # collision this whole rule exists to prevent. Warn loudly rather than
  # let that be discovered from a flaky/racy test run.
  echo "warning: simpool binary not found (checked \$SIMPOOL_BIN, /opt/homebrew/bin, /usr/local/bin, \$PATH) — falling back to a single shared BAZEL_TEST_%(device_type)s_%(os_version)s simulator with no concurrency safety; install simpool (see https://github.com/bitomule/simpool#build) to fix" >&2
fi

if [[ -n "${TEST_PREMATURE_EXIT_FILE:-}" ]]; then
  touch "$TEST_PREMATURE_EXIT_FILE"
fi

if [[ -z "${DEVELOPER_DIR:-}" ]]; then
  echo "error: Missing \$DEVELOPER_DIR" >&2
  exit 1
fi

if [[ -n "${DEBUG_XCTESTRUNNER:-}" ]]; then
  set -x
fi

create_xcresult_bundle="%(create_xcresult_bundle)s"
if [[ -n "${CREATE_XCRESULT_BUNDLE:-}" ]]; then
  create_xcresult_bundle=true
fi

custom_xcodebuild_args=(%(xcodebuild_args)s)
simulator_name=""
device_id=""
command_line_args=(%(command_line_args)s)
attachment_lifetime="%(attachment_lifetime)s"
destination_timeout="%(destination_timeout)s"
while [[ $# -gt 0 ]]; do
  arg="$1"
  case $arg in
    --simulator_name=*)
      simulator_name="${arg##*=}"
      ;;
    --xcodebuild_args=*)
      xcodebuild_arg="${arg#--xcodebuild_args=}" # Strip "--xcodebuild_args=" prefix
      custom_xcodebuild_args+=("$xcodebuild_arg")
      ;;
    --destination=platform=iOS,id=*)
      device_id="${arg##*=}"
      ;;
    --command_line_args=*)
      command_line_args+=("${arg##*=}")
      ;;
    --xctestrun_attachment_lifetime=*)
      attachment_lifetime="${arg##*=}"
      ;;
    *)
      echo "error: Unsupported argument '${arg}'" >&2
      exit 1
      ;;
  esac
  shift
done

# simpool: if a pool slot handed us its simulator's name, target that
# instead of whatever --simulator_name the caller passed (or the
# BAZEL_TEST_<type>_<os> default computed below) — simulator_creator.py
# already reuses purely by device name, so this alone is what lands it on
# the pool's already-booted, already-provisioned simulator instead of a
# brand-new one. simpool_slot_name tells the resolver below that this name
# is not just a default to reuse-or-create — it is a specific slot simpool
# already provisioned, so failing to find it is an error, never a cue to
# create a same-named simulator the pool doesn't know about (see the
# resolver's own comment).
simpool_slot_name=false
if [[ -n "${SIMPOOL_NAME_0:-}" ]]; then
  simulator_name="$SIMPOOL_NAME_0"
  simpool_slot_name=true
fi

# Retrieve the basename of a file or folder with an extension.
basename_without_extension() {
  local filename
  filename=$(basename "$1")
  echo "${filename%.*}"
}

test_tmp_dir="$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/test_tmp_dir.XXXXXX")"
if [[ -z "${NO_CLEAN:-}" ]]; then
  trap 'rm -rf "${test_tmp_dir}"' EXIT
else
  test_tmp_dir="${TMPDIR:-/tmp}/test_tmp_dir"
  rm -rf "$test_tmp_dir"
  mkdir -p "$test_tmp_dir"
  echo "note: keeping test dir around at: $test_tmp_dir"
fi

test_bundle_path="%(test_bundle_path)s"
test_bundle_name=$(basename_without_extension "$test_bundle_path")
test_bundle_binary="$test_tmp_dir/$test_bundle_name.xctest/$test_bundle_name"

if [[ "$test_bundle_path" == *.xctest ]]; then
  cp -cRL "$test_bundle_path" "$test_tmp_dir"
  # Need to modify permissions as Bazel will set all files to non-writable, and
  # Xcode's test runner requires the files to be writable.
  chmod -R 777 "$test_tmp_dir/$test_bundle_name.xctest"
else
  unzip -qq -d "${test_tmp_dir}" "${test_bundle_path}"
fi

# Delta update won't update the binary if it has the same timestamp
touch "$test_bundle_binary"

build_for_device=false
test_execution_platform="iPhoneSimulator.platform"
if [[ -n "$device_id" ]]; then
  test_execution_platform="iPhoneOS.platform"
  build_for_device=true
fi

# In case there is no test host, test_host_path will be empty
test_host_path="%(test_host_path)s"
if [[ -n "$test_host_path" ]]; then
  test_host_name=$(basename_without_extension "$test_host_path")

  if [[ "$test_host_path" == *.app ]]; then
    cp -cRL "$test_host_path" "$test_tmp_dir"
    # Need to modify permissions as Bazel will set all files to non-writable,
    # and Xcode's test runner requires the files to be writable.
    chmod -R 777 "$test_tmp_dir/$test_host_name.app"
  else
    unzip -qq -d "${test_tmp_dir}" "${test_host_path}"
    mv "$test_tmp_dir"/Payload/*.app "$test_tmp_dir"
    # When extracting an ipa file we don't know the name of the app bundle
    test_tmp_dir_test_host_path=$(find "$test_tmp_dir" -name "*.app" -type d -maxdepth 1 -mindepth 1 -print -quit)
    test_host_name=$(basename_without_extension "$test_tmp_dir_test_host_path")
  fi
fi

# Basic XML character escaping for environment variable substitution.
function escape() {
  local escaped=${1//&/&amp;}
  escaped=${escaped//</&lt;}
  escaped=${escaped//>/&gt;}
  escaped=${escaped//'"'/&quot;}
  echo "$escaped"
}

# Gather command line arguments for `CommandLineArguments` in the xctestrun file
xctestrun_cmd_line_args_section=""
if [[ -n "${command_line_args:-}" ]]; then
  xctestrun_cmd_line_args_section="\n"
  saved_IFS=$IFS
  IFS=","
  for cmd_line_arg in ${command_line_args[@]}; do
    xctestrun_cmd_line_args_section+="      <string>$cmd_line_arg</string>\n"
  done
  IFS=$saved_IFS
  xctestrun_cmd_line_args_section="    <key>CommandLineArguments</key>\n    <array>$xctestrun_cmd_line_args_section    </array>"
fi

# Add the test environment variables into the xctestrun file to propagate them
# to the test runner
default_test_env="TEST_PREMATURE_EXIT_FILE=$TEST_PREMATURE_EXIT_FILE,TEST_SRCDIR=$TEST_SRCDIR,TEST_UNDECLARED_OUTPUTS_DIR=$TEST_UNDECLARED_OUTPUTS_DIR,XML_OUTPUT_FILE=$XML_OUTPUT_FILE"
test_env="%(test_env)s"
env_inherit=%(test_env_inherit)s
for env_var in "${env_inherit[@]:-}"; do
  # If the environment variable is set, add it to the test environment
  if declare -p "$env_var" &>/dev/null; then
    if [[ -n "$test_env" ]]; then
      test_env="$test_env,$env_var=${!env_var}"
    else
      test_env="$env_var=${!env_var}"
    fi
  fi
done
if [[ -n "$test_env" ]]; then
  test_env="$test_env,$default_test_env"
else
  test_env="$default_test_env"
fi

passthrough_env=()
xctestrun_env=""
saved_IFS=$IFS
IFS=","
for test_env_key_value in ${test_env}; do
  IFS="=" read -r key value <<< "$test_env_key_value"
  xctestrun_env+="<key>$(escape "$key")</key><string>$(escape "$value")</string>"
  passthrough_env+=("SIMCTL_CHILD_$key=$value")
done
IFS=$saved_IFS

xcrun_target_app_path=""
xcrun_test_host_bundle_identifier=""
xcrun_test_bundle_path="__TESTROOT__/$test_bundle_name.xctest"
xcrun_is_xctrunner_hosted_bundle="false"
xcrun_is_ui_test_bundle="false"
test_type="%(test_type)s"
if [[ -n "$test_host_path" ]]; then
  xctestrun_test_host_path="__TESTROOT__/$test_host_name.app"
  xctestrun_test_host_based=true
  # If this is set in the case there is no test host, some tests hang indefinitely
  xctestrun_env+="<key>XCInjectBundleInto</key><string>$(escape "__TESTHOST__/$test_host_name.app/$test_host_name")</string>"

  developer_path="$(xcode-select -p)/Platforms/$test_execution_platform/Developer"
  libraries_path="$developer_path/Library"

  # Added in Xcode 16.0
  testing_framework_path="$libraries_path/Frameworks/Testing.framework"
  if [[ -d "$testing_framework_path" ]]; then
    xctestrun_env+="<key>DYLD_FRAMEWORK_PATH</key><string>$libraries_path/Frameworks</string>"
  fi

  if [[ "$test_type" = "XCUITEST" ]]; then
    xcrun_is_xctrunner_hosted_bundle="true"
    xcrun_is_ui_test_bundle="true"
    xcrun_target_app_path="$xctestrun_test_host_path"
    # If ui testing is enabled we need to copy out the XCTRunner app, update its info.plist accordingly and finally
    # copy over the needed frameworks to enable ui testing
    readonly runner_app_name="$test_bundle_name-Runner"
    readonly runner_app="$runner_app_name.app"
    readonly runner_app_destination="$test_tmp_dir/$runner_app"
    cp -R "$libraries_path/Xcode/Agents/XCTRunner.app" "$runner_app_destination"
    chmod -R 777 "$runner_app_destination"
    xctestrun_test_host_path="__TESTROOT__/$runner_app"
    xcrun_test_host_bundle_identifier="com.apple.test.$runner_app_name"
    plugins_path="$test_tmp_dir/$runner_app/PlugIns"
    mkdir -p "$plugins_path"
    mv "$test_tmp_dir/$test_bundle_name.xctest" "$plugins_path"
    test_bundle_binary="$plugins_path/$test_bundle_name.xctest/$test_bundle_name"
    mkdir -p "$plugins_path/$test_bundle_name.xctest/Frameworks"
    # We need this dylib for 14.x OSes. This intentionally doesn't use `test_execution_platform`
    # since this file isn't present in the `iPhoneSimulator.platform`.
    # No longer necessary starting in Xcode 15 - hence the `-f` file existence check
    libswift_concurrency_path="$(xcode-select -p)/Platforms/iPhoneOS.platform/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS.simruntime/Contents/Resources/RuntimeRoot/usr/lib/swift/libswift_Concurrency.dylib"
    if [[ -f "$libswift_concurrency_path" ]]; then
      cp "$libswift_concurrency_path" "$plugins_path/$test_bundle_name.xctest/Frameworks/libswift_Concurrency.dylib"
    fi
    xcrun_test_bundle_path="__TESTHOST__/PlugIns/$test_bundle_name.xctest"

    /usr/bin/sed \
      -e "s@\$(WRAPPEDPRODUCTNAME)@XCTRunner@g"\
      -e "s@WRAPPEDPRODUCTNAME@XCTRunner@g"\
      -e "s@\$(WRAPPEDPRODUCTBUNDLEIDENTIFIER)@$xcrun_test_host_bundle_identifier@g"\
      -e "s@WRAPPEDPRODUCTBUNDLEIDENTIFIER@$xcrun_test_host_bundle_identifier@g"\
      -i "" \
      "$runner_app_destination/Info.plist"

    readonly runner_app_frameworks_destination="$runner_app_destination/Frameworks"
    mkdir -p "$runner_app_frameworks_destination"
    cp -R "$libraries_path/Frameworks/XCTest.framework" "$runner_app_frameworks_destination/XCTest.framework"
    cp -R "$libraries_path/PrivateFrameworks/XCTestCore.framework" "$runner_app_frameworks_destination/XCTestCore.framework"
    cp -R "$libraries_path/PrivateFrameworks/XCTAutomationSupport.framework" "$runner_app_frameworks_destination/XCTAutomationSupport.framework"
    cp -R "$libraries_path/PrivateFrameworks/XCUnit.framework" "$runner_app_frameworks_destination/XCUnit.framework"
    cp "$developer_path/usr/lib/libXCTestSwiftSupport.dylib" "$runner_app_frameworks_destination/libXCTestSwiftSupport.dylib"
    cp "$developer_path/usr/lib/libXCTestBundleInject.dylib" "$runner_app_frameworks_destination/libXCTestBundleInject.dylib"
    # Added in Xcode 14.3
    xctestsupport_framework_path="$libraries_path/PrivateFrameworks/XCTestSupport.framework"
    if [[ -d "$xctestsupport_framework_path" ]]; then
      cp -R "$xctestsupport_framework_path" "$runner_app_frameworks_destination/XCTestSupport.framework"
    fi
    # Added in Xcode 16.0
    if [[ -d "$testing_framework_path" ]]; then
      cp -R "$testing_framework_path" "$runner_app_frameworks_destination/Testing.framework"
    fi

    # On Xcode 16.3 and later, XCUIAutomation is not private anymore
    xcuiautomation_path="$libraries_path/Frameworks/XCUIAutomation.framework"
    if [[ ! -d "$xcuiautomation_path" ]]; then
        xcuiautomation_path="$libraries_path/PrivateFrameworks/XCUIAutomation.framework"
    fi
    cp -R "$xcuiautomation_path" "$runner_app_frameworks_destination/XCUIAutomation.framework"

    if [[ "$build_for_device" == true ]]; then
      # XCTRunner is multi-archs. When launching XCTRunner on arm64e device, it
      # will be launched as arm64e process by default. If the test bundle is arm64
      # bundle, the XCTRunner which hosts the test bundle will fail to be
      # launched. So removing the arm64e arch from XCTRunner can resolve this
      # case.
      /usr/bin/lipo "$test_tmp_dir/$runner_app/XCTRunner" -remove arm64e -output "$test_tmp_dir/$runner_app/XCTRunner"
    fi
    test_host_mobileprovision_path="$test_tmp_dir/$test_host_name.app/embedded.mobileprovision"
    # Only engage signing workflow if the test host is signed
    if [[ -f "$test_host_mobileprovision_path" ]]; then
      cp "$test_host_mobileprovision_path" "$test_tmp_dir/$runner_app/embedded.mobileprovision"
      xctrunner_entitlements="$test_tmp_dir/$runner_app/RunnerEntitlements.plist"
      test_host_binary_path="$test_tmp_dir/$test_host_name.app/$test_host_name"
      codesigning_team_identifier=$(codesign -dvv "$test_host_binary_path"  2>&1 >/dev/null | /usr/bin/sed -n  -E 's/TeamIdentifier=(.*)/\1/p')
      codesigning_authority=$(codesign -dvv "$test_host_binary_path"  2>&1 >/dev/null | /usr/bin/sed -n  -E 's/^Authority=(.*)/\1/p'| head -n 1)
      /usr/bin/sed \
        -e "s@BAZEL_CODESIGNING_TEAM_IDENTIFIER@$codesigning_team_identifier@g" \
        -e "s@BAZEL_TEST_HOST_BUNDLE_IDENTIFIER@$xcrun_test_host_bundle_identifier@g" \
        "%(xctrunner_entitlements_template)s" > "$xctrunner_entitlements"
      codesign -f \
        --entitlements "$xctrunner_entitlements" \
        --timestamp=none -s "$codesigning_authority" \
        "$plugins_path/$test_bundle_name.xctest"
      find "$test_tmp_dir/$runner_app/Frameworks" \
        -type d \
        -name "*.framework" \
        -exec codesign -f --timestamp=none -s "$codesigning_authority" --entitlements "$xctrunner_entitlements" {} \;
      find "$test_tmp_dir/$runner_app/Frameworks" \
        -type f \
        -name "*.dylib" \
        -exec codesign -f --timestamp=none -s "$codesigning_authority" --entitlements "$xctrunner_entitlements" {} \;
      codesign -f \
        --entitlements "$xctrunner_entitlements" \
        --timestamp=none \
        -s "$codesigning_authority" \
        "$test_tmp_dir/$runner_app"
    fi
  fi
else
  xctestrun_test_host_path="__PLATFORMS__/$test_execution_platform/Developer/Library/Xcode/Agents/xctest"
  xctestrun_test_host_based=false
fi

sanitizer_dyld_env=""
readonly sanitizer_root="$test_tmp_dir/$test_bundle_name.xctest/Frameworks"
for sanitizer in "$sanitizer_root"/libclang_rt.*.dylib; do
  [[ -e "$sanitizer" ]] || continue

  if [[ -n "$sanitizer_dyld_env" ]]; then
    sanitizer_dyld_env="$sanitizer_dyld_env:"
  fi
  sanitizer_dyld_env="${sanitizer_dyld_env}${sanitizer}"
done

main_thread_checker_dyld_env=""
readonly main_thread_checker_root="$test_tmp_dir/$test_bundle_name.xctest/Frameworks"
main_thread_checker="$main_thread_checker_root/libMainThreadChecker.dylib"
if [[ -e "$main_thread_checker" ]]; then
    main_thread_checker_dyld_env="$main_thread_checker"
fi

xctestrun_libraries=""
if [[ "$test_type" != "XCUITEST" ]]; then
  xctestrun_libraries="__PLATFORMS__/$test_execution_platform/Developer/usr/lib/libXCTestBundleInject.dylib"
fi

if [[ -n "$sanitizer_dyld_env" ]]; then
  if [[ -n "$xctestrun_libraries" ]]; then
    xctestrun_libraries="${xctestrun_libraries}:${sanitizer_dyld_env}"
  else
    xctestrun_libraries="${sanitizer_dyld_env}"
  fi
fi

if [[ -n "$main_thread_checker_dyld_env" ]]; then
  if [[ -n "$xctestrun_libraries" ]]; then
    xctestrun_libraries="${xctestrun_libraries}:${main_thread_checker_dyld_env}"
  else
    xctestrun_libraries="${main_thread_checker_dyld_env}"
  fi
fi

TEST_FILTER="%(test_filter)s"
xctestrun_skip_test_section=""
xctestrun_only_test_section=""

# Use the 'TESTBRIDGE_TEST_ONLY' environment variable set by Bazel's
# '--test_filter' flag to set the xctestrun's skip/only parameters.
#
# Any test prefixed with '-' will be passed to 'SkipTestIdentifiers'. Otherwise
# the tests is passed to 'OnlyTestIdentifiers',
if [[ -n "${TESTBRIDGE_TEST_ONLY:-}" || -n "${TEST_FILTER:-}" ]]; then
  if [[ -n "${TESTBRIDGE_TEST_ONLY:-}" && -n "${TEST_FILTER:-}" ]]; then
    ALL_TESTS="$TESTBRIDGE_TEST_ONLY,$TEST_FILTER"
  elif [[ -n "${TESTBRIDGE_TEST_ONLY:-}" ]]; then
    ALL_TESTS="$TESTBRIDGE_TEST_ONLY"
  else
    ALL_TESTS="$TEST_FILTER"
  fi

  saved_IFS=$IFS
  IFS=","; for TEST in $ALL_TESTS; do
    if [[ $TEST == -* ]]; then
      if [[ -n "${SKIP_TESTS:-}" ]]; then
        SKIP_TESTS+=",${TEST:1}"
      else
        SKIP_TESTS="${TEST:1}"
      fi
    else
      if [[ -n "${ONLY_TESTS:-}" ]]; then
          ONLY_TESTS+=",$TEST"
      else
          ONLY_TESTS="$TEST"
      fi
    fi
  done

  IFS=$saved_IFS

  if [[ -n "${SKIP_TESTS:-}" ]]; then
    xctestrun_skip_test_section="\n"
    for skip_test in ${SKIP_TESTS//,/ }; do
      xctestrun_skip_test_section+="      <string>$skip_test</string>\n"
    done
    xctestrun_skip_test_section="    <key>SkipTestIdentifiers</key>\n    <array>$xctestrun_skip_test_section    </array>"
  fi

  if [[ -n "${ONLY_TESTS:-}" ]]; then
    xctestrun_only_test_section="\n"
    for only_test in ${ONLY_TESTS//,/ }; do
      xctestrun_only_test_section+="      <string>$only_test</string>\n"
    done
    xctestrun_only_test_section="    <key>OnlyTestIdentifiers</key>\n    <array>$xctestrun_only_test_section    </array>"
  fi
fi

readonly profraw="$test_tmp_dir/coverage.profraw"

reuse_simulator=%(reuse_simulator)s
if [[ -n "${SIMPOOL_NAME_0:-}" ]]; then
  # simpool: never let reuse_simulator=false delete a slot's simulator out
  # from under the pool — it's shared and borrowed, not this test's to
  # destroy. It goes back to the pool booted, exactly as simpool left it.
  reuse_simulator=true
fi

# simpool: resolve/create the simulator ourselves instead of shelling out to
# rules_apple's own simulator_creator.py. Its CLI is not a stable contract
# across rules_apple releases — 4.3.3 took `os_version`/`device_type`
# positionally, 4.4.0 replaced that with `--os_version`/`--device_type`
# flags plus SIMULATOR_*-env fallbacks — and since Bazel resolves the whole
# module to one version via MVS (frequently *not* whatever a leaf
# dependency like this one requests: something else in the graph can float
# it higher), a caller's actual resolved version is not this rule's to
# predict. Reimplementing the ~20 lines of `xcrun simctl` this actually
# needs sidesteps that entirely, matching by device name exactly the way
# simulator_creator.py does.
simulator_id="unused"
if [[ "$build_for_device" == false ]]; then
  # macOS's /bin/bash is stuck on 3.2 (GPLv3), which mis-parses a quoted
  # heredoc (`<<'PY' ... PY`) containing an apostrophe when it's nested
  # directly inside a `$(...)` command substitution — "unexpected EOF
  # while looking for matching `''" pointing at the *outer* $( , nowhere
  # near the real apostrophe. Writing the script out first and running it
  # as a separate, unnested command sidesteps the bug entirely.
  simpool_resolver="$test_tmp_dir/simpool_resolve_simulator.py"
  cat > "$simpool_resolver" <<'PY'
import json
import os
import random
import string
import subprocess
import sys
import time


def simctl(*args):
    return subprocess.check_output(["xcrun", "simctl", *args]).decode()


def find_by_name(name, runtime_id):
    # Scoped to the requested runtime, exactly like upstream
    # simulator_creator.py's `devices.get(runtime_identifier)` — searching
    # every runtime bucket (as an earlier version of this resolver did)
    # can reuse-by-name a simulator sitting under an unavailable/mismatched
    # runtime, which upstream never does.
    devices = json.loads(simctl("list", "devices", "-j"))["devices"]
    for device in devices.get(runtime_id, []):
        if device["name"] == name:
            return device
    return None


def find_by_udid(udid):
    devices = json.loads(simctl("list", "devices", "-j"))["devices"]
    for group in devices.values():
        for device in group:
            if device["udid"] == udid:
                return device
    return None


def boot(udid):
    # Mirrors upstream simulator_creator.py's _boot_simulator exactly:
    # `bootstatus` exit 149 means "already booted" (confirmed by checking
    # the device's own state rather than trusted blindly), 164/165 are
    # known-benign "strange but usable" states that get ignored, and
    # anything else re-raises so a genuinely broken simulator fails here
    # instead of surfacing later as an unrelated-looking `xcodebuild`
    # error. The trailing `time.sleep(3)` is also load-bearing upstream
    # behavior: `bootstatus` alone doesn't wait long enough for the
    # simulator to be ready, and upstream added this specifically to stop
    # tests flaking on a simulator that returns before it's truly usable.
    try:
        output = simctl("bootstatus", udid, "-b")
        print(output, file=sys.stderr)
    except subprocess.CalledProcessError as e:
        exit_code = e.returncode
        if exit_code == 149:
            device = find_by_udid(udid)
            if device and device["state"].lower() == "booted":
                print(f"Simulator ({udid}) is already booted", file=sys.stderr)
                exit_code = 0
        if exit_code in (164, 165):
            print(f"Ignoring 'simctl bootstatus' exit code {exit_code}", file=sys.stderr)
        elif exit_code != 0:
            print(f"'simctl bootstatus' exit code {exit_code}", file=sys.stderr)
            raise
    time.sleep(3)


device_type = os.environ["SIMULATOR_DEVICE_TYPE"]
os_version = os.environ["SIMULATOR_OS_VERSION"]
reuse = os.environ.get("SIMULATOR_REUSE_SIMULATOR") == "true"
name = os.environ.get("SIMULATOR_NAME") or f"BAZEL_TEST_{device_type}_{os_version}"
# Set when SIMPOOL_NAME_0 supplied `name` above: it is a specific slot
# simpool already provisioned, not just a default to reuse-or-create.
is_simpool_slot = os.environ.get("SIMPOOL_SLOT_NAME") == "true"
runtime_id = "com.apple.CoreSimulator.SimRuntime.iOS-" + os_version.replace(".", "-")

device = find_by_name(name, runtime_id) if reuse else None
if device is not None:
    # For a simpool slot (is_simpool_slot), this `!= "booted"` branch should
    # be unreachable by the time we get here: the outer `simpool with` that
    # re-exec'd this whole script already called EnsureProvisioned, which
    # blocks on `xcrun simctl bootstatus -b` (plus the same settle margin
    # `boot()` below applies) before ever handing the slot over — see
    # internal/pool/provision.go. Kept anyway as defense-in-depth and as the
    # actual boot path for the non-simpool, reuse-by-name fallback (when
    # `simpool` isn't installed at all, see the file header), where nothing
    # upstream of this script has booted anything yet.
    if device["state"].lower() != "booted":
        boot(device["udid"])
    print(device["udid"])
    sys.exit(0)

if is_simpool_slot:
    # The pool slot's own simulator should already exist under this exact
    # name -- if it doesn't, something is wrong (a stale/mismatched pool,
    # a race, meta.json pointing somewhere real EnsureProvisioned didn't
    # actually reach). Silently creating a new simulator that happens to
    # collide with a deterministic pool slot name is worse than a leak: it
    # produces a device simpool doesn't know the UDID of, sitting under a
    # name simpool's own name-based recovery (EnsureProvisioned) treats as
    # authoritative, and `internal/pool/provision.go` refuses to guess
    # between duplicates -- so that slot goes dead until a human deletes
    # one of the two simulators by hand. Fail loudly instead.
    print(
        f"error: no simulator named '{name}' in runtime {runtime_id} "
        "(the simpool slot should already have provisioned it)",
        file=sys.stderr,
    )
    sys.exit(1)

if not reuse:
    # Reuse is off and there is no fixed pool name to protect -- generate
    # a unique name so this run can never collide with (or get mistaken
    # for) a simulator a concurrent invocation is using.
    name += "_" + "".join(random.choices(string.ascii_letters + string.digits, k=8))

udid = simctl("create", name, device_type, runtime_id).strip()
print(f"Created new simulator '{name}' ({udid})", file=sys.stderr)
boot(udid)
print(udid)
PY
  simulator_id="$(SIMULATOR_NAME="$simulator_name" \
    SIMULATOR_DEVICE_TYPE="%(device_type)s" \
    SIMULATOR_OS_VERSION="%(os_version)s" \
    SIMULATOR_REUSE_SIMULATOR="$reuse_simulator" \
    SIMPOOL_SLOT_NAME="$simpool_slot_name" \
    /usr/bin/python3 "$simpool_resolver")"
fi

test_exit_code=0
readonly testlog=$test_tmp_dir/test.log
test_file=$(file "$test_bundle_binary")

intel_simulator_hack=false
architecture="arm64"
if [[ $(arch) == arm64 && "$test_file" != *arm64* ]]; then
  intel_simulator_hack=true
  architecture="x86_64"
fi

should_use_xcodebuild=false
if [[ "$build_for_device" == true  ]]; then
  echo "note: Using 'xcodebuild' because build for device was requested"
  should_use_xcodebuild=true
fi
if [[ -n "$test_host_path" ]]; then
  echo "note: Using 'xcodebuild' because test host was provided"
  should_use_xcodebuild=true
fi
# shellcheck disable=SC2050
if [[ "%(test_order)s" == random ]]; then
  echo "note: Using 'xcodebuild' because random test order was requested"
  should_use_xcodebuild=true
fi
if [[ "$create_xcresult_bundle" == true ]]; then
  echo "note: Using 'xcodebuild' because XCResult bundle was requested"
  should_use_xcodebuild=true
fi
if [[ -n "$xctestrun_cmd_line_args_section" ]]; then
  echo "note: Using 'xcodebuild' because '--command_line_args' was provided"
  should_use_xcodebuild=true
fi
if [[ -n "$xctestrun_skip_test_section" || -n "$xctestrun_only_test_section" ]]; then
  echo "note: Using 'xcodebuild' because test filter was provided"
  should_use_xcodebuild=true
fi
if (( ${#custom_xcodebuild_args[@]} )); then
  echo "note: Using 'xcodebuild' because '--xcodebuild_args' was provided"
  should_use_xcodebuild=true
fi

# Run a pre-action binary, if provided.
pre_action_binary=%(pre_action_binary)s
SIMULATOR_UDID="$simulator_id" \
  "$pre_action_binary"

if [[ "$should_use_xcodebuild" == true ]]; then
  if [[ -z "$test_host_path" && "$intel_simulator_hack" == true ]]; then
    echo "error: running x86_64 tests on arm64 macs using 'xcodebuild' requires a test host" >&2
    exit 1
  fi

  # Set xctest attachment liftime
  xctestrun_attachment_lifetime_section+="    <key>SystemAttachmentLifetime</key>\n"
  xctestrun_attachment_lifetime_section+="    <string>$attachment_lifetime</string>\n"
  xctestrun_attachment_lifetime_section+="    <key>UserAttachmentLifetime</key>\n"
  xctestrun_attachment_lifetime_section+="    <string>$attachment_lifetime</string>"

  readonly xctestrun_file="$test_tmp_dir/tests.xctestrun"
  /usr/bin/sed \
    -e "s@BAZEL_INSERT_LIBRARIES@$xctestrun_libraries@g" \
    -e "s@BAZEL_TEST_BUNDLE_PATH@$xcrun_test_bundle_path@g" \
    -e "s@BAZEL_TEST_ENVIRONMENT@$xctestrun_env@g" \
    -e "s@BAZEL_TEST_HOST_BASED@$xctestrun_test_host_based@g" \
    -e "s@BAZEL_TEST_HOST_PATH@$xctestrun_test_host_path@g" \
    -e "s@BAZEL_TEST_HOST_BUNDLE_IDENTIFIER@$xcrun_test_host_bundle_identifier@g" \
    -e "s@BAZEL_TEST_PRODUCT_MODULE_NAME@${test_bundle_name//-/_}@g" \
    -e "s@BAZEL_IS_XCTRUNNER_HOSTED_BUNDLE@$xcrun_is_xctrunner_hosted_bundle@g" \
    -e "s@BAZEL_IS_UI_TEST_BUNDLE@$xcrun_is_ui_test_bundle@g" \
    -e "s@BAZEL_TARGET_APP_PATH@$xcrun_target_app_path@g" \
    -e "s@BAZEL_TEST_ORDER_STRING@%(test_order)s@g" \
    -e "s@BAZEL_DYLD_LIBRARY_PATH@__PLATFORMS__/$test_execution_platform/Developer/usr/lib@g" \
    -e "s@BAZEL_COVERAGE_OUTPUT_DIR@$test_tmp_dir@g" \
    -e "s@BAZEL_COMMAND_LINE_ARGS_SECTION@$xctestrun_cmd_line_args_section@g" \
    -e "s@BAZEL_ATTACHMENT_LIFETIME_SECTION@$xctestrun_attachment_lifetime_section@g" \
    -e "s@BAZEL_SKIP_TEST_SECTION@$xctestrun_skip_test_section@g" \
    -e "s@BAZEL_ONLY_TEST_SECTION@$xctestrun_only_test_section@g" \
    -e "s@BAZEL_ARCHITECTURE@$architecture@g" \
    -e "s@BAZEL_TEST_BUNDLE_NAME@$test_bundle_name.xctest@g" \
    -e "s@BAZEL_PRODUCT_PATH@$xcrun_test_bundle_path@g" \
    "%(xctestrun_template)s" > "$xctestrun_file"

  if [[ -n "${DEBUG_XCTESTRUNNER:-}" ]]; then
    echo
    echo "xctestrun contents:"
    cat "$xctestrun_file"
    echo
  fi

  args=(
    -xctestrun "$xctestrun_file" \
  )

  if [[ -n "$destination_timeout" ]]; then
    args+=(-destination-timeout "$destination_timeout")
  fi

  if [[ "$build_for_device" == true ]]; then
    args+=(-destination "platform=iOS,id=$device_id")
  else
    args+=(-destination "id=$simulator_id")
  fi

  readonly result_bundle_path="$TEST_UNDECLARED_OUTPUTS_DIR/tests.xcresult"
  # TEST_UNDECLARED_OUTPUTS_DIR isn't cleaned up with multiple retries of flaky tests
  rm -rf "$result_bundle_path"
  if [[ "$create_xcresult_bundle" == true ]]; then
    args+=(-resultBundlePath "$result_bundle_path")
  fi

  if (( ${#custom_xcodebuild_args[@]} )); then
    args+=("${custom_xcodebuild_args[@]}")
  fi

  xcodebuild test-without-building "${args[@]}" \
    2>&1 | tee -i "$testlog" || test_exit_code=$?
else
  platform_developer_dir="$(xcode-select -p)/Platforms/$test_execution_platform/Developer"
  xctest_binary="$platform_developer_dir/Library/Xcode/Agents/xctest"
  test_file=$(file "$test_tmp_dir/$test_bundle_name.xctest/$test_bundle_name")
  if [[ "$intel_simulator_hack" == true ]]; then
    sliced_xctest_binary="$test_tmp_dir/xctest_intel_bin"
    lipo -thin x86_64 -output "$sliced_xctest_binary" "$xctest_binary"
    xctest_binary=$sliced_xctest_binary
  fi

  SIMCTL_CHILD_DYLD_LIBRARY_PATH="$platform_developer_dir/usr/lib" \
    SIMCTL_CHILD_DYLD_FALLBACK_FRAMEWORK_PATH="$platform_developer_dir/Library/Frameworks" \
    SIMCTL_CHILD_DYLD_INSERT_LIBRARIES="$sanitizer_dyld_env" \
    SIMCTL_CHILD_LLVM_PROFILE_FILE="$profraw" \
    env "${passthrough_env[@]}" \
    xcrun simctl \
    spawn \
    "$simulator_id" \
    "$xctest_binary" \
    -XCTest All \
    "$test_tmp_dir/$test_bundle_name.xctest" \
    2>&1 | tee -i "$testlog" || test_exit_code=$?
fi

# simpool: distinguish "the test failed" from "the simulator is unusable".
# An ordinary assertion failure leaves the simulator alone (still Booted,
# still responsive) — nothing below fires for that, common, case. An
# infrastructure failure (CoreSimulator crashing, SpringBoard dying
# mid-run) can leave the device in a state `simctl list` still reports as
# "Booted" while it never responds to anything again, which
# EnsureProvisioned's own fast path (internal/pool/provision.go) would
# otherwise trust blindly on the NEXT acquisition — handing a wedged
# device straight to whoever asks next with no boot wait at all. Only
# probed on a failed run (never the happy path, so this costs nothing when
# tests pass) and only for a pool-owned slot (SIMPOOL_NAME_0 unset means
# the non-pool fallback path, which never reuses this simulator anyway).
# The probe itself is read-only (mirrors internal/simctl.SpringBoardReady's
# own readiness check); only a failed probe triggers the shutdown.
#
# Both calls run through simpool_run_bounded: a device already suspected of
# being wedged is exactly the device a plain, unbounded `xcrun simctl` call
# is most likely to hang against — and unlike every bounded simctl call on
# the Go side, this test action holds its pool slot's flock for its entire
# run, so a hang here would strand that slot until Bazel's own test timeout
# kills the action, turning "the simulator is unusable" into "the pool has
# lost a slot" — the opposite of what this probe exists to prevent.
if [[ "$test_exit_code" -ne 0 && -n "${SIMPOOL_NAME_0:-}" ]]; then
  wedge_probe_timeout="${SIMPOOL_WEDGE_PROBE_TIMEOUT:-15}"
  if ! simpool_run_bounded "$wedge_probe_timeout" xcrun simctl spawn "$simulator_id" launchctl list >/dev/null 2>&1; then
    echo "warning: simulator $simulator_id no longer responds (or didn't answer within ${wedge_probe_timeout}s) after a failed test run — shutting it down so the pool re-verifies it before handing it to the next consumer, instead of reusing it as-is" >&2
    simpool_run_bounded "$wedge_probe_timeout" xcrun simctl shutdown "$simulator_id" >/dev/null 2>&1 || true
  fi
fi

# Run a post-action binary, if provided.
post_action_binary=%(post_action_binary)s
post_action_determines_exit_code="%(post_action_determines_exit_code)s"
post_action_exit_code=0
if [[ -n "${result_bundle_path:-}" ]]; then
  TEST_EXIT_CODE=$test_exit_code \
    TEST_LOG_FILE="$testlog" \
    SIMULATOR_UDID="$simulator_id" \
    TEST_XCRESULT_BUNDLE_PATH="$result_bundle_path" \
    "$post_action_binary" || post_action_exit_code=$?
else
  TEST_EXIT_CODE=$test_exit_code \
    TEST_LOG_FILE="$testlog" \
    SIMULATOR_UDID="$simulator_id" \
    "$post_action_binary" || post_action_exit_code=$?
fi

if [[ "$post_action_determines_exit_code" == true ]]; then
  if [[ "$post_action_exit_code" -ne 0 ]]; then
    echo "error: post_action exited with '$post_action_exit_code'" >&2
    exit "$post_action_exit_code"
  fi
fi

if [[
  "$test_exit_code" -eq 0 &&
  "$create_xcresult_bundle" == true &&
  "${KEEP_XCRESULT_ON_SUCCESS:-1}" != "1"
]]; then
  # Reduce download size by removing the xcresult bundle if the test run was successful
  rm -r "$result_bundle_path"
fi

if [[ "$reuse_simulator" == false ]]; then
  # Delete will shutdown down the simulator if it's still currently running.
  xcrun simctl delete "$simulator_id"
fi

profdata="$test_tmp_dir/$simulator_id/Coverage.profdata"
if [[ "$should_use_xcodebuild" == false ]]; then
  profdata="$test_tmp_dir/coverage.profdata"
fi

if [[ "${COLLECT_PROFDATA:-0}" == "1" && -f "$profdata" ]]; then
  cp -R "$profdata" "$TEST_UNDECLARED_OUTPUTS_DIR"
fi

if [[ "$post_action_determines_exit_code" == true ]]; then
  if [[ "$post_action_exit_code" -ne 0 ]]; then
    echo "error: post_action exited with '$post_action_exit_code'" >&2
    exit "$post_action_exit_code"
  fi
else
  if [[ "$test_exit_code" -ne 0 ]]; then
    echo "error: tests exited with '$test_exit_code'" >&2
    exit "$test_exit_code"
  fi
fi

if [[ "${ERROR_ON_NO_TESTS_RAN:-1}" == "1" ]]; then
  parallel_testing_enabled=false
  if grep -q "-parallel-testing-enabled YES" "$testlog"; then
    parallel_testing_enabled=true
  fi

  # Fail when bundle executes nothing
  no_tests_ran=false
  if [[ $parallel_testing_enabled == true ]]; then
    echo "Parallel testing is enabled" >&2
    # When executing tests in parallel, test start markers are absent when no
    # tests are run.
    test_execution_count=$(grep -c -e "Test suite '.*' started.*" "$testlog")
    if [[ "$test_execution_count" == "0" ]]; then
      no_tests_ran=true
    fi
  else
    echo "Testing is serialized" >&2
    # Assume the final 'Executed N tests' or 'Executed 1 test' is the
    # total execution count for the test bundle.
    xctest_target_execution_count=$(grep -e "Executed [[:digit:]]\{1,\} test.*," "$testlog" | tail -n1)
    swift_testing_target_execution_count=$(grep -e "Test run with [[:digit:]]\{1,\} test.*" "$testlog" | tail -n1 || true)
    if echo "$xctest_target_execution_count" | grep -q -e "Executed 0 tests, with 0 failures" && \
      [ -z "$swift_testing_target_execution_count" ] ; then
      echo "No tests ran -> no count lines found" >&2
      no_tests_ran=true
    fi

    if echo "$xctest_target_execution_count" | grep -q -e "Executed 0 tests, with 0 failures" && \
      echo "$swift_testing_target_execution_count" | grep -q -e "Test run with 0 tests" ; then
      echo "No tests ran -> count line was 0" >&2
      no_tests_ran=true
    fi
  fi

  if [[ $no_tests_ran == true ]]; then
    echo "error: no tests were executed, is the test bundle empty?" >&2
    exit 1
  fi
fi

# When tests crash after they have reportedly completed, XCTest marks them as
# a success. These 2 cases are Swift fatalErrors, and C++ exceptions. There
# are likely other cases we can add to this in the future. FB7801959
if grep -q \
  -e "^Fatal error:" \
  -e "^.*:[0-9]\+:\sFatal error:" \
  -e "^libc++abi.dylib: terminating with uncaught exception" \
  "$testlog"
then
  echo "error: log contained test false negative" >&2
  exit 1
fi

if [[ "${COVERAGE:-}" -ne 1 || "${APPLE_COVERAGE:-}" -ne 1 ]]; then
  # Normal tests run without coverage
  if [[ -f "${TEST_PREMATURE_EXIT_FILE:-}" ]]; then
    rm -f "$TEST_PREMATURE_EXIT_FILE"
  fi

  exit 0
fi

if [[ "$should_use_xcodebuild" == false ]]; then
  xcrun llvm-profdata merge "$profraw" --output "$profdata"
fi

lcov_args=(
  -instr-profile "$profdata"
  -ignore-filename-regex='.*external/.+'
  -path-equivalence=".,$PWD"
)
has_binary=false
IFS=";"
arch=$(uname -m)
for binary in $TEST_BINARIES_FOR_LLVM_COV; do
  if [[ "$has_binary" == false ]]; then
    lcov_args+=("${binary}")
    has_binary=true
    if ! file "$binary" | grep -q "$arch"; then
      arch=x86_64
    fi
  else
    lcov_args+=(-object "${binary}")
  fi

  lcov_args+=("-arch=$arch")
done

llvm_coverage_manifest="$COVERAGE_MANIFEST"
readonly provided_coverage_manifest="%(test_coverage_manifest)s"
if [[ -s "${provided_coverage_manifest:-}" ]]; then
  llvm_coverage_manifest="$provided_coverage_manifest"
fi

readonly error_file="$test_tmp_dir/llvm-cov-error.txt"
llvm_cov_status=0
xcrun llvm-cov \
  export \
  -format lcov \
  "${lcov_args[@]}" \
  @"$llvm_coverage_manifest" \
  > "$COVERAGE_OUTPUT_FILE" \
  2> "$error_file" \
  || llvm_cov_status=$?

# Error ourselves if lcov outputs warnings, such as if we misconfigure
# something and the file path of one of the covered files doesn't exist
if [[ -s "$error_file" || "$llvm_cov_status" -ne 0 ]]; then
  echo "error: while exporting coverage report" >&2
  cat "$error_file" >&2
  exit 1
fi

if [[ -n "${COVERAGE_PRODUCE_JSON:-}" ]]; then
  llvm_cov_json_export_status=0
  xcrun llvm-cov \
    export \
    -format text \
    "${lcov_args[@]}" \
    @"$llvm_coverage_manifest" \
    > "$TEST_UNDECLARED_OUTPUTS_DIR/coverage.json" \
    2> "$error_file" \
    || llvm_cov_json_export_status=$?
  if [[ -s "$error_file" || "$llvm_cov_json_export_status" -ne 0 ]]; then
    echo "error: while exporting json coverage report" >&2
    cat "$error_file" >&2
    exit 1
  fi
fi

if [[ -f "${TEST_PREMATURE_EXIT_FILE:-}" ]]; then
  rm -f "$TEST_PREMATURE_EXIT_FILE"
fi
