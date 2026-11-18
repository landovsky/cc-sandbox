---
type: evidence
area: C
date: 2026-11-18
provenance: Sonnet subagent, web search and fetch only; context given: hub jobs, tree shape, invariants, tiers; no repo access, no external models
---

[Index](../../README.md) › [Unified model](../../unified-model.md) › [Cooperation](../cooperation.md) › cooperation-transport-survey

# Session-hub transport survey (as of 2026-11-18)
Tags: [D] documented in primary source, [R] reported by secondary source/blog, [I] my inference.
Scores 1-5. Fit solo = ~120 live interactive tmux sessions, one dev. Fit 500 = multi-tenant company.
## 0. Headline findings (read first)
1. Claude Code now ships NATIVE cross-session messaging (v2.1.224+ mac/Linux). [D]
   https://code.claude.com/docs/en/cross-session-messaging.md
   - Each session binds a per-session Unix socket (`/tmp/cc-socks-<uid>` fallback); path in `/status`
     "Peer address" and env `CLAUDE_CODE_MESSAGING_SOCKET`; token in `CLAUDE_CODE_MESSAGING_TOKEN`. [D]
   - Delivery is turn-aware: "reads the message between tool calls during an active turn. When idle,
     Claude Code starts a new turn with the message." [D]  (exactly the hub's "between turns" need)
   - Addressing by session name (`--name`/`/rename`), disambiguated by short id; not by task id. [D]
   - Inbound control `crossSessionInbound` = accept | hold | refuse; default holds if receiver bypasses
     permissions and sender doesn't identify as bypassing. Held msgs: max 100; queue max 50; dedupe + rate
     limit; ~1M char cap; plain text only; a message can never approve permissions or change config. [D]
   - Held messages expire (`dialogExpiry`, default 5 min); `-p` workers need `crossSessionInbound: accept`. [D]
   - Receipts: only "sent" (written) + `notify_when_idle` subscription (one notice when target next goes
     idle/exits, 12h expiry, local-only, main conversation only). No per-message "consumed" receipt. [D]
   - Cross-machine goes via Anthropic servers + Remote Control (needs claude.ai login). Containers/host cannot
     reach each other's sockets. [D]
   - Unknown: the socket's line protocol/JSON schema for third-party posters is NOT documented beyond the
     optional `{"type":"auth","token":...}` first line and the "own-child" verification rules. A hub that is
     not a child process of the session gets default inbound rules (may be "held"). Needs a spike. [D]/[I]
2. Channels is still a research preview (shipped v2.1.80, ~2026-03-19; third-party post says still preview at
   v2.1.287, Oct 2026). [D]/[R] https://code.claude.com/docs/en/channels
   and https://code.claude.com/docs/en/channels-reference
   - Mechanism: an MCP stdio server declares `experimental['claude/channel']` and emits
     `notifications/claude/channel {content, meta}`; rendered as `<channel source=... meta...>` in context. [D]
   - Claude Code does NOT ack: "the await resolves when written to the transport, not when Claude has
     processed it"; silent drop if not registered/blocked. Receipt must be built via a reply tool. [D]
   - Events queue; "if several arrive while Claude is busy they are delivered together on the next turn". [D]
   - Requires claude.ai login or Console API key (docs now say both), not Bedrock/Vertex/Foundry. Team/Enterprise
     orgs: blocked until Owner enables `channelsEnabled`; `allowedChannelPlugins` can replace the Anthropic
     allowlist with an internal marketplace. Custom channels need `--dangerously-load-development-channels`
     (interactive only, full-screen confirmation dialog each start) until on an allowlist. Flags hidden from
     `--help`; "flag syntax and protocol contract may change". [D]
   - NEW RISK: on the v2 MCP client runtime (default >=2.1.232 w/ flags, >=2.1.274 otherwise) a channel server
     that negotiates MCP revision 2026-07-28 is NOT registered as a channel, because that revision cannot
     carry channel messages; workaround `MCP_PROTOCOL_NEGOTIATION=legacy` (also disables MCP tasks). [D]
     https://code.claude.com/docs/en/mcp.md
