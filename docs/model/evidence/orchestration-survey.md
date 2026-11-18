[Index](../../README.md) › [Unified model](../../unified-model.md) › [Orchestration](../orchestration.md) › orchestration-survey

# Orchestration survey: outside tools vs CC Flow / cc-tree (as of 2026-11-18)

Method: web search plus page fetches on 2026-11-18, no installs. Tags: **[D]** documented (vendor docs or
repo page), **[R]** reported (third-party article), **[I]** inferred (my reasoning).
Limits: WebFetch summaries of GitHub pages often omitted release numbers or dates, so versions marked
"unknown" are unknown. Many 2026 sources are SEO blog roundups, so I treat them as weak evidence.

## 0. CC Flow constraints used as the yardstick

1. Interactive TUI sessions, one per task node, in tmux. tmux owns lifecycle.
2. Runs on a Claude subscription, not the Agent SDK or API.
3. Human-only decision queue (the ❓ queue); consent must come from a human.
4. Manager/worker hierarchy with briefs, Outcomes and Accept criteria, stored as Markdown in git.
5. Offline-capable start; no secrets inside the sandbox.
6. Tiers from solo to 500 people.

**Billing context [R], important for constraint 2.** On 2026-05-13 Anthropic announced that Agent SDK,
`claude -p` and third-party tools using subscription auth would move to a separate API-priced credit pool
on June 15. Anthropic then reversed this on June 15 and left subscription limits unchanged. Anthropic's
legal doc also bars using Free/Pro/Max OAuth tokens in third-party products, and the Help Center advises
shared production automation to use API billing.
Source: https://gist.github.com/MagnaCapax/d9177e35b355853f03c730dfcaa693ef,
https://www.digitalapplied.com/blog/anthropic-claude-credit-overhaul-june-15-2026,
https://alternativeto.net/news/2026/2/anthropic-officially-bans-using-subscription-authentication-for-third-party-claude-use.
I could not confirm the current third-party-harness policy. **Policy risk is the main reason to prefer
driving the real `claude` TUI.** [I]

## 1. Session managers and terminal multiplexers (closest to CC Flow)

### 1.1 agtx (fynnfluegge/agtx)
- What: Rust TUI kanban board that runs agents in tmux windows with one git worktree per task. [D]
- Version/status: about 1.7k stars, 130 commits, Apache-2.0. Releases section empty in capture and the only
  version string seen is an example badge `0.2.8`, so current version **unknown**. [D]
- Self-host: local only; SQLite state in `~/.config/agtx/`, per-project `.agtx/`. No server. [D]
- Drives: **interactive CLIs** via tmux paste (Claude, Codex, Grok, Cursor, OpenCode, Antigravity, Gemini,
  Copilot, pi). Multi-harness; a different agent can be assigned per phase. [D]
- Human gates: phase gates via plugins; `auto_trust=false` by default so trust prompts go to the human; the
  orchestrator advances only to Review ("Merging is your call") and escalates stuck tasks with a badge. [D]
  There is no separate decision queue. [I]
- Durable state: SQLite plus artifact files, not Markdown in git, and `.agtx/` is gitignored. [D]
- Vendor risk: single-maintainer OSS, low lock-in, bus-factor risk. [I]
- Solo: strong fit. 500: poor (local single-user, no RBAC or audit). [I]
- Borrow: `agtx mcp-serve` (board exposed to agents over MCP), artifact-file phase completion, 15 s idle
  detection, paired-device mobile approval queue (single-use codes, queued actions).
- URL: https://github.com/fynnfluegge/agtx

### 1.2 Agent of Empires (AoE)
- What: Rust session manager for coding agents; tmux session per agent, TUI + web UI + CLI + HTTP API,
  optional Docker/Podman/Apple container isolation, worktrees, hooks, status detection, notifications. [D]
- Backed by Mozilla.ai; about 4.7k stars; **MIT**; 3.3k commits. Version unknown (Releases empty in capture). [D]
- Agents: Claude Code, OpenCode, Mistral Vibe, Codex, Gemini, Pi, Copilot CLI, Factory Droid. [D]
- Drives interactive CLIs. Sessions survive TUI or SSH loss because tmux is the lifecycle owner, **the same
  principle as CC Flow**. [D]
