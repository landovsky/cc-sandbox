---
type: design
status: draft
version: 2.2
title: "Agent fence: CC Flow as a client"
task: ai-tooling:#6gqx
date: 2026-11-19
supersedes: unified-model.md v1 parts of §1–§2 (orchestration E, surfaces D, cooperation C)
---

[Index](README.md) › Unified model › 3. CC Flow as a client

**Docs:** [1 · contract](cc-unified-model.md) · [2 · tiers](cc-unified-model-tiers.md) · **3 · CC Flow as a client** · [4 · working log](cc-unified-model-log.md)

# CC Flow as a client

CC Flow is one client of the [security contract](cc-unified-model.md). The platform contains no
CC Flow-specific code. Docs 1 and 2 can be deployed without CC Flow.

CC Flow is the **cockpit**: it shows sessions and steers them, and it runs on the Mac or the hp.
If it's down, only visibility is lost, never progress (doc 1, invariant 28). Every hook point
below says what happens while it's away.

## Hook points

1. **Launch.** CC Flow calls the platform's "start a session" call with an identity, a grant
   profile, a machine and a billing mode. The tree address `<tree>:<node>/g<gen>` goes in as the
   identity `label`, and the CC Flow generation maps to the platform's generation. For another
   machine, CC Flow runs the call there over Tailscale SSH, and the pane on the cockpit
   machine attaches to the remote session. The start call also fixes the session's **message
   scope rule** (its owner and manager), which the daemon keeps, so messaging never asks CC Flow.
   - **A manager starting workers** (*proposal, log N10*): the manager's `start` goes to the session
     daemon on its machine, which calls the target machine's launcher directly over Tailscale SSH,
     using only a grant profile already approved for that node (a start carries no authority). The launcher reports the new session as an event, and CC
     Flow records the node when it sees that event, now or on replay. So a manager can start
     workers while CC Flow is down.
2. **Events.** CC Flow subscribes to every machine's session event stream (started, idle,
   finished, hook events) to colour the tree and the tmux panes. Each daemon keeps its events in
   a journal, and CC Flow resumes from the last position it saw, so a cockpit restart or a move
   to the other machine loses nothing.
3. **Tree verbs.** `outcome`, `question`, `subtask`, `checkpoint`, `rename`, `done(self)`,
   `msg.*` and so on come from CC Flow's own MCP server, **behind the tool door like any other
   tool**. The address comes from the session token, never from arguments.
   - **Solo tier (no tool door yet):** the server runs as a stdio server inside the fence, with no
     network and no secret. It only writes requests into the outbox, and the session daemon
     forwards them to CC Flow.
   - **While CC Flow is down** the daemon acknowledges each verb as queued and delivers the
     queue in order when CC Flow is back. `msg.*` doesn't wait: it goes daemon to daemon.
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
| Manager → worker message | the minimal messaging path. The scope rule (owner and manager per node) is fixed in the start call and enforced by the daemons |
| Manager verbs (`start`, `deliver`) | rate-capped (`start` ≤ 3/min). `start` goes to the target launcher directly and CC Flow records it afterwards (proposal, log N10) |

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
- **CC Flow is portable, and optional to sessions.** It runs on the Mac or the hp. If it's
  down, sessions keep working and messaging each other, and Tom can't watch or drive them
  through CC Flow until it's back (or started on the other machine). Then the daemons replay
  events and deliver queued verbs, and the tree catches up (doc 1, invariant 28).
- **CC Flow never loads a fenced worktree's agent-written files** (`CLAUDE.md`, settings,
  hooks) into an unfenced manager without review (doc 1, invariant 25).
- **A manager's message carries no authority.** A worker's ask goes to the human, not to its manager.
- **The `ccf-` prefix** on platform components (`ccf-hostd`) is a leftover from v1. A rename is
  open in [the log](cc-unified-model-log.md#new-in-v2).
