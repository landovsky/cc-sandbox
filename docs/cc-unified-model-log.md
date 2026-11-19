---
type: log
status: working
version: 2.1
title: "Agent fence: working log (changes, conflicts, decisions, build list)"
task: ai-tooling:#6gqx
date: 2026-11-19
supersedes: unified-model.md v1 §4–§6
---

[Index](README.md) › Unified model › 4. Working log

**Docs:** [1 · contract](unified-model.md) · [2 · tiers](unified-model-tiers.md) · [3 · CC Flow as a client](unified-model-cc-flow.md) · **4 · working log**

# Working log

## v2 → v2.1 (call, 2026-11-19)

- **Build scope.** Tom's own machines only: the Mac (through Docker), the hp and spare Hetzner
  capacity. Steering from one place is in the first build, and the MCP gateway is not.
- **Central doors.** The model and egress doors run once, on Hetzner, over Tailscale. They're a
  single point of failure, and that's accepted.
  - Consequence: the **session token moves to solo**, because a central door can't tell sessions
    apart by address.
  - Identity gains `machine`, and each machine signs its own tokens.
- **Billing per session.** The subscription for Tom's development work, an API key for ops work.
  Friends run their own instances with their own keys and never touch Tom's setup, so easy
  installation is now a goal.
- **New controls:**
  - poisoned worktree: hooks off on the host, agent-written steering files untrusted
    (invariants 24 and 25);
  - pinned harness and image (invariant 26);
  - central doors only on Tailscale, accepting known machine keys only (invariant 27).
- **New known limitations:** runaway cost, a stolen phone or surface, the central door as a
  single point of failure, the docker socket on the Mac.
- **Descoped:** door failover. The Mac comes back through Docker; native Mac virtualization stays deferred.

## v1 → v2

- **Split into four docs.** Contract, tiers, CC Flow as a client, and this log.
- **The threats come first.** The contract now opens with the four threats (provider leak,
  compromised code, over-eager agent, prompt injection), and each lists its controls, tier and
  residual risk.
- **CC Flow taken out of the core.** Identity is now `owner / session-id / generation` plus a
  label, and CC Flow is a client.
- **Tiers are solo, friends and company,** each listing its components. The old `local` tier is
  now "unfenced, outside the contract".
- **New invariants:**
  - host keys live under a dedicated uid;
  - no agent runs in the docker group;
  - one outbox per generation;
  - irreversible means ask or forbid;
  - the model door is the exception to backend independence;
  - an agent never has more document access than its owner;
  - decisions are idempotent, and surfaces are untrusted.
- **Added:** the grant plan (scout phase plus one approval), a surface-agnostic approval channel,
  the agent-to-agent rules, the minimal no-hub messaging path, and company document access.
- **Descoped:** the hub, signed envelopes and relay; knowledge notes and Graphiti; other
  harnesses; OAuth custody; gVisor; Mac hosts; taint tracking.
- **Weakened:** the source IP in session tokens is logged, not trusted.

## New in v2

These are open, with no ❓ node yet.

- **N1 · Rename `ccf-hostd`** to a neutral name (for example `ccsbx-hostd`), and the same for
  any `ccf-` platform piece. Only CC Flow's own MCP server keeps `ccf`.
- **N2 · MCP at the solo tier.** Remote MCP servers go through the egress door as HTTPS routes
  with an injected token, without per-tool filtering. *My resolution, because criteria put the
  tool door in friends. Confirm.*
- **N3 · CC Flow verbs at the solo tier** run as a stdio server inside the fence that writes
  only to the outbox. *Same reason as N2. Confirm.*
