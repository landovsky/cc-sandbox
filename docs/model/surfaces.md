---
type: analysis
status: draft
title: "Area D: Surfaces: how Tomas (and later client staff) sees and drives sessions"
task: ai-tooling:#5g2y (milestone ai-tooling:#6rrd)
date: 2026-11-18
inputs: docs/unified-session-model.md, docs/cc-flow-on-kubernetes.md, docs/coding-harness-control-surfaces.md,
  doc 68 (c84519e) §6.3–6.4, §5.2, K17–K18, MR-26/27; doc 69 decision surface (2b98c77); doc 24 (deep link);
  docs/architecture/remote-control-security-stash.md; doc 11 (herdr); doc 35 (terminal/web parity);
  ~/.dotfiles/bin/{cc-tree-web,cc-pick,cc-goto,notify-tomas-telegram}; neighbour model/orchestration.md
evidence: model/evidence/surfaces-web-terminals.md, model/evidence/surfaces-mux-dashboards-push.md
---

# D. Surfaces

**In one line.** Keep tmux as the lifecycle host and the CLI surface on `local` and `sandbox`.
Make the **web Tree the one authenticated decision surface** (❓ answers, grant approvals with a
passkey for anything credentialed). Give it a **read-only screen and a send box now, built on the
host's tmux**, not after G1. Telegram and push only *point* at that surface. Every surface
addresses a session by `<tree>:<node>` and reaches it only through CC Flow verbs or the hub's
`deliver`; none of them runs inside a fence or holds a credential the fence could use.

## 1. What Tomas has today

Paths are in `~/.dotfiles` unless noted. **live** = used daily · **draft** = written, not built · **idea**.

| Surface | What it is | Where | State |
|---|---|---|---|
| Ghostty + tmux (Mac, hp) | Cmd keys → tmux prefix bytes; one window per task, `@node`/`@claude_*` options, colours by hook | `ghostty.config`, `tmux.conf`, `claude/hooks/tmux-claude-*.sh`, tmux 3.4 on hp | live |
| Tree view | Cmd+E popup, fzf or Textual, one key table `PICK_KEYS`; `^q` answers a ❓ | `bin/lib/cc_tree.py:5506`, `cc_tree_tui*.py` | live |
| cc-pick | Cmd+. cross-machine session list via the bus; Enter focuses, `^R` replies | `bin/cc-pick` | live |
| cc-goto | Cmd+J menu, one key per pinned node, opens or starts it | `bin/cc-goto`, `cc-flow/goto.conf` | live |
| web Tree (cc-tree-web) | `:4320` on hp via Tailscale Serve; installable PWA (manifest, **no service worker, so no push**); writes only `done/answer/add/start` events; write auth = `Tailscale-User-Login` header == owner | `bin/cc-tree-web:655`, `web/tree-page.html` | live |
| Decisions page | `cc-tree decisions <milestone>` generates one page of open ❓s with a Rec each; chat shows only a pointer | doc 69, `cc_tree_decisions.py` | draft → partly live (orchestration.md §1) |
| MD Canvas | `:4300` comments on live `.md`; Submit wakes the session with `ccmsg`; **no app auth** (map gap 3) | `mdcanvas/`, `claude/mods/md-canvas-links` | live |
| Telegram out | `notify-tomas-telegram <bot>` → devops-telegram `/notify` (k3s, public ingress) | `bin/notify-tomas-telegram` | live |
| Telegram in | replies stored by devops-telegram; hp `msg-router` polls `/replies` each minute → `cc-tree deliver` | map §7.3 | live (replies only; no buttons) |
| Remote Control | `claude-rc-server` unit **deleted** (systemd shows `not-found`, stale failed state since 2026-11-10); `cc-rc` still in `claude!`'s last line | `bin/cc-rc`, RC stash | being removed (doc 68: `disableRemoteControl`) |
| Phone deep dive | none today besides RC; doc 68 interim = SSH client into hp's tmux | doc 68 §6.3 | idea |
| ccattach | `ccattach <tree:node>` → `kubectl exec … tmux attach` via the Tailscale API-server proxy | doc 68 §6.3 | draft (pods only) |
| herdr | agent multiplexer, 0.9.3 installed on hp, keymap mirrors tmux | `herdr/config.toml`, doc 11 | trial, parked |