- Human gates: none described in the page I read. [D] Durable state: location not described. [D]
- Solo: good. 500: weak (no multi-user governance seen). [I]
- Borrow: status detection, HTTP API plus web UI for phone viewing, sandbox profiles.
- URL: https://github.com/agent-of-empires/agent-of-empires

### 1.3 CLIshake (github.com/Clishakehq)
- What: Go single binary, "terminal coordination layer"; a human lead directs Claude, Codex, OpenCode,
  Copilot CLI and Antigravity in one tmux-backed session with attributed messages (`@name/@team/@role`), a
  shared task board, approval gates and an append-only audit log. [D]
- Status: **very early**. 3 stars, 0 issues/PRs, org has 1 follower and 3 repos, MIT, last update 2026-07-13.
  No version found. [D] Approval-gate mechanics are not documented on the page I saw. [D]
- Local-first, no server or telemetry. [D]
- Fit: closest in vocabulary (adapter contract, approval gates, audit log) but unproven. Treat as a design
  reference, not a dependency. [I]
- URL: https://github.com/Clishakehq

### 1.4 Newcomers and incumbents (all [R] unless noted)
| Tool | Status 2026-11 | License | Note |
|---|---|---|---|
| Claude Squad (smtg-ai) | alive but quiet; about 8.8k stars [D]; last release reported Aug 2026 [R] | AGPL-3.0 + CLA [D] | tmux + worktrees; Claude/Codex/Gemini/Aider/OpenCode/Amp. AGPL is a problem for embedding. |
| Vibe Kanban (BloopAI) | **sunsetting**: company shut down 2026-04-10, repo community-maintained [R]; banner confirmed [D] | Apache-2.0 per [R]; page names none | about 28.7k stars; local `npx`; kanban UI idea is the best-known. Hosted pieces gone. |
| Crystal (Stravu) | **deprecated** 2026-02-26, successor Nimbalyst [R] | n/a | Do not adopt. |
| Conductor | **joining Meta**; cloud shut down 2026-11-20 [R]; local Mac app future unknown | proprietary | Do not adopt. |
| Sculptor (Imbue) | early research preview, Mac + Linux [R] | unknown | Little data. |
| Nimbalyst | Crystal's successor, "visual workspace" [R] | OSS per [R] | Not evaluated in depth. |
| uzi | no current information found | - | Not evaluated. |
Sources: https://www.plyloft.com/blog/ai-coding-agent-orchestrators,
https://www.augmentcode.com/tools/open-source-agent-orchestrators, https://www.vibekanban.com/,
https://github.com/smtg-ai/claude-squad.
Pattern: **the commercial wrappers (Vibe Kanban, Crystal, Conductor) died or were acqui-hired within 12
months.** Single-vendor risk is real in this layer. [I]

## 2. Claude Code native features

### 2.1 Agent view / `claude --bg` / `claude agents`
- Status: **research preview**; UI "may change". [D] Local only (no cloud). [D]
- Mechanics: a **supervisor daemon** hosts each session's PTY itself and is **not built on tmux**. It
  restarts crashed sessions, stops idle unattached ones after about 1 h (resumable from disk), and pins
  with Ctrl+T. State is in `~/.claude/daemon/roster.json` and `~/.claude/jobs/<id>/` and is not a stable
  interface; `claude agents --json` is the supported read path. [D]
- Needs-input handling: yellow state covers permission prompts, questions, MCP elicitations; hooks
  `agent_needs_input` and `agent_completed`; peek can answer questions but **not permission dialogs**. [D]
- Dispatched sessions go to `.claude/worktrees/`; they commit and push and open draft PRs, never merge. [D]
- Uses subscription quota; ten sessions burn it about 10x. [D] Claude only. License: proprietary.
- Fit: it **conflicts with "tmux owns lifecycle"** (the daemon owns the PTY). [I] Useful as a read-only
  status feed (`claude agents --json`, `Notification` hook types) into the ❓ queue. [I]
- URL: https://code.claude.com/docs/en/agent-view

