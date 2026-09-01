#!/usr/bin/env python3
"""Exercise production federation binaries across independent host/hub upgrades.

With no binary arguments this builds four same-tree smoke binaries with distinct
build identities. Acceptance jobs can instead pass unrelated prebuilt images;
the protocol assertions and upgrade sequence are identical in both modes.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import shutil
import signal
import socket
import subprocess
import tempfile
import time
from typing import BinaryIO


PROTOCOL_VERSION = 3
START_TIMEOUT = 20.0
MISMATCH_HOST_ID = "mismatch-must-not-register"


def sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def binary_version(path: pathlib.Path, hub: bool) -> str:
    command = [str(path), "--version"] if hub else [str(path), "version"]
    result = subprocess.run(command, check=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    value = result.stdout.strip()
    if not value or "\n" in value or len(value) > 256:
        raise RuntimeError(f"invalid version output from {path}")
    return value


def build(repo: pathlib.Path, output: pathlib.Path, package: str, version: str) -> None:
    go = shutil.which("go")
    if not go:
        raise SystemExit("go is required when prebuilt binary paths are omitted")
    output.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        [go, "build", "-trimpath", f"-ldflags=-X main.version={version}", "-o", str(output), package],
        cwd=repo,
        check=True,
    )


def reserve_address() -> str:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        host, port = listener.getsockname()
    return f"{host}:{port}"


def wait_listen(address: str, process: subprocess.Popen[bytes]) -> None:
    host, raw_port = address.rsplit(":", 1)
    deadline = time.monotonic() + START_TIMEOUT
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"process exited before listening: {process.returncode}")
        try:
            with socket.create_connection((host, int(raw_port)), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"timed out waiting for {address}")


def stop(process: subprocess.Popen[bytes] | None) -> None:
    if process is None or process.poll() is not None:
        return
    process.send_signal(signal.SIGTERM)
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


class Observer:
    def __init__(self, address: str, build_id: str) -> None:
        host, raw_port = address.rsplit(":", 1)
        self.socket = socket.create_connection((host, int(raw_port)), timeout=5)
        self.socket.settimeout(START_TIMEOUT)
        self.reader: BinaryIO = self.socket.makefile("rb")
        self.send({
            "type": "hello", "version": PROTOCOL_VERSION,
            "build": build_id, "generation": 1,
            "host_id": "binary-pair-observer", "host_name": "binary-pair-observer",
        })
        hello = self.read()
        if hello.get("type") != "hello_ok" or hello.get("version") != PROTOCOL_VERSION:
            raise RuntimeError(f"invalid hub handshake: {hello}")
        self.hub_build = str(hello.get("build", ""))

    def send(self, message: dict[str, object]) -> None:
        body = json.dumps(message, sort_keys=True, separators=(",", ":")).encode() + b"\n"
        self.socket.sendall(body)

    def read(self) -> dict[str, object]:
        line = self.reader.readline()
        if not line:
            raise EOFError("hub connection closed")
        value = json.loads(line)
        if not isinstance(value, dict):
            raise RuntimeError("hub emitted a non-object frame")
        return value

    def wait_host(self, build_id: str, minimum_generation: int = 1) -> dict[str, object]:
        deadline = time.monotonic() + START_TIMEOUT
        while time.monotonic() < deadline:
            message = self.read()
            if message.get("type") != "roster":
                continue
            for host in message.get("hosts", []):
                if not isinstance(host, dict) or host.get("id") == "binary-pair-observer":
                    continue
                if host.get("build") == build_id and int(host.get("generation", 0)) >= minimum_generation:
                    return host
        raise RuntimeError(f"timed out waiting for host build {build_id}")

    def assert_host_absent(self, host_id: str) -> None:
        self.send({"type": "snapshot", "peers": []})
        deadline = time.monotonic() + START_TIMEOUT
        while time.monotonic() < deadline:
            message = self.read()
            if message.get("type") != "roster":
                continue
            host_ids = {
                str(host.get("id", ""))
                for host in message.get("hosts", [])
                if isinstance(host, dict)
            }
            if host_id in host_ids:
                raise RuntimeError(f"rejected host {host_id} appeared in the hub roster")
            return
        raise RuntimeError("timed out waiting for a post-rejection roster")

    def close(self) -> None:
        try:
            self.reader.close()
        finally:
            self.socket.close()


def reject_mismatch(address: str) -> None:
    host, raw_port = address.rsplit(":", 1)
    with socket.create_connection((host, int(raw_port)), timeout=5) as connection:
        connection.settimeout(2)
        connection.sendall(json.dumps({
            "type": "hello", "version": PROTOCOL_VERSION + 1,
            "host_id": MISMATCH_HOST_ID, "host_name": MISMATCH_HOST_ID,
            "generation": 999,
        }, separators=(",", ":")).encode() + b"\n")
        try:
            response = connection.recv(1)
        except (ConnectionResetError, BrokenPipeError):
            response = b""
        if response:
            raise RuntimeError("mismatched protocol received a registration response")


def launch_hub(binary: pathlib.Path, address: str, log: BinaryIO) -> subprocess.Popen[bytes]:
    process = subprocess.Popen([str(binary), "--listen", address], stdout=log, stderr=log)
    wait_listen(address, process)
    return process


def launch_host(binary: pathlib.Path, address: str, state: pathlib.Path, log: BinaryIO) -> subprocess.Popen[bytes]:
    environment = os.environ.copy()
    environment["AGENT_SESSIONS_HUB"] = address
    environment["AGENT_SESSIONS_HOST_NAME"] = "binary-pair-host"
    return subprocess.Popen(
        [str(binary), "daemon", "run", "--state-root", str(state)],
        env=environment,
        stdout=log,
        stderr=log,
    )


def resolve_binaries(args: argparse.Namespace, root: pathlib.Path, work: pathlib.Path) -> tuple[list[pathlib.Path], str]:
    supplied = [args.host_a, args.host_b, args.hub_a, args.hub_b]
    if any(supplied) and not all(supplied):
        raise SystemExit("provide all four prebuilt paths or none")
    if args.require_prebuilt and not all(supplied):
        raise SystemExit("--require-prebuilt requires all four prebuilt paths")
    if all(supplied):
        binaries = [pathlib.Path(value).resolve() for value in supplied]
        expected_names = ["agent-sessions", "agent-sessions", "agent-sessions-hub", "agent-sessions-hub"]
        for binary, expected_name in zip(binaries, expected_names, strict=True):
            if not binary.is_file() or binary.is_symlink() or not os.access(binary, os.X_OK):
                raise SystemExit(f"unsafe or non-executable binary: {binary}")
            if binary.name != expected_name:
                raise SystemExit(f"prebuilt binary must retain the production basename {expected_name}: {binary}")
        return binaries, "prebuilt"
    host_a = work / "host-a" / "agent-sessions"
    host_b = work / "host-b" / "agent-sessions"
    hub_a = work / "hub-a" / "agent-sessions-hub"
    hub_b = work / "hub-b" / "agent-sessions-hub"
    build(root, host_a, "./cmd/agent-sessions", "binary-pair-host-a")
    build(root, host_b, "./cmd/agent-sessions", "binary-pair-host-b")
    build(root, hub_a, "./cmd/agent-sessions-hub", "binary-pair-hub-a")
    build(root, hub_b, "./cmd/agent-sessions-hub", "binary-pair-hub-b")
    return [host_a, host_b, hub_a, hub_b], "same-tree-smoke"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path(__file__).resolve().parents[2])
    parser.add_argument("--host-a")
    parser.add_argument("--host-b")
    parser.add_argument("--hub-a")
    parser.add_argument("--hub-b")
    parser.add_argument("--require-prebuilt", action="store_true")
    args = parser.parse_args()
    root = args.repo.resolve()
    with tempfile.TemporaryDirectory(prefix="agent-sessions-binary-pair-") as temporary:
        work = pathlib.Path(temporary)
        binaries, mode = resolve_binaries(args, root, work)
        host_a, host_b, hub_a, hub_b = binaries
        hashes = [sha256(path) for path in binaries]
        if hashes[0] == hashes[1] or hashes[2] == hashes[3]:
            raise RuntimeError("independent host or hub binaries are byte-identical")
        host_builds = [binary_version(host_a, False), binary_version(host_b, False)]
        hub_builds = [binary_version(hub_a, True), binary_version(hub_b, True)]

        address = reserve_address()
        state = work / "host-state"
        processes: list[subprocess.Popen[bytes]] = []
        observer: Observer | None = None
        with (work / "process.log").open("w+b") as process_log:
            try:
                hub = launch_hub(hub_a, address, process_log)
                processes.append(hub)
                host = launch_host(host_a, address, state, process_log)
                processes.append(host)
                observer = Observer(address, "observer-build-a")
                first = observer.wait_host(host_builds[0])
                first_hub_build = observer.hub_build
                reject_mismatch(address)
                observer.assert_host_absent(MISMATCH_HOST_ID)

                observer.close()
                observer = None
                stop(hub)
                hub = launch_hub(hub_b, address, process_log)
                processes.append(hub)
                observer = Observer(address, "observer-build-b")
                after_hub = observer.wait_host(host_builds[0], int(first["generation"]))

                stop(host)
                host = launch_host(host_b, address, state, process_log)
                processes.append(host)
                after_host = observer.wait_host(host_builds[1], int(first["generation"]) + 1)
                reject_mismatch(address)
                observer.assert_host_absent(MISMATCH_HOST_ID)
                print(json.dumps({
                    "type": "federation.binary_pair", "status": "passed", "protocol": PROTOCOL_VERSION,
                    "mode": mode, "host_hashes": hashes[:2], "hub_hashes": hashes[2:],
                    "host_builds": host_builds,
                    "hub_builds": [first_hub_build, observer.hub_build],
                    "host_generations": [first["generation"], after_hub["generation"], after_host["generation"]],
                    "mismatch_refused_before_registration": True,
                }, sort_keys=True, separators=(",", ":")))
            finally:
                if observer is not None:
                    observer.close()
                for process in reversed(processes):
                    stop(process)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