**FOUND (inferred from code, not exercised):** cc-tree-web trusts `Tailscale-User-Login` and the
Origin header and binds 127.0.0.1. `tailscale serve` strips a forged header from tailnet clients,
but **any process on hp that reaches loopback can send both headers itself**. That includes every
`local`-tier agent, so an unfenced agent could post `answer` or `start` events as Tomas. Sandboxed
sessions can't (`--internal` network, no host). Consequence for the model: a header is enough to
identify a *viewer*, never to authorise a *decision*. See R3 and build item D4.

## 2. What surfaces must do in the unified model

Invariants, applied:

- **No secret inside the fence.** Surfaces live on the host or in tree-core, outside every fence.
  No surface endpoint is reachable from a fence (sandbox: `--internal`; pod: network policy). The
  screen view is text from the *host* side (`capture-pane`), redacted with `ccsp scan` before it
  leaves the host. The send box writes through `deliver`, never through a shell.
- **Sessions don't depend on the backend.** With hp's web, the hub and Telegram all down, a Mac or
  hp still starts, watches and drives sessions from Ghostty + tmux + tree view. Surfaces are
  readers and requesters; none of them is on a session's start path.
- **Consent by a human.** A decision (❓ answer, grant approval) carries a human principal *and a
  proof an agent can't make*. Tier the proof the way doc 68 §5.2 tiers grants: viewer identity
  for reading; owner identity + nonce + confirm tap for low; one-time code bound to the request
  for medium on Telegram; **WebAuthn assertion** for medium on web and for every high. No service
  account, manager or bus message can approve.
- **tmux owns the lifecycle on local/sandbox.** Surfaces never kill, respawn or resume a session.
  Their verbs are CC Flow's (`start`, `fresh start`, `deliver`, `answer`), and CC Flow decides
  per ADR-0002/3/4. "Stop" from a surface = `deliver("/exit")` on local/sandbox, drain on pod.

Requirements:

| # | Requirement | Why |
|---|---|---|
| S1 | One **roster** (address, machine, host tier, harness, state, attached?, open ❓/grants) feeds every surface | cc-pick, tree view, web and Telegram show different truths today |
| S2 | **Attach** works the same verb on every tier: `ccattach <addr>` | the pane is local on local/sandbox, ssh on host-uid, exec on pod |
| S3 | **Read-only screen** on phone/web: text, redacted, `no-store`, only while viewed | doc 68 RQ-13 option B; replaces the phone's SSH shell |
| S4 | **Send box**: one line via `deliver`, nonce + confirm, provenance `owner·web` | send must stay turn-aware and logged (area C) |
| S5 | **While a human is attached, automated delivery waits** | doc 68 step 8; today send-keys can interleave with typing |
| S6 | **Decision cards** for ❓ and grants on one page per milestone (doc 69), answerable from the phone | ADR-0007: decisions are data |
| S7 | **Notifications point, never decide**: deep link to the card; a one-tap answer only for low tier | MR-26: a phone/Telegram compromise must not approve high-tier reach |
| S8 | Harness-neutral: nothing assumes Claude Code's TUI or Remote Control | doc 24 lock-in note; Codex/OpenCode later |
| S9 | 500-person path: OIDC (Keycloak realm), per-user ownership on attach and view, recording outside the pod | doc 68 K17, MR-28 |

## 3. Outside solutions

Full survey, sources and scores: `evidence/surfaces-web-terminals.md` and
`evidence/surfaces-mux-dashboards-push.md`. Tags: [documented] vendor docs or release notes,
[reported] issues, blogs, [inferred] mine.

### 3.1 CLI surface and lifecycle host: is tmux still right?

