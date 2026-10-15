"""Minimal OpenAI-compatible SSE server used to rehearse failover.

It answers /v1/models and /v1/chat/completions so the failover drill can prove
that after the primary provider dies the turn is actually *answered* by the
fallback — not merely that a fallback event was emitted.
"""
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL = "drill-model-v1"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # keep the console readable
        pass

    def _json(self, payload, status=200):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            self._json({
                "object": "list",
                "data": [{"id": MODEL, "object": "model", "owned_by": "drill"}],
            })
            return
        self._json({"error": {"message": "not found"}}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}

        if req.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            chunks = ["容灾演练", "成功", "：", "本次回复", "由备用服务商", "完成", "。"]
            for c in chunks:
                frame = {
                    "id": "drill", "object": "chat.completion.chunk", "created": int(time.time()),
                    "model": MODEL,
                    "choices": [{"index": 0, "delta": {"content": c}, "finish_reason": None}],
                }
                self.wfile.write(b"data: " + json.dumps(frame).encode() + b"\n\n")
                self.wfile.flush()
            final = {
                "id": "drill", "object": "chat.completion.chunk", "created": int(time.time()),
                "model": MODEL,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 42, "completion_tokens": 17, "total_tokens": 59},
            }
            self.wfile.write(b"data: " + json.dumps(final).encode() + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return

        self._json({
            "id": "drill", "object": "chat.completion", "created": int(time.time()),
            "model": MODEL,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "容灾演练成功：本次回复由备用服务商完成。"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 42, "completion_tokens": 17, "total_tokens": 59},
        })


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 9911), Handler).serve_forever()
