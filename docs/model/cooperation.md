---
type: analysis
status: draft
area: C — inter-agent cooperation (session hub, discovery, context storage and exchange)
task: ai-tooling:#6vb5 (milestone ai-tooling:#6rrd)
date: 2026-11-18
inputs: docs/unified-session-model.md, docs/cc-flow-on-kubernetes.md, docs/coding-harness-control-surfaces.md,
  doc 68 (c84519e) §1.4, §4 and evidence 24-messaging-hub, doc 69 (c0cabab), ai-tooling-map.md,
  model/orchestration.md and model/surfaces.md (as of 23:17), evidence/cooperation-*.md
---

# C. Inter-agent cooperation

**In one line.** Cooperation has two planes. **Messages** are short-lived, go through the hub
and carry references. **Context** is durable, lives in the tree repo in git, and is
written only by a host-side writer that stamps who wrote it. The hub stamps `from` on a message,
and the context writer stamps `author` on a checkpoint, an outcome, an artefact or a note. Both
follow the same rule: identity comes from the pane or the fence, never from the text. Buy almost
nothing: the market converged on "a SQLite or git mailbox plus a nudge" with no signing, scoping
or tenancy, and Claude Code now ships a documented, turn-aware native inbox that is the best
last hop.

## 1. What Tomas has today

