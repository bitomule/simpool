#!/bin/bash
# simulator_locator.sh — the `create_simulator_action` for simpool_ios_test_runner.
#
# `ios_xctestrun_runner`'s stock `create_simulator_action` (Google's
# simulator_creator.py) is invoked by the runner's template.sh with no
# arguments — only SIMULATOR_DEVICE_TYPE/SIMULATOR_OS_VERSION/
# SIMULATOR_REUSE_SIMULATOR in the environment, never a simulator name — so
# left alone it reuses-or-creates a fixed "BAZEL_TEST_<type>_<os>" simulator
# of its own, never the slot simpool already holds the flock on. This
# script looks up that slot's simulator by name instead, using
# SIMPOOL_NAME_0 — the environment variable `simpool with` exports for slot
# 0 — so the test runs on the pooled simulator and nothing new is created.
set -euo pipefail

name="${SIMPOOL_NAME_0:-}"
if [[ -z "$name" ]]; then
  echo "error: SIMPOOL_NAME_0 is not set — run this test under \`simpool with --device <device> --os <os> -- bazel test <target>\`" >&2
  exit 1
fi

udid=$(/usr/bin/python3 - "$name" <<'PY'
import json
import subprocess
import sys

name = sys.argv[1]
devices = json.loads(subprocess.check_output(["xcrun", "simctl", "list", "devices", "-j"]))["devices"]
for group in devices.values():
    for device in group:
        if device["name"] == name:
            print(device["udid"])
            sys.exit(0)
sys.exit(1)
PY
) || {
  echo "error: no simulator named '$name' in the default device set (the simpool slot should already have provisioned it)" >&2
  exit 1
}

# `simpool with` already boots the slot's simulator before handing out its
# name, so this is normally a fast no-op; kept as a defensive fallback for a
# slot some other consumer left shut down between `with` and this action
# actually running.
xcrun simctl bootstatus "$udid" -b >&2 || true

echo "$udid"
