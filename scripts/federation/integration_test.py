#!/usr/bin/env python3
"""Black-box two-host federation test using isolated registries and Unix peers."""

from __future__ import annotations

import json
import os
import pathlib
import queue
import signal
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time


def wait_for(predicate, timeout=10.0, description="condition"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.05)
    raise AssertionError(f"timed out waiting for {description}")


class FakePeer:
    def __init__(self, root: pathlib.Path, session: str, name: str, bypass: bool = False):
        self.root = root
        self.session = session
        self.name = name
        self.socket_path = root / f"{session}.sock"
        self.registry = root / "config" / "sessions"
        self.registry.mkdir(parents=True, exist_ok=True)
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(str(self.socket_path))
        self.listener.listen()
        self.listener.settimeout(0.2)
        self.frames: queue.Queue[dict] = queue.Queue()
        self.closed = threading.Event()
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()
        self.identity_process = None
        identity_pid = os.getpid()
        if bypass:
            self.identity_process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(300)",
                 "--permission-mode", "bypassPermissions"]
            )
            identity_pid = self.identity_process.pid
        record = {
            "pid": identity_pid,
            "sessionId": session,
            "cwd": f"/work/{session}",
            "startedAt": int(time.time() * 1000),
            "version": "integration-fixture/1",
            "peerProtocol": 1,
            "kind": "interactive",
            "entrypoint": "claude" if session.endswith("a") else "codex",
            "name": name,
            "nameSource": "explicit",
            "status": "idle",
            "permissionMode": "bypassPermissions" if bypass else "default",
            "messagingSocketPath": str(self.socket_path),
        }
        self.registry_file = self.registry / f"{identity_pid}.json"
        self.registry_file.write_text(json.dumps(record))

    def _serve(self):
        while not self.closed.is_set():
            try:
                conn, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with conn:
                payload = b""
                while True:
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    payload += chunk
                for line in payload.splitlines():
                    if line.strip():
                        self.frames.put(json.loads(line))

    def send(self, target_socket: str, message_id: str, text: str):
        address = "uds:" + str(self.socket_path)
        content = (
            f'<cross-session-message from="{address}" '
            f'from-session="{self.session}" from-name="{self.name}" '
            'from-mode="prompting">\n'
            f"{text}\n</cross-session-message>"
        )
        frame = {
            "msgV": 1,
            "msg_id": message_id,
            "type": "user",
            "message": {"role": "user", "content": content},
            "priority": "next",
            "from": address,
        }
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
            conn.connect(target_socket)
            conn.sendall(json.dumps(frame, separators=(",", ":")).encode() + b"\n")

    def receive(self, timeout=10.0):
        return self.frames.get(timeout=timeout)

    def close(self):
        if self.closed.is_set():
            return
        self.closed.set()
        self.registry_file.unlink(missing_ok=True)
        self.listener.close()
        self.thread.join(timeout=1)
        self.socket_path.unlink(missing_ok=True)
        if self.identity_process is not None:
            self.identity_process.terminate()
            self.identity_process.wait(timeout=2)


def shadow_for(registry: pathlib.Path, peer_id: str):
    if not registry.exists():
        return None
    for path in registry.glob("*.json"):
        try:
            row = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        if row.get("federatedBy") == "peer-federator" and row.get("federatedPeerId") == peer_id:
            return row, path
    return None


def frame_text(frame: dict) -> str:
    return frame.get("message", {}).get("content", "")