| Piece | What it does | Where | State |
|---|---|---|---|
| **session-bus** | Elixir/Phoenix hub (presence, routing) + a spoke per machine; `ccmsg` resolves exactly one target or refuses; native inbox first, `send-keys` 512 B chunks as fallback; msglog 30 days, redacted | `~/.dotfiles/session-bus/` (`lib/session_bus/native.ex:56`, `bin/ccmsg`), hub container on hp `:4011`, Mac spoke via launchd | **live**; S1 fail-open if `SB_SECRET` unset, S2 non-constant compare, open `/sessions` leaks `gist`, the secret leaks in `docker inspect`, a non-git copy in `~/sb-m2`, no restart policy |
| `ccq ccreply ccfan ccgroup ccwho`, `/msg` skill | list, raw reply, deliberate fan-out, groups (`~/.claude/cc-groups.json`) | `session-bus/bin/`, `claude/skills/msg` | live; `--kind` goes only to cc-events (87 % untyped, doc 68 S70) |
| **`cc-tree deliver`** | the only door for new work: message / reseed / checkpoint-then-reseed / hold / start, from `session_health` + `Refresh:` | `bin/lib/cc_tree_deliver.py` (ADR-0004) | live; the switch defaults to *log* mode (inferred, not re-verified) |
| Manager hears workers | worker state changes → manager over the bus, shadow by default | `cc_tree_hear.py`, `heard.log` | live, mostly `dry` (inferred) |
| **Checkpoints** | `/checkpoint` → hooks capture → `cc-tree ingest` → write-once `nodes/<id>/checkpoints/<ts>-<machine>-<sid8>.md` (Done/Found/Decided/Open/Next) | `cc_tree.py` `store_checkpoint`, `latest_ckpt:2399` | live; ~15/day, median 3.7 KB (doc 29) |
| **Briefs** | `build_brief` (`cc_tree.py:4190`): newest checkpoint or milestone brief + Outcome + Accept + rules + worktree; write-once `briefs/` | `cc_tree.py:4235` | live |
| **Outcome**, ❓ nodes | node fields in `tree.md`; ❓ answers appended to the Outcome | `tree.md`, `cc_tree_decisions.py` | live |
| **Artefacts** | PostToolUse spool → Stop folds into `nodes/<id>/artifacts-<machine>.jsonl`; id `art:<5>` = sha1 of a locator (`git:`/`file:`/`url:`/`pr:`) | `cc_tree_artifacts.py` (doc 53; its header "nothing built" is stale) | live |
| Link layer | one id namespace, `cc-tree resolve --json` | `cc_tree.py:3552` (doc 51) | resolver live, `card` not built |
| **MD Canvas** | annotate a `.md`; Submit wakes the session with `ccmsg`; "who touched this doc" from `artifacts who` | `~/.dotfiles/mdcanvas`, `mdcanvas.service :4300` | live; no app auth (D's map gap 3) |
| **Claude Code memory** | per-cwd `~/.claude/projects/<slug>/memory/` + `MEMORY.md` index; 33 indexed dirs, 780 files, 4.3 MB; nothing shared across projects | `~/.claude/projects/*/memory/` | live, per-project only |
| **claude-memory-store** | Graphiti MCP on Neo4j, LiteLLM → Vertex Gemini, local bge-m3; `/learn` writes to it | `~/git/claude-memory-store`, 4 containers on hp, registered in one project of `~/.claude.json` | live, used by a few projects (inferred) |
| `~/git/messaging-hub` | **not an agent hub.** A Rails 8 + Postgres hub for *human* channels (Telegram now, WhatsApp later): per-tenant event log pulled by cursor, idempotent outbox, fail-closed guard | `docs/overview.md` | M1 in progress; the brief's "Elixir hub" is session-bus |
| Native session registry | `~/.claude/sessions/<pid>.json`: `sessionId`, `pidDomain`, `peerProtocol: 1`, `peerFeatures [notify_idle, …]`, **`messagingSocketPath: /run/user/1000/cc-socks/<pid>.sock`**, `tmux`, `status` | written by Claude Code 2.1.325 | live, read by the bus, cc-park and hooks |

Three channels that don't meet: live text goes over the bus (kept 30 days on hp only), durable
context lives in the tree (pushed to GitHub), and facts live in per-project memory or Graphiti.
Only the first two are joined, through `cc-tree deliver`.

## 2. What cooperation must do in the unified model

**Hub** (doc 68 §4, kept): address by task (`<tree>:#<id>`, `ms:<tree>:#<id>`, `desk:`, `human:`); a
durable inbox per address; **turn-aware delivery** (work waits for a turn boundary with no human
attached and no draft; answers and control may land mid-turn); receipts
`queued → delivered → consumed` (+ `held`, `expired`, `failed`, `refused`); a hub-stamped,
**signed envelope** (Ed25519 over JCS); `kind` mandatory; trust tiers by ingress; "authority
does not travel" (no message grants anything); breakers on loops, storms and cost.

**Discovery**: a scoped directory, not pods finding each other. A worker sees its owner, its
manager and its siblings; a manager sees its workers. Entries are A2A Agent Card shaped
(doc 24 §5.2). The launcher registers a session; the session never registers itself.

**Context exchange** (new in this doc):
- X1 **Pass by reference.** A message body stays small (the native inbox caps at ~1 M chars
  [D]). Anything durable is a context object, and the message carries `refs: ["ckpt:…", "art:…@<sha>"]`.
- X2 **One writer, stamped provenance.** Every context write from a session goes through the
  host-side writer (E's agent door, `orchestration.md` E4), which stamps `author {address, gen,
  session, machine, harness}` and `trust` from the pane's identity, and commits with trailers
  (`Node:`, `Claude-Session:`, `Trust:`).
- X3 **Scoped reads.** A fenced session sees its own node, its milestone's brief and outcomes, and
  what was explicitly shared with it, not the whole tree repo (the tree holds every Outcome).
  Area B decides; C enforces at the reader.
- X4 **Handing a file over** = share an artefact by id and content hash; the recipient gets a
  read-only copy in its inbox directory, never a shared mount between two fences.
- X5 **Shared knowledge** is markdown in git with a `proposed → accepted` lifecycle. Agents
  propose; a manager inside its milestone, or Tomas, accepts. Everything injected into a prompt
  is labelled with its provenance, and untrusted text never becomes an instruction.
- X6 **Derived indexes are disposable.** Search (FTS, embeddings, Graphiti) is rebuilt from the
  files and filters by scope *before* retrieval.

Tomas's invariants as they bind here:
- **No secret inside the fence.** No bus secret in agents (closes S41/S42). The agent talks to a
  local door or socket. The signing key lives only in the hub, and Graphiti's LLM key stays outside.
- **Sessions don't depend on the backend.** Sends go `queued-local` when the hub is down, the
  context writer is local (the tree clone), and briefs are built from the local clone.
- **Consent by a human.** A message never approves anything. Promoting knowledge to `accepted`
  at tree or global scope, and sharing across trees, need a human or a pre-granted ceiling (B).
- **tmux owns the lifecycle** on local and sandbox. The hub delivers into a session and never
  starts, stops or reseeds one. That stays `cc-tree deliver` (E).

**What the hub must NOT become** (doc 68 MR-39, the tree service as a cross-session pivot):
1. not a **credential holder** beyond its own signing key (no Telegram, GitHub or model keys:
   messaging-hub and tree-core keep theirs);
2. not a **session creator or reseeder** (launcher and `cc-tree deliver` own that);
3. not the **tree writer** or the context store: it carries refs, the writer is E's;
4. not an **approver**: there are no grant verbs, and `kind: answer` to a ❓ is routed to E, never applied;
5. not a **reachable privileged endpoint**: agents reach only their door or socket, and the door
   holds nothing (doc 68 `session-api`);
6. not a **typist into panes** from a network service: the last hop is host-local (spoke or relay);
7. not an **amplifier**: no label or group addresses, and broadcast is a separate verb with a dry run and a grant;
8. not a **memory or LLM service**: no summarising, embedding or rewriting of bodies;
9. not a **workflow engine**: timing of work, retries of tasks and fresh-vs-message belong to E;
10. not **required for a session to run** (B0).

## 3. Outside solutions (as of 2026-11-18)

Full survey with sources: [`evidence/cooperation-transport-survey.md`](evidence/cooperation-transport-survey.md),
[`evidence/cooperation-memory-survey.md`](evidence/cooperation-memory-survey.md) (two Sonnet
subagents with web search; context given: the hub jobs, the tree shape, the invariants, the tiers;
no repo access, no external models). D = documented, R = reported, I = inferred, O = observed here.

**What changed since doc 68 (6 Nov):**
- **Claude Code's cross-session messaging is now documented** (≥ 2.1.224) [D,
  code.claude.com/docs/en/cross-session-messaging]. It provides a per-session Unix socket and is
  turn-aware: the message is read between tool calls, or starts a turn if the session is idle.
  `crossSessionInbound: accept|hold|refuse`, and a bypass-mode receiver holds by default. Text only.
  "A message can never approve permissions or change config." The receipt is only "sent" plus a
  one-shot `notify_when_idle`. Host and container sockets do not connect [D]. Cross-machine
  delivery runs through Anthropic + Remote Control [D], which is not used. The line protocol for a
  poster that isn't a child process is undocumented.
- **Channels are still a research preview**, give no ack, need allowlists and org enabling, and on
  the v2 MCP runtime a server that negotiates MCP 2026-07-28 is **not registered as a channel** [D].
- **MCP 2026-07-28** is stateless with no standard push, and tasks are now an extension [D]. MCP is
  good for request/response tools and poor as a push path.
- **A2A 1.0** sits under the LF and AAIF, with JWS-signed Agent Cards (about 1.5 % of crawled cards
  are signed [R]). IBM's ACP merged into A2A (archived 2025-08) [D]. Zed's Agent Client Protocol is
  editor-to-agent and the wrong layer [D]. AGNTCY (directory, OASF, SLIM) is Cisco-led and heavy [R].
- **NATS stayed Apache-2.0 under CNCF** (May 2025 settlement) [D]. **Jepsen on 2.12.1 lost
  acknowledged writes**: the default fsync is every 2 min, and four issues were still open on
  2026-11-10 [R].
- **Memory market**: Mem0 removed OpenMemory MCP (Jul 2026) and moved graph memory to its paid
  platform [R]. Zep CE is dead (Apr 2025) [D]. Letta's **Context Repositories** (Feb 2026, memory
  as a git repo that subagents branch and merge) is the same idea as the tree [R]. Basic Memory 1.0
  (Oct 2026, markdown canonical, AGPL-3.0) [R]. Memory poisoning is OWASP ASI06; MINJA and
  AgentPoison show detectors are weak and structural controls work [R].

**Shortlist, scored** (maturity 1–5 · self-host · licence · vendor risk · fit solo / 500):

| Sub-layer | Candidate | Mat | Host | Licence | Risk | Solo | 500 | Verdict |
|---|---|---|---|---|---|---|---|---|
| envelope + directory shape | **A2A 1.0 shapes** (Message/Parts, task states, JWS+JCS) | 4 | y | Apache-2 | L | 4 | 4 | **pick** (shape only, not the wire) |
| | A2A as transport | 3 | y | Apache-2 | L | 1 | 3 | no: needs a listening server per agent |
| | AGNTCY Dir/SLIM | 2 | y | Apache-2 | M | 1 | 3 | watch, for cross-org later |
| durable inbox, solo | **SQLite (WAL) tables + HTTP long poll** | 5 | y | PD | L | 5 | 1 | **pick** (doc 68 K7 solo, kept) |
| | MCP Agent Mail, tmux-message-bus, xats | 1–2 | y | MIT | H | 3 | 1 | prior art; none sign, scope or tenant |
| durable inbox, company | **Postgres** (SKIP LOCKED, RLS per tenant, LISTEN/NOTIFY) | 4 | y | PG | L | 3 | 5 | **pick** as the system of record |
| | NATS JetStream (account per tenant, auth callout) | 4 | y | Apache-2 | M | 3 | 5 | add only for fan-out, leaf nodes or regions; `sync_interval: always`, R3 |
| | Redis/Valkey Streams | 4 | y | BSD/tri | L | 2 | 3 | no edge here |
| | Temporal (MIT) / Restate (BSL) | 5 / 3 | y | MIT / BSL | L / H | 1 | 3 | an inbox isn't a workflow; see E's ❓ E-3 |
| agent-facing interface | **MCP tool server** (`send/reply/lookup/status/ctx_*`) | 4 | y | spec | L | 5 | 5 | **pick**: pull is the contract |
| last hop into Claude Code | **native cross-session inbox** | 3 | local | prop. | H | 5 | 3 | **pick**, behind a relay in the fence; paste as fallback |
| | Channels | 2 | local | prop. | H | 2 | 2 | skip until GA |
| | Agent Teams (`~/.claude/teams/*/inboxes`) | 2 | local | prop. | H | 1 | 1 | one team per session; not a fleet bus |
| task context | tree repo (keep) vs Beads (Dolt), Agent Teams task list | — | — | — | — | — | — | **build on the tree**: nothing keeps git as truth with scoped writes |
| shared knowledge | Letta, Mem0, Graphiti, Cognee, LangMem, Basic Memory | 2–4 | mixed | Apache/MIT/AGPL | M–H | 2–4 | 2–3 | **build (thin)**: markdown + git + proposed/accepted; borrow Letta's branch/merge and Basic Memory's rebuildable index |
| retrieval | rg → SQLite FTS5 (+embeddings) → Graphiti/Cognee | 3–5 | y | — | L–M | 4 | 3 | **staged**; Graphiti (already live) as a derived index |
| artefact provenance | git blob/commit hash + signed commits + trailers → in-toto/DSSE later | 4 | y | Apache-2 | L | 4 | 5 | **pick**; MCP `resource_link` to hand it to the agent |

**RECOMMENDED 3RD PARTY, per sub-layer.**
- Wire shapes: **A2A 1.0** (Agent Card for directory entries, task-state words, JWS over JCS).
- Solo transport: **SQLite**.
- Company transport: **Postgres**, with **NATS JetStream** optional on top.
- Agent interface: the **MCP** spec.
- Last hop: **Claude Code native messaging** (vendor feature, accepted with a fallback).
- Retrieval: **Graphiti** (kept, demoted to an index), with **SQLite FTS5** first.
- Provenance: **git signing**, later **in-toto/Sigstore**.

**Build: nothing fits** for the hub's semantics (router, directory, receipts, signing, trust,
breakers) or for the context writer's scope and provenance rules. Every product surveyed lacks
signed envelopes, scoped directories, message-level authz and tenancy (transport survey §9). This
is the "small product" doc 68 already budgeted.

## 4. Contracts with the other areas

Shapes are JSON. "Door" = E's agent door (spool `/run/ccf/requests.jsonl` on sandbox, `session-api` on pod).

**Envelope (fixed now; doc 68 §4.4 plus `refs`).** The hub stamps `from`, `trust`, `resolved`,
`seq`, `sig`. The sender supplies `to`, `kind`, `body{text, refs[]}`, `reply_to`, `expects_reply`
and `idempotency_key`. Kinds: `dispatch | question | answer | report | fyi | ack | notice |
control.stop`. Of these, `fyi`, `ack` and `notice` start no turn. Receipt events
`{msg_id, state: queued|held|delivered|consumed|expired|failed|refused, at, via: native|paste, reason?}`.

**Context ref and object.** `ckpt:<tree>:#<id>/<ts>`, `brief:…`, `outcome:<tree>:#<id>`,
`art:<5>@<sha12>`, `note:<scope>/<slug>`, `dec:<tree>:#<id>`, all resolved by `cc-tree resolve`
(doc 51). Each object's front matter: `{ref, kind, scope: node|milestone|tree|global,
author{address, gen, session, machine, harness}, trust: owner|agent|untrusted, status:
proposed|accepted|superseded, derived_from[], sha}`.

**→ E. Orchestration (CC Flow).** C calls `resolve(address) -> SessionRecord{gen, host, harness,
machine}`. **The registry is E's; the hub caches it** (agree with E's X4). E calls
`hub.deliver(address, envelope)` only from `cc-tree deliver` (ADR-0004 stays the only entry), plus
`hub.presence(address)` and `hub.receipts(msg_id)`. Context writes ride E's door: E's
`checkpoint.store`, `outcome.set` and `question.add`, plus three new verbs C defines:
`artifact.share(art, to)`, `note.propose(scope, text)` and `ctx.get(ref)`. E stamps identity and C's
writer applies the provenance and scope rules (one module, `cc_tree_ctx.py`). Message sends from
inside a fence (`msg.send`, `msg.reply`) are door verbs too.

**→ B. Permissions** (B's API, `permissions.md` §4). C calls `check(principal, action, resource,
context) -> allow|deny|ask` per envelope (`message.send`, Address), per read (`context.read`,
Node|Artifact) and per share (`artifact.write` on the recipient's inbox). It calls
`allowed(principal, message.send, Address)` to build the scoped directory, so the directory never
lists what a send would refuse. B's default policy is the same scope as §2 Discovery. **Asks of
B**, not in its action list yet: `context.write(scope)`, `note.accept(scope)` and `broadcast(group)`.
`ask` becomes a ❓ through E. Messages may carry `grant_refs` (ids only), and the hub never reads
them as authority. Trust in the envelope maps from B's platform-set `provenance`:
`human` → `owner`, `delivery`/`scheduler` → `agent`, `untrusted` → `untrusted`.

**→ D. Surfaces.** D calls `hub.deliver` through `cc-tree deliver` with `from: human:<login>`,
`channel`, `nonce` (surfaces §4), and shows receipts. **D's "injected" = C's `delivered`.** D calls
`attach.begin/end(address, human)` (a lease with a 60 s heartbeat). While the lease is held, C holds
`dispatch`/`fyi` and lets `answer`/`control.*` through. D renders context objects (MD Canvas)
with their `author` and `trust`. A canvas Submit is `trust: untrusted` until D4's proof exists.

**→ A. MCP gateway.** C's agent tools are one MCP server, `ccf-coop` (`bus_send`, `bus_reply`,
`bus_lookup`, `bus_status`, `ctx_get`, `ctx_search`, `note_propose`, `art_share`; `kind` a required
enum). On sandbox it is a stdio server inside the fence that only writes to the door; it holds
nothing. On pod, A may serve it at the gateway, as E proposes for `ccflow`. Identity always comes
from the door or gateway, never from a tool argument. A pending A's doc: if A fronts every MCP
server, `ccf-coop` and E's `ccflow` should be **one** server with one verb table.

**→ Session host (unified model).** The last hop per host:
- `local`: the spoke writes to `messagingSocketPath` (or pastes).
- `sandbox`: the host writes signed envelopes into a read-only `/run/ccf/inbox/`, and a ~100-line
  **relay inside the fence** (no credential) checks the signature, waits for the boundary and
  posts to the container's own socket.
- `pod`: the sidecar.

`consumed` = UserPromptSubmit sees `body_sha256` (hook spool, unified item 3).

## 5. Build list delta (on top of unified §3 items 0–7 and orchestration E1–E9)

| # | Piece | Size | Order / needs | Works when |
|---|---|---|---|---|
| C1 | **Bus M0** (doc 68 Phase 0): the spoke owns `SB_SECRET`; fail closed; constant-time compare; `/sessions` behind auth; no secret in socket params or `docker inspect`; restart policy; drop `~/sb-m2` | S | first; no deps | `curl /sessions` without auth → 401; `docker inspect session-bus` and an agent's env show no secret; hub survives a reboot |
| C2 | **Message ids, `kind` required, receipts**: `ccmsg` returns `msg_id`; `consumed` from a UserPromptSubmit hook matching `body_sha256`; receipts in the msglog and `ccq --receipt <id>` | S | after C1 | a truncated paste shows `delivered` but not `consumed`; S13 can't recur silently |
| C3 | **Durable inbox (solo)**: `ccf-hub` host module (Python, beside cc-tree): SQLite inbox per address; accept → store → deliver at the boundary (Stop-hook state + D's attach lease) → receipt; `queued-local` outbox per machine; Elixir keeps presence and cross-machine routing in shadow | M | after C2; ❓ C-4 | a message to a busy or closed worker lands once, at its next idle, after a hub restart; a message while Tomas is attached waits |
| C4 | **Sandbox last hop**: relay in the image; `crossSessionInbound: accept` in the sandbox managed settings; host writes `/run/ccf/inbox/*.json`; a spike first on the documented socket protocol | S | with unified item 3/4b; ❓ C-3 | `ccmsg <sandboxed node>` arrives as a native frame (not keystrokes) and reports `consumed` |
| C5 | **Signed envelopes**: Ed25519 key only in the hub; JCS; spoke and relay refuse unsigned envelopes; rendered with a random boundary and `trust` | S | after C3 | a hand-written file in the inbox dir is refused; a hub-sent one is delivered |
| C6 | **Scoped directory**: `bus_lookup` returns A2A-card JSON for owner, manager and siblings only; human names → candidates, never a silent pick | S | after C3; B's scopes | a worker's lookup of another tree's node returns nothing; an ambiguous name returns a list |
| C7 | **Context writer + provenance**: one `cc_tree_ctx.py` behind E's door; front matter + commit trailers on checkpoints, outcomes, artefact events, notes | S | with E4 | every new checkpoint carries `author.address/gen/session` and a `Trust:` trailer; a request claiming another address is refused |
| C8 | **Scoped read for fences**: the sandbox mounts only its node dir (RO) plus a `ctx_get` door verb for refs B allows; no `~/cc-tree` mount | S | with C7 | a sandboxed worker reads its milestone brief; reading a sibling tree's Outcome is refused and logged |
| C9 | **Artefact share**: `art_share art:x --to <addr>` → B check → RO copy at `/run/ccf/inbox/art/<art>@<sha>` + an `fyi` message with the ref | S | after C7, C4 | the recipient opens the exact bytes (sha matches); a share outside scope → ❓ |
| C10 | **Knowledge notes**: `<tree>/knowledge/*.md` and `_global/knowledge/` with a `MEMORY.md`-style index; `note_propose` → `proposed`; accept by a manager (tree scope within its milestone) or Tomas (global); the brief includes accepted notes labelled with their source; `/learn` writes a proposal | M | after C7; ❓ C-2 | a fact proposed by one worker appears, labelled, in the next brief of a sibling only after acceptance |
| C11 | **`ccf-coop` MCP server**: the agent tools of §4 over the door | S | after C3, C7 | agents send typed messages without a shell CLI; untyped sends drop under 5 % in a week |
| C12 | **Search index**: SQLite FTS5 over tree + knowledge, filtered by scope before retrieval; Graphiti ingests accepted notes (outside the fence) | S | after C10 | `ctx_search` from a worker never returns a ref its `ctx.read` would refuse |
| C13 | **Company backend**: inbox and receipts on Postgres with RLS per tenant; NATS JetStream only if fan-out or regions need it | M (L with NATS) | first client; ❓ C-1 | the C3 tests pass unchanged against Postgres |

Minimum for "a sandboxed worker cooperates like a local one": C1 + C2 + C4 + C7 (S each),
on top of unified items 1–3 and E4. Hub durability (C3) is the next step and the biggest fix to
today's incidents (3.7 % failed sends).

## 6. Risks and conflicts

**Top 3 risks**

1. **The last hop is Anthropic's.** Native messaging is documented now, but the posting protocol
   for a third party isn't, and `hold`/`refuse` semantics or the socket path can change per release
   (2.1.325 here: `/run/user/<uid>/cc-socks/`, not doc 68's `/tmp/cc-socks`). Mitigation: pin and
   record `peerProtocol`, keep the bracketed-paste fallback, add a daily probe per host, and keep the
   `consumed` receipt independent of the socket (prompt hash).
2. **Shared context as a poisoning channel.** One compromised or confused worker writes a note or
   an outcome that every sibling's brief then injects (ASI06, MINJA). Mitigation: `proposed` is
   never injected; acceptance needs a human or a manager inside its own milestone; injected text
   carries `author`/`trust` and a boundary; git history gives rollback; red-team fakes in notes
   (doc 68 §10).
3. **Hub scope creep → MR-39.** Every "small" convenience (auto-reseed, approving from a reply,
   holding a Telegram token for notices, a summary LLM) turns the hub into the pivot. Mitigation:
   the ten NOT rules in §2 as block tests; the hub's process holds one key; code review against the list.

Also noted: the tree repo, with every Outcome, checkpoint and future note, is pushed to GitHub,
which is fine solo and a problem for client cells (GDPR erasure in git history). Plan per-tenant
repos or encrypted blobs with key deletion (memory survey §8).

**CONFLICTS**

> Merged and resolved (or turned into ❓) in [unified-model.md §4](../unified-model.md#4-conflicts); build items renumbered in §5.

| # | With | Conflict | Position |
|---|---|---|---|
| K1 | doc 68 K7 / §1.4 (company messages = NATS JetStream per tenant) | Jepsen found acknowledged-write loss on 2.12.1 with issues still open; receipts and authz are hub code anyway; Tomas's messaging-hub already runs the Postgres event log + outbox pattern | **Postgres as the system of record**; NATS optional on top for fan-out or regions. ❓ C-1 |
| K2 | unified model item 4b ("mount `/tmp/cc-socks` out, teach `SessionBus.Native` the container's `pidDomain`") | docs say host↔container sockets don't connect [D]; the live path is `$XDG_RUNTIME_DIR/cc-socks/<pid>.sock` [O] | an in-fence relay posting to its own local socket; the host only writes files. ❓ C-3 |
| K3 | harness doc ("input to live Claude sessions = a custom Channel server") | Channels: still preview, no ack, broken on MCP 2026-07-28 | native messaging + relay; Channels never needed (agrees with unified C3) |
| K4 | orchestration.md §4 envelope `kind: message\|tick\|answer\|work`; surfaces.md `kind: message` for human sends | doc 68 fixed eight kinds, and `fyi/ack/notice` must start no turn | keep doc 68's kinds: human sends → `dispatch` or `answer`; E's `tick` → `notice`, `work` → `dispatch` |
| K5 | orchestration.md E5 (`ccmsg` is a manager-only door verb, v2) | workers must report and reply from day one | `msg.reply` and `msg.send` to the worker's own owner or manager in E4; free addressing stays v2 |
| K6 | surfaces.md receipts `queued → injected → consumed` | naming only | use `delivered` (doc 68); D displays it as "injected" if it likes |
| K7 | the brief ("~/git/messaging-hub, Elixir hub") | it's a Rails human-channel hub, unrelated to session-bus | keep separate: it's an **ingress** (Telegram replies → `trust: owner` via msg-router), never the session hub (it holds channel credentials: MR-39) |
| K8 | claude-memory-store as the `/learn` truth | a graph with LLM rewrites can't be the source of truth for scoped, attributed knowledge | markdown notes are the truth, and Graphiti indexes accepted notes. ❓ C-2 |

| K9 | permissions.md §4 (actions; `provenance: human\|delivery\|scheduler\|untrusted`) | B has no `context.write`, `note.accept` or `broadcast`; its provenance words differ from doc 68's `trust` tiers | B adds the three actions; the envelope keeps `trust` (doc 68) mapped 1:1 from B's provenance (§4) |

Area A had published only evidence when this was written. The `ccf-coop` contract is my proposal,
to be reconciled by the integrator (F, #6gqx). B, D and E were read at 23:17–23:30.

## Decisions for Tomas (❓ under ai-tooling:#6vb5)

1. **#4tq4 C-1 Company message store**: Postgres as the system of record (rec), or NATS JetStream per tenant (doc 68).
2. **#5vcs C-2 Shared knowledge truth**: markdown notes in the tree repo with Graphiti as a derived
   index (rec), or keep Graphiti as the truth for `/learn`, or per-project memory only.
3. **#5gf6 C-3 Sandbox last hop**: an in-fence relay to the native socket (rec), or send-keys only, or a
   cross-namespace socket mount (contradicts the docs).
4. **#43ax C-4 Where the durable inbox lives (solo)**: a Python `ccf-hub` module beside cc-tree with the
   Elixir hub in shadow, then retired (rec, doc 68's "hub module in tree-core"), or a SQLite inbox
   added to the Elixir session-bus.
