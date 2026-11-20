#!/usr/bin/env python3
# units/lib/hub_listen.py — may the hub's admin secret be sent to HUB_URL? One rule, the same as ccsbx-root's checkHubListener.
# Why: the secret goes to whatever answers at the hub's address. On hp today the hub listens only on the tailnet address, so a
# local uid could bind 127.0.0.1:4011, pass a check that looks at one listener, and be handed the secret. So: EVERY LISTEN socket on
# the port (tcp and tcp6) must belong to the owner, and the address we send to must be assigned to THIS host (/proc/net/fib_trie: a typo
# for another device's address must not pass) and be one of those listener addresses or be covered by a wildcard listener (0.0.0.0 or ::).
# The same refusals, in the same order, as ccsbx-root's checkHubURL + checkHubListener (n1-a-reach 1bfa14c): url form, nothing listening,
# not this host's address, a foreign listener, nobody listening at that address. The address must be a canonical dotted quad (no leading
# zeros: curl and Go would read 010 as octal or reject it). A wildcard "::" is not guaranteed to take IPv4 (IPV6_V6ONLY), which is
# harmless: the rule still demands the owner's uid, so the worst case is a connection that fails. A socket with an unknown owner,
# no listener at all, or a table line we cannot parse, is a refusal.
#   hub_listen.py HUB_URL OWNER_UID     exit 0: yes; exit 1: refused (the reason on stdout); exit 2: usage
# HUB_URL is http://A.B.C.D:PORT (an IPv4 literal: no host names, so there is no resolver to trick). Reads /proc/net/tcp and tcp6.
# Test hook: --net-dir DIR reads DIR/tcp, DIR/tcp6 and DIR/fib_trie instead; honoured only with CCSBX_ROOT_PREFIX set (never in a real run).
import os, re, socket, sys

def parse_ip(h):
    b = bytes.fromhex(h)
    if len(b) == 4:
        return socket.inet_ntoa(b[::-1])
    words = [b[i:i + 4][::-1] for i in range(0, 16, 4)]
    raw = b"".join(words)
    if raw == b"\0" * 16: return "::"
    if raw[:12] == b"\0" * 10 + b"\xff\xff": return socket.inet_ntoa(raw[12:])   # IPv4-mapped
    return socket.inet_ntop(socket.AF_INET6, raw)

def listeners(net_dir, port):
    out = []
    for name in ("tcp", "tcp6"):
        try: lines = open(os.path.join(net_dir, name)).read().splitlines()[1:]
        except OSError as e: return None, "cannot read %s/%s: %s" % (net_dir, name, e.strerror)
        for ln in lines:
            f = ln.split()
            try:
                if len(f) < 8 or f[3] != "0A": continue           # 0A = LISTEN
                ip, p = f[1].rsplit(":", 1)
                if int(p, 16) != port: continue
                out.append((parse_ip(ip), int(f[7])))
            except (ValueError, OSError) as e:
                return None, "cannot parse a line of %s/%s (%s): %r" % (net_dir, name, e, ln[:60])
    return out, ""

def local_addrs(net_dir):
    """The IPv4 addresses assigned to this host: the '/32 host LOCAL' entries of fib_trie (the kernel delivers them locally)."""
    try: lines = open(os.path.join(net_dir, "fib_trie")).read().splitlines()
    except OSError as e: return None, "cannot read %s/fib_trie: %s" % (net_dir, e.strerror)
    out, last = set(), None
    for ln in lines:
        m = re.search(r"\|-- (\d+\.\d+\.\d+\.\d+)$", ln)
        if m: last = m.group(1); continue
        if last and "/32 host LOCAL" in ln: out.add(last)
    return out, ""

OCTET = r"(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)"

def decide(url, owner_uid, net_dir):
    m = re.fullmatch(r"http://(%s(?:\.%s){3}):(\d{1,5})" % (OCTET, OCTET), url)
    if not m: return "hub url '%s' is not http://A.B.C.D:PORT (a canonical IPv4 literal, no leading zeros; no host names)" % url
    ip, ps = m.group(1), m.group(2)
    port = int(ps)
    if not 1 <= port <= 65535 or str(port) != ps: return "hub url '%s': the port must be 1..65535 without leading zeros" % url
    ls, err = listeners(net_dir, port)
    if ls is None: return err
    if not ls: return "nothing listens on port %d" % port
    mine, err = local_addrs(net_dir)
    if mine is None: return err
    if ip not in mine: return "%s is not an address of this host: the secret is NOT sent" % ip
    for a, uid in ls:
        if uid != owner_uid: return "uid %d listens on %s:%d (not the owner, uid %d): the secret is NOT sent" % (uid, a, port, owner_uid)
    if not any(a == ip or a in ("0.0.0.0", "::") for a, _ in ls):
        return "no listener on port %d has the address %s (listeners: %s): the hub is not where the url says" % (port, ip, ", ".join(sorted(a for a, _ in ls)))
    return ""

def main(argv):
    net_dir = "/proc/net"
    if "--net-dir" in argv:
        i = argv.index("--net-dir")
        if not os.environ.get("CCSBX_ROOT_PREFIX"): print("--net-dir is a test hook (needs CCSBX_ROOT_PREFIX)"); return 2
        net_dir = argv[i + 1]; del argv[i:i + 2]
    if len(argv) != 2 or not argv[1].isdigit(): print(__doc__.split("\n", 1)[0]); return 2
    why = decide(argv[0], int(argv[1]), net_dir)
    if why: print(why); return 1
    return 0

if __name__ == "__main__": sys.exit(main(sys.argv[1:]))