def write_lane_fixture(root: pathlib.Path) -> pathlib.Path:
    path = root / "lane-fixture.py"
    path.write_text(
        """#!/usr/bin/env python3
import json
import os
import pathlib
import signal
import sys
import time

root = pathlib.Path(__file__).resolve().parent
args = sys.argv[1:]
body = sys.stdin.buffer.read()
record = {"pid": os.getpid(), "args": args, "input": body.decode(errors="replace")}
with (root / "fixture-invocations.jsonl").open("a") as stream:
    stream.write(json.dumps(record, separators=(",", ":")) + "\\n")
print(json.dumps(record, separators=(",", ":")), flush=True)
print("fixture stderr", file=sys.stderr, flush=True)

if "--fixture-block" in args:
    def stop(_signum, _frame):
        (root / f"fixture-cancelled-{os.getpid()}").write_text("cancelled\\n")
        raise SystemExit(130)
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    while True:
        time.sleep(0.1)

raise SystemExit(7 if "--fixture-exit-7" in args else 0)
"""
    )
    path.chmod(0o755)
    return path


def main() -> int:
    if len(sys.argv) != 2:
        raise SystemExit("usage: integration_test.py PEER_FEDERATOR")
    binary = str(pathlib.Path(sys.argv[1]).resolve())
    root = pathlib.Path(tempfile.mkdtemp(prefix="peer-federator-it-", dir="/tmp"))
    processes: list[subprocess.Popen] = []
    peers: list[FakePeer] = []
    logs = []
    try:
        lane_fixture = write_lane_fixture(root)
        peer_a = FakePeer(root / "a", "session-a", "interactive-a", bypass=True)
        peer_b = FakePeer(root / "b", "session-b", "interactive-b")
        peers.extend([peer_a, peer_b])

        probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()

        def start(label, *args):
            log_path = root / f"{label}.log"
            log = log_path.open("wb")
            logs.append(log)
            process = subprocess.Popen([binary, *args], stdout=log, stderr=log)
            processes.append(process)
            return process

        hub = start("hub", "hub", "--listen", f"127.0.0.1:{port}")

        def hub_ready():
            if hub.poll() is not None:
                return False
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    return True
            except OSError:
                return False

        wait_for(hub_ready, description="hub listener")
        agent_a = start(
            "agent-a",
            "agent", "--hub", f"127.0.0.1:{port}", "--host", "host-a", "--name", "alpha",
            "--enable-remote-lanes",
            "--claude-config-dir", str(peer_a.root / "config"),
            "--runtime-dir", str(root / "a-run"),
            "--codex-lane", str(lane_fixture), "--claude-lane", str(lane_fixture),
        )
        agent_b = start(
            "agent-b",
            "agent", "--hub", f"127.0.0.1:{port}", "--host", "host-b", "--name", "beta",
            "--enable-remote-lanes",
            "--claude-config-dir", str(peer_b.root / "config"),
            "--runtime-dir", str(root / "b-run"),
            "--codex-lane", str(lane_fixture), "--claude-lane", str(lane_fixture),
        )

        shadow_b_match = wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b"),
            description="host-b shadow on host-a",
        )
        shadow_b_on_a, old_shadow_record = shadow_b_match
        shadow_a_on_b = wait_for(
            lambda: shadow_for(peer_b.registry, "host-a/session-a"),
            description="host-a shadow on host-b",
        )[0]
        assert shadow_b_on_a["name"] == "interactive-b--beta"
        assert shadow_a_on_b["name"] == "interactive-a--alpha"
        assert shadow_a_on_b["permissionMode"] == "bypassPermissions"

        hosts = json.loads(
            subprocess.run(
                [binary, "hosts", "--runtime-dir", str(root / "a-run")],
                text=True,
                capture_output=True,
                timeout=3,
                check=True,
            ).stdout
        )
        assert hosts["protocol_version"] == 2
        assert hosts["hosts"] == [
            {"id": "host-b", "name": "beta", "capabilities": ["claude-lane", "codex-lane"]}
        ]

        # Having launchers installed is not authority to execute them. A third
        # agent without the explicit opt-in joins the roster with no lane
        # capabilities even when both executable overrides are supplied.
        agent_c = start(
            "agent-c-disabled",
            "agent", "--hub", f"127.0.0.1:{port}", "--host", "host-c", "--name", "gamma",
            "--claude-config-dir", str(root / "c-config"),
            "--runtime-dir", str(root / "c-run"),
            "--codex-lane", str(lane_fixture), "--claude-lane", str(lane_fixture),
        )
        disabled_host = wait_for(
            lambda: next(
                (
                    host
                    for host in json.loads(
                        subprocess.run(
                            [binary, "hosts", "--runtime-dir", str(root / "a-run")],
                            text=True, capture_output=True, timeout=3, check=True,
                        ).stdout
                    )["hosts"]
                    if host["id"] == "host-c"
                ),
                None,
            ),
            description="disabled host in roster",
        )
        assert disabled_host == {"id": "host-c", "name": "gamma"}
        agent_c.terminate()
        # The race build can spend more than three seconds synchronously
        # reaping the two remote discovery shadows during graceful shutdown.
        agent_c.wait(timeout=8)
        wait_for(
            lambda: all(
                host["id"] != "host-c"
                for host in json.loads(
                    subprocess.run(
                        [binary, "hosts", "--runtime-dir", str(root / "a-run")],
                        text=True, capture_output=True, timeout=3, check=True,
                    ).stdout
                )["hosts"]
            ),
            description="disabled host removal",
        )

        remote_start = subprocess.run(
            [
                binary, "lane", "--runtime-dir", str(root / "a-run"),
                "--source-session", "session-a", "--host", "beta", "--product", "codex", "--",
                "start", "--name", "remote-worker", "-",
            ],
            input="REMOTE_INPUT_TOKEN\n",
            text=True,
            capture_output=True,
            timeout=5,
            check=True,
        )
        remote_record = json.loads(remote_start.stdout)
        assert remote_record["input"] == "REMOTE_INPUT_TOKEN\n"
        assert remote_record["args"] == [
            "start", "--name", "remote-worker", "-", "--persistent", "--notify",
            "uds:" + shadow_a_on_b["messagingSocketPath"],
        ]
        assert "fixture stderr" in remote_start.stderr

        duplicate = subprocess.run(
            [
                binary, "agent", "--hub", f"127.0.0.1:{port}",
                "--host", "host-a-duplicate",
                "--claude-config-dir", str(peer_a.root / "config"),
                "--runtime-dir", str(root / "a-run"),
            ],
            text=True,
            capture_output=True,
            timeout=3,
            check=False,
        )
        assert duplicate.returncode != 0
        assert "already owns" in duplicate.stderr

        duplicate_registry = subprocess.run(
            [
                binary, "agent", "--hub", f"127.0.0.1:{port}",
                "--host", "host-a-duplicate-registry",
                "--claude-config-dir", str(peer_a.root / "config"),
                "--runtime-dir", str(root / "a-other-run"),
            ],
            text=True,
            capture_output=True,
            timeout=3,
            check=False,
        )
        assert duplicate_registry.returncode != 0
        assert "claude registry" in duplicate_registry.stderr

        doctor = json.loads(
            subprocess.run(
                [
                    binary, "doctor", "--hub", f"127.0.0.1:{port}",
                    "--claude-config-dir", str(peer_a.root / "config"),
                    "--runtime-dir", str(root / "a-run"),
                ],
                text=True,
                capture_output=True,
                timeout=3,
                check=True,
            ).stdout
        )
        assert doctor["ok"] is True
        assert doctor["hub_compatible"] is True
        assert doctor["messageable_local_peers"] == 1
        assert doctor["unmessageable_live_records"] == 0
        assert doctor["agent_running"] is True

        status = json.loads(
            subprocess.run(
                [binary, "status", "--runtime-dir", str(root / "a-run")],
                text=True,
                capture_output=True,
                timeout=3,
                check=True,
            ).stdout
        )
        assert status["connected"] is True
        assert status["local_peers"] == 1
        assert status["remote_peers"] == 1
        assert status["shadows"] == 1

        # A shadow is independently supervised. Killing only that child must
        # replace its PID and keep the socket reachable without restarting
        # either host agent. Socket inodes may be reused after unlink.
        old_shadow_pid = shadow_b_on_a["pid"]
        os.kill(old_shadow_pid, signal.SIGKILL)
        shadow_b_on_a = wait_for(
            lambda: (
                candidate
                if (candidate := shadow_for(peer_a.registry, "host-b/session-b"))
                and candidate[0].get("pid") != old_shadow_pid
                else None
            ),
            description="independent shadow replacement",
        )[0]
        wait_for(lambda: not old_shadow_record.exists(), description="dead shadow registry cleanup")

        peer_a.send(shadow_b_on_a["messagingSocketPath"], "a-to-b", "HELLO_FROM_A")
        received_b = peer_b.receive()
        assert "HELLO_FROM_A" in frame_text(received_b)
        assert received_b["from"] == "uds:" + shadow_a_on_b["messagingSocketPath"]
        assert f'from="uds:{shadow_a_on_b["messagingSocketPath"]}"' in frame_text(received_b)

        peer_b.send(shadow_a_on_b["messagingSocketPath"], "b-to-a", "REPLY_FROM_B")
        received_a = peer_a.receive()
        assert "REPLY_FROM_B" in frame_text(received_a)
        assert received_a["from"] == "uds:" + shadow_b_on_a["messagingSocketPath"]

        # An abrupt destination-agent loss removes its shadows and host, and
        # the liveness-pipe watchdog reaps a blocking native CLI that would
        # otherwise be reparented indefinitely with its collection lock held.
        crash_proxy = subprocess.Popen(
            [
                binary, "lane", "--runtime-dir", str(root / "a-run"),
                "--source-session", "session-a", "--host", "host-b", "--product", "codex", "--",
                "wait", "remote-worker", "--fixture-block",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        assert crash_proxy.stdout is not None
        crash_record = json.loads(crash_proxy.stdout.readline())
        crash_fixture_pid = crash_record["pid"]
        agent_b.kill()
        agent_b.wait(timeout=3)
        crash_proxy.communicate(timeout=5)
        assert crash_proxy.returncode != 0
        wait_for(
            lambda: (root / f"fixture-cancelled-{crash_fixture_pid}").exists(),
            description="native lane cancellation after destination agent SIGKILL",
        )
        wait_for(
            lambda: not pathlib.Path(f"/proc/{crash_fixture_pid}").exists(),
            description="native lane process reaped after destination agent SIGKILL",
        )
        wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b") is None,
            description="host-b shadow removal after agent SIGKILL",
        )
        wait_for(
            lambda: shadow_for(peer_b.registry, "host-a/session-a") is None,
            description="host-a shadow cleanup after owning agent SIGKILL",
        )
        agent_b = start(
            "agent-b-restarted",
            "agent", "--hub", f"127.0.0.1:{port}", "--host", "host-b", "--name", "beta",
            "--enable-remote-lanes",
            "--claude-config-dir", str(peer_b.root / "config"),
            "--runtime-dir", str(root / "b-run"),
            "--codex-lane", str(lane_fixture), "--claude-lane", str(lane_fixture),
        )
        shadow_b_on_a = wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b"),
            description="host-b shadow after agent restart",
        )[0]
        wait_for(
            lambda: shadow_for(peer_b.registry, "host-a/session-a"),
            description="host-a shadow after agent restart",
        )

        # A hub loss cancels an active proxy and makes every new remote spawn
        # fail closed. There is no agent-to-agent or destination-side fallback.
        blocking = subprocess.Popen(
            [
                binary, "lane", "--runtime-dir", str(root / "a-run"),
                "--source-session", "session-a", "--host", "host-b", "--product", "codex", "--",
                "wait", "remote-worker", "--fixture-block",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        assert blocking.stdout is not None
        blocking_record = json.loads(blocking.stdout.readline())
        assert blocking_record["args"][-1] == "--fixture-block"
        hub.terminate()
        hub.wait(timeout=3)
        _, blocking_stderr = blocking.communicate(timeout=5)
        assert blocking.returncode != 0
        assert "hub is disconnected" in blocking_stderr or "stream ended" in blocking_stderr
        wait_for(
            lambda: (root / f"fixture-cancelled-{blocking_record['pid']}").exists(),
            description="remote lane cancellation after hub loss",
        )
        offline_hosts = subprocess.run(
            [binary, "hosts", "--runtime-dir", str(root / "a-run")],
            text=True,
            capture_output=True,
            timeout=3,
            check=False,
        )
        assert offline_hosts.returncode != 0
        assert "hub is disconnected" in offline_hosts.stderr
        offline_spawn = subprocess.run(
            [
                binary, "lane", "--runtime-dir", str(root / "a-run"),
                "--source-session", "session-a", "--host", "host-b", "--product", "codex", "--",
                "start", "--name", "must-not-run", "-",
            ],
            input="NO_SPAWN\n",
            text=True,
            capture_output=True,
            timeout=3,
            check=False,
        )
        assert offline_spawn.returncode != 0
        assert "hub is disconnected" in offline_spawn.stderr
        fixture_rows = [
            json.loads(line)
            for line in (root / "fixture-invocations.jsonl").read_text().splitlines()
            if line
        ]
        assert not any("must-not-run" in row["args"] for row in fixture_rows)

        # A hub restart removes every federated row, then agents reconnect and
        # republish without disturbing either real local peer.
        wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b") is None,
            description="host-b shadow removal after hub restart",
        )
        wait_for(
            lambda: shadow_for(peer_b.registry, "host-a/session-a") is None,
            description="host-a shadow removal after hub restart",
        )
        hub = start("hub-restarted", "hub", "--listen", f"127.0.0.1:{port}")
        wait_for(hub_ready, description="restarted hub listener")
        shadow_b_on_a = wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b"),
            description="host-b shadow after hub restart",
        )[0]
        wait_for(
            lambda: shadow_for(peer_b.registry, "host-a/session-a"),
            description="host-a shadow after hub restart",
        )
        peer_a.send(shadow_b_on_a["messagingSocketPath"], "after-restart", "AFTER_HUB_RESTART")
        assert "AFTER_HUB_RESTART" in frame_text(peer_b.receive())

        old_shadow_path = pathlib.Path(shadow_b_on_a["messagingSocketPath"])
        peer_b.close()
        wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b") is None,
            description="remote shadow removal after session exit",
        )
        wait_for(lambda: not old_shadow_path.exists(), description="remote shadow socket cleanup")

        # Recreating the same peer exercises rapid registry churn and must
        # produce a fresh, working shadow rather than retain stale state.
        peer_b = FakePeer(root / "b", "session-b", "interactive-b")
        peers.append(peer_b)
        shadow_b_on_a = wait_for(
            lambda: shadow_for(peer_a.registry, "host-b/session-b"),
            description="host-b shadow after peer recreation",
        )[0]
        peer_a.send(shadow_b_on_a["messagingSocketPath"], "after-churn", "AFTER_PEER_CHURN")
        assert "AFTER_PEER_CHURN" in frame_text(peer_b.receive())

        assert agent_a.poll() is None and agent_b.poll() is None and hub.poll() is None
        print(
            "integration PASS: routing, remote lane proxy, hub-loss fail-closed, "
            "agent crash, hub restart, peer churn, reply rewriting, and cleanup"
        )
        return 0
    except Exception:
        for log in logs:
            log.flush()
        for path in sorted(root.glob("*.log")):
            print(f"===== {path.name} =====", file=sys.stderr)
            print(path.read_text(errors="replace"), file=sys.stderr)
        raise
    finally:
        for peer in peers:
            peer.close()
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
        for process in reversed(processes):
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
        for log in logs:
            log.close()
        shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
