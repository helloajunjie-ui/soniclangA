"""Measures what the current control-tree backend can actually see.

DESIGN §14 C1 picks 控件树/UIAutomation because Windows coverage is the widest.
This drill reports the real coverage on this machine instead of assuming it: an
honest number here tells a future milestone whether UIAutomation is needed, and a
silently empty tree on every Chromium app would look like a bug in the agent
rather than a limitation of the technique.
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
        r = urllib.request.urlopen(req, timeout=20)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def count(n):
    return 1 + sum(count(c) for c in (n.get("children") or []))


def main():
    s, b = call("GET", "/control/apps")
    if s != 200:
        print("无法列出窗口，请先开启 control 模块")
        return 1
    apps = [a for a in json.loads(b) if not a.get("blocked") and a.get("title")]

    print(f"{'窗口':44s} {'类名':26s} {'节点':>5s}")
    print("-" * 80)

    thin, rich = [], []
    for a in apps:
        call("POST", "/control/allow", {"appId": a["id"], "note": "coverage drill"})
        s, b = call("POST", f"/control/apps/{a['id']}/inspect", {"maxDepth": 6, "maxNodes": 300})
        if s != 200:
            continue
        snap = json.loads(b)
        n = count(snap["root"])
        title = (a["title"] or "")[:42]
        print(f"{title:44s} {(a.get('class') or ''):26s} {n:5d}")
        (thin if n <= 3 else rich).append((title, a.get("class") or ""))

    print()
    print("=" * 68)
    print(f"可枚举控件树（>3 节点）：{len(rich)}/{len(thin) + len(rich)}")
    print("仅窗口本身（≤3 节点）：")
    for t, c in thin:
        print(f"    - {t}  [{c}]")
    print()
    print("说明：Chromium/Electron 系应用把界面渲染在单个 HWND 内，")
    print("      EnumChildWindows 看不到内部元素；这类窗口需要 UI Automation")
    print("      （COM 接口）才能拿到真正的控件树。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
