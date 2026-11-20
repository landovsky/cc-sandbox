# M2c: remote start, `POST /v1/sessions` on the target daemon (the contract, revision 2, pinned before it is built)

What this is: the HTTP contract for starting a fenced session on ANOTHER machine's daemon (plan §3 M2c, §4.1),
as cc-tree's C12 client will call it. It exists so the client and the daemon are written against the same
reply shapes, and so each question the reviews raised has one written answer. Revision 2 folds in the eight
contract changes of the M2a security review (C1-C8) and the questions of the C12 client (N2-F). Direct mode: not
evidence.

## The call
`POST http://<machine>:7480/v1/sessions` (the tailnet listener of M2a: plain HTTP over the tailnet's WireGuard hop, there is no TLS terminator in front of it; a client that wants `https://` needs one added, and the daemon would then see the proxy's address, not the caller's, so WhoIs could not classify it), `Content-Type: application/json` (any
other type: `415 {"refused":"unsupported_media_type"}`), body = the LaunchSpec of plan §4.1 (at most 256 KiB),
decoded STRICTLY: an unknown field is refused.

**Only the owner starts remotely (C1).** The caller must be class **owner** (M2a: the owner's login on a listed
device). A peer daemon is class peer and gets `403 forbidden`: none of the planned callers needs a peer to
start a session, and a peer start would be a fenced-pod start if an operator proxy's tag ever leaked into the
peer set. (If peer start is ever added, everything in the spec must be a subset of the calling session's own
values: profile, grants, msg_scope, realm.) Anything else is 403 before the body is read. `started_by` is never
in the body: the daemon stamps it from the caller, `owner:<login>@<device>`, and keeps it in the session record
and the journal's `session.started`.

## (1) Replies: status code plus JSON
Success is `200` with the StartReply of §4.1 (the same bytes for a retry, see (5)). Every refusal, the class
refusal included (C8), is ONE shape: `{"refused":"<code>","message":"<one line, no request bytes>"}` (plus the
fields named below), the shape `ccsbx start --json` already prints. Clients read `refused`.

| code | status | meaning |
|---|---|---|
| `forbidden` | 403 | the caller is not the owner (a peer, another login, an unlisted device), or WhoIs failed |
| `profile_not_approved` | 403 | the profile is not approved for THIS machine |
| `grant_not_allowed` | 403 | a requested grant is not in the profile's allowed grants on THIS machine, see (2) |
| `resume_not_yours` | 403 | `resume.sid` is not a session this caller started, see (2) |
| `already_running` | 409 | the label is LIVE (or its start has finished launching) here or on a listed peer; body adds `machine` and, when known, `sid` and `state` so the client can reattach without a second call |
| `start_in_progress` | 409 + `Retry-After` (seconds, jittered) | the label is being started here or on a peer (a reservation exists), or this very `request_id` is still launching; retry, do not treat as failure |
| `rate_capped` | 429 + `Retry-After` | more than 3 remote starts a minute for one `started_by`, 10 for one calling machine, or the remote hourly budget (separate from the local one) |
| `machine_full` | 503 + `Retry-After` | the live-session cap |
| `peer_unreachable` | 503 + `Retry-After` | a listed peer could not be asked, see (3) |
| `peer_set_mismatch` | 503 | this machine's list of peers differs from a peer's, see (3) |
| `billing_unavailable`, `image_not_pinned`, `wrong_machine`, `bad_spec`, `request_id_reused`, `not_built` | 422 | the spec is not startable as sent; fix and resend |
| `unsupported_media_type` | 415 | not `application/json` |
| `internal` | 500 | the start failed on this machine for a reason that is not the caller's; the message says nothing of it (the daemon's log has it) |
| (body unreadable, over 256 KiB, not JSON, unknown field) | 400 `bad_spec` | |
`message` never contains the request's text (a brief can hold anything).

