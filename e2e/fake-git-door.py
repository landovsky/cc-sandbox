#!/usr/bin/env python3
"""e2e/fake-git-door.py — a fake egress door for the git route, for e2e/08-sidecar only.

What it is: `git http-backend` behind a loopback HTTP listener, serving <root>/git/<owner>/<repo>.git, with
http.receivepack on in the repo's own config. What it is NOT: a door. It checks no token and no proof (the real doors
do, and Go tests cover that), applies no U16 rule (that is gate M), and answers only on 127.0.0.1. It records one line
per request in <log> (method, path, and whether an Authorization or X-Ccsbx-* header arrived) so a test can assert what
a door would have seen.

  fake-git-door.py ROOT PORT LOG
"""
import http.server, subprocess, sys, urllib.parse

ROOT, PORT, LOG = sys.argv[1], int(sys.argv[2]), sys.argv[3]


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _run(self):
        u = urllib.parse.urlsplit(self.path)
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        seen = ",".join(k for k in self.headers.keys() if k.lower() == "authorization" or k.lower().startswith("x-ccsbx"))
        with open(LOG, "a") as f:
            f.write(f"{self.command} {u.path}?{u.query} headers={seen or '-'}\n")
        env = {"PATH": "/usr/bin:/bin", "GIT_PROJECT_ROOT": ROOT, "GIT_HTTP_EXPORT_ALL": "1", "REMOTE_USER": "door",
               "REQUEST_METHOD": self.command, "PATH_INFO": u.path, "QUERY_STRING": u.query,
               "CONTENT_TYPE": self.headers.get("Content-Type", ""), "CONTENT_LENGTH": str(len(body)),
               "HTTP_GIT_PROTOCOL": self.headers.get("Git-Protocol", ""), "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}
        p = subprocess.run(["/usr/lib/git-core/git-http-backend"], input=body, env=env, capture_output=True)
        head, _, payload = p.stdout.partition(b"\r\n\r\n")
        status, headers = 200, []
        for line in head.split(b"\r\n"):
            k, _, v = line.partition(b": ")
            if k.lower() == b"status":
                status = int(v.split()[0])
            elif k:
                headers.append((k.decode(), v.decode()))
        self.send_response(status)
        for k, v in headers:
            if k.lower() != "content-length":
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    do_GET = do_POST = _run

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
