# `ccsbx-root relay-mint`: spec for stream A (M2b, decision D2)

What this is: the one thing hostd needs from root to send a signed message across machines. It exists so
that hostd never holds the session-bus hub's admin secret (decision (1) of 20 Nov: hostd reaches the bus only
through its spoke over HTTP). It is a security path: an independent review before merge.

## Why root
A fenced sender is trusted on the hub only as a `relay` principal minted for ONE session generation with the
public half of that generation's Ed25519 key (B6c, B3). Minting needs the hub's admin secret. hostd (uid
ccsbx) generates the keypair in memory and keeps the private half; root registers the public half.

## Scope: hp only, for now
`relay-mint` mints on the machine that RUNS THE HUB (hp). The hub's admin secret never leaves hp: it is not
copied to the Mac, to future-two or to any other machine, and `hub_url` must be loopback (below). So today
only hp's hostd can send a signed message; a fenced manager on the Mac cannot reply through the hub until
one of these exists:
- **A per-machine minter principal** (proposal): the hub learns a third role, `minter`, bound to one machine
  and able to do exactly one thing, `POST /v1/principals {"role":"relay",...}` for a `sid` that THAT machine's
  own spoke has tracked as fenced with `machine == the minter's machine`. Each machine's root holds only its
  own minter credential (a different secret per machine, revocable one at a time), never the admin secret.
  The relay it mints is bound to the sid and to that machine by the hub, so the Mac cannot mint a relay for
  an hp session. The hub change (a role and its scope rule) goes through the dev manager like every hub change.
- **Or U5/M9**: the hp-signed session token carries the generation's public key and replaces the relay
  principal altogether (the receiver verifies the chain instead of the hub vouching for a key).
Until then the Mac side of T:xmachine-msg waits (`tests/xmachine-mac-test.sh` skips).

## The verb
```
sudo -n /usr/local/sbin/ccsbx-root relay-mint -- <sid> <gen> <pub>
```
- `sid`: `^s[0-9a-z]{4,32}$`. `gen`: a positive decimal integer. `pub`: url-safe base64 without padding of
  exactly 32 bytes (43 characters). Anything else: refused, exit 2, nothing sent anywhere.
- Allowed callers: the service uid only (the sudoers line D writes for the hostd unit lists this verb for
  `ccsbx`, not for the owner's uid).
- Output: one JSON line on stdout, `{"token":"sbp1.…"}`, nothing else on stdout. The token is never written to
  a log, to stderr, to the audit line or to a file.

## Checks, in this order, before the admin secret is read
1. Load `<state_dir>/sessions/<sid>.json` with the same trust checks `run` uses (owner, mode, no symlink).
2. `record.Gen == gen` and `record.Ended == nil`: a stale or ended generation is refused (`stale_generation`).
3. The generation is running: `<run_dir>/s/<sid>/g<gen>/PID` exists and `CLOSED` does not. (A pane that was
   never started, or already closed, has no business sending.)
4. The record's machine is this machine.
5. Rate: at most one mint per (sid, gen) per 10 s (a file under the run dir): a loop in hostd must not hammer
   the hub. A repeat after that re-mints (the hub replaces the earlier token for the same sid and gen). The
   limit and hostd's retry agree: hostd mints again only after a 401 on its relay token, and then asks with
   `--after-401` so root allows ONE mint inside the 10 s window when the previous token was refused; a
   plain repeat inside the window is refused (exit 2, `rate`), and hostd backs off instead of retrying.

## The secret and the hub
- Admin secret: `/etc/ccsbx/hub-admin.secret`, root:root, mode 0600, read with the same trusted-file check as
  `pins_file` (regular file, owned by root, not group/other accessible, parent likewise). Never from an
  argument, an environment variable or a file hostd can write. Missing or readable by others: refused, exit 3.
- Hub address: `hub_url` in `ccsbx-root.toml` (default `http://127.0.0.1:4011`, loopback only; a non-loopback
  host is refused at config load). No redirects followed. Timeout 10 s.
- Request: `POST <hub_url>/v1/principals` with `Authorization: Bearer <admin secret>` and body
  `{"role":"relay","sid":"<sid>","gen":"<gen>","pub":"<pub>"}`. The reply's `token` is printed. A hub that
  answers anything but 201 with a token: exit 3, the status code (not the body) on stderr.

## Revoke
Root revokes, not hostd (hostd has no revoke verb). In `run`'s cleanup, where it writes `CLOSED`:
`DELETE <hub_url>/v1/principals/relay:<sid>/<gen>` (the id url-encoded as one segment), best effort, logged
with sid and gen. `stop` and `gc` do the same for a generation they close. The hub's own 6 h relay lifetime is
the backstop when root could not reach the hub.

## Audit
One line per mint and per revoke in root's log: time, verb, sid, gen, the first 8 characters of sha256(pub),
result. Never the token, never the secret.

## Refusals and exits
| exit | meaning |
|---|---|
| 0 | token printed |
| 2 | refused: bad arguments, unknown sid, stale or ended generation, not running, wrong machine, rate |
| 3 | the hub or the secret: unreachable, 401/403, no secret, secret not trusted |

## Tests this needs
Each refusal above; the secret never appears in stdout, stderr, the audit line or a child process's
arguments; a symlinked or group-readable secret is refused; a fake hub that answers 201 / 401 / 500 / hangs;
the revoke runs in `run`'s cleanup and in `gc` (a stub hub records the DELETE).

## What hostd does with it
`cmd/ccsbx-hostd/hubremote.go`: `rootMinter()` runs exactly the line above (plus `--after-401` before `--` on the one retry) through `start.SudoBin`/`start.RootBin`
(only if the helper is installed and trusted), reads the token from stdout, and keeps the key and token in
memory per (sid, gen). On a 401/403 from the hub it mints again once.
