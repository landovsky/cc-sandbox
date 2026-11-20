# C0a — nested attach spike (2026-11-20, hp-ubuntu, direct mode: not evidence)

Question (plan §3 C0a, ❓ Q14): can a Claude Code TUI that lives in `tmux -L ccsbx` be shown inside
Tom's own tmux (an "attach window"), locally and over ssh, without losing keys, colours, scroll,
copy or resize? A ✗ on a core key would switch Q14 to "the pane lives in Tom's tmux".

How: `spike/c0a.sh` (private tmux servers; nothing of Tom's touched). The "TUI" is a raw-key logger.
tmux 3.4, Tom's `tmux.conf` values for the outer side (escape-time 0).

| What | Result | Detail |
|---|---|---|
| Keys, 21 of them (arrows, Enter, Tab, S-Tab, Esc, Ctrl-C/D/L/R, Alt-b, Alt-Enter, PgUp/PgDn, Home/End, F1, Ctrl-Left, Shift-Enter) | ✓ | Every key reaches the inner program with the same bytes nested as in a pane that is not nested. |
| Esc and the key after it, **with tmux's default `escape-time` 500 ms** | ⚠ | `C0A_ESCAPE_TIME=500 spike/c0a.sh`: Esc arrives late and glued to the next key (`1b03` for Esc then Ctrl-C). Not a problem on Tom's machines (his `tmux.conf` sets 0), **but only if the ccsbx server gets the same**: see "the ccsbx server's own config" below. |
| 24-bit colour | ✓ | `38;2;r;g;b` written by the inner program is on the outer screen untouched. |
| Copy: OSC 52 from the fenced program | ✓ (by design) | With the launcher's `set-clipboard off` + `allow-passthrough off` the write never reaches the outer tmux's buffers. Consequence: copying out of the fenced pane is **not** possible by the program's own OSC 52; Tom copies with the outer tmux's selection of what is on screen. |
| Resize | ✓ | Outer window resized to 140x40; the inner client followed (`140x40`). |
| Prefix | ⚠ | With the default prefix on both servers the outer tmux takes `C-b`, so the inner server's prefix is unreachable unless typed twice. Harmless for an attach window (nothing to drive in the inner tmux), but see below. |
| Over ssh (`ssh -t host tmux -L ccsbx attach …`) | not measured | sshd runs on hp but `ssh localhost` does not log in without a prompt here (no key for it), and nobody was at a terminal. `ccsbx attach <sid>` now exists (it is the remote argv the StartReply promises) and execs the same tmux attach: run `spike/c0a.sh --ssh <host>` from a machine that can ssh to hp. |
| Mouse-wheel scrolling, selecting with the mouse, flicker, a real Claude Code TUI | not measured | They need a person at a terminal (and a model door for the real TUI). |

## The ccsbx server's own config (found while measuring; not applied, it is U2's launcher)

A fresh tmux server **loads `~/.tmux.conf`**. Measured on hp: a new `tmux -L x` has `escape-time 0`,
`prefix C-b`, **`allow-passthrough on`**, `status on`. So the ccsbx server today
- inherits Tom's key bindings and plugins (some could run commands on events in the *fenced* pane's server);
- has `allow-passthrough on` from the moment it starts until the launcher's `set-option` in the same
  invocation turns it off (a tiny window, but a window);
- shows a second status line inside the outer window (one wasted row of the TUI), and
- on a machine whose conf has the default `escape-time`, delays Esc.

Recommendation for `internal/start/tmux.go` (stream A): start the server with `-f /dev/null` (or a
shipped `ccsbx.conf`) and set `status off`, `prefix None`, `escape-time 0`, `set-clipboard off`,
`allow-passthrough off`, `history-limit` explicitly, in the same invocation as `new-session`.
`T:tmux-hardening` should then also assert `status off`/`prefix None`.

## Verdict for Q14

No ✗ on a core key, colour or resize: **keep the attach-window design** (the pane in `tmux -L ccsbx`,
Tom's tmux shows an attach window). Open items before the first real use: the over-ssh half and the
real Claude TUI (mouse scroll, selection copy) need a person; the launcher config above should be done first.
