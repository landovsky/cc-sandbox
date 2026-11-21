#!/usr/bin/env python3
"""u4s-proxy.py: a loopback logging proxy for the U4s spike (cc-sandbox:#3y22).

What: listens on 127.0.0.1:<port>, forwards every request byte-for-byte to an
HTTPS upstream (default api.anthropic.com), and streams the response back
(SSE included). For each request it appends one JSON line to the log:
method, path, request header NAMES, the full `anthropic-beta` value(s),
the server-tool types named in the request body, the response status,
response header names, and whether the response stream mentions the
web-search server tool.

Why: U4s has to learn what an OAuth (subscription) Claude Code sends to a
base-URL gateway (which beta values, which auth header) and whether web
search survives the gateway, without ever writing a credential to disk.

Secret rule: values of authorization, x-api-key, cookie, set-cookie and
proxy-authorization are never logged or printed. Only the header NAME is
recorded, plus the auth SCHEME word (e.g. "Bearer") and a coarse token
shape (prefix up to the second '-', e.g. "sk-ant-", and its length).
No other header value is recorded except anthropic-beta / anthropic-version.

Recipes:
  # start (foreground, Ctrl-C to stop)
  python3 -I spike/u4s-proxy.py --port 47199 --log /tmp/u4s.jsonl
  # point an unfenced Claude Code at it
  ANTHROPIC_BASE_URL=http://127.0.0.1:47199 claude -p 'say hi'
  # read the log
  jq . /tmp/u4s.jsonl

  # half 2 (needs a setup-token, R5b): inject mode. The client holds a dummy
  # token; the proxy strips authorization/x-api-key and injects the token.
  install -m 600 /dev/null ~/.u4s-token && read -rs T && printf %s "$T" > ~/.u4s-token
  python3 -I spike/u4s-proxy.py --inject-token-file ~/.u4s-token --add-beta oauth-2025-04-20
  ANTHROPIC_BASE_URL=http://127.0.0.1:47199 ANTHROPIC_AUTH_TOKEN=dummy claude -p 'say hi'

Flags (also env): --port (U4S_PORT, 47199), --upstream (U4S_UPSTREAM,
api.anthropic.com), --log (U4S_LOG, ./u4s.jsonl), --inject-token-file
(U4S_INJECT_TOKEN_FILE, off; re-read per request so a swapped file takes
effect), --add-beta (U4S_ADD_BETA, comma list appended to anthropic-beta in
inject mode if not already present; off).
"""
import argparse
import http.client
import http.server
import json
import os
import re
import ssl
import sys
import threading
import time

SECRET = {"authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization"}
HOP = {"connection", "keep-alive", "proxy-connection", "transfer-encoding", "te",
       "trailer", "upgrade", "host", "accept-encoding"}
LOG_LOCK = threading.Lock()


def shape(name, value):
    """Non-secret description of a credential header: scheme + prefix + length."""
    v = value.strip()
    scheme = ""
    if name == "authorization" and " " in v:
        scheme, v = v.split(" ", 1)
    m = re.match(r"^([A-Za-z]+-[A-Za-z]+-)", v)  # e.g. sk-ant-  (stops before oat01/api03)
    return {"scheme": scheme, "prefix": m.group(1) if m else "", "len": len(v)}