## (2) Fields a remote caller controls (C2)
`grants`: `"grants":["name",...]`, at most 16, each `^[a-z0-9][a-z0-9._-]{0,63}$`. A name is accepted only if it
is in the profile's allowed grant names **for this machine** (daemon configuration, never the request): else
`403 grant_not_allowed`. Checked at the daemon AND again at the doors, so the doors' policy is independent of
the ask. The requested grants and `started_by` are recorded in the session record and in `session.started`.
`harness_args`: only what the profile allows (a per-profile allow-list, `--model <name>` for the default
profile), else `bad_spec`. `msg_scope` names who may message the session and WHERE each of them lives:
`{"manager":{"node":"tree:id","machine":"<machine>"},"peers":["tree:id",...],"peer_machines":{"tree:id":"<machine>"},"reply_window_s":N}`.
`machine` is the hub's machine id of the tree record's `Host:` (what that machine's spoke is called on the hub);
it comes from the owner's start spec at launch, never from the fence. An UNFENCED manager or peer is heard, and
sent to, only on that machine: a node belongs to the machine its tree record names, so another machine's owner
token that tracks the same node is neither heard as the manager nor sent the worker's reports (round 5). A
fenced manager is verified by its signature and needs no machine. A scope that names an unfenced node without
its machine reaches nobody unfenced (refused, not guessed), so cc-tree must send it. `msg_scope`, `realm` and `resume` are owner-only (every remote caller is, C1);
`resume.sid` must be a session whose record's `started_by` is this same caller, else `403 resume_not_yours`.

## (3) One live generation per label, across machines (C3, C4)
No distributed lock exists, so no order is trusted. The rule is: **refuse when ANY listed peer has the label
live OR reserved, whatever the machines' names**. Worst case both sides refuse and retry, which is safe;
neither can start two generations.
1. Under the daemon's lock, briefly: refuse if the label is live or reserved locally; else write a RESERVATION
   (label, machine, `request_id`, started_by, time) to disk, and release the lock. Local starts and remote
   starts of the same label take the same reservation.
2. Outside the lock, ask every listed peer IN PARALLEL, one total deadline of 3 s:
   `GET /v1/resolve?label=<label>&v=2` (peer class) answering `{"live":[{machine,sid,gen,state}],"reserved":{machine,ts}|null}`.
   (`v=2` is new; without it `/v1/resolve` answers as today, one session or 404.)
3. Any peer live: drop the reservation, `already_running`. Any peer reserved: drop it, `start_in_progress` with a
   jittered `Retry-After` (1-5 s). A peer that does not answer in time: drop it, `peer_unreachable`.
4. Otherwise launch. The reservation is held until the launch outcome is KNOWN: renewed while the launch runs
   (it may exceed 60 s on k8s), kept on disk, and reconciled at daemon start (a reservation with no live record
   and no launch in flight is dropped; one with a launch in flight is resolved from the record).
The check runs for EVERY start, a local `ccsbx start` on the unix socket included (same reservation, same peers): otherwise a
remote start from the Mac and a local start on hp one second apart would both launch. A local retry of a request already
started is answered by the start's own idempotency, not refused because its label is now live.
Every machine must ask the same set of peers: the list is one shared setting (`--peers name=url,...`: the OTHER machines, `url` their
tailnet listener, plain http), and a daemon compares
the hash of its list with each peer's (`/v1/info` carries `peer_set`) at start and when asking; a difference is
`peer_set_mismatch`, so asymmetric lists cannot produce silent doubles.
**Accepted, in writing:** one listed peer being down refuses EVERY start on EVERY machine (with hp down, the Mac
cannot start anything). A partition refuses starts, it never doubles them. No `force` exists; if the owner ever
needs one it is an owner-only, journaled flag added then, not now.

## (4) `repo`, `base`, `branch`, `owner`, `workdir.path` (C5, C6)
The repo form (`workdir.repo/base/branch`) needs the git route (H1) and is refused `not_built` until then (a
Mac-to-hp start by repo is therefore refused until H1 lands; path workdirs work), but the values are validated NOW,
at parse time, so a later route cannot receive a bad one: `repo` `^github\.com/[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`;
`base` and `branch` `^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$` with no `..`, no `//`, no trailing `/` or `.lock`;
`owner` `^[a-z_][a-z0-9_-]{0,31}$`. When H1 uses them: `git check-ref-format --branch` on top of the charset;
the branch always fully qualified (`refs/heads/<branch>`); the base resolved with
`git rev-parse --verify --end-of-options <base>^{commit}`; `--end-of-options` (git ≥ 2.24) before every revision
argument (after a plain `--` an argument is a PATHSPEC, not a revision, and `-b`'s value cannot follow it at
all); one argv element each, never a shell. `owner` must equal this machine's `--owner-user` (the local account
the sessions run as; cc-tree sends the caller's `$USER`, so the two accounts must match or every start is
`bad_spec`).
A remote `workdir.path` is NOT checked by shape only: before any record is written it must pass the same
`--worktree-root` check a local start applies (strictly inside a configured root, symlinks resolved, no hidden
home directories) and be a git WORKTREE ROOT (its own `.git` at exactly that path: not a parent of repos, not a
subdirectory of one, not a plain directory); else `bad_spec` with a fixed message (no path or OS text). It must not equal,
contain or sit inside another live session's workdir. In HELPER mode hostd cannot look inside the owner's home, so a remote
start asks root (`ccsbx-root worktree-check`, §4b; built: n1-a-wc) and is refused `not_built` only when no checker is installed; direct mode needs `--remote-start-direct` and `--owner-user` = the user hostd runs as. Nothing can point the fence at `~/.ssh`,
`~` or another repo. A remote start carries no gitdir from the caller; in helper mode root derives it (§4b), in direct mode the daemon does.

