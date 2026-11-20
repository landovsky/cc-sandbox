#!/usr/bin/env python3
"""e2e/loopback-shim.py — a loopback TCP listener that forwards every connection to a unix socket, byte for byte.

Why it exists: cc-tree reaches a daemon over HTTP at CCSBX_DAEMON_URLS ("tests, a tunnel"), and sends no credential: the
daemon classes a caller by Tailscale WhoIs, which a laptop-less test cannot supply. The daemon's LOCAL unix socket is
owner class. This shim is the tunnel: 127.0.0.1:PORT -> <state>/hostd.sock, nothing parsed, nothing added, 127.0.0.1
only. It stands in for the tailnet leg and NOTHING else (no WhoIs, no TLS), and says so in the test that uses it.

  loopback-shim.py PORT UNIX_SOCKET
"""
import socket, sys, threading

port, path = int(sys.argv[1]), sys.argv[2]


def pump(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def serve(c):
    u = socket.socket(socket.AF_UNIX)
    try:
        u.connect(path)
    except OSError:
        c.close()
        return
    threading.Thread(target=pump, args=(c, u), daemon=True).start()
    pump(u, c)


l = socket.socket()
l.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
l.bind(("127.0.0.1", port))
l.listen(16)
while True:
    c, _ = l.accept()
    threading.Thread(target=serve, args=(c,), daemon=True).start()
