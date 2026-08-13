#!/usr/bin/env python3
"""Minimal Claude-compatible UDS peer used only by federation smoke tests."""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import signal
import socket
import sys
import time
import urllib.parse


def frame(source_socket: str, session: str, name: str, message_id: str, text: str):
    address = "uds:" + source_socket
    content = (
        f'<cross-session-message from="{address}" from-session="{session}" '
        f'from-name="{name}" from-mode="prompting">\n'
        f"{text}\n</cross-session-message>"
    )
    return {
        "msgV": 1,
        "msg_id": message_id,
        "type": "user",
        "message": {"role": "user", "content": content},
        "priority": "next",
        "from": address,
    }


def send_frame(target: str, value: dict):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.connect(target)
        conn.sendall(json.dumps(value, separators=(",", ":")).encode() + b"\n")


def serve(args):
    registry = pathlib.Path(args.registry_dir)
    registry.mkdir(parents=True, exist_ok=True)
    socket_path = pathlib.Path(args.socket)
    socket_path.parent.mkdir(parents=True, exist_ok=True)
    socket_path.unlink(missing_ok=True)
    registry_file = registry / f"{os.getpid()}.json"
    capture = pathlib.Path(args.capture)
    capture.parent.mkdir(parents=True, exist_ok=True)
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(str(socket_path))
    listener.listen()
    listener.settimeout(0.25)
    record = {
        "pid": os.getpid(),
        "sessionId": args.session,
        "cwd": args.cwd,
        "startedAt": int(time.time() * 1000),
        "version": "peer-fixture/1",
        "peerProtocol": 1,
        "kind": "interactive",
        "entrypoint": args.entrypoint,
        "name": args.name,
        "nameSource": "explicit",
        "status": "idle",
        "messagingSocketPath": str(socket_path),
    }
    registry_file.write_text(json.dumps(record))
    stopping = False

    def stop(_signum, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        while not stopping:
            try:
                conn, _ = listener.accept()
            except socket.timeout:
                continue
            with conn:
                payload = b""
                while True:
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    payload += chunk
            for line in payload.splitlines():
                if not line.strip():
                    continue
                value = json.loads(line)
                with capture.open("a") as output:
                    output.write(json.dumps(value, separators=(",", ":")) + "\n")
                if args.reply and value.get("type") == "user":
                    target = urllib.parse.unquote(str(value.get("from", "")).removeprefix("uds:"))
                    if target:
                        send_frame(
                            target,
                            frame(str(socket_path), args.session, args.name, f"reply-{value.get('msg_id', 'message')}", args.reply),
                        )
    finally:
        listener.close()
        registry_file.unlink(missing_ok=True)
        socket_path.unlink(missing_ok=True)


def send(args):
    send_frame(args.target, frame(args.socket, args.session, args.name, args.message_id, args.message))


def parser():
    root = argparse.ArgumentParser()
    commands = root.add_subparsers(dest="command", required=True)
    server = commands.add_parser("serve")
    server.add_argument("--registry-dir", required=True)
    server.add_argument("--socket", required=True)
    server.add_argument("--session", required=True)
    server.add_argument("--name", required=True)
    server.add_argument("--capture", required=True)
    server.add_argument("--reply", default="")
    server.add_argument("--cwd", default="/fixture")
    server.add_argument("--entrypoint", choices=("claude", "codex"), default="claude")
    sender = commands.add_parser("send")
    sender.add_argument("--socket", required=True)
    sender.add_argument("--session", required=True)
    sender.add_argument("--name", required=True)
    sender.add_argument("--target", required=True)
    sender.add_argument("--message-id", required=True)
    sender.add_argument("--message", required=True)
    return root


def main():
    args = parser().parse_args()
    if args.command == "serve":
        serve(args)
    else:
        send(args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
