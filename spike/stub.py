"""Fake Anthropic API: answers every /v1/messages with a canned streamed reply 'STUB-OK'.
No real model, no credential. Logs request paths to stdout."""
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
REPLY = "STUB-OK"
def sse(ev, data): return f"event: {ev}\ndata: {json.dumps(data)}\n\n".encode()
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def _body(self):
        n = int(self.headers.get("content-length") or 0); return self.rfile.read(n) if n else b""
    def _json(self, code, obj):
        b = json.dumps(obj).encode(); self.send_response(code)
        self.send_header("content-type", "application/json"); self.send_header("content-length", str(len(b)))
        self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        print(time.time(), "GET", self.path, flush=True); self._json(200, {})
    def do_HEAD(self):
        self.send_response(200); self.send_header("content-length","0"); self.end_headers()
    def do_POST(self):
        raw = self._body(); print(time.time(), "POST", self.path, len(raw), flush=True)
        if "/v1/messages/count_tokens" in self.path: return self._json(200, {"input_tokens": 10})
        if not self.path.startswith("/v1/messages"): return self._json(200, {})
        try: req = json.loads(raw)
        except Exception: req = {}
        model = req.get("model", "stub")
        msg = {"id": "msg_stub", "type": "message", "role": "assistant", "model": model, "content": [],
               "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 1}}
        if not req.get("stream"):
            msg["content"] = [{"type": "text", "text": REPLY}]; msg["stop_reason"] = "end_turn"
            return self._json(200, msg)
        self.send_response(200); self.send_header("content-type", "text/event-stream")
        self.send_header("cache-control", "no-cache"); self.send_header("connection", "close"); self.end_headers()
        w = self.wfile.write
        w(sse("message_start", {"type": "message_start", "message": msg}))
        w(sse("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}))
        w(sse("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": REPLY}}))
        w(sse("content_block_stop", {"type": "content_block_stop", "index": 0}))
        w(sse("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 3}}))
        w(sse("message_stop", {"type": "message_stop"})); self.wfile.flush(); self.close_connection = True
ThreadingHTTPServer((sys.argv[1] if len(sys.argv) > 1 else "0.0.0.0", int(sys.argv[2]) if len(sys.argv) > 2 else 8080), H).serve_forever()