| Candidate | Maturity | License | Vendor risk | Scriptable state (`@options`-like) | Fit solo | Fit 500 |
|---|---|---|---|---|---|---|
| **tmux** (3.8, 2026-09-09; hp runs 3.4) [documented] | 5 | ISC | low | yes: user options, hooks, `capture-pane`, `send-keys`, `attach -r`, control mode | 5 | 4 (per host; people attach through a gateway) |
| zellij 0.46.0 (2026-11-11) [documented] | 4 | MIT | low–med | partial: `list-panes`, `send-keys`, `dump-screen`, `subscribe`, web client with read-only tokens; **no per-pane user options** | 3 | 3 |
| herdr 0.9.5 (2026-10-14; 0.9.3 on hp) [documented] | 2 (pre-1.0, VC-funded, cloud coming) | Apache-2.0 | med–high | yes, agent states + socket API; tokens not restored after restart (doc 11) | 3 | 2 |
| abduco / dtach / shpool | 2–3 | various | low | detach only, no state | 1 | 1 |
| WezTerm mux | 2 (no upstream release since 2024) [reported] | MIT | med | Lua; Mac-only UX | 2 | 1 |
| Agent View (`claude agents`, `--bg`) | 3 (research preview) | proprietary | high (Claude-only) | `--json` with `status` + `waitingFor` [documented]; own supervisor and PTYs, **not tmux**, local-only, no phone surface | 1 as host | 1 |
| Agent of Empires 1.25 · agtx · herdr-web | 2–3 | MIT | med (small teams) | tmux underneath; state from hooks or scraping | borrow ideas | — |

Ghostty 1.4.0 (2026-10-27) still has no tmux control-mode (`-CC`) GUI; only iTerm2 does
[documented]. Ghostty 1.4 parses a draft program-status OSC 7501 (idle/working/done/blocked/
failed) but its GUI ignores it so far [documented].

**Verdict: keep tmux, as both lifecycle host and CLI surface, on local, sandbox and host-uid.**
cc-tree's ~150 `tmux()` calls, the hooks and every agent manager surveyed sit on it, it has a
read-only attach (`attach -r`) and a capture primitive the screen view needs, and the
sandbox measurement (0.45 s) showed the container as a plain pane process needs nothing else.
zellij is the only real alternative and it lacks user options, so the state model would be
rewritten for no new capability. herdr stays parked with doc 11's triggers. On pods the inner
tmux is hidden behind `ccattach` (unified doc C2). tmux control mode is **not** needed: polling
`capture-pane` every 2 s while a page is open is cheaper and harness-neutral [inferred].
**RECOMMENDED 3RD PARTY, CLI surface + lifecycle host: tmux** (keep). Borrow: Agent View's
`waitingFor` vocabulary for the roster's `state` (D1); AoE's PWA auth pattern (passkey, device
revoke, Host/Origin check) for D4; emit OSC 7501 from the hook layer when Ghostty's GUI uses it (XS, later).

### 3.2 Web attach (read-only screen + send box)

No product has read-only view + a send box + OIDC + recording [inferred from the survey]. Shortlist:

| Candidate | Maturity | Self-host | License | Vendor risk | Read-only | Fit solo | Fit 500 |
|---|---|---|---|---|---|---|---|
| ttyd on `tmux attach -r` | 4 (12.7k★; last tag 1.7.7 Mar 2024, commits to Aug 2026) [documented] | 5 | MIT | low | yes, default [documented] | 4 | 3 behind oauth2-proxy |
| gotty (sorenisanerd fork) | 3 (v1.11.0, 2026-11-05) [documented] | 5 | MIT | med (one maintainer) | yes, default | 3 | 3 |
| sshx | 3 | **no**: self-host unsupported [documented] | MIT | high | read-only link | 1 | 1 |
| upterm | 3 (v0.41.1, 2026-11) | 5 | Apache-2.0 | med | `--read-only` | 2 (guests need ssh, not a browser) | 2 |
| Coder OSS | 4 | 5 (Postgres, wildcard DNS) | AGPL + Premium | med (open-core drift; Tasks removed ≥ v2.37, `coder/agentapi` archived 2026-09-13 [documented]) | **no** | 1 | 3 as a whole workspace platform |
| Teleport | 5 | 5 | CE commercial since v16, free < 100 staff, no OIDC/SAML in CE, moderated sessions Enterprise-only [documented] | med | observer mode (Ent.) | 2 | 4, **paid Enterprise** |
| Tailscale SSH + recording | 4 | n/a (SaaS control plane) | proprietary | med | — | 4 identity | 3 (`tsrecorder` deprecated, EOS Aug 2027; PAM beta, closed [documented]) |

