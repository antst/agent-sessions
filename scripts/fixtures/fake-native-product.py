#!/usr/bin/env python3
"""Stateful non-secret native-product fixture for unified service acceptance."""

import json
import os
import pathlib
import shutil
import sys


PRODUCT = pathlib.Path(sys.argv[0]).name
ROOT = pathlib.Path(os.environ["AGENT_SESSIONS_FAKE_NATIVE_ROOT"])
STATE_PATH = ROOT / "state" / f"{PRODUCT}.json"


def load_state():
    if not STATE_PATH.exists():
        return {}
    return json.loads(STATE_PATH.read_text(encoding="utf-8"))


def save_state(state):
    STATE_PATH.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = STATE_PATH.with_suffix(".tmp")
    temporary.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
    os.chmod(temporary, 0o600)
    temporary.replace(STATE_PATH)


def fail_once_if_requested(operation):
    selected = os.environ.get("AGENT_SESSIONS_FAKE_FAIL_PRODUCT", "")
    marker = ROOT / "state" / f"failed-{PRODUCT}"
    if selected == PRODUCT and operation == "install" and not marker.exists():
        marker.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        marker.touch(mode=0o600)
        raise SystemExit(71)


def make_tree_writable(root):
    if not root.exists():
        return
    for directory, directories, _ in os.walk(root):
        os.chmod(directory, 0o700)
        for child in directories:
            os.chmod(pathlib.Path(directory) / child, 0o700)


def codex(args):
    state = load_state()
    if args == ["plugin", "marketplace", "list", "--json"]:
        rows = []
        if state.get("marketplace"):
            rows.append({"name": "agent-sessions", "root": state["source"]})
        print(json.dumps({"marketplaces": rows}))
    elif args == ["plugin", "list", "--json"]:
        rows = [{"pluginId": "agent-sessions@agent-sessions"}] if state.get("plugin") else []
        print(json.dumps({"installed": rows}))
    elif args[:3] == ["plugin", "marketplace", "add"] and len(args) == 4:
        state.update(marketplace=True, source=args[3])
        save_state(state)
    elif args == ["plugin", "marketplace", "remove", "agent-sessions"]:
        state.update(marketplace=False, source="")
        save_state(state)
    elif args == ["plugin", "add", "agent-sessions@agent-sessions"]:
        fail_once_if_requested("install")
        state["plugin"] = True
        save_state(state)
    elif args == ["plugin", "remove", "agent-sessions@agent-sessions"]:
        state["plugin"] = False
        save_state(state)
    else:
        raise SystemExit(f"unsupported codex fixture argv: {args!r}")


def claude(args):
    state = load_state()
    if args == ["plugin", "marketplace", "list", "--json"]:
        rows = []
        if state.get("marketplace"):
            rows.append({"name": "agent-sessions", "path": state["source"]})
        print(json.dumps({"marketplaces": rows}))
    elif args == ["plugin", "list", "--json"]:
        rows = []
        if state.get("plugin"):
            rows.append({"id": "agent-sessions@agent-sessions", "scope": "user"})
        print(json.dumps({"plugins": rows}))
    elif args[:5] == ["plugin", "marketplace", "add", "--scope", "user"] and len(args) == 6:
        state.update(marketplace=True, source=args[5])
        save_state(state)
    elif args == ["plugin", "marketplace", "remove", "--scope", "user", "agent-sessions"]:
        state.update(marketplace=False, source="")
        save_state(state)
    elif args == ["plugin", "install", "--scope", "user", "agent-sessions@agent-sessions"]:
        fail_once_if_requested("install")
        state["plugin"] = True
        save_state(state)
    elif args == ["plugin", "uninstall", "--scope", "user", "agent-sessions@agent-sessions"]:
        state["plugin"] = False
        save_state(state)
    else:
        raise SystemExit(f"unsupported claude fixture argv: {args!r}")


def grok(args):
    state = load_state()
    plugin_root = pathlib.Path.home() / ".grok" / "plugins" / "agent-sessions"
    if args == ["inspect", "--json"]:
        plugins, servers = [], []
        if state.get("enabled") and plugin_root.is_dir():
            plugins.append({
                "name": "agent-sessions", "scope": "user", "path": str(plugin_root), "enabled": True,
                "provides": {"skills": 2, "mcpServers": 1},
            })
            servers.append({
                "name": "agent_sessions", "transport": "stdio",
                "target": str(plugin_root / "scripts" / "native-entry"),
                "source": {"type": "plugin", "plugin_name": "agent-sessions", "path": str(plugin_root)},
            })
        print(json.dumps({"plugins": plugins, "mcpServers": servers}))
    elif len(args) >= 3 and args[:2] == ["plugin", "install"]:
        fail_once_if_requested("install")
        state["enabled"] = True
        save_state(state)
    elif args[:3] == ["plugin", "uninstall", "agent-sessions"]:
        state["enabled"] = len(args) == 4 and args[3] == "--keep-data"
        save_state(state)
    else:
        raise SystemExit(f"unsupported grok fixture argv: {args!r}")


def qwen(args):
    state = load_state()
    qwen_home = pathlib.Path(os.environ["QWEN_HOME"])
    extension_root = qwen_home / "extensions" / "agent-sessions"
    policy_path = qwen_home / "extension-store" / "state.json"
    if args[:2] == ["extensions", "install"] and len(args) >= 3:
        fail_once_if_requested("install")
        source = pathlib.Path(args[2])
        if extension_root.exists():
            make_tree_writable(extension_root)
            shutil.rmtree(extension_root)
        extension_root.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        shutil.copytree(source, extension_root)
        make_tree_writable(extension_root)
        (extension_root / ".qwen-extension-install.json").write_text(
            json.dumps({"source": str(source), "type": "local"}), encoding="utf-8"
        )
        policy_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        policy_path.write_text(json.dumps({
            "version": 2, "generation": int(state.get("generation", 0)) + 1,
            "extensions": {"fixture": {"name": "agent-sessions", "defaultActivation": "enabled"}},
        }), encoding="utf-8")
        state.update(installed=True, source=str(source), generation=int(state.get("generation", 0)) + 1)
        save_state(state)
    elif args == ["extensions", "uninstall", "agent-sessions"]:
        if extension_root.exists():
            make_tree_writable(extension_root)
            shutil.rmtree(extension_root)
        policy_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        policy_path.write_text(json.dumps({
            "version": 2, "generation": int(state.get("generation", 0)) + 1, "extensions": {},
        }), encoding="utf-8")
        state.update(installed=False, source="", generation=int(state.get("generation", 0)) + 1)
        save_state(state)
    else:
        raise SystemExit(f"unsupported qwen fixture argv: {args!r}")


if len(sys.argv) < 2:
    raise SystemExit("native fixture operation is required")
{"codex": codex, "claude": claude, "grok": grok, "qwen": qwen}[PRODUCT](sys.argv[1:])