3. MCP latest spec = 2026-07-28 (final; RC 2026-05-21). Stateless: no initialize/session id; server-initiated
   elicitation/sampling/roots replaced by Multi Round-Trip Requests; change notifications via one
   `subscriptions/listen` stream; Tasks moved to extension `io.modelcontextprotocol/tasks` (poll `tasks/get`,
   new `tasks/update`); Roots/Sampling/Logging and legacy HTTP+SSE deprecated (>=12 months). [D]
   https://blog.modelcontextprotocol.io/posts/2026-07-28/
   Consequence: MCP is a fine request/response tool surface (inbox_read/ack/send) and a poor push path. [I]
4. Agent Teams (experimental, `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`): per-agent JSON mailbox
   `~/.claude/teams/{team}/inboxes/{agent}.json`, one team per session, no cross-session team, no resume of
   in-process teammates. Not a fit for 120 independent sessions. [D]
   https://code.claude.com/docs/en/agent-teams.md
   Docs themselves say: for separate sessions use cross-session messaging.
## 1. A2A (Agent2Agent)
- Version: 1.0.0 is latest on the official spec page (prior 0.1.0, 0.2.6, 0.3.0). [D]
  https://a2a-protocol.org/latest/specification/ . Secondary: v1.0 released 2026-03-12 [R]; one source
  claims v1.0.1 in May [R, unverified].
- Governance: Google announced 2025-04-09; Linux Foundation project 2025-06-23 [R]; accepted into Agentic AI
  Foundation (AAIF, home of MCP/goose/AGENTS.md) as Growth Stage project, Aug 2026 [R]
  https://aaif.io/blog/a2a-joins-aaif . Steering: Google, Microsoft, Cisco, AWS, Salesforce, ServiceNow,
  SAP, IBM, Bloomberg [R]. 150+ supporting orgs at 1-yr mark; prod in Azure AI Foundry, Bedrock AgentCore,
  Agentforce [R].
- Bindings: JSON-RPC, gRPC, HTTP+JSON/REST; `spec/a2a.proto` is normative; `A2A-Version` header;
  content type `application/a2a+json`. [D]
- Task states: active UNSPECIFIED/SUBMITTED/WORKING; terminal COMPLETED/FAILED/CANCELED/REJECTED;
  interrupted INPUT_REQUIRED/AUTH_REQUIRED. [D]
- Ops: SendMessage (blocking default, `returnImmediately`), SendStreamingMessage, SubscribeToTask,
  push notifications = agent POSTs StreamResponse to client-registered webhook (url, id, taskId, token,
  AuthenticationInfo), gated by `capabilities.pushNotifications`. [D]
- Multi-tenancy: optional opaque `tenant` routing field on requests. [D]
- Signed Agent Cards: JWS (RFC 7515) + JCS canonicalization (RFC 8785), `signatures[]`, spec section 8.4 [R].
  Adoption of signing is tiny: 91/6,019 crawled cards (1.5%), 52 verify; 65.5% of cards still say 0.3.x [R,
  single crawl 2026-09-01] https://agenstry.com/blog/state-of-the-agent-card
- Discovery: no central registry, no authorization framework in spec [R]; docs moved discovery toward "AI
  Catalog" registries Oct 2026 [R]. Google "ARD" (Agentic Resource Discovery) draft v0.9, 2026-06-17 [R].
- SDKs: Python, JS, Java, Go, .NET [R].
- Fit: A2A is agent-as-server RPC (client calls a remote agent endpoint, task-centric). The hub model is
  store-and-forward to agents that cannot listen and are only receptive between turns. Not a transport
  fit. Good SHAPE to borrow: Message/Part/Task, task-state vocabulary (esp. INPUT_REQUIRED/AUTH_REQUIRED),
  `contextId/taskId`, tenant field, JWS-signed card, push-webhook config. [I]
  Verdict: borrow shape; optionally expose a read-only A2A facade (Agent Card + tasks/get) later for
  external/client-company agents.
## 2. "ACP" disambiguation
(a) IBM BeeAI Agent Communication Protocol: merged into A2A under LF AI & Data, announced 2025-08-29;
    ACP team winds down, repo archived, BeeAI now uses A2A; IBM's Kate Blair joined A2A TSC. [D/R]
    https://lfaidata.foundation/communityblog/2025/08/29/acp-joins-forces-with-a2a-under-the-linux-foundations-lf-ai-data/
    Status: dead as a separate protocol. Ignore.
