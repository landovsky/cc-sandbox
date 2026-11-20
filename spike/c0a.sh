#!/usr/bin/env bash
# C0a spike: nested attach — a TUI in `tmux -L ccsbx` shown inside Tom's tmux (plan §3 C0a, ❓ Q14).
#
#   spike/c0a.sh [--ssh HOST]    run the checks that can run without a human at a terminal and print
#                                a ✓ / ✗ / ⚠ line each (all in private tmux servers; nothing of Tom's is touched)
#   spike/c0a.sh --help        (C0A_ESCAPE_TIME=500 spike/c0a.sh: tmux's default escape-time instead of Tom's 0)
# What it measures, with a raw-key logger as the "TUI":
#   keys     every key sent to the OUTER pane arrives at the inner program as the same bytes as when it is
#            sent to a pane that is not nested (arrows, Enter, Tab, Esc, Ctrl-C/D/L/R, Alt-b, PgUp/PgDn, Home/End, F1)
#   colours  a 24-bit colour written by the inner program survives to the outer screen
#   copy     an OSC 52 clipboard write from the inner program does NOT reach the outer tmux's buffers
#            (the hardening U2 puts on the ccsbx server: set-clipboard off, allow-passthrough off)
#   resize   resizing the outer window resizes the inner client
#   prefix   what an outer-prefix key does to the inner program (it must not reach it)
# With --ssh HOST the attach runs through `ssh -t HOST tmux -L …` (BatchMode; skipped if ssh can't log in).
# NOT measured here (need a person at a real terminal): mouse wheel scrolling, selection copy by mouse,
# a real Claude Code TUI, flicker. They are listed as "not measured" in spike/c0a.md.
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
SSH=""; [ "${1:-}" = "--ssh" ] && SSH=${2:-localhost}
ESCT=${C0A_ESCAPE_TIME:-0}   # Tom's tmux.conf sets escape-time 0; C0A_ESCAPE_TIME=500 shows tmux's default
P=$$; OUTER=c0a-out-$P; INNER=c0a-in-$P; DIRECT=c0a-dir-$P
D=$(mktemp -d); trap 'tmux -L $OUTER kill-server 2>/dev/null; tmux -L $INNER kill-server 2>/dev/null; tmux -L $DIRECT kill-server 2>/dev/null' EXIT
cat > "$D/logger.py" <<'PY'
import os, sys, tty, select
out = sys.argv[1]; fd = sys.stdin.fileno(); tty.setraw(fd)
sys.stdout.write("READY\r\n"); sys.stdout.flush()
while True:
    r, _, _ = select.select([fd], [], [], 0.1)
    if not r: continue
    b = os.read(fd, 64)
    if not b: break
    open(out, "a").write(b.hex() + "\n")
    if b == b"\x1d": break   # Ctrl-] ends it
PY
cat > "$D/paint.py" <<'PY'
import sys, time
sys.stdout.write("\x1b[38;2;12;200;99mTRUECOLOR\x1b[0m \x1b[1;31mBOLDRED\x1b[0m\r\n")
sys.stdout.write("\x1b]52;c;U1BJS0VELUNMSVBCT0FSRA==\x07")    # OSC 52: set the clipboard to a canary
sys.stdout.flush(); time.sleep(30)
PY
ok() { printf '✓ %-9s %s\n' "$1" "$2"; }; bad() { printf '✗ %-9s %s\n' "$1" "$2"; }; warn() { printf '⚠ %-9s %s\n' "$1" "$2"; }
tm() { tmux -L "$1" "${@:2}"; }

# the key list: name, tmux send-keys argument(s)
KEYS=("Up:Up" "Down:Down" "Left:Left" "Right:Right" "Enter:Enter" "Tab:Tab" "S-Tab:BTab" "Esc:Escape" "C-c:C-c" "C-d:C-d" "C-l:C-l" "C-r:C-r" "M-b:M-b" "M-Enter:M-Enter" "PgUp:PageUp" "PgDn:PageDown" "Home:Home" "End:End" "F1:F1" "C-Left:C-Left" "S-Enter:S-Enter")
run_keys() { # $1 = tmux server to send to, $2 = log file
  for k in "${KEYS[@]}"; do tm "$1" send-keys -t t "${k#*:}" 2>/dev/null; sleep 0.25; echo "--${k%%:*}" >> "$2"; done
}

# --- direct (not nested) baseline ------------------------------------------------------------------
tm $DIRECT -f /dev/null new-session -d -s t -x 100 -y 30 "python3 $D/logger.py $D/direct.log" || { echo "tmux failed"; exit 1; }
tm $DIRECT set-option -s escape-time $ESCT
sleep 0.6; run_keys $DIRECT "$D/direct.log"; tm $DIRECT kill-server 2>/dev/null

