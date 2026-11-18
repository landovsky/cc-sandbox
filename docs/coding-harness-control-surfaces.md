# Coding harness control surfaces

Nov 18, 2026 · @Tomáš Landovský

## Intent

Goal: an app that manages running instances of several coding harnesses — sees what each one is doing (messages, tool calls, state) and can send messages into it. Context is CC Tree: a project → milestone → task tree where tasks own agent sessions.

The research question: how can each harness be driven programmatically, both when the manager spawns it and when a human already has it open in a terminal?

Constraints carried in from CC Tree:

- Interactive sessions live in a persistent host (tmux) and are never terminated/resumed, so background processes survive.
- Runs on a Claude subscription — cannot rely on the Claude Agent SDK.
- Must work offline: if the backend is down, a machine can still create tasks and start sessions locally.
- Harness is a first-class concept: Claude Code first, others later.
- The tree UI stays in the terminal; tmux keeps hosting sessions.

Research done 8 Nov 2026; tool versions drift fast, so re-check before building.

## Decisions so far

- **Antigravity dropped** — too immature: no server, no attach to a live session, headless stdout bugs until recently, reverse-engineered storage.
- **No point building a standalone unifier** — several open-source ones exist (see below); the value is in CC Tree's own adapters, not a generic layer.
- **Core harnesses:** OpenCode, Codex, Claude Code.
- **Candidates for later:** GitHub Copilot CLI (widely used, SDK + server mode), Pi (open, OpenRouter-friendly, best RPC protocol). Cursor CLI only if already paying for it.

## Key findings

The real split is **owning the process** vs **attaching to a live TUI**. Every harness can be spawned headless with a JSON stream; only OpenCode and Codex let a second client drive a session a human is also watching.

| Capability | OpenCode | Codex | Claude Code |
| --- | --- | --- | --- |
| Send to a live session | Yes — `/tui/append-prompt` + `/tui/submit-prompt`, or `prompt_async` | Yes — if the TUI runs as `--remote` client of a shared app-server | Via Channels (research preview) or tmux `send-keys` |
| Own a headless session | HTTP + SSE, or `opencode acp` | app-server JSON-RPC (stdio stable, WebSocket experimental) | `-p --input-format stream-json --output-format stream-json` |
| Live events | SSE `/event`, `/global/event` | `turn/*`, `item/*`, delta notifications | stdout stream (owned) / hooks (attached) |
| Interrupt / steer | Abort endpoint | First-class steer + interrupt | control\_request `interrupt` (owned only) |
| Approvals from manager | Permission events + respond endpoint | Server→client JSON-RPC request | `can_use_tool` (owned) / `PreToolUse` hook (attached) |
| History / discovery | `GET /session`, messages | `thread/list`, rollout JSONL | `~/.claude/projects/*/*.jsonl`; Agent View state (undocumented) |
| Subagents | Child sessions, fully addressable | Child threads, fully addressable | Visible via hooks + transcripts; not addressable |

Observation backbone that works across harnesses: **hooks posting to a local endpoint + tailing the transcript file whose path the hook hands over**. OpenCode is the exception — use its SSE stream (or a small plugin).

## Per-harness findings

### OpenCode — best fit

Client/server by design: the TUI is just a client of a server with an OpenAPI 3.1 spec at `/doc`. `opencode serve` runs headless; `opencode attach <url>` attaches a TUI to a running server.

- Send: `POST /session/{id}/prompt_async`; `/tui/append-prompt`, `/tui/submit-prompt` drive the *visible* TUI (used by IDE plugins).
- Read: SSE `/event` (deltas, `session.idle`, permissions, tool parts); `/global/event` multiplexes all instances.
- In-process: plugins (`tool.execute.before/after`, `chat.message`, `event`). Also `opencode acp`.
- Gotchas: TUI picks a **random port** unless launched with `--port`. `opencode serve` next to a running TUI starts a *new* server. Auth via `OPENCODE_SERVER_PASSWORD` (basic auth); binds 127.0.0.1 by default.

### Codex — very good

`codex app-server` is the backend for the VS Code extension, desktop app and TUI. JSON-RPC 2.0 without the `"jsonrpc"` field, JSONL framing.