### (4b) The helper-mode worktree check: the call hostd makes (R-H, cc-sandbox:#6w6t; A builds ccsbx-root's side to this)
hostd calls it (wired: `rootWorktreeCheck`); with no helper installed a remote start in helper mode is refused `not_built`. Note: root requires the path to hold NO symlink, so hostd passes the path exactly as the spec carries it and the answer must be that same path. hostd cannot look inside the owner's home, so it asks root, read-only:

    sudo -n ccsbx-root worktree-check -- <path>

- **Argument:** exactly one, after `--`: the `workdir.path` as the (already shape-checked) spec carries it. No option, no environment, no stdin; anything
  else is exit 2 before any filesystem access. `ccsbx-root.toml` lists the verb read-only (like `status`); it writes nothing and takes no lock.
- **What root checks, as root, on the path's REAL (symlink-resolved) form:** it is strictly inside one of `allowed_roots` (not the root itself, no `..`,
  no `/root2` prefix confusion, no hidden directory under a root that equals a home); it is a directory; it holds its OWN `.git` at exactly that
  path (a directory that is a git directory, or a `.git` file whose `gitdir:` target is host-owned and inside an allowed root), not a symlink; it does
  not equal, contain or sit inside another live session's workdir (root reads the run dir). The same rules `ccsbx-root run` applies at launch,
  so a path that passes here is one the run will accept.
- **stdout on success: EXACTLY two lines, `<path>\n<gitdir>\n`, both non-empty, clean, absolute:** the resolved path (what hostd records and
  mounts), then the gitdir root derived. hostd refuses anything else (a one-line answer or an empty second line, as an older ccsbx-root would give,
  would record a start with no gitdir that `ccsbx-root run` then refuses). The first line must equal the path asked about; the gitdir must pass
  `CheckGitDirLexical` (inside a configured root). hostd records the second line as the session's `GitDir` (a caller never supplies one); `ccsbx-root run`
  compares its own derivation with the record. stderr: one fixed line per refusal, no path or OS text. Nothing else on stdout.
- **Exit codes:** `0` the path is a git worktree root under `allowed_roots`; `2` refused: bad arguments, not under a root, not a directory, no own
  `.git`, a `.git` that is a symlink or not host-owned, overlaps a live session's workdir; `3` could not tell (a read error, the config unreadable): hostd
  answers `internal` (this machine's trouble, not the caller's spec), never "ok".
- **hostd's side:** called once per remote start, after the shape and policy checks and BEFORE any record is written or any peer is asked, with a 5 s
  bound (SIGTERM at the deadline, 1 s WaitDelay, fixed environment: a stalled helper cannot hold the daemon lock); any exit but 0, a timeout, or a stdout that is not `<path>` or `<path>\n<gitdir>` with the first line the path asked about is `bad_spec` ("workdir is not a git
  worktree root"), with a fixed message. The returned path replaces `workdir.path` in the record. Dead-pane records are ended before root is asked (root counts `Ended == nil` records as live). A missing helper is `not_built`, any other failure `bad_spec`. It runs only for remote starts (`started_by` set); local
  helper-mode starts keep the client-side `ResolveClientPaths`.
- **Tests A owes:** each refusal above (symlink out, sibling prefix, plain directory, repo subdirectory, parent of repos, `.git` symlink, foreign-owned `.git`,
  overlap) exits 2 with no path in stderr; a good worktree exits 0 with exactly one line; a root-owned config error exits 3; extra arguments, options or stdin are exit 2.

## (5) `request_id` idempotency (C7)
Keyed per caller (`started_by`) and `request_id`, so a peer or another login cannot poison the owner's ids.
The daemon stores a hash of the request and the first reply, for at least 24 hours (not shorter than a lid's
lifetime). **The hash is sha256 of the canonical encoding (sorted keys, no whitespace) of the decoded spec WITHOUT
`brief`**: cc-tree rewrites the brief text between a lost answer and its retry, and a retry never starts anything
or applies a new brief, so the brief is not part of "the same request". Everything else, `client.lid` included,
is. Cases:
- a retry with the same hash AFTER the first attempt finished: the FIRST reply, status 200, the same StartReply
  (or the first refusal's reply is NOT replayed: a refused attempt created nothing and a retry is a fresh attempt);
- a retry while the first attempt is still reserving or launching: `409 start_in_progress` + `Retry-After`,
  never `already_running` (which would tell the client its start failed);
- the same `request_id` with a different hash: `422 request_id_reused`.

## (6) Rate caps (C8)
Remote starts have their own budgets: 3 a minute per `started_by`, 10 a minute per calling machine (the device
WhoIs names), and an hourly cap, all SEPARATE from the local caller's, so remote callers cannot use up the owner's
local starts. `429 rate_capped` + `Retry-After`; cc-tree maps it to exit 75.

## Not here
Stop, prompt, replace and deliver on the tailnet (M2d); the repo/branch workdir (H1); Tailscale SSH (never).
