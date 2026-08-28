#!/usr/bin/env python3
"""Exercise real distinct-build host/hub handshakes in disposable roots."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time


def fail(message: str) -> None:
    raise AssertionError(message)


def run_json(command: list[str], environment: dict[str, str]) -> dict:
    completed = subprocess.run(
        command, check=True, text=True, env=environment,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return json.loads(completed.stdout)


def wait_hub_status(
    hub: pathlib.Path, environment: dict[str, str], process: subprocess.Popen[str]
) -> dict:
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if process.poll() is not None:
            stdout, stderr = process.communicate()
            fail(f"binary-pair hub exited before status: stdout={stdout!r} stderr={stderr!r}")
        try:
            status = run_json([str(hub), "status", "--json"], environment)
        except (subprocess.CalledProcessError, json.JSONDecodeError):
            time.sleep(0.05)
            continue
        metadata = status.get("metadata", {})
        if status.get("event") == "hub.status" and metadata.get("pid", 0) > 0:
            return metadata
        time.sleep(0.05)
    fail("binary-pair hub did not publish status")


def write_host_configuration(root: pathlib.Path, listener: str, host_id: str) -> None:
    configuration_root = root / "configuration" / "agent-sessions"
    state_root = root / "state" / "agent-sessions"
    runtime_root = root / "runtime" / "agent-sessions"
    if sys.platform == "darwin":
        runtime_root = pathlib.Path("/tmp") / f"agent-sessions-{os.getuid()}"
    configuration_root.mkdir(parents=True, mode=0o700)
    configuration = {
        "schema_version": 1,
        "host_id": host_id,
        "host_name": host_id,
        "hub_address": listener,
        "remote_lanes_enabled": False,
        "state_root": str(state_root),
        "runtime_root": str(runtime_root),
        "revision": 1,
        "updated_at": int(time.time() * 1000),
    }
    path = configuration_root / "config.json"
    path.write_text(json.dumps(configuration, separators=(",", ":")))
    path.chmod(0o600)


def stop(process: subprocess.Popen[str]) -> None:
    if process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def run_pair(host: pathlib.Path, hub: pathlib.Path, host_id: str, compatible: bool) -> dict:
    with tempfile.TemporaryDirectory(prefix="agent-sessions-binary-pair-") as root_text:
        root = pathlib.Path(os.path.realpath(root_text))
        for name in ("home", "configuration", "state", "runtime"):
            (root / name).mkdir(mode=0o700)
        environment = os.environ.copy()
        environment.update({
            "HOME": str(root / "home"),
            "XDG_CONFIG_HOME": str(root / "configuration"),
            "XDG_STATE_HOME": str(root / "state"),
            "XDG_RUNTIME_DIR": str(root / "runtime"),
        })
        hub_process = subprocess.Popen(
            [str(hub), "--listen", "127.0.0.1:0"], env=environment, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        host_process: subprocess.Popen[str] | None = None
        try:
            hub_status = wait_hub_status(hub, environment, hub_process)
            listener = hub_status.get("listener")
            if not isinstance(listener, str) or not listener:
                fail(f"binary-pair hub omitted listener: {hub_status}")
            write_host_configuration(root, listener, host_id)
            host_process = subprocess.Popen(
                [str(host), "daemon"], env=environment, text=True,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
            deadline = time.monotonic() + 12
            host_state = ""
            connected_hosts = -1
            while time.monotonic() < deadline:
                if hub_process.poll() is not None:
                    stdout, stderr = hub_process.communicate()
                    fail(f"binary-pair hub exited: stdout={stdout!r} stderr={stderr!r}")
                if host_process.poll() is not None:
                    stdout, stderr = host_process.communicate()
                    fail(f"binary-pair host exited: stdout={stdout!r} stderr={stderr!r}")
                try:
                    hub_status = run_json([str(hub), "status", "--json"], environment).get("metadata", {})
                    connected_hosts = hub_status.get("connected_hosts", -1)
                    host_status = run_json([str(host), "status", "--json"], environment)
                    host_state = host_status.get("result", {}).get("federation", {}).get("state", "")
                except (subprocess.CalledProcessError, json.JSONDecodeError):
                    time.sleep(0.05)
                    continue
                if compatible and connected_hosts == 1 and host_state == "connected":
                    break
                if not compatible and connected_hosts == 0 and host_state == "incompatible":
                    break
                time.sleep(0.05)
            else:
                fail(
                    f"binary-pair convergence failed compatible={compatible} "
                    f"connected_hosts={connected_hosts} host_state={host_state!r}"
                )
            return {
                "host_state": host_state,
                "connected_hosts": connected_hosts,
                "host_runtime_version": host_status["result"]["runtime_version"],
                "hub_runtime_version": hub_status["runtime_version"],
            }
        finally:
            if host_process is not None:
                stop(host_process)
            stop(hub_process)


def main(argv: list[str]) -> int:
    if len(argv) != 6:
        raise SystemExit(
            f"usage: {argv[0]} HOST_A HUB_A HOST_B HUB_B MISMATCH_HUB"
        )
    host_a, hub_a, host_b, hub_b, mismatch_hub = [pathlib.Path(value).resolve() for value in argv[1:]]
    for path in (host_a, hub_a, host_b, hub_b, mismatch_hub):
        if not path.is_file() or path.is_symlink() or not os.access(path, os.X_OK):
            fail(f"binary-pair image is not an executable regular file: {path}")
    first = run_pair(host_a, hub_b, "pair-host-a", True)
    second = run_pair(host_b, hub_a, "pair-host-b", True)
    mismatch = run_pair(host_a, mismatch_hub, "pair-host-mismatch", False)
    print(json.dumps({
        "type": "unified.federation.binary_pairs.passed",
        "equal_protocol_forward": first,
        "equal_protocol_reverse": second,
        "protocol_mismatch": mismatch,
    }, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
