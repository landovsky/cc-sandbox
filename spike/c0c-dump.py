"""C0c: record the FIELD NAMES (and JSON types, never values) of every hook/statusLine payload.
Usage in settings: "command": "python3 /spike/c0c-dump.py <Event>". Appends to $C0C_OUT (default /tmp/c0c/fields.txt)."""
import json, os, sys
ev = sys.argv[1] if len(sys.argv) > 1 else "unknown"
try:
    d = json.loads(sys.stdin.read() or "{}")
except ValueError:
    d = {"_unparsable": True}


def walk(prefix, v, depth, out):
    out.add(f"{ev}\t{prefix}\t{type(v).__name__}")
    if isinstance(v, dict) and depth < 2:
        for k in v:
            walk(f"{prefix}.{k}", v[k], depth + 1, out)


out = set()
if isinstance(d, dict):
    for k in d:
        walk(k, d[k], 1, out)
p = os.environ.get("C0C_OUT", "/tmp/c0c/fields.txt")
os.makedirs(os.path.dirname(p), exist_ok=True)
with open(p, "a") as f:
    f.write("\n".join(sorted(out)) + "\n")
