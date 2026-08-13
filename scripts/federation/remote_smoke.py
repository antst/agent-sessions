#!/usr/bin/env python3
"""Run a deterministic peer round trip across this host and one SSH host."""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import shlex
import shutil
import socket
import subprocess
import tempfile
import time


def run(command, **kwargs):
    return subprocess.run(command, check=True, text=True, **kwargs)


def wait_for(predicate, timeout=20.0, description="condition"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.1)
    raise AssertionError(f"timed out waiting for {description}")


def local_shadow(registry: pathlib.Path, peer_id: str):
    if not registry.exists():
        return None
    for path in registry.glob("*.json"):
        try:
            row = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        if row.get("federatedBy") == "peer-federator" and row.get("federatedPeerId") == peer_id:
            return row
    return None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--remote", required=True)
    parser.add_argument("--local-host", default=socket.gethostname())
    parser.add_argument("--local-ip", required=True)
    parser.add_argument("--binary", default="peer-federator")
    args = parser.parse_args()

    binary_path = shutil.which(args.binary) or args.binary
    binary = str(pathlib.Path(binary_path).resolve())
    fixture = str((pathlib.Path(__file__).parent / "peer_fixture.py").resolve())
    remote_name = args.remote.split(".", 1)[0]
    local_root = pathlib.Path(tempfile.mkdtemp(prefix="peer-federator-lan-", dir="/tmp"))
    remote_root = run(
        ["ssh", "-o", "BatchMode=yes", args.remote, "mktemp -d /tmp/peer-federator-lan.XXXXXX"],
        capture_output=True,
    ).stdout.strip()
    if not remote_root.startswith("/tmp/peer-federator-lan."):
        raise AssertionError(f"unexpected remote temp path: {remote_root}")
    local_processes: list[subprocess.Popen] = []
    remote_pids: list[int] = []
    local_logs = []

    def local_start(name, command):
        log = (local_root / f"{name}.log").open("wb")
        local_logs.append(log)
        process = subprocess.Popen(command, stdout=log, stderr=log)
        local_processes.append(process)
        return process

    def ssh(command, capture=False):
        return run(
            ["ssh", "-o", "BatchMode=yes", args.remote, "bash", "-lc", shlex.quote(command)],
            capture_output=capture,
        )

    def remote_start(command, log_name):
        script = (
            f"cd {shlex.quote(remote_root)}; "
            f"nohup {command} >{shlex.quote(log_name)} 2>&1 </dev/null & "
            "pid=$!; echo $pid"
        )
        pid = int(ssh(script, capture=True).stdout.strip())
        remote_pids.append(pid)
        return pid

    try:
        run(["scp", "-q", binary, fixture, f"{args.remote}:{remote_root}/"])
        ssh(f"chmod 0755 {shlex.quote(remote_root)}/peer-federator {shlex.quote(remote_root)}/peer_fixture.py")

        probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        probe.bind(("0.0.0.0", 0))
        port = probe.getsockname()[1]
        probe.close()

        hub = local_start("hub", [binary, "hub", "--listen", f"0.0.0.0:{port}"])

        def hub_ready():
            if hub.poll() is not None:
                return False
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    return True
            except OSError:
                return False

        wait_for(hub_ready, description="local hub")

        local_config = local_root / "config"
        local_registry = local_config / "sessions"
        local_socket = local_root / "local-peer.sock"
        local_capture = local_root / "local-capture.jsonl"
        local_fixture = local_start(
            "fixture",
            [
                "python3", fixture, "serve", "--registry-dir", str(local_registry),
                "--socket", str(local_socket), "--session", "lan-local-session",
                "--name", "lan-local-interactive", "--capture", str(local_capture),
                "--entrypoint", "claude",
            ],
        )
        wait_for(lambda: local_socket.exists(), description="local fixture")
        local_agent = local_start(
            "agent",
            [
                binary, "agent", "--hub", f"127.0.0.1:{port}",
                "--host", "host-local", "--name", args.local_host,
                "--claude-config-dir", str(local_config),
                "--runtime-dir", str(local_root / "runtime"),
            ],
        )

        remote_fixture_pid = remote_start(
            "python3 ./peer_fixture.py serve --registry-dir ./config/sessions "
            "--socket ./remote-peer.sock --session lan-remote-session "
            "--name lan-remote-interactive --capture ./remote-capture.jsonl "
            "--reply REPLY_FROM_REMOTE --entrypoint codex",
            "fixture.log",
        )
        remote_start(
            "./peer-federator agent "
            f"--hub {shlex.quote(args.local_ip + ':' + str(port))} "
            f"--host host-remote --name {shlex.quote(remote_name)} "
            f"--claude-config-dir {shlex.quote(remote_root + '/config')} "
            f"--runtime-dir {shlex.quote(remote_root + '/runtime')}",
            "agent.log",
        )

        remote_shadow = wait_for(
            lambda: local_shadow(local_registry, "host-remote/lan-remote-session"),
            description="remote peer shadow on local host",
        )
        assert remote_shadow["name"] == f"lan-remote-interactive--{remote_name}"

        run(
            [
                "python3", fixture, "send", "--socket", str(local_socket),
                "--session", "lan-local-session", "--name", "lan-local-interactive",
                "--target", remote_shadow["messagingSocketPath"],
                "--message-id", "lan-a-to-b", "--message", "HELLO_ACROSS_VLAN",
            ]
        )

        def local_reply():
            if not local_capture.exists() or not local_capture.stat().st_size:
                return None
            rows = [json.loads(line) for line in local_capture.read_text().splitlines() if line]
            return next((row for row in rows if "REPLY_FROM_REMOTE" in row.get("message", {}).get("content", "")), None)

        reply = wait_for(local_reply, description="remote reply on local peer")
        assert reply["from"] == "uds:" + remote_shadow["messagingSocketPath"]

        remote_capture = ssh(f"cat {shlex.quote(remote_root)}/remote-capture.jsonl", capture=True).stdout
        assert "HELLO_ACROSS_VLAN" in remote_capture

        ssh(f"kill {remote_fixture_pid}")
        remote_pids.remove(remote_fixture_pid)
        wait_for(
            lambda: local_shadow(local_registry, "host-remote/lan-remote-session") is None,
            description="remote session removal across VLAN",
        )
        assert local_agent.poll() is None and local_fixture.poll() is None and hub.poll() is None
        print(
            f"remote smoke PASS: {args.local_host} -> {args.remote} -> {args.local_host}, qualified discovery, "
            "reply rewriting, and remote session cleanup"
        )
    except Exception:
        for log in local_logs:
            log.flush()
        for path in sorted(local_root.glob("*.log")):
            print(f"===== local {path.name} =====")
            print(path.read_text(errors="replace"))
        try:
            print("===== remote logs =====")
            print(ssh(f"for f in {shlex.quote(remote_root)}/*.log; do echo ====\"$f\"; cat \"$f\"; done", capture=True).stdout)
        except Exception as log_error:
            print(f"could not read remote logs: {log_error}")
        raise
    finally:
        if remote_pids:
            quoted = " ".join(str(pid) for pid in remote_pids)
            subprocess.run(
                ["ssh", "-o", "BatchMode=yes", args.remote, "bash", "-lc", shlex.quote(f"kill {quoted} 2>/dev/null || true")],
                check=False,
            )
            time.sleep(1)
        subprocess.run(
            [
                "ssh", "-o", "BatchMode=yes", args.remote, "bash", "-lc",
                shlex.quote(f"case {shlex.quote(remote_root)} in /tmp/peer-federator-lan.*) rm -rf -- {shlex.quote(remote_root)};; esac"),
            ],
            check=False,
        )
        for process in reversed(local_processes):
            if process.poll() is None:
                process.terminate()
        for process in reversed(local_processes):
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
        for log in local_logs:
            log.close()
        shutil.rmtree(local_root, ignore_errors=True)


if __name__ == "__main__":
    main()
