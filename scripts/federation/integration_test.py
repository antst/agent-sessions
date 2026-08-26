#!/usr/bin/env python3
"""Protocol-v3 grouped federation integration test.

This test deliberately uses the public host-agent registration and AgentFrame
APIs. It asserts that federation creates no per-peer Claude shadows.
"""

import json
import os
import pathlib
import queue
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time


def wait_for(predicate, description, timeout=10.0):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = predicate()
            if value:
                return value
        except Exception as exc:  # diagnostics retain the last transient error
            last = exc
        time.sleep(0.05)
    raise AssertionError(f"timed out waiting for {description}; last={last}")


def reserve_port():
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    port = listener.getsockname()[1]
    listener.close()
    return port


class Managed:
    def __init__(self, argv, log_path, stdin=False, stdout=False):
        self.log = open(log_path, "ab", buffering=0)
        self.process = subprocess.Popen(
            argv,
            stdin=subprocess.PIPE if stdin else subprocess.DEVNULL,
            stdout=subprocess.PIPE if stdout else self.log,
            stderr=self.log,
            start_new_session=True,
        )

    def stop(self):
        if self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait(timeout=5)
        self.log.close()


class Peer(Managed):
    def __init__(self, fixture, runtime_dir, session, product, name, groups, log_path):
        super().__init__(
            [fixture, "--runtime-dir", str(runtime_dir), "--session", session,
             "--product", product, "--name", name, "--groups", ",".join(groups)],
            log_path, stdin=True, stdout=True,
        )
        self.events = queue.Queue()
        self.pending = []
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()
        self.wait_event(lambda row: row.get("event") == "ready", "peer ready")

    def _read(self):
        for line in self.process.stdout:
            try:
                self.events.put(json.loads(line))
            except Exception as exc:
                self.events.put({"event": "decode_error", "error": str(exc), "line": line.decode(errors="replace")})

    def command(self, command_id, frame):
        body = json.dumps({"id": command_id, "frame": frame}, separators=(",", ":")) + "\n"
        self.process.stdin.write(body.encode())
        self.process.stdin.flush()
        return self.wait_event(
            lambda row: row.get("event") == "result" and row.get("id") == command_id,
            f"result {command_id}",
        )

    def wait_delivery(self, message_id, timeout=5.0):
        return self.wait_event(
            lambda row: row.get("event") == "delivery" and row.get("frame", {}).get("message_id") == message_id,
            f"delivery {message_id}", timeout,
        )

    def wait_event(self, predicate, description, timeout=10.0):
        for index, row in enumerate(self.pending):
            if predicate(row):
                return self.pending.pop(index)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                row = self.events.get(timeout=min(0.2, deadline - time.monotonic()))
            except queue.Empty:
                if self.process.poll() is not None:
                    raise AssertionError(f"{description}: peer exited {self.process.returncode}")
                continue
            if predicate(row):
                return row
            self.pending.append(row)
        raise AssertionError(f"timed out waiting for {description}; pending={self.pending}")

    def assert_no_delivery(self, message_id, duration=0.4):
        try:
            self.wait_delivery(message_id, duration)
        except AssertionError:
            return
        raise AssertionError(f"unexpected delivery {message_id}")


def agent_command(binary, hub, host, root):
    return [
        binary, "agent", "--hub", hub, "--host", host, "--name", host,
        "--runtime-dir", str(root / "runtime"), "--state-dir", str(root / "state"),
        "--claude-config-dir", str(root / "claude"),
    ]


def service_rows(config_root):
    rows = []
    for path in (config_root / "sessions").glob("*.json"):
        try:
            row = json.loads(path.read_text())
        except Exception:
            continue
        if row.get("agentService"):
            rows.append(row)
        assert not row.get("federatedBy"), f"legacy shadow row survived: {row}"
    return rows


def frame(kind, message_id, **fields):
    result = {"version": 1, "type": kind, "message_id": message_id}
    result.update(fields)
    return result


def discovered_sessions(peer, command_id):
    response = peer.command(command_id, frame("discover", command_id))
    assert "error" not in response, response
    return {item["session_id"] for item in response["result"].get("peers", [])}