### 2.2 Subagents
Same-session helpers that return a result to the caller. [D] No durable state across sessions, Claude only.
Fits "inside one worker" (research, review), not CC Flow's session-per-task model. [I]
URL: https://code.claude.com/docs/en/sub-agents.md

### 2.3 Agent teams (experimental, off by default)
- `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`. Lead + teammates, mailbox JSON under
  `~/.claude/teams/{team}/inboxes/`, shared task list in `~/.claude/tasks/`, split panes in tmux or
  iTerm2. [D]
- Known limits [D]: no resume of in-process teammates, one team per session, no nested teams, lead fixed
  for life, task status lags, teammates cannot give consent for another agent.
- **Matches CC Flow's consent rule:** a teammate "can't approve a permission prompt or supply consent on
  your behalf" and relayed approvals are untrusted. [D] Teammate permission prompts surface in the lead,
  which can be a bottleneck. [D]
- Fit: validates the manager/worker shape but is session-scoped and ephemeral, so no git-backed tree. [I]
  Borrow: `TeammateIdle` / `TaskCreated` / `TaskCompleted` hooks as Accept-criteria enforcement points.
- URL: https://code.claude.com/docs/en/agent-teams

### 2.4 Claude Code on the web and scheduled tasks (formerly Routines), cloud sessions
- Cloud VM sessions on Anthropic's infrastructure for Pro/Max/Ultra/Team/Enterprise seats; **no cloud
  sessions with a Console API key or Bedrock/Vertex**. [R]
  Routines launched 2026-04-14 in research preview, trigger by schedule, HTTP or GitHub event; reportedly
  renamed "scheduled tasks" on 2026-11-01 and the per-day cap reportedly dropped 2026-10-31. [R, conflicting]
- Same subscription, so it fits constraint 2 for an extra host tier. [I]
- URLs: https://claude.com/blog/introducing-routines-in-claude-code,
  https://wmedia.es/en/tips/claude-code-routines-cloud-agents

## 3. Agent frameworks and durable engines

### 3.1 Temporal
- Server: **MIT**, self-hostable (needs Postgres/MySQL/Cassandra plus ops work). [R]
  Cloud pricing is reportedly pay-as-you-go from about $50 per million actions since Sept 2026, Business
  from $500/mo (sources conflict). [R]
- AI: OpenAI Agents SDK integration GA (2026-03-23) [R]; reportedly a broader GA of integrations
  (Google ADK, LangGraph, Vercel AI SDK, etc.) on 2026-10-30 (single third-party source) [R]; "Temporal
  Agent Harness" early look 2026-08-20, which says human approval waits are durable. [D blog]
  Human-in-the-loop = `workflow.wait_condition()` + signal/update, which survives worker restarts. [R]
- Drives headless agents or APIs by default. Driving a **tmux-hosted TUI** would be a custom activity
  (send-keys, heartbeat, wait for Outcome file). [I] That is workable but nothing off the shelf does it.
- Multi-harness: harness-agnostic since it just runs code. [I]
- Vendor risk: low (MIT server, large community). Maturity: high.
- Solo: overkill (cluster to run). 500: strong for unattended, scheduled, retried multi-step jobs. [I]
- Borrow: signals/updates as the model for ❓ answers; workflow history as the audit trail; heartbeat
  timeouts for stuck sessions.
- URLs: https://temporal.io/blog/temporal-agent-harness-durable-agent-infrastructure,
  https://temporal.io/blog/announcing-openai-agents-sdk-integration,
  https://github.com/temporalio/ai-integrations,
  https://www.activepieces.com/blog/temporal-self-hosted-setup-and-costs-2026-guide

### 3.2 LangGraph (+ Platform, now "LangSmith Deployment")
- Library **MIT**; 1.0 Oct 2025, 1.1 2026-03-10, 1.2 2026-05-12, PyPI 1.2.19 as of 2026-10-30. [R]
- HITL: `interrupt()` + checkpointer + `Command(resume=...)`; durable via Postgres checkpointer. [R]
- Platform renamed LangSmith Deployment in Oct 2025; the self-hosted Agent Server is reportedly under
  Elastic-2.0 and needs an Enterprise plan and licence key. [R] I could not confirm whether a free
  self-hosted tier still exists.
