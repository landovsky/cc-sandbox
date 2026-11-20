# M2c: remote start, `POST /v1/sessions` on the target daemon (the contract, pinned before it is built)

What this is: the HTTP contract for starting a fenced session on ANOTHER machine's daemon (plan §3 M2c, §4.1),
as cc-tree's C12 client will call it. It exists so the client and the daemon are written against the same
reply shapes, and so each question the C12 review raised has one written answer. Direct mode: not evidence.

## The call
`POST https://<machine>:7480/v1/sessions` (the tailnet listener of M2a), `Content-Type: application/json`, body
= the LaunchSpec of plan §4.1 (at most 256 KiB), decoded STRICTLY: an unknown field is refused. The caller is
the tailnet's word (owner or peer class, M2a); anything else is 403 before the body is read. `started_by` is
never in the body: the daemon stamps it from the caller, `owner:<login>@<device>` or `peer:<device>`, and it
is kept in the session record and the journal's `session.started`.

## (1) Replies: status code plus JSON
Success is `200` with the StartReply of §4.1 (the same bytes for a retry, see (5)). Every refusal is a JSON
object `{"refused":"<code>","message":"<one line, no request bytes>"}`, the shape `ccsbx start --json` already
prints, with this status:

| code | status | meaning |
|---|---|---|
| (class) | 403 `{"error":"forbidden"}` | the caller is neither owner nor listed peer, or WhoIs failed |
| `profile_not_approved` | 403 | the profile is not approved for THIS machine |
| `already_running` | 409 | the label is live (or being started) here or on a listed peer; `message` names the machine |
| `rate_capped` | 429 + `Retry-After` | more than 3 starts a minute for one `started_by`, 10 for one calling machine, or the hourly cap |
| `machine_full` | 503 + `Retry-After` | the live-session cap |
| `billing_unavailable`, `image_not_pinned`, `wrong_machine`, `bad_spec`, `request_id_reused`, `not_built` | 422 | the spec is not startable as sent; fix and resend |
| `peer_unreachable` | 503 + `Retry-After` | a listed peer could not be asked, see (3): refused rather than guessed |
| (body unreadable, over 256 KiB, not JSON, unknown field) | 400 `{"refused":"bad_spec",...}` | |
`message` never contains the request's text (a brief can hold anything).

## (2) `grants`
`LaunchSpec.grants` IS part of the spec (`"grants":["name",...]`, the grant names the node's `Grants:` line
asks for; decided at the doors, never by the daemon). The strict decoder accepts it; each name must match
`^[a-z0-9][a-z0-9._-]{0,63}$`, at most 16 (else `bad_spec`). C12 may send it.

## (3) One live generation per label, across machines
Atomic on one machine (the start runs under the daemon's lock). Across machines a distributed lock does not
exist, so: under its lock the daemon (a) writes a RESERVATION for the label (label, machine, time, 60 s),
(b) asks every listed peer `GET /v1/resolve?label=` (peer class), which answers `{live:[...], reserved:{machine,ts}|null}`,
(c) refuses `already_running` if any peer has it live, or has it reserved by a machine whose name sorts BEFORE
this one (the deterministic tie-break when two machines start the same label at once; the loser's reservation
is dropped), (d) refuses `peer_unreachable` (503) when a listed peer does not answer within 2 s, because a
guess either way breaks the rule, and only then launches and turns the reservation into the live record.
Honest limit: during a network partition both sides may be unable to ask each other; they refuse rather than
start, so a partition refuses starts, it does not double them.

## (4) `repo`, `base`, `branch`, `owner`
The repo form (`workdir.repo/base/branch`) needs the git route (H1) and is refused `not_built` until then, but
the values are validated NOW, at parse time, with a strict charset, so a later route cannot receive a bad one:
`repo` `^github\.com/[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`; `base` and `branch`
`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$` with no `..`, no `//`, no trailing `/` or `.lock`, no `@{`; `owner`
`^[a-z_][a-z0-9_-]{0,31}$`. Whatever is validated reaches `git` only AFTER a literal `--` argument, as one argv
element each, never through a shell. `owner` must also be this machine's configured owner user
(`--owner-user`, the local account the sessions run as); otherwise `bad_spec`.
A path workdir (`workdir.path`) is checked by SHAPE only (the daemon cannot enter the owner's home) and
ccsbx-root validates it for real at run; the daemon cannot compute a gitdir for a remote caller, so a remote
start has no gitdir (git verbs are unavailable to it until the git route resolves one).

## (5) `request_id` idempotency
Per caller (`started_by` identity, not the body) and `request_id`: a retry with the SAME spec returns the FIRST
reply, status 200, the same StartReply, and starts nothing; the same `request_id` with a DIFFERENT spec is
`422 request_id_reused`. (The daemon keeps a hash of the submitted spec in the record.) A retry after a
refusal is a fresh attempt: nothing was created.

## Not here
Stop, prompt, replace and deliver on the tailnet (M2d); the repo/branch workdir (H1); Tailscale SSH (never).