(b) AGNTCY (Cisco Outshift, donated to Linux Foundation 2025-07-29; 75+ members) [R]
    https://docs.agntcy.org/  https://github.com/agntcy
    - OASF (agent schema), Agent Directory (federated registry; Go), SLIM (secure low-latency
      messaging; Rust; pub/sub, streaming, MLS encryption; helm-slim-v2.2.0 2026-08-13), identity service.
    - Does not replace MCP/A2A; SLIM is a transport binding under A2A; OASF<->A2A card conversion. [R]
    - 2026: proposal (aaif/project-proposals #57) to move into AAIF; outcome not found. Directory is one of
      4 reference impls of ARD draft. [R]
    - Fit: agent<->agent discovery/secure messaging across orgs. MLS group encryption + directory is
      interesting for the "500 people / client companies" phase, but heavy and young for the hub. [I]
(c) Agent Client Protocol (Zed; editor<->agent JSON-RPC over stdio) [R]
    https://zed.dev/blog/claude-code-via-acp
    - Claude Code is not native; adapter wraps the Claude SDK: now `@agentclientprotocol/claude-agent-acp`
      (older `@zed-industries/claude-code-acp` stopped at 0.16.2); adapter 0.101.0 on 2026-11-05; supports
      /remote-control since Oct 2026 (PR #1197) [R, version numbers inconsistent across sources].
    - It is CLIENT<->AGENT (UI/editor driving one agent), not agent<->agent. Irrelevant to the hub except as a
      possible "human attach" surface. Do not use for inter-session messaging. [I]
## 3. NATS JetStream
- Durability: R1/R3/R5 streams, Raft, ack/redelivery, durable consumers, WorkQueue/Interest/Limits
  retention, msg dedupe by Nats-Msg-Id. [D docs.nats.io]
  CAVEAT: Jepsen on 2.12.1 found acknowledged-write loss (e.g. 14.1% lost in power-failure test; larger with
  bitflips/split-brain) [R third-party summary]; default fsync is every 2 min, `sync_interval: always`
  mitigates; four Jepsen-filed issues (#7549,#7556,#7564,#7567) still open on 2026-11-10; no independent
  re-test after 2.12.3 [R]. https://www.synadia.com/blog/jepsen-nats-2-12-1
  Plan: run `sync_interval: always`, R3 for company, treat hub DB as system of record. [I]
- Versions: stable 2.15.1 (2026-10-09), 2.14 (May 2026: cron-style scheduled messages, high-throughput
  publish) [R]. Scheduled messages could implement "deliver when idle / retry later". [I]
- Per-tenant accounts: each account = isolated subject space, users, perms, per-account JetStream limits
  (JS is opt-in per account); imports/exports for controlled cross-tenant sharing. [D]
  https://docs.nats.io/learn/security/accounts-and-multitenancy
- Auth callout (>=2.10): delegate auth to your service (OIDC/SSO); `allowed_accounts` 2.11+; patched
  timeout handling in 2.12.6. [D]  https://docs.nats.io/learn/security/auth-callout
- KV: built on streams `KV_<bucket>`, per-key TTL/watch -> usable for presence/heartbeat. [D]
  Caveat: presence via KV TTL is coarse; ephemeral presence better as core-NATS pub/sub. [I]
- Ops for solo: single static binary, ~tens of MB; solo does not need clustering, but then no HA and the
  durability caveat applies to a single node's fsync policy. A second moving part vs plain SQLite. [I]
- License/governance: Apache-2.0. Synadia tried to move server to BSL and pull NATS from CNCF (Apr 2025);
  settled 2025-05-01: NATS stays Apache-2.0 under CNCF, trademarks assigned to LF, domain+repos stay with
  CNCF; Synadia must rename any proprietary fork. [D/R]
  https://www.cncf.io/blog/2025/05/01/protecting-nats-and-the-integrity-of-open-source-cncfs-commitment-to-the-community/
  https://www.theregister.com/off-prem/2025/05/02/cncf-and-synadia-settle-nats-dispute/386288
  Residual single-vendor risk: medium (Synadia is the main contributor).
- Fit: strong for company (tenant isolation primitives are the best of the bunch). Overkill solo. Receipts,
  turn-aware gating, directory, authz are still hub logic in all cases. [I]
## 4. Redis Streams / Valkey streams
- Streams: consumer groups, XACK, PEL (pending entries), XAUTOCLAIM for redelivery; durable via AOF
  (`appendfsync everysec` default = up to ~1s loss; `always` = safer, slower); async replication, so
  failover can lose acked writes (WAIT mitigates). [D redis.io; I for guidance]
- Licensing: Redis 7.4 (Mar 2024) -> RSALv2/SSPLv1; Redis 8.0 GA 2025-05-01 adds AGPLv3 as third option
  (tri-license); Valkey (LF fork of 7.2.4) BSD-3-Clause. [R]
  https://acemq.com/guides/redis-licensing/  https://en.wikipedia.org/wiki/Valkey
- Versions: Valkey 9.1.3 (2026-10-26); Redis 8.10.3 (same day). Redis-only stream features: XDELEX/XACKDEL
  (8.2), XREADGROUP CLAIM (8.4), idempotent XADD (8.6), XNACK (8.8); stream RDB compat between the two
  is drifting. [R]
- Tenancy: ACLs on key prefixes + per-DB; no true account isolation. [I]
- Fit: consumer-group semantics map well to inbox + pending/ack. Streams have no per-message delayed
  delivery, no subject hierarchy. Another daemon for solo; weaker tenant isolation than NATS for company.
  If chosen: Valkey (BSD, AWS ElastiCache default [R]). Verdict: no clear win over SQLite (solo) or
  Postgres/NATS (company). [I]
## 5. Temporal and Restate
- Temporal: MIT (server + SDKs). [R] Signals/Updates/durable timers; "wait for turn boundary" = a workflow
  per task waiting on a signal. OpenAI Agents SDK integration GA 2026-03-23; plugins for Pydantic AI,
  Vercel AI SDK, LangGraph, Google ADK, Strands, Mastra mostly preview; sandbox integration Apr 2026. [R]
  https://temporal.io/blog/announcing-openai-agents-sdk-integration
  Self-host: server services + Postgres 12+ (advanced visibility in Postgres since 1.20, no Elasticsearch);
  ~2vCPU/4GB server + DB for small prod; cost driver = Postgres I/O from per-step event history; schema
  migration before upgrades; server 1.32.x current. [R]
  Fit: heavy for "send a text, deliver when idle". Interactive Claude sessions are not Temporal workers;
  you would model the hub's delivery state machine as workflows, which adds an engine for little gain. No
  Claude-Code-specific features found. [I]
- Restate: server is BSL 1.1 (production OK incl. internal platforms; forbids offering a "Public Restate
  Platform Service"; each release converts to Apache-2.0 after 4 years); SDKs MIT. [R]
  https://restate.dev/  Single binary with embedded state; awakeables (durable promises) + signals + durable
  timers; 1.7.x stable (1.7.13 on 2026-10-01), 1.8.0-rc in Oct 2026 [R]. Younger than Temporal [R].
  BSL + single vendor (Restate Inc.) = high single-vendor risk; fine for solo/internal, check the additional
  grant before any client-company hosting. [I]
- Verdict both: workflow engines solve retries/sagas/long waits, which the hub only partly needs. The
  awakeable pattern is a good mental model for "pending delivery until idle", but implementing it as a
  table + state machine is simpler. Skip for v1; revisit if orchestration (multi-step task graphs) lands. [I]
## 6. Solo options: SQLite queues, Postgres queues, Elixir
- SQLite as queue: claim via `BEGIN IMMEDIATE` + `UPDATE ... RETURNING`, claim_id as fencing token (litequeue,
  River SQLite, Oban Lite all use this pattern) [R]. WAL mode; single writer is fine at 120 sessions
  (msg rate is human-scale). [I]
- litequeue: tiny Python lib; author says little real-world scaling data [R]. Huey: Python task queue with
  SQLite backend (MIT) [R]. honker: Apache-2.0 SQLite extension with NOTIFY/LISTEN-like semantics, durable
  queues, streams, scheduler [R] https://github.com/russellromney/honker (young; check maturity).
  River: Go/Java/JS/Ruby/Rust queue on Postgres or SQLite, MPL-2.0 [R] https://github.com/riverqueue/river
- pgmq (Tembo, Postgres ext; visibility-timeout semantics), PgQue (Apache-2.0, zero-bloat) [R].
  Postgres queues are the natural company step: same SQL schema, row-level security for tenants,
  LISTEN/NOTIFY for wakeups, `FOR UPDATE SKIP LOCKED`. [I]
- Oban (Elixir): core Apache-2.0, backed by Postgres/SQLite/MySQL; Oban Web now OSS (Apache-2.0, v2.11+); Pro
  is paid ($150/mo; durable workflows, rate limits). No license change found. [R] https://github.com/oban-bg/oban
  Phoenix PubSub: in-memory, at-most-once, NOT durable; fine only for waking an online long-poll. [I from
  known design; not re-verified]
- Verdict: hand-rolled hub tables (messages, deliveries, receipts) on SQLite now, behind a storage interface
  so Postgres swap is mechanical. A generic queue lib doesn't model turn-aware delivery or receipts anyway.
  [I]
## 7. MCP as the agent-facing channel
- See headline #3. Tools (`inbox_peek`, `inbox_ack`, `send`, `whois`) work on both stdio and Streamable HTTP;
  stateless spec means the hub MCP server can be horizontally scaled without session affinity. [D/I]
- Elicitation: 2025-06-18 introduced, 2025-11-25 added URL-mode elicitation + experimental tasks; in
  2026-07-28 server-initiated elicitation is replaced by MRTR (`input_required` result then client retry). [R/D]
- Tasks extension is relevant as the shape for "long-running work with state", AWS-contributed [D]; Claude
  Code uses MCP tasks only on revision 2026-07-28 connections. [D]
- No standard MCP "agent inbox" or push-to-model primitive. Push only exists via Claude Code's own
  `claude/channel` extension. [D/I]
- Pin: Claude Code `MCP_PROTOCOL_NEGOTIATION=legacy` only matters if using channels.
## 8. Claude Code first-party options (summary table)
| Feature | Status | Turn-aware | Receipt | Needs | Verdict for hub |
|---|---|---|---|---|---|
| Cross-session messaging (socket) | GA-ish, on by default >=2.1.224 [D] | yes, between tool calls / new turn if idle | sent only; notify_when_idle | local socket; no login for same-machine | best last hop, spike wire format |
| Channels | research preview [D] | queued, grouped next turn | none (build reply tool) | claude.ai/Console auth, allowlist or dev flag, org enable | fragile; optional later |
| Agent Teams | experimental [D] | yes | write-to-file ok | env flag, 1 team/session | not for fleet |
| SendMessage/ListAgents tools | with messaging [D] | yes | n/a | permission rules can deny | agents already know these |
| Hooks (UserPromptSubmit, Stop, TeammateIdle...) | GA [D] | event-based | custom | local | use for turn-boundary signals + pull |
Notes:
- Messages carry "from another Claude session, not you": cannot approve permissions or change CLAUDE.md. Good
  security property, but hub-originated instructions carry no more authority. [D]
- Isolation: socket in `/tmp/cc-socks-<uid>`; sandboxed Bash needs `sandbox.network.allowUnixSockets`. [D]
- Containers: host<->container sockets unreachable; k8s pods need hub to run in-pod sidecar or use HTTP pull
  through MCP tools. [D/I]
## 9. Off-the-shelf "inbox for coding agents"
- MCP Agent Mail (Dicklesworthstone): FastMCP HTTP server; identities, inbox/outbox, threads, FTS5, advisory
  file leases; Git = canonical, SQLite = index. Python original now marked DEPRECATED in favor of
  `mcp_agent_mail_rust`. [R] https://github.com/Dicklesworthstone/mcp_agent_mail
  Single-maintainer, pull-based (agents must poll), no turn-aware push. Maturity 2, borrow ideas (file leases,
  contact policy).
- agent-letterbox-tmux (SimonMallas): durable letters + terminal bell for tmux agents. [R]
  https://github.com/SimonMallas/agent-letterbox-tmux
- tmux-message-bus (Nikiforov, Jun 2026): durable, ordered, at-least-once bus; Claude Code hooks + shared
  SQLite. [R] https://nikiforovall.blog/ai/2026/06/27/tmux-message-bus.html  Closest prior art to this plan.
- cross-agent-teams / xats: local MCP daemon, shared mailbox + wake channel (Claude Code channel push,
  tmux paste for Codex/opencode/cursor). [R] https://jtianling.com/en/cross-agent-teams-release.html
- Cyclops (tmux durable mailbox, FIFO per recipient), Octobots (SQLite "taskbox" queue), CAO, dmux. [R]
  https://github.com/andyrewlee/awesome-agent-orchestrators
- Letta: server-side agent-to-agent tools described as deprecated (Mar 2026; brittle, loops); moving to
  subagents + CLI messaging with `--from-agent`; Conversations API (Jan 2026). [R]
  https://www.letta.com/blog/our-next-phase/  Not a fit (memory-first harness, own runtime).
- AutoGen: maintenance mode since 2025-10-02; Microsoft Agent Framework 1.0 GA 2026-04-03 (.NET/Python;
  Go preview). In-process framework, not a bus for external CLI agents. AG2 = community fork (Apache-2.0). [R]
- Takeaway: everyone converges on SQLite/Git mailbox + hook/tmux/channel nudge. None provide signed
  envelopes, scoped directory, or message-level authz; none are multi-tenant. Build, don't buy. [I]
## 10. Scored table
Maturity 1-5; self-host y/n; vendor risk L/M/H; fit solo / fit 500 (1-5).
| Option | Mat | Self-host | License | Vendor risk | Solo | 500 | One-line verdict |
|---|---|---|---|---|---|---|---|
| A2A 1.0 (wire) | 3 | y | Apache-2.0 [R] | L (LF/AAIF) | 1 | 3 | RPC to listening agents; borrow shapes, don't carry |
| A2A (shape/JWS cards) | 4 | y | Apache-2.0 | L | 4 | 4 | Reuse Message/Task/state vocab + JWS/JCS signing |
| BeeAI ACP | 1 | n/a | Apache-2.0 | n/a | 0 | 0 | Merged into A2A, archived |
| AGNTCY (OASF/Dir/SLIM) | 2 | y | Apache-2.0 [R] | M (Cisco-led) | 1 | 3 | Watch for cross-org directory/MLS; too heavy now |
| Agent Client Protocol (Zed) | 3 | y | Apache-2.0 | M | 1 | 1 | Editor<->agent, wrong layer |
| NATS JetStream | 4 | y | Apache-2.0 | M | 3 | 5 | Best tenancy primitives; set sync_interval always, keep DB as record |
| Redis/Valkey Streams | 4 | y | Valkey BSD / Redis tri-lic | L (Valkey) | 2 | 3 | Works, no edge over NATS/Postgres here |
| Temporal | 5 | y | MIT | L | 1 | 3 | Right tool for workflows, wrong weight for inbox |
| Restate | 3 | y | BSL 1.1 + MIT SDKs | H | 2 | 2 | Nice awakeables; BSL + young |
| SQLite tables (hand-rolled) | 5 | y | public domain | L | 5 | 1 | Solo default |
| litequeue/Huey/honker | 2-3 | y | MIT/Apache | M (solo maintainers) | 3 | 1 | Don't need; model is not a job queue |
| Postgres (pgmq/River/SKIP LOCKED) | 4 | y | PG / MPL-2.0 / Apache | L | 3 | 5 | Natural company-phase store; RLS tenants |
| Oban / Phoenix PubSub | 5/5 | y | Apache-2.0 | L | 2 | 3 | Only if hub is Elixir; PubSub not durable |
| MCP tools (2026-07-28) | 4 | y | MIT/spec | L (AAIF) | 5 | 5 | Agent-facing request/response surface |
| MCP push | 1 | - | - | - | 0 | 0 | No standard push; only claude/channel ext. |
| CC cross-session messaging | 3 | y (local) | proprietary | H (Anthropic) | 5 | 3 | Best last hop; protocol undocumented, spike |
| CC Channels | 2 | y | proprietary | H | 2 | 2 | Preview, allowlist, no ack, v2 runtime break |
| CC Agent Teams | 2 | y | proprietary | H | 1 | 1 | One team per session; not a fleet bus |
| MCP Agent Mail | 2 | y | MIT [R] | H (1 maintainer, deprecated Py) | 2 | 1 | Mine for ideas only |
| tmux-message-bus / xats / letterbox | 1-2 | y | OSS | H | 3 | 1 | Prior art validating design |
| Letta / AutoGen / MAF | 3-4 | y | Apache/MIT | M | 1 | 1 | In-process frameworks, wrong fit |
## 11. Recommended picks per sub-layer
(a) Wire/envelope shape: A2A-derived JSON (Message with Parts, contextId/taskId, tenant, task-state
    vocabulary incl. INPUT_REQUIRED) in a hub-defined envelope, signed with JWS (RFC 7515) over JCS
    (RFC 8785), the same recipe A2A uses for Agent Cards. Receipt states: queued -> delivered (hub wrote to
    last hop) -> consumed (agent turn read it). Keep optional A2A facade for external parties. Confidence:
    medium-high. [I]
(b) Durable transport, solo: keep SQLite (WAL) + HTTP long poll, tables behind a storage interface.
    No NATS yet. Confidence: high. [I]
(c) Durable transport, company: hub stays system of record in Postgres (SKIP LOCKED, RLS per tenant,
    LISTEN/NOTIFY wake). Add NATS JetStream per-tenant accounts only when you need fan-out, leaf nodes, or
    cross-region; if so set `sync_interval: always`, R3, and don't treat it as the only copy (Jepsen
    findings open as of 2026-11). This is a softer stance than the blueprint's "NATS per tenant";
    justification is durability caveat + you must build receipts/authz on top regardless. Confidence: medium. [I]
(d) Agent-facing interface: MCP server (stdio and Streamable HTTP, built to 2026-07-28 but tolerant of
    older revisions) exposing send/peek/ack/whois tools; hub identity from per-session token. Pull is the
    contract; push is only a nudge. Confidence: high. [I]
(e) Last hop into live Claude Code session: layered.
    1. Primary: Claude Code native cross-session inbox socket (turn-aware by construction; no allowlist/login
       for same machine; works on Bedrock/Vertex). Spike needed: wire format for non-child posters,
       `crossSessionInbound: accept` rollout via managed/user settings, held-message behavior for sessions in
       bypass mode, and whether a hook fires on arrival (for the consumed receipt).
    2. Receipt path: hooks (UserPromptSubmit/PostToolUse/Stop) call hub `ack` when the agent reads the hub
       inbox via MCP tool; `notify_when_idle` for idle edges. [I]
    3. Fallback: tmux `send-keys`/paste guarded by idle detection (Stop hook marks idle), for sessions without
       messaging (older version, containers, bare mode).
    4. Channels: skip for now; revisit when out of preview and pinned off the v2-runtime handshake issue.
       For company, `allowedChannelPlugins` with an internal marketplace is the clean path if it GAs.
    Confidence: medium (undocumented socket protocol is the main unknown).
## 12. Open questions / what I could not verify
- Cross-session socket line protocol beyond auth line (not in docs).
- Whether Channels/messaging behave identically in containers/k8s pods (docs say host<->container sockets
  don't connect; same-container works).
- A2A 1.0.1 claim, AGNTCY->AAIF transfer outcome, exact SLIM/Directory maturity.
- Redis AOF/consumer-group claims and Phoenix PubSub semantics are from prior knowledge, not re-fetched.
- Jepsen figures are from a third-party summary; primary report at synadia.com/blog/jepsen-nats-2-12-1
  was not read in full.
- Search result summaries are secondary; primary pages fetched: Claude Code channels, channels-reference,
  mcp.md (sections), agent-teams, cross-session-messaging; MCP 2026-07-28 blog; A2A spec (first 100k chars
  only, Agent Card/JWS sections not read).