**What the screen view actually needs is smaller than any of these.** It's a text snapshot
(`tmux capture-pane -p -J -t <pane>`), not a terminal emulator in the browser. Doc 68 already
rejected a browser terminal (RQ-13, MR-27), and a phone terminal is poor for reading anyway
(doc 24). ttyd/gotty would add a raw pty to the browser, with key input one flag away.

**RECOMMENDED 3RD PARTY, web attach: build, on Tailscale identity.** Nothing fits the
"text snapshot + `deliver` box" shape; the build is about 150 lines inside cc-tree-web (D2, D3).
Tailscale Serve identity for viewing (solo), oauth2-proxy + Keycloak in front at 500 [documented
pattern]. **ttyd `-r` on `tmux attach -r` is the reserve** for the one case text can't cover
(a full-screen TUI on a laptop browser), owner-only, off by default. At 500 people with a
"developers want IDEs" ask, **Coder OSS** for the human workspace side (doc 68 Q43 stands).

### 3.3 Terminal attach to fenced sessions (laptop)

| Tier | ccattach does | Identity | Recording |
|---|---|---|---|
| local, sandbox | `tmux select-window` / `switch-client` (the pane is a host pane) | the OS user | none needed (it's Tomas's own tmux) |
| host-uid | `ssh agent-N@hp tmux -L agent-N attach` with Tailscale SSH (check mode for the pool) | tailnet identity | Tailscale recording while it lasts, then `asciinema`-style at the ssh forced command |
| pod | `kubectl exec … tmux attach` via the Tailscale operator API-server proxy (doc 68) | tailnet identity → impersonation | operator's exec recording [documented] → attach gateway with SSH certs at 500 |

