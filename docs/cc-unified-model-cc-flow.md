---
type: design
status: draft
version: 2.1
title: "Agent fence: CC Flow as a client"
task: ai-tooling:#6gqx
date: 2026-11-19
supersedes: unified-model.md v1 parts of §1–§2 (orchestration E, surfaces D, cooperation C)
---

[Index](README.md) › Unified model › 3. CC Flow as a client

**Docs:** [1 · contract](unified-model.md) · [2 · tiers](unified-model-tiers.md) · **3 · CC Flow as a client** · [4 · working log](unified-model-log.md)

# CC Flow as a client

CC Flow is one client of the [security contract](unified-model.md). The platform contains no
CC Flow-specific code. Docs 1 and 2 can be deployed without CC Flow.

## Hook points

1. **Launch.** CC Flow calls the platform's "start a session" call with an identity, a grant
   profile, a machine and a billing mode. The tree address `<tree>:<node>/g<gen>` goes in as the
   identity `label`, and the CC Flow generation maps to the platform's generation. For another
   machine, CC Flow runs the call there over Tailscale SSH, and the pane on the steering machine
   attaches to the remote session.
2. **Events.** CC Flow subscribes to every machine's session event stream (started, idle,
   finished, hook events) to colour the tree and the tmux panes.
3. **Tree verbs.** `outcome`, `question`, `subtask`, `checkpoint`, `rename`, `done(self)`,
   `msg.*` and so on come from CC Flow's own MCP server, **behind the tool door like any other
   tool**. The address comes from the session token, never from arguments.
   - **Solo tier (no tool door yet):** the server runs as a stdio server inside the fence, with no
     network and no secret. It only writes requests into the outbox, and the session daemon
     forwards them to CC Flow.
4. **Approvals.** CC Flow may show grant requests as cards in its ❓ queue, which makes it one
   more surface on the approval channel. The proof still comes from the platform's approval
   page; a ❓ answer is never a proof.

## Mapping CC Flow concepts

| CC Flow | Platform |
|---|---|
| Node `Host:` / `Harness:` | `machine` and `harness` in the start call. A change applies at the next generation |
| Node billing (dev vs ops) | `billing` in the start call: subscription for dev work, API key for ops work |
| Node `Grants:` / Arm! ceiling | a grant profile or ceiling, approved with a proof |
| Grant plan from a scout | a ❓ card that links to the approval page |
| Manager → worker message | the minimal messaging path. CC Flow supplies the scope rule (owner and manager per node) |
| Manager verbs (`start`, `deliver`) | CC Flow's own calls on the platform, rate-capped (`start` ≤ 3/min) |

## What stays in CC Flow (not in the platform)

- The tree, the ❓ queue, managers, arming, placement and the session registry.
- tmux as CC Flow's CLI surface: roster, screen view, send box, attach lease.
- The web Tree. It links to the platform's approval page, and it doesn't host approvals.
- Knowledge notes, the FTS5 index and Graphiti.
- Schedules (tschedule).

## Caveats

- **Managers stay unfenced** until CC Flow's manager verbs exist behind the tool door. They're
  outside the contract until then (doc 1, invariant 13). Invariants 12 and 14 (host keys under a
  dedicated uid, no agent in the docker group) are what keep an unfenced manager from minting
  tokens or escaping through docker.
- **CC Flow runs on one steering machine.** If that machine is down, sessions elsewhere keep
  running (doc 1, invariant 16), but nobody steers them until it's back.
- **CC Flow never loads a fenced worktree's agent-written files** (`CLAUDE.md`, settings,
  hooks) into an unfenced manager without review (doc 1, invariant 25).
- **A manager's message carries no authority.** A worker's ask goes to the human, not to its manager.
- **The `ccf-` prefix** on platform components (`ccf-hostd`) is a leftover from v1. A rename is
  open in [the log](unified-model-log.md#new-in-v2).