- `--listen stdio://` (stable) or `--listen ws://IP:PORT` (experimental) with capability or signed bearer token auth.
- TUI as thin client: `codex --remote ws://…`; threads survive TUI disconnects.
- Primitives Thread → Turn → Item; steer and interrupt are first-class; approvals arrive as server→client requests.
- Generate the schema for the installed version (`codex app-server generate-ts` / `generate-json-schema`) — the protocol drifts between releases.
- Manager pattern: one long-lived app-server per machine on loopback/tailnet; human TUIs connect with `--remote`, the manager connects as another client → same threads, same events.
- Attached observation: hooks (`~/.codex/hooks.json` or repo `.codex/hooks.json`, needs the hooks feature flag; repo hooks need per-user trust). Rollout files `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` are not a stable contract.
- `codex exec --json` is one-shot: no fork, no steer, interrupt only by kill.

### Claude Code — workable, no inbound API on the TUI

Combine five surfaces:

1. **Headless stream-json** (owned process): `claude -p --input-format stream-json --output-format stream-json --verbose`; control requests for interrupt, permission mode, `can_use_tool`. Types are @alpha. Under the subscription constraint, check current terms before relying on it.
2. **Channels** (research preview, ≥ v2.1.80): an MCP server Claude Code spawns that pushes `<channel>` events into the *running interactive* session; replies go back via a tool it exposes. Must be named in `--channels` at launch; sender allowlist; claude.ai login; Team/Enterprise admins must enable it. The clean manager → live-session path; maps naturally onto session-bus.
3. **Background sessions / Agent View** (≥ v2.1.139): `claude --bg`, `claude agents`, `attach|logs|stop|respawn <id>`; a per-user supervisor hosts sessions. State on disk is undocumented. Conflicts with "tmux owns lifecycle".
4. **Hooks**: richest set (SessionStart, UserPromptSubmit, Pre/PostToolUse, Notification, Stop, SubagentStart/Stop, PreCompact, SessionEnd); payload carries `session_id` + `transcript_path`.
5. **Transcripts**: `~/.claude/projects/<slug>/<session>.jsonl`, append-only.

Not useful: Remote Control (claude.ai/app only, no public protocol). Fallback: tmux `send-keys` + `capture-pane`.

## Nested subagents

Observation mostly carries over to subagents; sending to them mostly doesn't. It depends on whether the harness models a subagent as a full session or as something inside the parent.

- **Codex** — subagents are real child threads on the app-server, with parent link, depth and role in their metadata; `thread/list` filters for them; you can read, resume and send to a child directly. Caveats: since Multi-agent V2 the parent's dispatch instructions are stored encrypted (you see what the child does, not what it was told); a reported race left a child thread with no parent link.
- **OpenCode** — subagents are child sessions with a parent ID; they appear in `GET /session` and on the same SSE stream. Prompting a child directly is possible; how the parent reacts is unverified.
- **Claude Code** — subagents run inside the parent process and share its `session_id`. Hooks add `agent_id` + `agent_type`; `SubagentStop` gives `agent_transcript_path` (`subagents/agent-<id>.jsonl`). Gotchas: inside a subagent, `PreToolUse`'s `transcript_path` points at the parent's transcript; nesting depth is flattened in hooks (parent link only in an undocumented `agent-<id>.meta.json`, written after `SubagentStart`). No way to send to a subagent; Channels deliver to the main session only; Agent View doesn't list nested subagents.

Implication: treat subagents as annotations on their parent session, not as managed nodes (except Codex, where children could be promoted). Steer through the top-level session. Work that needs full control should be spawned by the manager as its own top-level session.

## Other harness candidates

| Harness | Control surface | Attach to running instance | Fit |
| --- | --- | --- | --- |
| GitHub Copilot CLI | `copilot --acp`; official SDKs (Node, Python, Go, .NET, technical preview) over JSON-RPC | SDK can connect to an existing CLI server (TCP per community ports) | Strongest 4th harness; multi-vendor models incl. Claude |
| Pi (pi-mono) | `--mode rpc` bidirectional JSONL; built-in steer and follow-up queues; Node SDK; Rust client `pi-codes` | Not found | Open source, any model via OpenRouter; best for experiments |
| Cursor CLI (`agent`) | `-p` with json/stream-json + partial deltas; `agent acp`; hooks, MCP | No | Only if already paying; stream-json schema undocumented |

