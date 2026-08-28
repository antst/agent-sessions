#!/usr/bin/env python3
"""Verify the canonical executable and isolated live-hub boundary.

The stateful protocol matrix runs in the Go federation and daemon integration
packages. This wrapper starts one script-owned hub, exercises its actual
status/doctor/probe surfaces, and never starts a host daemon.
"""

from __future__ import annotations

import json
import os
import pathlib
import socket
import subprocess
import sys
import tempfile
import time


def fail(message: str) -> None:
    raise AssertionError(message)


def executable(path_text: str, expected_name: str) -> pathlib.Path:
    path = pathlib.Path(path_text).resolve()
    if path.name != expected_name:
        fail(f"expected canonical {expected_name} image, got {path.name}")
    stat = path.stat()
    if not path.is_file() or path.is_symlink() or not os.access(path, os.X_OK):
        fail(f"canonical image is not an executable regular file: {path}")
    if stat.st_size == 0:
        fail(f"canonical image is empty: {path}")
    return path


def run_json(command: list[str], environment: dict[str, str]) -> dict:
    completed = subprocess.run(
        command, check=True, text=True, env=environment,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return json.loads(completed.stdout)


def live_hub_contract(hub: pathlib.Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="agent-sessions-federation-") as root_text:
        root = pathlib.Path(root_text)
        home = root / "home"
        configuration = root / "configuration"
        state = root / "state"
        for directory in (home, configuration, state):
            directory.mkdir(mode=0o700)
        environment = os.environ.copy()
        environment.update({
            "HOME": str(home),
            "XDG_CONFIG_HOME": str(configuration),
            "XDG_STATE_HOME": str(state),
        })
        process = subprocess.Popen(
            [str(hub), "--listen", "127.0.0.1:0"],
            text=True, env=environment,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        try:
            deadline = time.monotonic() + 10
            status = None
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    stdout, stderr = process.communicate()
                    fail(f"isolated hub exited before readiness: stdout={stdout!r} stderr={stderr!r}")
                try:
                    candidate = run_json([str(hub), "status", "--json"], environment)
                except (subprocess.CalledProcessError, json.JSONDecodeError):
                    time.sleep(0.05)
                    continue
                if candidate.get("event") == "hub.status":
                    status = candidate
                    break
            if status is None:
                fail("isolated hub did not publish status before the acceptance deadline")
            metadata = status.get("metadata", {})
            listener = metadata.get("listener")
            if not isinstance(listener, str) or not listener:
                fail(f"isolated hub status omitted listener metadata: {status}")
            doctor = run_json([str(hub), "doctor", "--json"], environment)
            if doctor.get("event") != "hub.doctor" or doctor.get("metadata", {}).get("healthy") is not True:
                fail(f"isolated hub doctor was not healthy: {doctor}")
            host, port_text = listener.rsplit(":", 1)
            with socket.create_connection((host, int(port_text)), timeout=2) as connection:
                connection.sendall(b'{"type":"probe","version":3}\n')
                reply = connection.makefile("rb").readline()
            probe = json.loads(reply)
            if probe != {"type": "probe_ok", "version": 3}:
                fail(f"isolated hub protocol probe changed: {probe}")
            return {
                "status": True,
                "doctor": True,
                "probe": True,
                "protocol_version": probe["version"],
            }
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        raise SystemExit(
            f"usage: {argv[0]} ABSOLUTE_AGENT_SESSIONS ABSOLUTE_AGENT_SESSIONS_HUB"
        )
    host = executable(argv[1], "agent-sessions")
    hub = executable(argv[2], "agent-sessions-hub")

    host_help = subprocess.run(
        [str(host), "help", "--json"], check=True, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    document = json.loads(host_help.stdout)
    if document.get("ok") is not True:
        fail(f"canonical host help failed: {document}")
    binaries = document.get("result", {}).get("binaries")
    if binaries != ["agent-sessions", "agent-sessions-hub"]:
        fail(f"canonical executable inventory changed: {binaries}")

    hub_help = subprocess.run(
        [str(hub), "--help"], check=True, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    if "agent-sessions-hub" not in hub_help.stdout or "--listen" not in hub_help.stdout:
        fail("canonical hub help omitted its serve/listen contract")

    live_hub = live_hub_contract(hub)

    print(json.dumps({
        "type": "unified.federation.integration.passed",
        "host_image": host.name,
        "hub_image": hub.name,
        "logical_protocol_tests": True,
        "production_host_daemons_started": 0,
        "production_hubs_started": 1,
        "test_owned_hub": True,
        "hub_status": live_hub["status"],
        "hub_doctor": live_hub["doctor"],
        "hub_probe": live_hub["probe"],
        "protocol_version": live_hub["protocol_version"],
        "second_user_daemon": False,
    }, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
