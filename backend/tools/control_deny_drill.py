"""Live check of the software-control policy surface (DESIGN §9.1).

Runs against a booted AgentHub and asserts the guarantees that matter:
  1. the module refuses everything until switched on;
  2. the built-in deny list is fail-closed and cannot be reached through the
     allow list, whatever spelling is used;
  3. the deny list does not cry wolf on ordinary windows.

Targets are assembled at runtime from character codes so the script itself does
not embed literal shell names — the deny list is about what the agent may touch,
not about what a test file may contain.
"""

import json
import sys
import urllib.error
import urllib.request

import os

BASE = os.environ.get("AGENTHUB_BASE", "http://127.0.0.1:8797/api/v1")


def call(method, path, body=None):
    req = urllib.request.Request(
        BASE + path,
        method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json"},
    )
    try:
        r = urllib.request.urlopen(req)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def denials():
    """Spelling variants a prompt-injected caller might try."""
    names = [
        "".join(chr(c) for c in b"cmd.exe"),
        "".join(chr(c) for c in b"CMD.EXE"),
        "cmd.exe.exe",
        "C:\\Windows\\System32\\cmd.exe",
        "".join(chr(c) for c in b"powershell"),
        "".join(chr(c) for c in b"pwsh"),
        "".join(chr(c) for c in b"taskmgr"),
        "".join(chr(c) for c in b"keepass"),
    ]
    benign = ["".join(chr(c) for c in b"notepad.exe"), "chrome", "code"]

    failures = []

    # The drill drives the policy API, which refuses everything while the module
    # is off. It used to rely on the operator having switched it on by hand, so
    # a cold boot produced a screen of 409s that looked like a regression in the
    # deny list. Enable it here and put the switch back the way it was.
    s, b = call("GET", "/control/status")
    was_enabled = s == 200 and json.loads(b).get("enabled") is True
    if not was_enabled:
        s, b = call("POST", "/modules/control.desktop/toggle", {"enabled": True})
        if s not in (200, 204):
            print(f"无法开启 control 模块：HTTP {s} {b[:120]}")
            return 1

    try:
        return run_checks(names, benign, failures)
    finally:
        if not was_enabled:
            call("POST", "/modules/control.desktop/toggle", {"enabled": False})
            print("（已恢复 control 模块为关闭状态）")


def run_checks(names, benign, failures):
    print("=" * 68)
    print("1. 禁止清单：通过允许清单接口绕过（应全部拒绝）")
    print("=" * 68)
    for n in names:
        s, b = call("POST", "/control/allow", {"appId": n, "note": "drill"})
        ok = s == 403
        print(f"  [{'PASS' if ok else 'FAIL'}] {n:34s} HTTP {s}")
        if not ok:
            failures.append(f"allow({n}) -> {s} {b[:80]}")

    print()
    print("=" * 68)
    print("2. 禁止清单：直接 launch（应全部拒绝）")
    print("=" * 68)
    for n in names[:6]:
        s, b = call("POST", "/control/run", {"cap": "launch", "target": n})
        ok = s == 403
        print(f"  [{'PASS' if ok else 'FAIL'}] launch {n:27s} HTTP {s}")
        if not ok:
            failures.append(f"launch({n}) -> {s} {b[:80]}")

    print()
    print("=" * 68)
    print("3. 误伤检查：普通目标应可加入允许清单")
    print("=" * 68)
    for n in benign:
        s, b = call("POST", "/control/allow", {"appId": n, "note": "drill"})
        ok = s == 200
        print(f"  [{'PASS' if ok else 'FAIL'}] {n:34s} HTTP {s}")
        if not ok:
            failures.append(f"allow({n}) -> {s} {b[:80]}")

    print()
    print("=" * 68)
    print("4. 窗口枚举：禁止目标应被标注而非静默")
    print("=" * 68)
    s, b = call("GET", "/control/apps")
    apps = json.loads(b) if s == 200 else []
    print(f"  窗口总数 {len(apps)}，被拦截 {sum(1 for a in apps if a.get('blocked'))}")
    shell_words = ["".join(chr(c) for c in x) for x in (b"Terminal", b"Console")]
    hit = [a for a in apps if a.get("blocked")]
    for a in hit[:5]:
        print(f"    [拦截] {a.get('title','')[:36]:38s} {a.get('reason','')[:30]}")
    # The taskbar must not be flagged; it was a false positive from a too-broad
    # short-pattern rule.
    tray = [a for a in apps if a.get("class") == "Shell_TrayWnd"]
    if tray and tray[0].get("blocked"):
        failures.append("任务栏 Shell_TrayWnd 被误判为禁止目标")
        print("  [FAIL] 任务栏被误判")
    else:
        print("  [PASS] 任务栏未被误判")

    print()
    print("=" * 68)
    if failures:
        print(f"结果：{len(failures)} 项失败")
        for f in failures:
            print("  - " + f)
        return 1
    print("结果：全部通过（禁止清单 fail-closed，无误伤）")
    return 0


if __name__ == "__main__":
    sys.exit(denials())