def main():
    if len(sys.argv) != 2:
        raise SystemExit(f"usage: {sys.argv[0]} PEER_FEDERATOR")
    binary = os.path.abspath(sys.argv[1])
    # AF_UNIX paths are capped at 103 characters on Darwin. Keep the socket
    # hierarchy independent of that platform's long per-user TMPDIR.
    root = pathlib.Path(tempfile.mkdtemp(prefix="gf-", dir="/tmp"))
    repo = pathlib.Path(__file__).resolve().parents[2]
    fixture = root / "grouped-peer-fixture"
    subprocess.run(
        ["go", "build", "-o", str(fixture), "./scripts/federation/grouped_peer_fixture.go"],
        cwd=repo, check=True,
    )
    port = reserve_port()
    hub_address = f"127.0.0.1:{port}"
    processes = []
    success = False
    try:
        hub = Managed([binary, "hub", "--listen", hub_address], root / "hub.log")
        processes.append(hub)
        wait_for(lambda: socket.create_connection(("127.0.0.1", port), timeout=0.2), "hub listener")

        host_a, host_b = root / "host-a", root / "host-b"
        agent_a = Managed(agent_command(binary, hub_address, "host-a", host_a), root / "agent-a.log")
        agent_b = Managed(agent_command(binary, hub_address, "host-b", host_b), root / "agent-b.log")
        processes.extend([agent_a, agent_b])
        wait_for(lambda: (host_a / "runtime" / "agent.sock").exists(), "host-a agent")
        wait_for(lambda: (host_b / "runtime" / "agent.sock").exists(), "host-b agent")

        peer_a = Peer(str(fixture), host_a / "runtime", "session-a", "codex", "alpha", ["project"], root / "peer-a.log")
        peer_b = Peer(str(fixture), host_b / "runtime", "session-b", "claude", "beta", ["project"], root / "peer-b.log")
        peer_qwen = Peer(str(fixture), host_b / "runtime", "session-qwen", "qwen", "gamma", ["project"], root / "peer-qwen.log")
        peer_hidden = Peer(str(fixture), host_b / "runtime", "session-hidden", "grok", "hidden", ["other"], root / "peer-hidden.log")
        processes.extend([peer_a, peer_b, peer_qwen, peer_hidden])

        qwen_socket = host_b / "runtime" / "fixture-session-qwen.sock"
        wait_for(lambda: qwen_socket.is_socket(), "real Qwen fixture destination socket")

        wait_for(lambda: discovered_sessions(peer_a, "discover-initial") == {"session-b", "session-qwen"}, "group-filtered remote discovery")
        assert discovered_sessions(peer_hidden, "discover-hidden") == set()
        wait_for(lambda: len(service_rows(host_a / "claude")) == 1, "one host-a service row")
        wait_for(lambda: len(service_rows(host_b / "claude")) == 1, "one host-b service row")

        sent = peer_a.command("send-a-b", frame("send", "send-a-b", targets=["beta"], content="HELLO_GROUPED"))
        assert sent["result"]["deliveries"] == [{"target": "host-b/session-b", "session_id": "session-b", "status": "accepted"}], sent
        delivered = peer_b.wait_delivery("send-a-b")["frame"]
        assert delivered["content"] == "HELLO_GROUPED"
        assert delivered["source"]["entrypoint"] == "codex"

        qwen_sent = peer_a.command("send-a-qwen", frame("send", "send-a-qwen", targets=["gamma"], content="HELLO_QWEN"))
        assert qwen_sent["result"]["deliveries"] == [{"target": "host-b/session-qwen", "session_id": "session-qwen", "status": "accepted"}], qwen_sent
        qwen_delivered = peer_qwen.wait_delivery("send-a-qwen")["frame"]
        assert qwen_delivered["content"] == "HELLO_QWEN"
        assert qwen_delivered["source"]["entrypoint"] == "codex"

        denied = peer_a.command("atomic-denied", frame("send", "atomic-denied", targets=["beta", "hidden"], content="NO"))
        assert "error" in denied, denied
        peer_b.assert_no_delivery("atomic-denied")
        peer_hidden.assert_no_delivery("atomic-denied")

        broadcast = peer_a.command("broadcast-project", frame("broadcast", "broadcast-project", group="project", content="ALL_PROJECT"))
        assert len(broadcast["result"]["deliveries"]) == 2, broadcast
        assert peer_b.wait_delivery("broadcast-project")["frame"]["content"] == "ALL_PROJECT"
        assert peer_qwen.wait_delivery("broadcast-project")["frame"]["content"] == "ALL_PROJECT"
        peer_hidden.assert_no_delivery("broadcast-project")

        # Restart the hub. Agents reconnect and grouped routing resumes without
        # creating a shadow row or restarting either peer.
        hub.stop()
        processes.remove(hub)
        hub = Managed([binary, "hub", "--listen", hub_address], root / "hub-restart.log")
        processes.append(hub)
        wait_for(lambda: discovered_sessions(peer_a, "discover-after-hub") == {"session-b", "session-qwen"}, "roster after hub restart", 15)
        peer_a.command("send-after-hub", frame("send", "send-after-hub", targets=["session-b"], content="AFTER_HUB"))
        assert peer_b.wait_delivery("send-after-hub")["frame"]["content"] == "AFTER_HUB"

        # Restart one host agent. The still-idle product fixture re-registers;
        # the public registry again contains one service row and no shadows.
        agent_b.stop()
        processes.remove(agent_b)
        agent_b = Managed(agent_command(binary, hub_address, "host-b", host_b), root / "agent-b-restart.log")
        processes.append(agent_b)
        wait_for(lambda: discovered_sessions(peer_a, "discover-after-agent") == {"session-b", "session-qwen"}, "peer re-registration after agent restart", 15)
        wait_for(lambda: len(service_rows(host_b / "claude")) == 1, "single service row after agent restart")
        peer_a.command("send-after-agent", frame("send", "send-after-agent", targets=["beta"], content="AFTER_AGENT"))
        assert peer_b.wait_delivery("send-after-agent")["frame"]["content"] == "AFTER_AGENT"

        peer_b.stop()
        processes.remove(peer_b)
        wait_for(lambda: discovered_sessions(peer_a, "discover-after-exit") == {"session-qwen"}, "exact remote peer removal")
        peer_qwen.stop()
        processes.remove(peer_qwen)
        wait_for(lambda: discovered_sessions(peer_a, "discover-after-qwen-exit") == set(), "remote Qwen peer removal")
        wait_for(lambda: not qwen_socket.exists(), "exact remote Qwen destination socket cleanup")
        assert peer_hidden.process.poll() is None, "unrelated remote peer was stopped by Qwen cleanup"
        assert len(service_rows(host_b / "claude")) == 1, "Qwen cleanup changed the destination service row"
        print("grouped federation integration: PASS")
        success = True
        return 0
    finally:
        for process in reversed(processes):
            process.stop()
        if not success:
            for path in sorted(root.rglob("*.log")):
                if path.stat().st_size:
                    print(f"===== {path.name} =====", file=sys.stderr)
                    print(path.read_text(errors="replace"), file=sys.stderr)
        import shutil
        shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