- Drives its own LLM-calling graphs (API-based), not TUIs. [I] Pulling Claude through the API breaks the
  subscription rule. [I]
- Fit: low. Borrow the interrupt/resume checkpoint idiom (already close to CC Flow checkpoints).
- URLs: https://cadenya.com/other-content/langgraph-in-production,
  https://www.developersdigest.tech/blog/langchain-langgraph-whats-new-2026

### 3.3 OpenAI Agents SDK (handoffs)
- Python 0.23.1 (2026-10-02) and TS 0.20.0 (2026-10-08), still < 1.0; MIT; library only; sandbox agents
  since April 2026. [R] Handoffs are LLM-decided delegation inside one process. [R]
- Built for OpenAI-API agents. Fit with a Claude-TUI orchestrator is poor. [I]
- Source: https://www.morphllm.com/ai-agent-framework

### 3.4 CrewAI
- 1.x; changelog reached 1.15.41 on 2026-11-13; MIT framework; AMP control plane (hosted, or in your
  cloud on Enterprise) with SSO/RBAC/HITL gates. [R] Role-playing crews and Flows over LLM APIs. [I]
- Fit: low (own agents, not TUIs). Borrow nothing structural beyond Flows' human-input step.
- URL: https://geekflare.com/open-source/crewai/

### 3.5 Dagger (container-use, Dagger agents)
- container-use: about 4k stars, last tag v0.4.2 (2025-08-19), "experimental", activity slowed, an open
  May 2026 request to archive it. [R] Dagger itself v0.21.6 (2026-06-10) with v1.0 betas in Aug 2026;
  LLM primitive still experimental. [R]
- Value: containerised, branch-per-agent environments over MCP, which is a sandbox primitive. It does not
  orchestrate TUI sessions. [I]
- Fit: possible sandbox backend for workers; do not depend on container-use. Borrow the
  "environment = container + git branch" idea.
- URLs: https://github.com/dagger/container-use, https://dagger.io/blog/llm/

## 4. Hosted and enterprise agent platforms

### 4.1 GitHub Agent HQ / Copilot coding agent (mission control)
- Public preview Feb 2026; Claude, Codex and Copilot agents assigned from github.com/copilot/agents,
  mobile or VS Code; `@Claude`/`@Codex` in PR comments; Control Plane gives allowlists, audit logs,
  sandboxing; Pro+ and Enterprise. [R] Google, Cognition and xAI agents reportedly joining. [R]
- Hosted, **not self-hostable**, proprietary, GitHub/Microsoft lock-in. Own headless agents, not your TUIs. [I]
  Approval is the PR review gate. [R]
- 500-tier: realistic for client companies already on GitHub Enterprise. Solo: Pro+ cost, possible.
  Borrow: PR-as-consent, agent allowlist, audit-log schema.
- URLs: https://thenewstack.io/github-agent-hq/, https://bex.co/blog/2026/07/28/github-agent-hq-multi-vendor-orchestrator

### 4.2 OpenHands
- Core (SDK, CLI, GUI, agent server) **MIT**; `enterprise/` dir source-available with a commercial licence
  (one-month trial). Enterprise 0.105.0 on 2026-11-12 (enterprise-server 1.92.0, agent-server 1.62.0). [R/D release notes]
  Licence enforcement ships off in that release. [D]
- **Self-hostable**: agent server on Docker/K8s via REST + WebSocket; Enterprise adds VPC/on-prem, SSO,
  RBAC; Super Admin and in-UI IdP are beta. The old Local GUI is deprecated. [R]
- Supports Claude via ACP models [D release notes], but its own agent loop is headless. [I]
- Fit: best open candidate for a **sovereign "host tier" at 500 people**, but its model (one agent
  conversation per sandbox) differs from CC Flow's tree. Cost: API billing. [I]
- URLs: https://docs.openhands.dev/enterprise/release-notes/0.105.0,
  https://docs.openhands.dev/enterprise/release-notes

