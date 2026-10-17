#!/usr/bin/env python3
"""Mock OpenAI-compatible LLM for local M2 chat-chain verification.
Streams SSE chunks for /v1/chat/completions and lists a model at /v1/models.
No network or real key required."""
import json
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

WORDS = ["你好，", "这是 ", "来自 ", "Mock ", "LLM ", "的 ", "流式 ", "回复。",
         "（AgentHub 对话链路已打通）"]


def user_text(body):
    try:
        msgs = body.get("messages", []) or []
        for m in reversed(msgs):
            if m.get("role") == "user":
                return str(m.get("content", ""))[:40]
    except Exception:
        pass
    return ""


class H(BaseHTTPRequestHandler):
    def _sse(self, prefix):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        text = (prefix + "｜") if prefix else ""
        for i, w in enumerate(WORDS):
            chunk = {"choices": [{"delta": {"content": (text if i == 0 else "") + w}}]}
            self.wfile.write(("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode("utf-8"))
            self.wfile.flush()
            time.sleep(0.04)
        tail = {"choices": [{"delta": {}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 12, "completion_tokens": 24, "total_tokens": 36}}
        self.wfile.write(("data: " + json.dumps(tail, ensure_ascii=False) + "\n\n").encode("utf-8"))
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def do_POST(self):
        if self.path.rstrip("/").endswith("/chat/completions"):
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n) or b"{}")
            self._sse(user_text(body))
        else:
            self.send_response(404)
            self.end_headers()

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"data": [{"id": "mock-model"}]}).encode("utf-8"))
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 9901), H).serve_forever()
