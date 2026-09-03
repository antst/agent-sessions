#!/usr/bin/env python3
"""One strict raw Agent Sessions protocol-v1 message.send client."""

import argparse
import json
import socket
import sys
from pathlib import Path


MAX_FRAME_BYTES = 1024 * 1024


def compact(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", required=True)
    parser.add_argument("--uuid", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--group", required=True)
    parser.add_argument("--params", required=True)
    parser.add_argument("--evidence", required=True)
    args = parser.parse_args()

    message_params = json.loads(args.params)
    if not isinstance(message_params, dict):
        raise ValueError("message params must be an object")

    frames = []
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(30)
    try:
        connection.connect(args.socket)
        stream = connection.makefile("rwb", buffering=0)

        def send(frame):
            frames.append({"direction": "send", "frame": frame})
            stream.write((compact(frame) + "\n").encode("utf-8"))

        def receive():
            line = stream.readline(MAX_FRAME_BYTES + 1)
            if not line or len(line) > MAX_FRAME_BYTES or not line.endswith(b"\n"):
                raise RuntimeError("daemon returned an invalid bounded JSON-RPC frame")
            frame = json.loads(line)
            if not isinstance(frame, dict) or frame.get("jsonrpc") != "2.0":
                raise RuntimeError("daemon returned a non-JSON-RPC object")
            frames.append({"direction": "receive", "frame": frame})
            return frame

        def call(request_id, method, params):
            send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
            while True:
                response = receive()
                if response.get("id") == request_id and ("result" in response or "error" in response):
                    return response
                if "id" in response and response.get("method") == "message.deliver":
                    send({
                        "jsonrpc": "2.0",
                        "id": response["id"],
                        "error": {
                            "code": -32002,
                            "message": "Session busy",
                            "data": {"uuid": args.uuid},
                        },
                    })
                    continue
                raise RuntimeError("daemon interleaved an unexpected JSON-RPC frame")

        hello = call("hello", "session.hello", {
            "protocol": 1,
            "uuid": args.uuid,
            "name": args.name,
            "groups": [args.group],
            "product": "claude",
            "info": {},
        })
        if hello.get("result") != {}:
            raise RuntimeError("session.hello was not acknowledged")
        response = call("send", "message.send", message_params)
        print(compact(response))
    finally:
        connection.close()
        evidence = Path(args.evidence)
        evidence.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        evidence.write_text(json.dumps({"frames": frames}, indent=2) + "\n", encoding="utf-8")
        evidence.chmod(0o600)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:  # Evidence above retains any completed frames.
        print(f"matrix_v1.py: {error}", file=sys.stderr)
        sys.exit(1)
