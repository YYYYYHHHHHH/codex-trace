#!/usr/bin/env python3
"""在 ~/.codex/hooks.json 中临时挂载/卸载 payload 采集 hook。

只追加、只删除自己的条目（按脚本路径识别），不改动其他 hooks。
用法：python3 -I scripts/capture-install.py install|uninstall|status
"""
import json
import os
import shutil
import sys
import time

HOOKS_PATH = os.path.expanduser("~/.codex/hooks.json")
BACKUP_DIR = os.path.expanduser("~/.codex-trace/backup")
SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "capture-hook.sh")
EVENTS = [
    "SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
    "PermissionRequest", "SubagentStart", "SubagentStop", "Stop", "Interrupt",
]
TIMEOUT_SECONDS = 3


def load() -> dict:
    with open(HOOKS_PATH, encoding="utf-8") as f:
        return json.load(f)


def save(data: dict) -> None:
    tmp = HOOKS_PATH + ".codex-trace.tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")
    os.replace(tmp, HOOKS_PATH)


def is_ours(entry: dict) -> bool:
    return any(SCRIPT in h.get("command", "") for h in entry.get("hooks", []))


def without_ours(data: dict) -> dict:
    hooks = {
        event: [e for e in entries if not is_ours(e)]
        for event, entries in data.get("hooks", {}).items()
    }
    return {**data, "hooks": {k: v for k, v in hooks.items() if v}}


def install() -> None:
    data = load()
    os.makedirs(BACKUP_DIR, exist_ok=True)
    backup = os.path.join(BACKUP_DIR, f"hooks.json.{time.strftime('%Y%m%dT%H%M%S')}")
    shutil.copy2(HOOKS_PATH, backup)

    base = without_ours(data)
    hooks = dict(base.get("hooks", {}))
    for event in EVENTS:
        entry = {"hooks": [{
            "type": "command",
            "command": f'"{SCRIPT}" {event}',
            "timeout": TIMEOUT_SECONDS,
        }]}
        hooks[event] = [*hooks.get(event, []), entry]
    save({**base, "hooks": hooks})
    print(f"已安装到 {len(EVENTS)} 个事件；备份：{backup}")


def uninstall() -> None:
    data = load()
    cleaned = without_ours(data)
    save(cleaned)
    backups = sorted(os.listdir(BACKUP_DIR)) if os.path.isdir(BACKUP_DIR) else []
    if not backups:
        print("已卸载；未找到备份，无法比对")
        return
    with open(os.path.join(BACKUP_DIR, backups[-1]), encoding="utf-8") as f:
        original = json.load(f)
    same = cleaned == without_ours(original)
    print(f"已卸载；与备份 {backups[-1]} {'一致' if same else '不一致，请人工检查'}")


def status() -> None:
    data = load()
    installed = [e for e, entries in data.get("hooks", {}).items() if any(map(is_ours, entries))]
    print("已挂载事件：", installed or "无")


COMMANDS = {"install": install, "uninstall": uninstall, "status": status}

if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in COMMANDS:
        sys.exit(__doc__)
    COMMANDS[sys.argv[1]]()