- **N4 · The policy at friends** is "a simple grant list" on embedded Cedar (#55ca), and
  "full policy" at company means the analyser-proven profiles plus a shared store. *Confirm
  that this matches what the criteria meant by "simple".*
- **N5 · Grants at solo** are static config with no ask tier (unlisted = forbidden). *Confirm.*
- **N6 · The session token at solo.** Each machine's launcher signs its tokens, and the central
  doors trust a list of machine keys. *Follows from the central doors. Confirm.*
- **N7 · Remote sessions in CC Flow.** The pane on the steering machine attaches to a session on
  another machine over Tailscale SSH. *Mechanism to be settled in the first slice.*
- **N8 · Unfenced agents on the Mac** can reach the Docker Desktop socket. Accept it as a
  limitation, or run no unfenced agents on the Mac.

## Decisions (❓ nodes), updated

The nodes and recommendations are unchanged unless the v2 note says otherwise.

**Still on the critical path**
- **#6f24** Model credential. **Decided:** billing is chosen per session, with the subscription
  for dev work and an API key with a hard cap for ops work. **Still open:** whether the central
  model door may hold the subscription login. Until then, subscription sessions run unfenced or
  on a stub, so this doesn't block the build.
- **#6d6g** Leave the docker group on the Linux machines (hp, Hetzner). **It's now required by
  invariant 14**, not optional, and makes `ccsbx` a fixed-flag root wrapper (U0). On the Mac, see N8.
- **Central vs per-machine doors.** **Decided: central**, on Hetzner.
- **#46tt** Rotate the pharmacy token now. It's already in git history.

**Unchanged, accept the recommendation**
- **#5gbg** Lifecycle invariant: yes. Now invariant 15, worded without tmux.
- **#3g4a** A container per session: yes, on every machine. The Mac uses Docker, not host users.
- **#2sw2** Workers fenced first, managers unfenced for now.
- **#2s5y** `Host:`/`Harness:` on the milestone, applied at the next generation. Now in doc 3.
- **#52bq** Keep tmux.
- **#26fv** agentgateway OSS.
- **#55ca** Cedar via cedar-go.
- **#6dvg** Milestone grant TTL of 30 days.
- **#6a6q** `local` recorded but unenforced. Now "unfenced, outside the contract".
- **#4srd** Turn AUTONOMY prose into grants after U9.
- **#52ds** A passkey for medium and high. The approval page now belongs to the platform, so fix its origin name rather than the web Tree's.
- **#3dxg** Telegram for low risk, plus Web Push.
- **#5sxw** Phone screen view now. This moved to CC Flow (doc 3).

**Moot or deferred by descoping**
- **#2a2e** OAuth custody and **#3scs** local `mcpOAuth` tokens: deferred to the company tier.
- **#6aw3** gVisor: descoped. It's listed as the shared-kernel limitation.
- **#5gf6** Sandbox last hop: send-keys from the session daemon. The relay is descoped.
- **#43ax** Durable inbox, **#4tq4** company store: deferred to the company tier.
- **#5vcs** Knowledge notes: CC Flow's own decision, not the platform's.
- **#2sfs** Temporal and **#65cb** hosted cloud agents: unchanged (not for solo).
- **#5r3g** Custom security components. At solo there are 3 (`ccsbx` + daemon, ccsp-model,
  ccsp-egress). Friends adds 3 (ccmg-policy, ccgrant, the approval page). The hub and relay are
  out, so 6 in total, in line with doc 68's ~6.

## Conflicts from v1 (still valid unless noted)

| # | Conflict | Outcome | v2 |
|---|---|---|---|
| 1 | never terminate vs pods stopping after idle | ❓ #5gbg | invariant 15 |
| 1b | live change of host or harness | next generation | invariant 15 |
| 2 | tmux in pod vs one tmux | one host tmux | doc 3 |
| 3 | Channel vs sidecar vs socket | relay | **send-keys; relay descoped** |
| 4 | API-key scrub vs Console key | the launcher picks the billing mode | invariant 18 |
| 5 | Max token in the client | ❓ #6f24 | — |
| 6 | backend dependence, hooks, RC | v1 | invariant 16, with the model exception |
| 7 | host users vs a container per session | ❓ #3g4a | a container everywhere, Docker on the Mac |
| 8 | agentgateway only at company | from the sandbox tier on | from **friends** |
| 9 | a pod per credentialed MCP server | egress route for SaaS | at solo, the egress route is the only MCP path (N2) |
| 10 | gateway resolves servers | catalog fixed at spawn | — |
| 11 | PreToolUse as a PEP | speed bump | invariant 3 |
| 11b | two policy sources | ccmg-policy → ccgrant | — |
| 12 | two CC Flow MCP servers | one `ccf` | in doc 3, behind the door |
| 13 | YAML evaluator vs Cedar | Cedar | — |
| 14 | grant TTL 30 vs 7 days | ❓ #6dvg | — |
| 15 | grants at network level only | path-aware git route | solo |
| 16 | authoritative grant record | signed JSONL | — |
| 17 | `grant.resolve` return | ceiling + grant ids | — |
| 18 | proof field | B's API | — |
| 19 | zero-modules rule | ccgrant is a separate binary | — |
| 20 | designated approval responder | tool prompts only | invariant 21 |
| 21 | NATS vs Postgres inbox | ❓ #4tq4 | deferred |
| 22 | envelope kinds | doc 68's kinds | descoped with the hub |
| 23 | worker messaging in v1 | to owner or manager | minimal path, doc 1 §7 |
| 24 | injected vs delivered | `delivered` | — |
| 25 | messaging-hub as agent hub | human channels only | — |
| 26 | Graphiti as truth | ❓ #5vcs | CC Flow's |
| 27 | missing actions | B adds them | — |
| 28 | session registry owner | CC Flow | doc 3 |
| 29 | agent door timing | spool now | session daemon |
| 30 | sandboxed managers | ❓ #2sw2 | doc 3 caveat |
| 31 | phone screen view | ❓ #5sxw | CC Flow's |
| 32 | separate write origin | deferred | — |
| 33 | Remote Control | gone | — |
| 34 | socket path | moot | moot (relay descoped) |
| 35 | one `/run/ccf` mount | split into `out/` and `in/` | invariant 11, one outbox per generation |
| 36 | number of custom components | ❓ #5r3g | 6, see above |

