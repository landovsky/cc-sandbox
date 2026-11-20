#!/usr/bin/env python3
"""sudoers_strip.py: remove NOPASSWD grants for docker*, apt-get, apt, dpkg, snap, dokku and sops from a sudoers file.

Why: those grants are a root shell for any agent on hp; stripping must be exact and refuse what it cannot parse, never guess.


U0 (hp-root cleanup, ❓ Q7). Reads a sudoers file, prints the cleaned file on stdout, and a
report on stderr. It never touches anything else: Defaults, comments, other users' rules and
NOPASSWD rules for other commands stay byte-for-byte. It is deliberately strict:

  * a NOPASSWD line it can't parse (a continuation line, several tag groups, an alias such as
    DOCKER_CMDS, a wrapper such as env or sudoedit, a sha256: digest, `ALL`)
    is NOT guessed at: exit 3, nothing printed, the installer stops and says which line;
  * a line that mixes removable and other commands is rewritten with only the others;
  * a line that grants only removable commands is dropped.

Exit: 0 ok (stdout = new content; stderr says what was removed or "nothing to remove");
3 unparseable NOPASSWD line; 2 usage.
Usage: sudoers_strip.py FILE [--remove docker*,apt-get,apt,dpkg,snap,dokku,sops]
Recipe: `sudoers_strip.py /etc/sudoers.d/90-tomas-nopasswd | diff /etc/sudoers.d/90-tomas-nopasswd -`
"""
import os
import re
import sys

# names are matched on the command's basename; a trailing * is a prefix (docker* also covers docker-compose)
DEFAULT_REMOVE = "docker*,apt-get,apt,dpkg,snap,dokku,sops"
ALIAS = re.compile(r"^[A-Z][A-Z0-9_]*$")          # a Cmnd_Alias: its members are not visible here
NEVER_GUESS = {"env", "sudoedit", "sudo", "sh", "bash"}   # wrappers: the command they run is an argument
LINE = re.compile(r"^(?P<head>\s*[%\w.@-]+\s+[\w.,*-]+\s*=\s*(?:\([^)]*\)\s*)?)NOPASSWD:\s*(?P<cmds>[^:]+)$")


def strip(text, remove):
    out, report = [], []
    for n, line in enumerate(text.splitlines(), 1):
        s = line.strip()
        if "NOPASSWD" not in line or s.startswith("#"):
            out.append(line)
            continue
        if line.rstrip().endswith("\\"):
            raise ValueError(f"line {n}: continuation line, edit by hand: {s[:60]}")
        m = LINE.match(line)
        if not m or "ALL" == m.group("cmds").strip():
            raise ValueError(f"line {n}: NOPASSWD rule I won't guess at, edit by hand: {s[:60]}")
        cmds = [c.strip() for c in m.group("cmds").split(",") if c.strip()]
        if not cmds:
            raise ValueError(f"line {n}: NOPASSWD with no command, edit by hand")
        keep, drop = [], []
        for c in cmds:
            first = c.split()[0]
            base = os.path.basename(first)
            if first == "ALL" or base in ("ALL", ""):
                raise ValueError(f"line {n}: command '{c}' is ALL or empty, edit by hand")
            if ALIAS.match(first) or base in NEVER_GUESS or first.startswith("sha"):
                raise ValueError(f"line {n}: '{c}' is an alias, a wrapper or a digest-pinned entry, edit by hand")
            (drop if any(base == r or (r.endswith("*") and base.startswith(r[:-1])) for r in remove) else keep).append(c)
        if not drop:
            out.append(line)
        elif keep:
            out.append(m.group("head") + "NOPASSWD: " + ", ".join(keep))
            report.append(f"line {n}: removed {', '.join(drop)}; kept {', '.join(keep)}")
        else:
            report.append(f"line {n}: dropped (granted only {', '.join(drop)})")
    return "\n".join(out) + ("\n" if text.endswith("\n") or out else ""), report


def main(argv):
    if not argv or argv[0] in ("-h", "--help"):
        print(__doc__)
        return 0 if argv else 2
    path, remove = argv[0], set(DEFAULT_REMOVE.split(","))
    if "--remove" in argv:
        remove = set(argv[argv.index("--remove") + 1].split(","))
    try:
        new, report = strip(open(path).read(), remove)
    except OSError as ex:
        print(f"sudoers_strip: {ex}", file=sys.stderr)
        return 2
    except ValueError as ex:
        print(f"sudoers_strip: {ex}", file=sys.stderr)
        return 3
    sys.stdout.write(new)
    print("\n".join(report) if report else "nothing to remove", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