# --- nested: inner server runs the logger, outer pane attaches to it ----------------------------------
tm $INNER -f /dev/null new-session -d -s t -x 100 -y 30 "python3 $D/logger.py $D/nested.log"
tm $INNER set-option -g set-clipboard off; tm $INNER set-option -g allow-passthrough off
tm $INNER set-option -g status off
if [ -n "$SSH" ]; then
  ssh -o BatchMode=yes -o ConnectTimeout=5 "$SSH" true 2>/dev/null || { warn ssh "ssh to $SSH does not log in without a prompt: the over-ssh half is not measured"; SSH=""; }
fi
ATT="tmux -L $INNER attach -t t"; [ -n "$SSH" ] && ATT="ssh -t -o BatchMode=yes $SSH tmux -L $INNER attach -t t"
tm $INNER set-option -s escape-time $ESCT
tm $OUTER -f /dev/null new-session -d -s t -x 100 -y 30 "$ATT"; tm $OUTER set-option -s escape-time $ESCT
sleep 1; run_keys $OUTER "$D/nested.log"

python3 - "$D/direct.log" "$D/nested.log" <<'PY'
import sys
def parse(p):
    out, cur = {}, []
    for l in open(p).read().split("\n"):
        if l.startswith("--"): out[l[2:]] = " ".join(cur); cur = []
        elif l: cur.append(l)
    return out
d, n = parse(sys.argv[1]), parse(sys.argv[2])
bad = [k for k in d if d[k] != n.get(k)]
for k in d:
    if k in bad: print(f"  key {k}: direct {d[k] or '(nothing)'} / nested {n.get(k) or '(nothing)'}")
sys.exit(1 if bad else 0)
PY
if [ $? -eq 0 ]; then ok keys "all ${#KEYS[@]} keys reach the inner program with the same bytes nested as direct"; else bad keys "keys above differ (nested vs direct)"; fi

# --- colours, OSC 52 ----------------------------------------------------------------------------------
tm $INNER kill-server 2>/dev/null; tm $OUTER kill-server 2>/dev/null; sleep 0.3
tm $INNER -f /dev/null new-session -d -s t -x 100 -y 30 "python3 $D/paint.py"
tm $INNER set-option -g set-clipboard off; tm $INNER set-option -g allow-passthrough off; tm $INNER set-option -g status off
tm $OUTER -f /dev/null new-session -d -s t -x 100 -y 30 "$ATT"; sleep 1.2
screen=$(tm $OUTER capture-pane -p -e -t t)
if grep -q $'38;2;12;200;99' <<<"$screen"; then ok colours "24-bit colour reaches the outer screen untouched"
elif grep -q $'38;5;' <<<"$screen"; then warn colours "24-bit colour is down-converted to 256 colours (set terminal-overrides ',*:RGB' on the ccsbx server and the outer one)"
else warn colours "colour not visible in the outer capture: $(head -c 120 <<<"$screen" | tr -d '\033')"; fi
if tm $OUTER show-buffer 2>/dev/null | grep -q .; then bad copy "an OSC 52 write from the inner program reached the outer tmux's paste buffer"
else ok copy "an OSC 52 clipboard write from the fenced program does not reach the outer tmux (set-clipboard off, allow-passthrough off)"; fi

# --- resize ---------------------------------------------------------------------------------------------
tm $OUTER resize-window -t t -x 140 -y 40 2>/dev/null; sleep 0.8
sz=$(tm $INNER list-clients -F '#{client_width}x#{client_height}' 2>/dev/null | head -1)
[ "$sz" = "140x40" ] && ok resize "outer window resized to 140x40: the inner client followed ($sz)" || warn resize "inner client is $sz after resizing the outer window to 140x40"

# --- prefix: the outer prefix must not reach the inner program, the inner prefix must not capture it ------------
if [ -z "$SSH" ]; then
  tm $INNER kill-server 2>/dev/null; tm $OUTER kill-server 2>/dev/null; sleep 0.3; rm -f "$D/nested.log"
  tm $INNER -f /dev/null new-session -d -s t -x 100 -y 30 "python3 $D/logger.py $D/nested.log"; tm $INNER set-option -g status off
  tm $OUTER -f /dev/null new-session -d -s t -x 100 -y 30 "$ATT"; sleep 1
  tm $OUTER send-keys -t t C-b; sleep 0.3; tm $OUTER send-keys -t t z; sleep 0.3
  got=$(cat "$D/nested.log" 2>/dev/null | tr '\n' ' ')
  if [ -z "${got// /}" ]; then warn prefix "with the default prefix (C-b) on both servers the outer tmux swallows C-b: the inner server's own prefix is unreachable unless typed twice. Fine for an attach pane (nothing to drive in the inner tmux); give the ccsbx server 'prefix None' and 'status off' so it adds no second status line and cannot steal keys"
  else ok prefix "C-b z reached the inner program as: $got"; fi
fi
echo "not measured (needs a person at a terminal): mouse-wheel scrolling, selecting with the mouse, flicker, a real Claude Code TUI"