### 4.3 Devin, Factory, Cursor background agents, Codex cloud, Jules (all [R])
| Product | Self-host | Notes |
|---|---|---|
| Devin (Cognition) | Enterprise VPC/dedicated (sources conflict) | Teams $80 base + $40/seat; closed |
| Factory Droids | Enterprise private/on-prem listed | Droid also runs as a CLI that AoE supports [D] |
| Cursor cloud agents | self-hosted workers only; control plane in Cursor cloud | Firecracker microVMs |
| Codex cloud | none; OpenAI-managed sandboxes | token credits since Apr 2026; CLI is open source |
| Jules (Google) | none | plan-review step before execution |
All proprietary, each with own agents. They are interchangeable "send task, receive PR" backends. [I]
Sources: https://securityboulevard.com/2026/06/top-9-ai-se-agent-platforms-in-2026/,
https://macroscope.com/content/best-cloud-ai-coding-agents-2026,
https://northflank.com/blog/top-devin-ai-alternatives-for-autonomous-coding. Verify vendor claims
directly before relying on them.

## 5. Multi-harness adapter protocol: ACP (Agent Client Protocol)
- Zed's protocol, adopted by JetBrains; ACP Agent Registry launched 2026-01-28; Zed 1.0 on 2026-04-29;
  SDKs in Python, TypeScript (2.2.0), Rust, Kotlin; ACP v2 in Nov 2026. [R]
- Adapters: `@agentclientprotocol/claude-agent-acp` (registry entry 0.106.0 on 2026-11-13) and
  `@agentclientprotocol/codex-acp` 2.5.0 (2026-10-29), which drives the Codex App Server. [R]
  Claude Code has no native ACP, so the adapter wraps the **Agent SDK**. [R]
- **Two problems for CC Flow** [R/I]: (1) it does not drive the interactive TUI (it is headless), so
  tmux-as-lifecycle and "the human can attach to the pane" are lost; (2) reported to bill against the Agent
  SDK credit pool, which is exactly the subscription risk in section 0.
- Its permission-request messages are a good model for the ❓ queue wire format. [I]
- URLs: https://zed.dev/blog/acp-progress-report, https://www.morphllm.com/agent-client-protocol,
  https://leanzero.net/blog/claude-code-in-zed-vs-the-terminal-what-the-acp-wire-actually-carries

## 6. Scored shortlist (1-5; vendor risk: 5 = low risk; fit = to CC Flow's model)
Scores are my judgement from the evidence above [I].

| Candidate | Maturity | Self-host | License | Vendor risk | Fit | Notes |
|---|---|---|---|---|---|---|
| Temporal | 5 | 4 | 5 (MIT) | 4 | 3 (4 as layer a) | needs custom TUI activity |
| agtx | 3 | 5 | 5 (Apache) | 3 | 4 | closest design, SQLite not git |
| Agent of Empires | 3 | 5 | 5 (MIT) | 4 | 3 | tmux-first, no gates seen |
| CLIshake | 1 | 5 | 5 (MIT) | 2 | 3 | right vocabulary, 3 stars |
| Claude Squad | 4 | 5 | 2 (AGPL) | 3 | 3 | no gates, AGPL |
| Claude agent view | 3 | 3 | 1 | 1 | 2 | daemon owns PTY |
| Claude agent teams | 2 | 3 | 1 | 1 | 3 | ephemeral, experimental |
| ACP adapters | 4 | 5 | 4 (not verified) | 4 | 2 | headless, SDK billing |
| OpenHands | 4 | 4 | 3 (MIT + commercial ent.) | 4 | 2 | own agent loop |
| LangGraph | 4 | 2 (server Elastic) | 4 | 3 | 2 | API agents |
| OpenAI Agents SDK | 3 | 5 | 5 | 2 | 1 | OpenAI-API centric |
| CrewAI | 4 | 3 | 4 | 3 | 1 | API agents |
| Dagger / container-use | 2 | 5 | 5 | 3 | 2 | stalled |
| GitHub Agent HQ | 3 | 1 | 1 | 1 | 2 | 500-tier buy |
| Devin/Factory/Cursor/Codex/Jules | 4 | 1-2 | 1 | 1 | 1 | interchangeable PR-producers |
| Vibe Kanban / Crystal / Conductor | n/a | n/a | n/a | 1 | n/a | dead or dying |

