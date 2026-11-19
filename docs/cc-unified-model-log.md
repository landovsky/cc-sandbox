---
type: log
status: working
version: 2.3
title: "Agent fence: working log (changes, conflicts, decisions, build list)"
task: ai-tooling:#6gqx
date: 2026-11-19
supersedes: unified-model.md v1 §4–§6
---

[Index](README.md) › Unified model › 4. Working log

**Docs:** [1 · contract](cc-unified-model.md) · [2 · tiers](cc-unified-model-tiers.md) · [3 · CC Flow as a client](cc-unified-model-cc-flow.md) · **4 · working log** · [5 · build plan](cc-unified-model-build-plan.md)

# Working log

## v2.2 → v2.3 (the build plan and its review panel, 2026-11-19)

The phased plan is [doc 5, the build plan](cc-unified-model-build-plan.md) (cc-sandbox:#326q). It
was drafted by three planners, then reviewed by a panel of three. These are the model changes it
made; its §11 says which panel findings were accepted and which were rejected.

- **future-two runs fences and doors as k3s pods** (Tom: "Pods, not docker"). The doors sit in
  their own namespace, `cc-sandbox-doors`, so the launcher's RBAC can't reach their Secrets.
  Invariant 12 gains a cluster clause. N9 is closed.
- **The token never enters the fence.** A per-session forwarder (or the pod sidecar) holds it,
  together with a per-generation key. Doors check proof of possession, so a copied token is
  useless. The same key signs message stamps. Doc 1 §3 and §4 are updated.
- **Messages:** the daemon goes to the other daemon via the session-bus hub. The daemon stamps
  and signs, and the bus only routes (doc 1 §7). The hub is a door (invariant 4) with
  per-principal credentials and fail-closed auth. If the hub is down, daemons deliver directly.
  The hub stays on the hp. Signed stamps are no longer descoped.
- **Doors in a cluster:** a session listener that never trusts identity headers, plus an owner
  listener reached only through the Tailscale proxy (invariant 27).
- **Low-tier approvals at solo** (amends N5). The owner's identity plus a one-time code sent to
  the phone, because an owner device isn't a human (invariant 5).
- **No Tailscale SSH anywhere** (amends N7 and N10). Starts go through the target machine's
  daemon API. Attach uses OpenSSH, or `kubectl exec` through the hp's root wrapper for
  future-two pods.
- **The daemon mints generations** and refuses a second live generation of a node. The scope
  rule names the manager by node.
- **New known limitations:**
  - the hub as a single point of failure for cross-machine messages (with a direct fallback);
  - an owner device is not a human;
  - Mac stamps are `fenced-mac`.
- **Renamed:** doc 4's `K1` ("fence host on future-two") becomes **F1**, which is dropped. Its id
  clashed with the k3s team's K1–K11. The pod path is F2–F5 in doc 5.
- **Found during review** (outside the model, but blocking it): hp already has root paths for
  tomas (NOPASSWD sudo on docker, apt-get, dokku and a tomas-writable sops; the docker and lxd
  groups). Doc 5 ❓ Q7 fixes them at the morning gate.

## v2.1 → v2.2 (Tom's cockpit rules, 2026-11-19)

- **Cockpit down = only visibility lost, never progress.** New invariant 28 (doc 1). Steering
  means driving sessions through CC Flow, nothing more. Gaps in v2.1 closed:
  - **Messaging:** the scope rule is fixed per session at launch and kept by the daemon (it no
    longer comes from CC Flow), and cross-machine messages go daemon to daemon (doc 1 §7).
  - **Events:** each daemon keeps an event journal and replays it when the cockpit reconnects.
  - **Tree verbs** (`done`, `outcome`, `checkpoint`…): the daemon queues them while CC Flow is
    down and delivers them in order later.
  - **A manager starting workers** no longer needs CC Flow: *proposal* N10.
  - Doc 3's caveat "nobody steers them until it's back" is reworded.
- **The cockpit is portable.** It runs on the Mac or the hp. "CC Flow on the Mac" and "the
  steering machine" are gone; nothing in the platform names a cockpit machine.
- **A third machine:** `future-two`, a Hetzner server already joined to the k3s cluster, runs
  sessions next to the Mac and the hp (Tom's choice). How it runs them and whether it also hosts
  the doors are open (N9). Doc 2 has a short note on a fence host that's also a k3s node (docker
  beside k3s, and the door host on the same server).
- **New known limitation:** a message to a machine that's down is dropped (doc 1 §9).
- **Links fixed:** the v2 docs linked to `unified-model*.md` (the v1 file). They now link to
  `cc-unified-model*.md`, and the index points at v2.

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

Status in v2.3: all of these are ❓ nodes under cc-sandbox:#326q (doc 5 §12).

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
- **N5 · Grants at solo** are static config with no ask tier (unlisted = forbidden). *Amended in
  v2.3:* a low-tier ask exists at solo (doc 5 P7, ❓ Q10).
- **N6 · The session token at solo.** Each machine's launcher signs its tokens, and the central
  doors trust a list of machine keys. *Follows from the central doors. Confirm.*
- **N7 · Remote sessions in CC Flow.** The pane on the cockpit machine attaches to a session on
  another machine over Tailscale SSH. *Amended in v2.3:* no Tailscale SSH. Attach uses OpenSSH,
  or `ccsbx attach` on the hp for future-two pods (❓ Q14, Q17).
- **N9 · `future-two` as the third machine.** *Decided:* Tom picked `future-two` (Hetzner Robot,
  Xeon E3-1275V6 4C/8T, 64 GB ECC, a `pool=shared` k3s worker, LUKS). *Decided in v2.3:* pods,
  doors as pods too, pinned there for the pilot (❓ Q1). The original question was:
  - whether its sessions run as **docker on the host** beside k3s, or as **k3s pods**;
  - whether the **central doors** (M1) run on it too, or on another Hetzner server.
- **N10 · A manager starts workers without CC Flow.** *Amended in v2.3:* an unfenced manager's
  `cc-tree start` calls the target daemon directly (no Tailscale SSH); fenced sessions can't
  start sessions in this milestone (❓ Q17). *Original proposal:* The manager's `start` goes to
  the daemon on its machine, which calls the target machine's launcher directly over Tailscale
  SSH with a profile already approved for that node. The new session shows up as an event, and
  CC Flow records the node from it, live or on replay. Rate cap (≤ 3/min) moves to the daemon.
  *Confirm, or keep `start` in CC Flow and accept that managers can't start workers while it's down.*
- **N8 · Unfenced agents on the Mac** can reach the Docker Desktop socket. Accept it as a
  limitation, or run no unfenced agents on the Mac. *v2.3 recommendation:* accept it, cap the Mac
  key at `mac-max`, and mark Mac stamps `fenced-mac` (❓ Q17).

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

**Superseded by [doc 5, the build plan](cc-unified-model-build-plan.md)** (v2.3). Doc 5 has the
phases, owners, acceptance tests and nightly prerequisites. The list below is kept for its ids,
and doc 5 reuses them.

Sizes: XS < ½ day, S ≈ 1–2 days, M ≈ a week. 🔑 = Tom's action, # = root on hp.

**First slice (hand this to the builder first).** CC Flow, running on the Mac, starts a session
on the hp. It runs fenced, talks to the model through the Hetzner model door on an API key, and pushes
its branch through the Hetzner egress door.
- **Works when:**
  - a real `say hi` answers;
  - a grep of the container's env, home and `/proc` finds no token;
  - the push lands on a branch, and a force-push is refused;
  - a token copied into another container gets 401;
  - the session's events show up in the tree on the Mac;
  - with CC Flow stopped, the session finishes its task and calls `done`; when CC Flow starts
    again (on the Mac or the hp), the missed events and the `done` show up in the tree.
- **Pieces:** U0, U0b, U1, U2, U3, U4, U5, U15, U16, M1, M2, M3 and M4, plus H1 and H2 as soon as U1 exists.

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
| M2 | **remote launch and events**: start over Tailscale SSH, events to whichever machine the cockpit is on, cross-machine messages daemon to daemon with the scope rule fixed at launch | M | U2, U3 |
| M3 | **event journal + replay**: each daemon keeps its events and replays from the cockpit's last-seen position on reconnect | S | U3 |
| M4 | **verb queue**: tree verbs queued in order while the cockpit is down, delivered when it's back | S | U3 |
| M5 | **portable cockpit**: CC Flow starts on the Mac or the hp with no config change and resumes from M3 | S | M2, M3 |
| M6 | **manager start without CC Flow** (N10, proposal): daemon → target launcher, CC Flow records it from the event | S | M2, M3, N10 |
| F1 (was K1, dropped in v2.3: pods) | **fence host on `future-two`** (N9): docker beside k3s, bridge range and iptables checked against flannel, kubelet reservations, host keys; or the fence as a pod, if N9 picks pods | S # | N9, U0b |
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