def server_tools(body):
    try:
        j = json.loads(body)
    except Exception:
        return None
    out = []
    for t in j.get("tools", []) or []:
        if isinstance(t, dict) and t.get("type") and t.get("type") != "custom":
            out.append(t["type"])
    return {"model": j.get("model"), "server_tools": out, "n_tools": len(j.get("tools") or [])}


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # silence default stderr access log
        pass

    def _body(self):
        if self.headers.get("transfer-encoding", "").lower() == "chunked":
            data = b""
            while True:
                n = int(self.rfile.readline().strip().split(b";")[0], 16)
                if n == 0:
                    self.rfile.readline()
                    return data
                data += self.rfile.read(n)
                self.rfile.readline()
        n = int(self.headers.get("content-length") or 0)
        return self.rfile.read(n) if n else b""

    def _do(self):
        t0 = time.time()
        body = self._body()
        rec = {"t": time.strftime("%H:%M:%S"), "method": self.command, "path": self.path,
               "req_headers": [k.lower() for k in self.headers.keys()],
               "beta": self.headers.get_all("anthropic-beta") or [],
               "version": self.headers.get("anthropic-version"),
               "cred": {k.lower(): shape(k.lower(), v) for k, v in self.headers.items()
                        if k.lower() in ("authorization", "x-api-key")}}
        if body and "/messages" in self.path:
            rec["body"] = server_tools(body)
        fwd = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
        if self.server.inject:
            fwd = {k: v for k, v in fwd.items()
                   if k.lower() not in ("authorization", "x-api-key", "anthropic-beta")}
            with open(self.server.inject) as f:
                fwd["Authorization"] = "Bearer " + f.read().strip()
            betas = [b.strip() for v in rec["beta"] for b in v.split(",") if b.strip()]
            betas += [b for b in self.server.add_beta if b not in betas]
            if betas:
                fwd["anthropic-beta"] = ",".join(betas)
            rec["injected"] = {"beta_sent": betas}
        fwd["Content-Length"] = str(len(body))
        conn = http.client.HTTPSConnection(self.server.upstream, 443, timeout=600,
                                           context=ssl.create_default_context())
        try:
            conn.request(self.command, self.path, body=body, headers=fwd)
            r = conn.getresponse()
        except Exception as e:
            rec["error"] = type(e).__name__
            self._write(rec)
            self.send_error(502)
            return
        rec["status"] = r.status
        rec["resp_headers"] = [k.lower() for k, _ in r.getheaders()]
        self.send_response(r.status)
        for k, v in r.getheaders():
            if k.lower() not in HOP and k.lower() != "content-length":
                self.send_header(k, v)
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        seen = {"server_tool_use": 0, "web_search_tool_result": 0, "web_search_requests": False}
        tail = b""
        err_snip = b""
        while True:
            chunk = r.read1(65536)
            if not chunk:
                break
            s = tail + chunk
            seen["server_tool_use"] += s.count(b'"type":"server_tool_use"')
            seen["web_search_tool_result"] += s.count(b'"type":"web_search_tool_result"')
            if re.search(rb'"web_search_requests":\s*[1-9]', s):
                seen["web_search_requests"] = True
            tail = chunk[-64:]
            if r.status >= 400 and len(err_snip) < 400:
                err_snip += chunk[:400 - len(err_snip)]
            self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
            self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")
        rec["resp_scan"] = seen
        if err_snip:  # upstream error bodies carry type/message, never our credential
            rec["error_body"] = err_snip.decode("utf-8", "replace")
        rec["ms"] = int((time.time() - t0) * 1000)
        self._write(rec)

    def _write(self, rec):
        line = json.dumps(rec)
        assert not re.search(r"sk-ant-[A-Za-z0-9]{3,}-[A-Za-z0-9_\-]{8,}", line), "token in log"
        with LOG_LOCK, open(self.server.log, "a") as f:
            f.write(line + "\n")

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = _do


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--port", type=int, default=int(os.environ.get("U4S_PORT", 47199)))
    p.add_argument("--upstream", default=os.environ.get("U4S_UPSTREAM", "api.anthropic.com"))
    p.add_argument("--log", default=os.environ.get("U4S_LOG", "u4s.jsonl"))
    p.add_argument("--inject-token-file", default=os.environ.get("U4S_INJECT_TOKEN_FILE"))
    p.add_argument("--add-beta", default=os.environ.get("U4S_ADD_BETA", ""))
    a = p.parse_args()
    s = http.server.ThreadingHTTPServer(("127.0.0.1", a.port), H)
    s.upstream, s.log = a.upstream, a.log
    s.inject = a.inject_token_file
    s.add_beta = [b.strip() for b in a.add_beta.split(",") if b.strip()]
    mode = f"inject(beta+={s.add_beta})" if s.inject else "observe"
    print(f"u4s-proxy 127.0.0.1:{a.port} -> https://{a.upstream} mode={mode} log={a.log}", file=sys.stderr)
    s.serve_forever()


if __name__ == "__main__":
    main()