## Build list by tier

Sizes: XS < ½ day, S ≈ 1–2 days, M ≈ a week. 🔑 = Tom's action, # = root on hp.

**First slice (hand this to the builder first).** CC Flow on the Mac starts a session on the
hp. It runs fenced, talks to the model through the Hetzner model door on an API key, and pushes
its branch through the Hetzner egress door.
- **Works when:**
  - a real `say hi` answers;
  - a grep of the container's env, home and `/proc` finds no token;
  - the push lands on a branch, and a force-push is refused;
  - a token copied into another container gets 401;
  - the session's events show up in the tree on the Mac.
- **Pieces:** U0, U0b, U1, U2, U3, U4, U5, U15, U16, M1 and M2, plus H1 and H2 as soon as U1 exists.

**Solo**

| # | Piece | Size | Needs |
|---|---|---|---|
| U0 | Leave the docker group on hp and Hetzner; `ccsbx` becomes a fixed-flag root wrapper | XS # | #6d6g |
| U0b | **Host keys** under a dedicated uid on every machine and the door host, checked by a test that runs as Tom's uid | S # | U0 |
| U1 | `ccsbx run` + image + managed settings; one outbox per generation; Docker Desktop on the Mac | M | — |
| U2 | generic start call (identity with `machine`, profile, billing); CC Flow's `Host:` maps to it | S | U1 |
| U3 | session daemon v1: events, outbox requests, minimal messaging via send-keys | M | U2 |
| U5 | **session token** signed by the machine key (moved from friends); doors trust a key list; IP logged only | S | U1 |
| H1 | **host git with hooks disabled**, and agent-written steering files flagged in the commit | S | U3 |
| H2 | **harness and image pin + update gate**: a new version is used only after U19 passes | S | U1, U19 |
| M1 | **central doors on Hetzner**: model and egress doors under their own uid, reachable only over Tailscale | S # | U0b |
| M2 | **remote launch and events**: start over Tailscale SSH, events back to the steering machine, cross-machine messages to the target daemon | M | U2, U3 |
| U4 | model door: credential injection, billing per session (API key first, subscription stub) | S 🔑 | M1, U5 |
| U7 | resume into the same volume | XS | U2 |
| U15 | egress door: static routes, GitHub App | S 🔑 | M1, U5, #46tt |
| U16 | path-aware git route; refuse force-pushes and protected branches | S | U15 |
| U17 | settings compile; verify deny under bypass | S | U1 |
| U19 | allow/block tests on every Claude Code bump | S, grows | U1 |
| I1 | install path for someone else's machines (one script per machine type, no Tom-specific steps) | S | the first slice |

**Friends**

| # | Piece | Size | Needs |
|---|---|---|---|
| U6 | tool door up (agentgateway, audit), on the central door host | S | U5 |
| U8–U10 | grant store, ccgrant (Cedar), revocation when the generation ends | S + M + XS | #55ca |
| U11 | ccmg-policy: catalog, pinning, fail closed | S–M | U6, U9 |
| U12 | grant requests on the approval channel; ❓ cards are one subscriber | S | U9 |
| U13 | platform approval page + passkey proofs; first decision wins | M 🔑 | #52ds |
| U14 | response redaction + outbound scan at the door | S | U11 |
| G1 | grant plan: scout prompt, plan → bundle, batched asks | S–M | U12, U13 |
| U18 | AUTONOMY prose → profiles | S 🔑 | U9, #4srd |
| U31 | Telegram low tier + Web Push | S + S | U13 |

**Company** (at the first client)
U33 OAuth custody, U36 per-user Google, the hub (U20–U26), the policy store on Postgres, the
OpenBao and Keycloak backends, pods with gVisor/Kata (U41), and an audit pipeline.

**CC Flow side** (doc 3, not the platform): U26 `ccf` MCP server + manager verbs, U27
knowledge notes, U28–U30 roster, screen view and attach lease, U32 `ccattach`, U38–U40 harness adapters.

**Tom's actions:** the model key (U4), the GitHub App (U15), the passkey (U13), the pharmacy
token rotation (#46tt), the U18 review, and #6d6g.
