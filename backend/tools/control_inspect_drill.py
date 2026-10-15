"""Live check of the control-tree inspector (DESIGN §9.1).

Verifies that inspect returns a bounded, well-formed tree for a real window and
that the bounds are actually enforced — an unbounded walk over a browser window
would hang the request and flood the model's context.
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


def main():
    # The module refuses everything while it is off, and the drill needs windows
    # to inspect. Enable it (recording the previous state) instead of asking the
    # operator to have switched it on beforehand — otherwise a cold boot reports
    # "无法列出窗口", which reads like a broken inspector.
    s, b = call("GET", "/control/status")
    was_enabled = s == 200 and json.loads(b).get("enabled") is True
    if not was_enabled:
        s, b = call("POST", "/modules/control.desktop/toggle", {"enabled": True})
        if s not in (200, 204):
            print(f"无法开启 control 模块：HTTP {s} {b[:120]}")
            return 1
    try:
        return inspect_once()
    finally:
        if not was_enabled:
            call("POST", "/modules/control.desktop/toggle", {"enabled": False})
            print("（已恢复 control 模块为关闭状态）")


def inspect_once():
    s, b = call("GET", "/control/apps")
    if s != 200:
        print(f"无法列出窗口：HTTP {s} {b[:120]}")
        return 1
    apps = [a for a in json.loads(b) if not a.get("blocked") and a.get("title")]
    if not apps:
        print("没有可用于测试的窗口")
        return 1

    target = apps[0]
    print(f"目标窗口：{target['title'][:50]}  ({target['id']})")

    failures = []

    # The allow-list mode is on by default once the module is enabled, so the
    # window under test has to be registered first — exactly what the console
    # does when the operator clicks "允许".
    s, b = call("POST", "/control/allow", {"appId": target["id"], "note": "inspect drill"})
    if s != 200:
        print(f"  登记允许清单失败 HTTP {s}: {b[:120]}")
        return 1
    print("已登记到允许清单")

    print()
    print("=" * 68)
    print("1. 默认探查")
    print("=" * 68)
    s, b = call("POST", f"/control/apps/{target['id']}/inspect", {})
    if s != 200:
        print(f"  [FAIL] HTTP {s}: {b[:120]}")
        return 1
    snap = json.loads(b)

    def count(n):
        return 1 + sum(count(c) for c in (n.get("children") or []))

    n = count(snap["root"])
    print(f"  [PASS] 根节点 type={snap['root'].get('type') or 'window'} 节点数={n} 截断={snap['truncated']}")
    for c in (snap["root"].get("children") or [])[:5]:
        print(f"      - type={c.get('type') or 'window':8s} name={(c.get('name') or '—')[:38]}")

    print()
    print("=" * 68)
    print("2. 节点预算必须生效（防爆炸）")
    print("=" * 68)
    s, b = call("POST", f"/control/apps/{target['id']}/inspect", {"maxNodes": 20, "maxDepth": 3})
    if s != 200:
        print(f"  [FAIL] HTTP {s}: {b[:120]}")
        failures.append("bounded inspect failed")
    else:
        small = json.loads(b)
        c = count(small["root"])
        ok = c <= 20
        print(f"  [{'PASS' if ok else 'FAIL'}] 请求上限 20，实际 {c}，截断标记={small['truncated']}")
        if not ok:
            failures.append(f"node budget ignored: {c} > 20")

    print()
    print("=" * 68)
    print("3. 无效窗口 id 必须被拒")
    print("=" * 68)
    s, b = call("POST", "/control/apps/h:99999999/inspect", {})
    ok = s >= 400
    print(f"  [{'PASS' if ok else 'FAIL'}] HTTP {s}: {b[:90]}")
    if not ok:
        failures.append("forged window id accepted")

    print()
    print("=" * 68)
    print("4. 未知能力必须被拒")
    print("=" * 68)
    s, b = call("POST", "/control/run", {"cap": "format_disk", "appId": target["id"]})
    ok = s >= 400
    print(f"  [{'PASS' if ok else 'FAIL'}] HTTP {s}: {b[:90]}")
    if not ok:
        failures.append("unknown capability accepted")

    print()
    if failures:
        print(f"结果：{len(failures)} 项失败")
        for f in failures:
            print("  - " + f)
        return 1
    print("结果：控件树探查全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
