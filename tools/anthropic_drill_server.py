#!/usr/bin/env python3
"""Mock Anthropic Messages API server for AgentHub integration drills.

Speaks enough of the native protocol to exercise the adapter end-to-end:
  - POST /v1/messages  (stream + non-stream)
  - GET  /v1/models   (paginated)
Auth is checked on x-api-key so key rotation/cooldown can be rehearsed.

Behaviour knobs (query string on /v1/messages):
  ?fail=401   always reject with an auth error (exercises key rotation)
  ?tool=1    answer with a tool_use block instead of text

Usage:
    python3 tools/anthropic_drill_server.py --port 9912 --key sk-ant-good
"""
import argparse
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

VERSION = "2023-06-01"
VALID_KEYS = set()
STATE = {"calls": 0}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("[anthropic-mock] " + fmt % args + "\n")

    def _json(self, code, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authed(self):
        key = self.headers.get("x-api-key", "")
        if key not in VALID_KEYS:
            self._json(401, {"type": "error", "error": {
                "type": "authentication_error",
                "message": f"invalid x-api-key: {key[:8]}..."}})
            return False
        return True

    def do_GET(self):
        if not self.path.startswith("/v1/models"):
            self._json(404, {"type": "error", "error": {"type": "not_found_error", "message": "no such endpoint"}})
            return
        if not self._authed():
            return
        # Page 1 advertises has_more so the adapter's pagination is exercised.
        # Any after_id means "past page 1" — keying on a specific cursor value
        # would make the mock itself return page 1 again for an intermediate
        # cursor and mask a real adapter bug behind a duplicate.
        if self.path.find("after_id=") >= 0:
            self._json(200, {"data": [{"id": "claude-mock-b", "display_name": "Claude Mock B",
                                       "max_input_tokens": 200000}],
                             "has_more": False, "last_id": "claude-mock-b"})
        else:
            self._json(200, {"data": [{"id": "claude-mock-a", "display_name": "Claude Mock A",
                                       "max_input_tokens": 200000,
                                       "capabilities": {"image_input": {"supported": True},
                                                        "thinking": {"supported": True}}}],
                             "has_more": True, "last_id": "claude-mock-a"})

    def do_POST(self):
        if not self.path.startswith("/v1/messages"):
            self._json(404, {"type": "error", "error": {"type": "not_found_error", "message": "no such endpoint"}})
            return
        n = int(self.headers.get("Content-Length") or 0)
        req = json.loads(self.rfile.read(n) or b"{}")
        STATE["calls"] += 1

        if "fail=401" in self.path:
            self._json(401, {"type": "error", "error": {
                "type": "authentication_error", "message": "forced failure for drill"}})
            return

        # Mirror the real API's validation so protocol bugs surface here too.
        missing = [f for f in ("model", "max_tokens", "messages") if not req.get(f)]
        if missing:
            self._json(400, {"type": "error", "error": {
                "type": "invalid_request_error",
                "message": f"missing required field(s): {', '.join(missing)}"}})
            return
        msgs = req["messages"]
        if not msgs or msgs[0].get("role") != "user":
            self._json(400, {"type": "error", "error": {
                "type": "invalid_request_error",
                "message": "first message must have role 'user'"}})
            return
        for i in range(1, len(msgs)):
            if msgs[i].get("role") == msgs[i - 1].get("role"):
                self._json(400, {"type": "error", "error": {
                    "type": "invalid_request_error",
                    "message": f"messages[{i}]: roles must alternate"}})
                return
            if msgs[i].get("role") not in ("user", "assistant"):
                self._json(400, {"type": "error", "error": {
                    "type": "invalid_request_error",
                    "message": f"messages[{i}]: invalid role {msgs[i].get('role')!r}"}})
                return
        if "system" in req and any(m.get("role") == "system" for m in msgs):
            self._json(400, {"type": "error", "error": {
                "type": "invalid_request_error",
                "message": "system must be the top-level field, not a message"}})
            return
        if self.headers.get("anthropic-version") != VERSION:
            self._json(400, {"type": "error", "error": {
                "type": "invalid_request_error",
                "message": f"missing/invalid anthropic-version header (got {self.headers.get('anthropic-version')!r})"}})
            return

        # Trigger a tool call the way a real model would: tools were offered,
        # nothing has been answered yet, and the user asked for arithmetic.
        # Keying off the request content (not a URL flag) is what makes this
        # usable through the real client, which never adds query parameters.
        last_user = ""
        for m in msgs:
            if m.get("role") == "user":
                c = m.get("content")
                if isinstance(c, str):
                    last_user = c
                elif isinstance(c, list):
                    for b in c:
                        if b.get("type") == "text":
                            last_user += b.get("text", "")
        wants_math = any(k in last_user for k in ("算", "计算", "1+1"))
        # A tool_result in the history means the model already got its answer.
        answered = any(
            b.get("type") == "tool_result"
            for m in msgs for b in (m.get("content") if isinstance(m.get("content"), list) else [])
        )
        want_tool = bool(req.get("tools")) and wants_math and not answered

        if not req.get("stream"):
            if want_tool and not answered:
                self._json(200, {"id": "msg_mock", "type": "message", "role": "assistant",
                                 "model": req["model"], "stop_reason": "tool_use",
                                 "content": [
                                     {"type": "thinking", "thinking": "需要调用计算器"},
                                     {"type": "text", "text": "我来算一下"},
                                     {"type": "tool_use", "id": "toolu_mock_1", "name": "calculator",
                                      "input": {"expression": "1+1"}}],
                                 "usage": {"input_tokens": 120, "output_tokens": 34}})
            else:
                text = "工具返回 2，所以 1+1 等于 2。" if answered else "你好，我是 Claude Mock。"
                self._json(200, {"id": "msg_mock", "type": "message", "role": "assistant",
                                 "model": req["model"], "stop_reason": "end_turn",
                                 "content": [{"type": "text", "text": text}],
                                 "usage": {"input_tokens": 88, "output_tokens": 12}})
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def frame(payload):
            data = ("data: " + json.dumps(payload) + "\n\n").encode("utf-8")
            self.wfile.write(("%x\r\n" % len(data)).encode() + data + b"\r\n")
            self.wfile.flush()

        frame({"type": "message_start", "message": {
            "id": "msg_mock", "type": "message", "role": "assistant", "model": req["model"],
            "content": [], "usage": {"input_tokens": 120, "output_tokens": 1}}})
        frame({"type": "ping"})
        # A thinking block first, so the reasoning path is exercised.
        frame({"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": ""}})
        frame({"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "用户想打招呼"}})
        frame({"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "sig=="}})
        frame({"type": "content_block_stop", "index": 0})

        if want_tool and not answered:
            frame({"type": "content_block_start", "index": 1,
                   "content_block": {"type": "text", "text": ""}})
            for piece in ("我来", "算一下"):
                frame({"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": piece}})
            frame({"type": "content_block_stop", "index": 1})
            frame({"type": "content_block_start", "index": 2,
                   "content_block": {"type": "tool_use", "id": "toolu_mock_1", "name": "calculator", "input": {}}})
            # Deliberately split mid-object: parsing per-delta would fail here.
            for piece in ('{"expr', 'ession":', '"1+1"}'):
                frame({"type": "content_block_delta", "index": 2,
                       "delta": {"type": "input_json_delta", "partial_json": piece}})
            frame({"type": "content_block_stop", "index": 2})
            frame({"type": "message_delta", "delta": {"stop_reason": "tool_use"},
                   "usage": {"input_tokens": 120, "output_tokens": 41}})
        else:
            text = "工具返回 2，所以 1+1 等于 2。" if answered else "你好，我是 Claude Mock。"
            frame({"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}})
            for i in range(0, len(text), 4):
                frame({"type": "content_block_delta", "index": 1,
                       "delta": {"type": "text_delta", "text": text[i:i + 4]}})
            frame({"type": "content_block_stop", "index": 1})
            frame({"type": "message_delta", "delta": {"stop_reason": "end_turn"},
                   "usage": {"input_tokens": 120, "output_tokens": 15}})

        frame({"type": "message_stop"})
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9912)
    ap.add_argument("--key", action="append", default=[],
                    help="accepted x-api-key (repeatable)")
    args = ap.parse_args()
    VALID_KEYS.update(args.key or ["sk-ant-mock"])
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"[anthropic-mock] listening on 127.0.0.1:{args.port}, keys={sorted(VALID_KEYS)}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()