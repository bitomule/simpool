#!/bin/bash
# simulator_locator.sh — the `create_simulator_action` for simpool_ios_test_runner.
#
# `ios_xctestrun_runner`'s stock `create_simulator_action` (Google's
# simulator_creator.py) is invoked by the runner's template.sh with no
# arguments — only SIMULATOR_DEVICE_TYPE/SIMULATOR_OS_VERSION/
# SIMULATOR_REUSE_SIMULATOR in the environment, never a simulator name — so
# left alone it reuses-or-creates a fixed "BAZEL_TEST_<type>_<os>" simulator
# of its own, never the slot simpool already holds the flock on.
#
# When SIMPOOL_NAME_0 is set (exported by `simpool with`), this resolves
# that name to its UDID instead — the slot's simulator, already provisioned
# and booted, so nothing new is ever created. When it isn't set — a bare
# `bazel test`, musts, CI, a dev who forgot `simpool with` — this falls back
# to the exact reuse-or-create-by-fixed-name behavior of the stock
# simulator_creator.py, so nothing regresses for callers not yet using
# simpool.
set -euo pipefail

exec /usr/bin/python3 - <<'PY'
import json
import os
import subprocess
import sys


def simctl(*args):
    return subprocess.check_output(["xcrun", "simctl", *args]).decode()


def find_by_name(name):
    devices = json.loads(simctl("list", "devices", "-j"))["devices"]
    for group in devices.values():
        for device in group:
            if device["name"] == name:
                return device
    return None


def boot(udid):
    # Best-effort: mirrors simulator_creator.py's tolerance of "already
    # booted" and a handful of benign simctl error codes. Output goes to
    # stderr — this script's stdout must be only the UDID.
    subprocess.run(
        ["xcrun", "simctl", "bootstatus", udid, "-b"],
        stderr=subprocess.STDOUT,
        stdout=sys.stderr,
    )


simpool_name = os.environ.get("SIMPOOL_NAME_0")
if simpool_name:
    device = find_by_name(simpool_name)
    if device is None:
        print(
            f"error: no simulator named '{simpool_name}' in the default "
            "device set (the simpool slot should already have provisioned "
            "it)",
            file=sys.stderr,
        )
        sys.exit(1)
    if device["state"].lower() != "booted":
        boot(device["udid"])
    print(device["udid"])
    sys.exit(0)

# Fallback path: no simpool slot in play. Behave exactly like
# rules_apple's stock simulator_creator.py so a bare `bazel test` (no
# `simpool with`) still works.
device_type = os.environ["SIMULATOR_DEVICE_TYPE"]
os_version = os.environ["SIMULATOR_OS_VERSION"]
reuse = os.environ.get("SIMULATOR_REUSE_SIMULATOR") is not None
fallback_name = f"BAZEL_TEST_{device_type}_{os_version}"
runtime_id = "com.apple.CoreSimulator.SimRuntime.iOS-" + os_version.replace(".", "-")

device = find_by_name(fallback_name) if reuse else None
if device is not None:
    if device["state"].lower() != "booted":
        boot(device["udid"])
    print(device["udid"])
    sys.exit(0)

udid = simctl("create", fallback_name, device_type, runtime_id).strip()
print(f"Created new simulator '{fallback_name}' ({udid})", file=sys.stderr)
boot(udid)
print(udid)
PY
