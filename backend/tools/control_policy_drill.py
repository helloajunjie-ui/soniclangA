"""软件控制策略面联调：门控默认值、严格模式、禁止清单拦截。

与 control_deny_drill.py 的区别：那份演练查的是"禁止清单能不能被拼写绕过"，
这份查的是"策略状态本身对不对"——默认是否关闭、开启后是否进入严格模式、
危险动作有没有被拒绝、禁止目标能不能混进允许清单。

用法：python tools/control_policy_drill.py
环境变量 AGENTHUB_BASE 覆盖地址，默认 http://127.0.0.1:8797/api/v1
"""

import json
import os
import urllib.error
import urllib.request

BASE = os.environ.get("AGENTHUB_BASE", "http://127.0.0.1:8797/api/v1")
MODULE = "control.desktop"

failures = []


def call(method, path, body=None):
    req = urllib.request.Request(
        BASE + path,
        method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.loads(r.read().decode() or "null")
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw.strip()


def check(label, ok, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {label}{(' — ' + detail) if detail else ''}")
    if not ok:
        failures.append(label)


def toggle(enabled):
    return call("POST", f"/modules/{MODULE}/toggle", {"enabled": enabled})


print("=== 1. 默认状态：必须关闭，且未进入严格模式 ===")
code, st = call("GET", "/control/status")
check("GET /control/status 返回 200", code == 200, f"HTTP {code}")
if isinstance(st, dict):
    check("默认 enabled=false", st.get("enabled") is False, f"enabled={st.get('enabled')}")
    check("平台被识别为 windows", st.get("platform") == "windows", str(st.get("platform")))
    check("后端标记为可用", st.get("supported") is True, str(st.get("supported")))
    check(
        "返回该平台确定的行为说明",
        isinstance(st.get("notice"), str) and "禁止清单" in st.get("notice", ""),
    )
else:
    check("status 是 JSON 对象", False, repr(st)[:80])

print()
print("=== 2. 关闭时不得泄漏窗口列表 ===")
code, body = call("GET", "/control/apps")
check("GET /control/apps 被拒绝（409）", code == 409, f"HTTP {code} {str(body)[:60]}")
code, _ = call("POST", "/control/run", {"cap": "list_apps"})
check("POST /control/run 被拒绝", code in (409, 403), f"HTTP {code}")

print()
print("=== 3. 开启模块：应自动进入严格模式 ===")
code, _ = toggle(True)
check("toggle 成功", code in (200, 204), f"HTTP {code}")
code, st = call("GET", "/control/status")
check("开启后 enabled=true", isinstance(st, dict) and st.get("enabled") is True, f"{st}")
check(
    "开启后 requireAllow=true（严格模式：只有登记过的应用可控）",
    isinstance(st, dict) and st.get("requireAllow") is True,
    f"requireAllow={st.get('requireAllow') if isinstance(st, dict) else st}",
)
if isinstance(st, dict):
    check("允许清单初始为空", st.get("allowlist") in ([], None), f"allowlist={st.get('allowlist')}")

print()
print("=== 4. 严格模式：未登记的应用必须被拒绝 ===")
code, apps = call("GET", "/control/apps")
check("开启后可枚举窗口", code == 200 and isinstance(apps, list), f"HTTP {code}")
ordinary = [a for a in apps if not a.get("blocked")] if isinstance(apps, list) else []
if ordinary:
    target = ordinary[0]
    code, body = call("POST", "/control/run", {"cap": "focus", "appId": target["id"]})
    check(
        "未登记窗口的聚焦请求被拒绝（403 not-allowed）",
        code == 403,
        f"HTTP {code} {str(body)[:70]}",
    )
else:
    check("存在普通窗口可供测试", False, "枚举结果为空")

print()
print("=== 5. 禁止目标不能混进允许清单 ===")
for target in ["cmd.exe", "powershell.exe", "taskmgr.exe", "C:\\Windows\\System32\\cmd.exe"]:
    code, body = call("POST", "/control/allow", {"appId": target, "note": "drill"})
    check(f"允许清单拒绝 {target}", code == 403, f"HTTP {code} {str(body)[:60]}")

print()
print("=== 6. 危险动作即使已登记也不得由 API 之外的路径执行 ===")
code, _ = call("POST", "/control/run", {"cap": "launch", "target": "cmd.exe"})
check("launch 禁止目标被拒绝", code == 403, f"HTTP {code}")
code, _ = call("POST", "/control/run", {"cap": "nope"})
check("未知能力被拒绝", code in (400, 403), f"HTTP {code}")

print()
print("=== 7. 收尾：恢复默认关闭 ===")
toggle(False)
code, st = call("GET", "/control/status")
check(
    "关闭后 enabled=false",
    isinstance(st, dict) and st.get("enabled") is False,
    f"enabled={st.get('enabled') if isinstance(st, dict) else st}",
)

print()
if failures:
    print(f"共 {len(failures)} 项未通过：")
    for f in failures:
        print("  -", f)
    raise SystemExit(1)
print("全部通过")
