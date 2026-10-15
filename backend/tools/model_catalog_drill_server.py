"""Mock vendor used to exercise model discovery and curation end to end.

Returns a catalogue with the shapes that actually break things: a name that
contains a slash (meta-llama/Llama-3-70B) and a duplicate prefix, so the
provider/model string handling and the ledger's keying are both covered.
"""

import http.server
import json
import socketserver

MODELS = [
    "abab6.5g-chat",
    "abab6.5s-chat",
    "deepseek-chat",
    "deepseek-reasoner",
    "meta-llama/Llama-3-70B",
    "qwen-max",
]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.endswith("/models"):
            payload = json.dumps(
                {"object": "list", "data": [{"id": m, "object": "model"} for m in MODELS]}
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *args):
        pass


socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", 8899), Handler) as httpd:
    httpd.serve_forever()