Skipped: Gemini CLI (no longer accepts free or AI Pro/Ultra sign-ins), Amp, Goose, Factory Droid, Kimi, Kiro, Qwen — reachable mostly via ACP or terminal scraping, nothing beyond the three above.

## Existing open-source unifiers

None combines structured events *and* live attach for all harnesses; the tmux-based managers mostly detect state by screen scraping.

| Project | Layer | How it works | Harnesses | Worth borrowing |
| --- | --- | --- | --- | --- |
| [ACP](https://agentclientprotocol.com/) | Protocol | JSON-RPC over stdio, spawn-and-own | OpenCode native; Claude, Codex via adapters; Copilot, Cursor native | Common session/prompt/permission shape. Claude adapter builds on the Agent SDK |
| [gate4agent](https://docs.rs/crate/gate4agent/latest) | Rust library | Pipe, PTY, ACP, daemon transports | Claude, Codex, Gemini, OpenCode | Per-CLI history readers, resume handling |
| [coder/agentapi](https://github.com/coder/agentapi) | HTTP server | In-memory terminal emulator; keystrokes in, parsed messages out | Claude, Codex, OpenCode, Gemini, Cursor, more | Works on real TUIs; fragile parsing, no structured tool calls |
| [Agent of Empires](https://github.com/agent-of-empires/agent-of-empires) | Session manager | tmux; TUI + web PWA | 13 CLIs incl. Claude, OpenCode, Codex | Prior art for tree/dashboard UX |
| [CLIshake](https://github.com/Clishakehq) | Session manager | tmux, one adapter contract | Claude, Codex, OpenCode, Copilot | Adapter contract design |
| [unified-agent-manager](https://github.com/RandomCodeSpace/unified-agent-manager) | Session manager | Private tmux socket, Bubble Tea TUI | Claude, Codex, Copilot, OpenCode | Agent View clone across harnesses |
| [agtx](https://github.com/fynnfluegge/agtx) | Orchestrator | Kanban + orchestrator agent, tmux + worktrees | Claude, Codex, Gemini, OpenCode, Cursor | Closest in spirit to CC Tree managers |

## Design implications for CC Tree

- **One adapter per harness with explicit capabilities** — spawn, send to live session, observe, interrupt, approve, list history. No pretended parity.
- **The manager launches every session with a known handle** — OpenCode with `--port` (or one `serve` + many `attach`), Codex TUI with `--remote` to a shared app-server, Claude with `--channels <own server>`, all inside a named tmux pane. Attach-after-the-fact is unreliable.
- **Observation = hooks → local endpoint, content = transcript tail.** Hook payloads give the session id ↔ transcript path mapping for free. OpenCode via SSE.
- **Input to live Claude sessions = a custom Channel server**, which can double as the session-bus inbox. tmux `send-keys` stays the universal fallback.
- **Lifecycle owner is tmux**, consistent with the persistent-host decision. Claude's `--bg` supervisor and Agent View would compete for that role.
- **Approval routing is a deliberate per-harness choice** — Codex and OpenCode broadcast approvals to every client, so the human TUI and the manager can race.
- **Subagents are annotations on their parent node**; Codex child threads are the only candidates for promotion to real nodes.
- **Pin versions and regenerate schemas**; treat on-disk formats as best-effort and hooks/APIs as primary.

## Invariants

- Every managed session is started by the manager (or its wrapper); nothing is discovered blindly.
- Each session has exactly one lifecycle owner: tmux.
- The manager never terminates and resumes interactive sessions to change them; it only sends input.
- The manager never stores or reuses harness login tokens; it only spawns official binaries.
- Subscription-backed harnesses run with API-key env variables scrubbed, so they never silently switch to billed API mode.
- Every server (OpenCode, Codex app-server, Channel server) binds to loopback or the tailnet IP only, always with auth.
- No session depends on the backend being up: a machine can start sessions and observe them locally offline.
- The manager knows whether a turn is in flight before injecting a message (events for Codex/OpenCode, hook state for Claude).
- Sending is never duplicated on retry: one manager message maps to at most one injected prompt.
- A subagent is always attributed to its top-level session, even when its own parent link is missing.
- Hook handlers are fail-open and fast: an observer crash or timeout never blocks or alters the agent.
- Transcript parse failures degrade to "unknown" state, never crash the manager.
- The harness version is recorded per session; adapters refuse unknown major versions.
- Approval decisions have a single designated responder per session (human TUI or manager), never both.
- Session ids are harness-native and stored verbatim; the tree's own task id maps to them, never replaces them.

## Open questions

- [ ] Is driving `claude -p` with stream-json acceptable on a Claude subscription, or must Claude stay interactive-only (Channels + hooks + tmux)?
- [ ] Can a Channel server reliably act as the manager's inbox for every session — one server per session, or one shared server routing by session?
- [ ] Does the Codex WebSocket app-server hold up for days-long sessions? It is still marked experimental.
- [ ] What happens in OpenCode when the manager prompts a child (subagent) session directly while the parent waits on it?
- [ ] Copilot CLI: confirm the official way to attach the SDK to an already running CLI server, and whether the human TUI can share it.
- [ ] Does Claude's Agent View supervisor interfere with tmux-hosted sessions (every session gets `$CLAUDE_JOB_DIR` since v2.1.139)?

## Links

**OpenCode**

- [Server API](https://opencode.ai/docs/server/)
- [CLI: attach, run --attach, acp](https://docs.opencode.ai/docs/cli)
- [Go SDK](https://github.com/orvice/opencode-go) · [Rust SDK](https://docs.rs/crate/opencode-codes/latest)
- [Claude-style hooks inside OpenCode](https://github.com/monks1975/opencode-agent-hooks)

**Codex**

- [app-server README (PR snapshot)](https://github.com/openai/codex/pull/7105/files)
- [Request: attach to running sessions](https://github.com/openai/codex/issues/11166)
- [Remote / WebSocket write-up](https://codex.danielvaughan.com/2026/03/31/codex-cli-app-server-remote-websocket/)
- [Hooks write-up](https://codex.danielvaughan.com/2026/03/30/codex-cli-hooks-engine)
- [Python client codex-control](https://github.com/therealwiki/codex-control) · [Rust client codex-codes](https://docs.rs/crate/codex-codes/latest)
- [Subagents over app-server (Elixir SDK guide)](https://hexdocs.pm/codex_sdk/Codex.Subagents.html)
- [Encrypted subagent instructions issue](https://github.com/openai/codex/issues/32753) · [Orphan child thread issue](https://github.com/openai/codex/issues/35781)

**Claude Code**

- [CLI reference](https://code.claude.com/docs/en/cli)
- [Channels](https://code.claude.com/docs/en/channels)
- [Agent View summary](https://www.dsebastien.net/claude-code-agent-view/) · [Agent View state-file issue](https://github.com/anthropics/claude-code/issues/59848)
- [Hook stdin schema reference](https://pablolion.github.io/claude-hookwatch/reference/hook-stdin-schema.html)
- [Subagent transcript\_path issue](https://claudeissues.com/issue/76333-pretooluse-hooks-in-subagents-receive-parent-sessions-transcript-path-instead-of) · [Subagent parent link request](https://claudeissues.com/issue/100198-feature-name-the-parent-and-display-name-in-the-subagentstart-hook)

**Other candidates**

- [GitHub Copilot SDK](https://github.com/magicVspace/copilot-sdk) · [Copilot CLI ACP server](https://techcommunity.microsoft.com/blog/azuredevcommunityblog/from-terminal-to-autonomous-coding-mastering-github-copilot-cli-acp-server/4508604)
- [Pi RPC mode](https://pidocs.seepine.com/en/rpc) · [pi-codes](https://docs.rs/pi-codes)
- [Cursor CLI cheat sheet](https://cheatsheets.zip/cursor-cli)

**Unifiers**

- [ACP](https://agentclientprotocol.com/) · [gate4agent](https://docs.rs/crate/gate4agent/latest) · [coder/agentapi](https://github.com/coder/agentapi)
- [Agent of Empires](https://github.com/agent-of-empires/agent-of-empires) · [CLIshake](https://github.com/Clishakehq) · [unified-agent-manager](https://github.com/RandomCodeSpace/unified-agent-manager) · [agtx](https://github.com/fynnfluegge/agtx)
- [cc-multi-cli-plugin](https://github.com/marceldarvas/cc-multi-cli-plugin)

**Antigravity (dropped, for reference)**

- [Hooks](https://antigravity.google/docs/hooks) · [CLI changelog](https://github.com/google-antigravity/antigravity-cli/blob/main/CHANGELOG.md)