**RECOMMENDED 3RD PARTY, terminal attach: Tailscale** (SSH and the k8s operator proxy) solo →
**Teleport Enterprise or a thin attach gateway with an OpenBao SSH CA** at 500 (doc 68 K17 stands;
Teleport CE can't, because its SSO is GitHub only [documented]). Record at the gateway, never in
the pod: `pipe-pane` inside a fence can be tampered with by the agent [inferred].

### 3.4 Notifications and one-tap answers (phone)

| Channel | Maturity | Self-host | Vendor risk | One-tap action | iOS | Fit solo | Fit 500 |
|---|---|---|---|---|---|---|---|
| **Telegram bot, inline keyboard** (`callback_query`, `callback_data` ≤ 64 B, `answerCallbackQuery` ≤ ~30 s) [documented] | 5 | bot server yes (devops-telegram exists) | med (Telegram) | yes | yes | 5 | 2 (clients won't use Telegram) |
| **Web Push from the web Tree PWA** (VAPID; works on a tailnet-only Serve origin, since the push service is public and Serve has a real cert [reported]) | 4 | yes, standard | low | opens the card; approve is a 2nd tap (iOS has no custom actions in Safari web push [inferred, unverified]) | Home-Screen install needed (16.4+) [documented] | 4 | 4 |
| ntfy (self-hosted + `upstream-base-url: https://ntfy.sh` for iOS) | 4 | yes | low–med | HTTP action buttons [reported, unverified] | delayed for hours without the upstream [reported] | 3 | 4 |
| Pushover | 4 | no | low | no callbacks | yes | 2 | 2 |
| Gotify | 3 | yes | low | no | no real iOS app | 1 | 1 |

**RECOMMENDED 3RD PARTY, notifications: Telegram inline keyboard solo** (it already exists, end to
end, as notify + msg-router), **limited to low-tier answers** with an opaque, one-use,
expiring token bound to the request id and Tomas's user id; medium/high get only an "Open" deep
link. **Web Push from the web Tree** (standard, no vendor) as the second leg and the **500-tier
primary**, because it lands on the authenticated surface that holds the passkey. ntfy only if a
client bans both. Funnel is never needed.

## 4. Contracts with the other areas

Surfaces call; nothing calls surfaces except notifications. All data below is JSON.

**→ E. Orchestration (CC Flow)**: consistent with `orchestration.md` §4. Surfaces call CC Flow
verbs only: `start`, `answer(❓, text, proof)`, `arm/disarm`, `fresh_start`, `place`, `deliver`.
They read the roster:
```
Roster row {address, gen, machine, host, harness, state: working|asks-you|your-turn|bg|idle|unknown,
            attached: [human], open: {questions:n, grants:n}, health_line, deep_links{web,canvas,attach}}
```
E's `session_health` line is the row's `health_line`. **Ask of E:** `answer` and grant decisions
carry a `proof` field (below), not just `by=human`.

**→ C. Cooperation (hub)**: the send box and Telegram replies go through `hub.deliver(address,
envelope)` with `kind: message`, `from: human:<login>`, `channel: web|telegram|tree-view`,
`nonce`. Surfaces show the receipt (`queued → injected → consumed`). **Surfaces call C** for
`attach.begin(address, human) / attach.end` (a lease with a 60 s heartbeat). While a lease is held
the hub keeps automated envelopes queued (S5). Source of the signal on local/sandbox: tmux
`client-attached`/`client-detached` and `#{window_active_clients}` on the bound window [inferred:
check per tmux 3.4]. Screen data does **not** go through the hub: the host's surface agent reads
`capture-pane` directly. On pod, the sidecar serves it.

**→ B. Permissions**: B creates the request; E turns it into a ❓ card; the surface shows it and
returns a decision:
```
Card {request_id, address, gen, scope (host:port | route | mcp server), tier: low|medium|high,
      subjects: [session|task|milestone] (narrowest preselected), why (labelled agent text),
      provenance: owner|agent|untrusted, ttl, scopeHash, expires_at}
Decision {request_id, scopeHash, decision: approve|deny, narrowed_to?, subject, ttl, approver: human:<login>,
          channel: web|tree-view|telegram, nonce, sig, proof: {kind: header|otp|webauthn, assertion}}
```
Field names follow `permissions.md` §4 `grant.decide`; `proof` is this doc's addition. B verifies `proof` against the tier (a `header` proof is refused for medium and high). Surfaces
render `provenance: untrusted` in red and never offer one-tap approve for it.

**→ A. MCP gateway**: two cases reach a human. (1) **OAuth connect** for an MCP server (the
gateway holds the token): A asks for a grant (B), and the approved card carries the gateway's
authorise URL; the human completes OAuth in a browser *outside the fence*, and the token lands at
A. (2) **MCP elicitation** (a server asks the user a question mid-call) becomes a ❓ card through
E, answer returned to A [inferred: depends on A's design]. Surfaces never see an MCP token.

## 5. Build list delta (on top of unified-session-model §3 and orchestration.md §5)

| # | Piece | Size | Order / needs | Works when |
|---|---|---|---|---|
| D1 | **Roster**: `cc-tree sessions --json` = E1's record + tmux state + bus presence; cc-pick, tree view and web read it | S | with E1 | the web Tree, cc-pick and the tree view show the same state and `host` for a sandboxed node |
| D2 | **Screen view** in cc-tree-web: `GET /api/screen/<addr>` → last 80 lines of `capture-pane -p -J`, through `ccsp scan` redaction, `no-store`, polled every 2 s only while the page is visible; every view logged; log-only day first | S | D1; item 1–2 (sandbox panes are host panes) | the phone shows a sandboxed worker's screen within 3 s; a planted fake secret on screen arrives redacted; the view log has one line per open |
| D3 | **Send box**: `POST /api/send` → `cc-tree deliver` with nonce + confirm tap, provenance `owner·web` | XS | D2; area C receipts | a typed line is answered by the session; a replayed nonce is refused; the hub log shows `from: human:tomas`, `channel: web` |
| D4 | **Decision proof**: cc-tree-web registers Tomas's passkey (WebAuthn) and signs medium/high decisions; header-only writes limited to non-decision events (`start`, `done`, `add`). A same-uid `local` agent can still forge those, an accepted residual because it already holds Tomas's shell; nothing a header proves can approve | M | B's tiers; ❓ D-3 (stable origin) | a `curl` from a local agent with forged headers gets 403 on `answer`; approving a high-tier card without the passkey is refused by B |
| D5 | **Attach lease**: tmux hooks on the bound window → `attach.begin/end` to the hub | XS | C's hold-while-attached | while Tomas is in the tab, a manager's `ccmsg` waits; it lands within 5 s of switching away |
| D6 | **Push**: see §3.4 rec; ❓/grant card → notification with a deep link to the card | S | D4 for anything above low | a new ❓ buzzes the phone within 10 s; tapping opens the card, not the chat |
| D7 | **Telegram buttons for low tier**: inline keyboard on a ❓/grant notice; callback bound to request id, Tomas's user id, one use; medium gets an OTP to type on the web card | S | devops-telegram callback route (k3s repo, Tomas's own) | a low-tier egress grant approved from Telegram applies; the same button pressed twice does nothing; a medium card offers no button |
| D8 | **ccattach** for every tier: local/sandbox = select-window; host-uid = Tailscale SSH; pod = exec via operator proxy (doc 68) | XS → M | XS now; M with pods | `ccattach ai-tooling:x` lands in the session on each tier it supports, and says which tier it used |
| D9 | **Cleanup**: drop `cc-rc` from `claude!`'s last line for every tier; `systemctl --user reset-failed claude-rc-server`; MD Canvas Submit marked `untrusted` until it gets D4's proof | XS | — | no RC registration on new sessions; canvas-woken prompts carry `trust: untrusted` |
| D10 | **500 tier**: oauth2-proxy + Keycloak in front of web Tree, per-user ownership on roster/screen/attach, recording at the attach gateway | L | first client | a second user sees only their own sessions; an attach shows in the audit log outside the pod |

**Minimum for "drive a sandboxed agent from the phone"**: D1 + D2 + D3 (S + S + XS). With D4
it can also approve grants safely; with D6 it tells you when to look.

## 6. Risks and conflicts

**Top 3 risks**

1. **The web Tree becomes the high-value target** (MR-26, MR-27): it shows screens, sends input
   and approves reach. An XSS or a forged header is a write as Tomas. Mitigation: strict CSP, no
   CDN scripts on the decision page, screen text rendered as text only, D4's proof, a separate write origin once it moves off hp.
2. **Approval fatigue moves to the phone.** One-tap buttons make "yes" cheaper. Mitigation: buttons
   only for low tier, ceilings and the auto tier first (B), a daily count of human approvals
   against doc 68's ≤ 5 target, `can-wait` items batched into the 08:00 digest.
3. **The screen view leaks.** A secret printed in a pane reaches the phone (and its caches).
   Mitigation: `ccsp scan` redaction before it leaves the host, `no-store`, only while visible,
   view log, and the red-team fakes (doc 68 §10) planted on screen to test it.

**CONFLICTS**

- **Doc 68 Q4 (phone): "screen view only after G1; before G1, SSH into hp's tmux."** I disagree
  on timing. On local and sandbox the pane is a host pane, so D2 + D3 are about 150 lines on top
  of what cc-tree-web already has, and they are **narrower** than an SSH shell as Tomas on the
  agents' host (doc 68 §12.6 point 3 calls that route "not free"). Rec: build D2/D3 now, skip
  the SSH route. Doc 68's four conditions (text only, redacted, `no-store`, writes only via
  `deliver` with nonce + confirm, log-only day first) all still apply. ❓ D-1.
- **Doc 68 "separate write origin" for the send box.** Fine for pods, but on hp today it means a
  second Serve port with its own passkey RP ID. Defer until the web Tree moves off hp (Phase 3);
  until then a nonce + confirm + D4 on one origin.
- **orchestration.md §4 → B: `grant.decide(request_id, approve|deny, by=human)`.** It needs
  `approver, channel, nonce, proof` (doc 68's AccessRequest contract already names approver,
  channel, nonce, signature). Without `proof` B can't tell a passkey approval from a forged
  header. Not editing E's doc; flagged for the integrator.
- **permissions.md §4 (D row): WebAuthn step-up "for `high`"**. Doc 68 §5.2 puts medium on
  "web, Mac; Telegram only with a one-time code", and B's own T6 notes the answer path is reachable
  by every local agent. A header-signed decision for medium is forgeable from loopback on hp, so I
  ask for a passkey (web) or an OTP (Telegram) on **medium** too; low stays header + nonce + tap,
  and only for fenced requesters. Flagged, B's doc not edited.
- **Doc 24 (RC primary, web terminal fallback).** Superseded by doc 68 and this doc: RC gone, no
  browser terminal by default; the text screen view is the vendor-neutral way in.
- **Unified model rule 2 ("the tmux pane is the handle on every host").** Agreed. This doc adds
  that the *screen* is read from that pane, so the screen view inherits the rule for free on
  local and sandbox.