## 7. Recommendation per sub-layer

**(a) Durable workflow engine for unattended/scheduled multi-step jobs: Temporal, but only at the
team/enterprise tier.** [I] Reasons: MIT, self-hostable, `wait_condition` + signals map directly onto
"wait for a human ❓ answer", heartbeats map onto stuck-session detection. Solo tier: keep systemd timers,
tschedule and cron, since running a Temporal cluster costs more than the problem is worth. Design the
job spec so the same Markdown job can be executed by either. No existing Temporal integration drives
tmux TUIs, so you must write that activity. LangGraph/CrewAI/OpenAI SDK are rejected because they want
their own API-billed agent loop.

**(b) Session manager / multiplexer: build (keep tmux + cc-tree); borrow from agtx and AoE.** [I] No
tool combines tmux ownership, Markdown-in-git tree state, manager/worker briefs and a human-only decision
queue. Do not adopt Claude Squad (AGPL, no gates) or agent view (daemon-owned PTY, local-only,
preview). AoE (MIT, tmux, web/HTTP, containers) is the best optional viewer or phone dashboard; it is
the only candidate here that shares the "tmux owns the session" principle. Use `claude agents --json` and
the `agent_needs_input` hook as signal sources, not as the manager.

**(c) Task/kanban/decision UI: build a thin one over the Markdown tree.** [I] agtx is the best reference
(board over MCP, phase gates, queued mobile approvals). Vibe Kanban's open-source remnant is a UI
reference only; its company is gone. Nothing offers a ❓ queue where consent is human-only except agent
teams' rule, which is session-scoped.

**(d) Multi-harness adapter protocol: do not adopt ACP as the runtime layer; define a thin tmux adapter
contract (spawn, send brief, detect idle/needs-input, read Outcome file), and use ACP's schema as a
vocabulary reference.** [I] ACP bypasses the TUI and, for Claude, rides the Agent SDK (billing risk).
Revisit an ACP adapter for Codex/OpenCode later if you want headless workers. CLIshake's "adapter contract
as config" and agtx's per-agent plugin files show the TUI-adapter pattern already works.

**(e) Hosted cloud agent as an extra host tier: buy, two picks.** [I]
- Solo / Claude-first: **Claude Code on the web + scheduled tasks**. It uses the same subscription, so
  no new billing path. Treat it as a remote worker host whose result is a branch or PR that a manager
  checks against Accept. Caveats: no offline start, no API-key mode, research-preview status.
- 500-person client companies already on GitHub: **GitHub Agent HQ** (multi-vendor, audit log, allowlists,
  PR review as the consent gate). For sovereignty or on-prem demands: **OpenHands Enterprise**
  (self-hosted, commercial licence, API billing).
- Devin/Factory/Cursor/Codex/Jules: optional per-client choice; wrap as "task in, PR out" adapters only.
  Build nothing for this layer except the adapter and the intake of results.

## 8. What nothing fits (so it stays built)
1. Human-only consent queue spanning many sessions, with ❓ decisions stored as Markdown in git. [I]
2. Tree of trees/milestones/tasks as the state of record in git (agtx uses SQLite; agent view and
   teams use opaque `~/.claude` JSON). [D]
3. Subscription-driven interactive TUI control **with** durable scheduling. Everything durable (Temporal,
   LangGraph, OpenHands) assumes API-billed headless agents. [I]
4. Offline local start with a gateway-injected secret model. Not addressed by any source found. [I]

## 9. Gaps in this survey
- No versions found for agtx, AoE or CLIshake (release pages empty in fetches).
- CLIshake approval-gate mechanics and uzi: no data.
- Claude Code scheduled-task rename and cap removal rest on third-party posts; verify on Anthropic docs.
- Current Anthropic position on subscription use by third-party harnesses after the June 15 reversal:
  unconfirmed.
- LangSmith self-hosted free tier, Devin VPC availability: conflicting sources.
- Agent view doc was read only through character 100,000 of 146,212.
