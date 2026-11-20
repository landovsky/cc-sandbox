"""C0c fake Anthropic API: the first request of a turn answers with ONE Bash tool call ("true"), the
request that carries its tool_result answers 'STUB-OK'. So a -p run fires SessionStart,
UserPromptSubmit, PreToolUse, PostToolUse, Stop and SessionEnd without a model. No credential."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def sse(ev, data):
    return f"event: {ev}\ndata: {json.dumps(data)}\n\n".encode()


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        self._json(200, {})

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(n) if n else b""
        if "/v1/messages/count_tokens" in self.path:
            return self._json(200, {"input_tokens": 10})
        if not self.path.startswith("/v1/messages"):
            return self._json(200, {})
        try:
            req = json.loads(raw)
        except ValueError:
            req = {}
        last = (req.get("messages") or [{}])[-1].get("content")
        has_result = isinstance(last, list) and any(isinstance(b, dict) and b.get("type") == "tool_result" for b in last)
        tool = (not has_result) and any(t.get("name") == "Bash" for t in req.get("tools", []))
        msg = {"id": "msg_stub", "type": "message", "role": "assistant", "model": req.get("model", "stub"), "content": [],
               "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 1}}
        blocks = ([{"type": "tool_use", "id": "toolu_stub1", "name": "Bash", "input": {"command": "true", "description": "stub"}}]
                  if tool else [{"type": "text", "text": "STUB-OK"}])
        stop = "tool_use" if tool else "end_turn"
        if not req.get("stream"):
            msg["content"], msg["stop_reason"] = blocks, stop
            return self._json(200, msg)
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("cache-control", "no-cache")
        self.send_header("connection", "close")
        self.end_headers()
        w = self.wfile.write
        w(sse("message_start", {"type": "message_start", "message": msg}))
        b = blocks[0]
        if tool:
            w(sse("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {**b, "input": {}}}))
            w(sse("content_block_delta", {"type": "content_block_delta", "index": 0,
                                          "delta": {"type": "input_json_delta", "partial_json": json.dumps(b["input"])}}))
        else:
            w(sse("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}))
            w(sse("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "STUB-OK"}}))
        w(sse("content_block_stop", {"type": "content_block_stop", "index": 0}))
        w(sse("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                                "usage": {"output_tokens": 3}}))
        w(sse("message_stop", {"type": "message_stop"}))
        self.wfile.flush()
        self.close_connection = True


ThreadingHTTPServer((sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1", int(sys.argv[2]) if len(sys.argv) > 2 else 8099), H).serve_forever()
