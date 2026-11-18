[Index](../../README.md) › [Unified model](../../unified-model.md) › [Surfaces](../surfaces.md) › surfaces-mux-dashboards-push

# Surfaces: multiplexers, dashboards, push (state as of 2026-11-18)

Tags: **[doc]** = vendor/official docs or API read this session; **[rep]** = third-party report/blog/search
snippet, not verified at source; **[inf]** = my inference. Scores 1-5 (5 best). "fit-500" = would it hold up
at ~500 tasks/sessions in the tree (mostly [inf]).

## 0. Verdict (short)

- Keep **tmux** as lifecycle host. Nothing evaluated gives a better host for "never kill/resume a session to
  change it" plus a scriptable surface; tmux 3.8 (2026-09-09) is alive [doc: api.github.com/repos/tmux/tmux/releases/latest].
- Biggest upgrade is not the host but the **state signal**: stop inferring "waiting" from capture-pane; use
  Claude Code hooks (Notification) writing a tmux @option, as AoE/herdr integrations do [rep] / Agent View
  exposes `waitingFor` [doc]. Ghostty 1.4 even ships a draft program-status OSC (7501) [doc].
- Best phone channel for "question / approve" with one tap: **Telegram inline keyboard** (callback_query),
  then ntfy (HTTP action buttons) as a self-hosted alternative; Web Push from the web Tree is the free
  third leg (iOS needs Home Screen install).

## A. Multiplexers / lifecycle hosts

### tmux
- 3.8 released 2026-09-09; 3.6 2025-11-26, 3.6a 2025-12-05, 3.6b 2026-05-20 [doc: GitHub API; rep: linuxiac/tmux.info]. ISC license [inf, long-standing].
- Scriptability: the reference CLI (set-option @user opts, capture-pane, send-keys, respawn-pane, wait-for,
  hooks, `-F` formats). Control mode (`-CC`) is a line protocol for GUI front-ends [doc: tmux wiki, long-standing].
- Agent-state detection: none built in; every tool layers on top (screen scraping or agent hooks).
- Remote/web: none native. SSH or ttyd/xterm.js wrappers (what AoE and herdr-web do) [rep].
- Control mode in terminals: iTerm2 supports it natively (tmux windows become native tabs) [rep: tmux.app, nexasphere].
  **Ghostty does NOT**: 1.4.0 (2026-10-27) fixes `-CC` crashes/security issues, "still not hooked up to the GUI"
  [doc: https://ghostty.org/docs/install/release-notes/1-4-0]; a community fork adds it on macOS [rep: github.com/eliasfloreteng/ghostty-tmux].
  Implication: on the Mac with Ghostty, plain `tmux` (not -CC) stays the answer.
- Fit: it is the incumbent for cc-tree's 150 tmux calls; ecosystem of agent tools targets it (AoE, agtx,
  agent-manager, NTM, CAO all tmux) [rep].

### Zellij
- v0.46.0 on 2026-11-11 [doc: GitHub API]; 0.44.0 (Mar 2026) added remote sessions over HTTPS, read-only
  `zellij watch`, read-only web tokens, native Windows, versioned client/server protocol [doc: https://zellij.dev/news/remote-sessions-windows-cli/]. MIT [inf].
- Automation (0.44): `action list-panes`, `send-keys --pane-id`, `dump-screen`, `subscribe` (live viewport
  stream), `run --blocking`, pane/tab creation returns IDs [doc, same URL]. Plugins (WASM) can read other panes'
  scrollback and highlight text [doc]. This is the closest rival to tmux scripting.
- Web client (0.43+): browser attach with tokens; mobile viewport/touch scrolling improved in 0.44 [doc]. Needs
  its own TLS/auth; no push.
- State detection: none for agents [inf]; a plugin could be built.
- Caveats: upgrading orphans old sessions across 0.44 ("last time" claimed) [doc]; user-option equivalent of
  tmux `@vars` for colour/state does not exist [inf] -> 150 call sites would need a rewrite. Not worth it.

### herdr (herdrdev/herdr, "the runtime your coding agents live on")
- Rust, Apache-2.0 (relicensed from AGPL around Aug 2026, YC) [doc: GitHub API spdx; rep: developersdigest]. Created 2026-03-27,
  ~51.7k stars, v0.9.5 on 2026-10-14, pushed 2026-11-07 [doc: GitHub API]. Pre-1.0, fast-moving; 498 open issues [doc: repo page].
- Real multiplexer (spaces/tabs/panes, background server, detach/reattach, saved SSH machines) [doc: repo page].
- State: blocked/working/done/idle from foreground process + terminal output; `herdr integration install claude`
  adds hook-based lifecycle + session-ID restore [rep]. Local Unix socket API (create panes, send prompts, read
  output, subscribe to state changes, "wait until blocked") [doc: repo page; rep].
- Remote: SSH only natively; third-party `herdr-web` (MIT, 17 stars) runs the TUI in xterm.js, real Web Push
  over VAPID, Tailscale Serve recommended, no auth beyond the tailnet, no approve button [doc: github.com/barnuri/herdr-web].
- Fit: strongest "what a tmux replacement for agents looks like"; but loses the @option colour model and
  cc-tree's tmux code; vendor/maintainer risk (startup, 1.5k+ commits in 8 months). Borrow ideas, do not migrate.

### abduco / dtach / shpool (detach-only)
- abduco: last commit 2020-04-30, effectively dormant [rep: bitplane-sessiond#1]. dtach: old, infrequent updates [rep]. No terminal state kept.
- shpool: Rust, active (92 commits in 12 mo to 2026-09, mostly one maintainer) [rep], keeps VT100 state to
  redraw, single attached client per session [rep]. No windows/options/scripting: cannot replace tmux's role.

### WezTerm mux
- No official upstream release since 20240203-110809-5046fc22; only forks/snapshots in 2026 (MacPorts has a 20261005 snapshot) [rep: Repology, GitHub forks]. Mux server exists, Lua scriptable.
  Maintenance risk high; skip.

### Scored (A)
| tool | maturity | self-host | license | vendor risk | fit-solo | fit-500 |
|---|---|---|---|---|---|---|
| tmux | 5 | 5 | 5 | 5 | 5 | 4 |
| zellij | 4 | 5 | 5 | 4 | 3 | 3 |
| herdr | 3 | 5 | 5 | 3 | 3 | 3 |
| shpool | 3 | 5 | 5 | 3 | 1 | 1 |
| abduco/dtach | 2 | 5 | 4 | 2 | 1 | 1 |
| WezTerm mux | 2 | 5 | 5 | 2 | 1 | 2 |
(vendor risk: 5 = lowest risk. fit-500: tmux slows with many options/clients but 500 windows is routine [inf].)

## B. Agent dashboards / managers

### Claude Code Agent View (`claude agents`, `--bg`)
- Research preview, launched 2026-05-11 (needs >= 2.1.139) [rep]; docs at https://code.claude.com/docs/en/agent-view [doc].
- A per-user **supervisor** hosts sessions, each with a PTY host process; **no tmux** [doc]. Idle/finished sessions
  unattached ~1 h are stopped and resume on attach/reply; `--bg` sessions auto-move into a git worktree [doc].
  -> sessions are supervisor-owned, so they are not tmux panes; cc-tree "send-keys / @option" model cannot touch them [inf].
- State taxonomy [doc]: working / **needs input** (question, permission prompt, sandbox/MCP elicitation) / idle /
  completed / failed / stopped; process-alive glyph; "Ready for review" group from PR state (via `gh` output).
- Scripting [doc]: `claude agents --json [--all]` is the "supported interface": `state` (working|blocked|done|failed|stopped),
  `status` (busy|waiting|idle), `waitingFor` (permission prompt|input needed|sandbox request|worker request|dialog open);
  `claude attach|logs|stop|respawn|rm`, `claude --bg --exec 'cmd'`. `~/.claude/jobs/<id>/state.json` explicitly not stable.
- Notifications [doc]: `Notification` hook fires with types `agent_needs_input` and `agent_completed`, plus
  `preferredNotifChannel`. Only while agent view open for terminal notifs.
- Web/phone: doc page is **local-only**; no phone surface for agent view itself [doc, partly truncated read].
- Borrow: the state vocabulary and `waitingFor` enum; Haiku one-line row summaries each turn; "N awaiting input"
  in tab title; group-by-state with Needs input on top; `agent_needs_input` hook as the push trigger.

### Claude Code Remote Control and claude.ai/code (status only)
- Remote Control: launched late Feb 2026 (research preview, Max first; now Pro/Max/Team/Enterprise per guides), >= v2.1.51;
  `claude remote-control`, `--remote-control`, `/remote-control`; up to 32 concurrent sessions per process; local
  session driven from phone/browser via claude.ai/code or Claude mobile app, QR code [rep: helpnetsecurity, wotai, datacamp].
- claude.ai/code web sessions run in cloud, separate from local. Both share one UI [rep].
- Fit: vendor-hosted phone surface for a *local* session, but one process per session and no cc-tree awareness [inf]. Complements, doesn't replace "web Tree".

### Agent of Empires (njbrake/agent-of-empires)
- Rust, **tmux-based**, MIT, 4.7k stars, created 2026-01-09, v1.25.0 on 2026-11-18, pushed same day [doc: GitHub API]. Very active.
- Surfaces: TUI, CLI, HTTP API, web dashboard (`aoe serve`, experimental) installable as PWA; tailnet access via
  Funnel or `--remote`; token + passphrase + passkey auth; "anyone who authenticates can send keystrokes" [doc: https://www.agent-of-empires.com/guides/web-dashboard/].
- ACP-structured view (plans, tool calls, diffs, approvals) and browser push claimed by a comparison article [rep: clawtab.cc]; push doc page URL guessed 404, not verified.
- Waiting detection: not stated in fetched docs [unverified]; tmux-pane-scraping per agent is the norm for tmux tools [inf].
- Borrow: PWA + passkey/passphrase auth model, device list/revoke, DNS-rebinding Host/Origin gate, 4-hourly token rotation.
  Caution: its own guide steers to Funnel (public) for stable PWA URL; Serve on the tailnet is enough for you [inf].

### agtx (fynnfluegge/agtx)
- Rust/Ratatui kanban TUI; one worktree + one tmux window per task on a private `tmux -L agtx` server; MCP server (`agtx serve`) exposes the board; plugins gsd/spec-kit [rep: GitHub README via search]. Several copies/forks of the README exist.
- Borrow: kanban columns Backlog->Planning->Running->Review->Done map onto cc-tree task state; private tmux socket idea (isolation from user tmux).

### agent-manager (YoanWai; "unified-agent-manager" and "CLIshake" not found as named)
- Go tmux TUI, private tmux server `agentmgr`, live status, answer a blocked agent without attaching, worktrees, diff review, many agents supported [rep: github.com/YoanWai/agent-manager]. Also NTM and AWS CAO exist as tmux orchestrators [rep].
- No project named unified-agent-manager or CLIshake surfaced in two searches; name may differ. Confidence low.

### coder/agentapi
- Wraps an agent in an in-memory terminal; `GET /status` = stable|running, `GET /events` SSE, `POST /message`. **Now deprecated, unmaintained** per its README [rep: search of github.com/coder/agentapi]. Do not adopt. Forks exist.

### Crystal / Nimbalyst / Conductor / Sculptor
- Crystal deprecated Feb 2026 (last v0.3.5, 2026-02-26), successor Nimbalyst (MIT, kanban, iOS companion app) [rep]. Conductor and Sculptor: not researched (no result). Desktop GUI apps, not tmux hosts; low relevance to a CLI/tmux design.

### Scored (B)
| tool | maturity | self-host | license | vendor risk | fit-solo | fit-500 |
|---|---|---|---|---|---|---|
| Claude Agent View | 3 | 5 | 2 (proprietary) | 3 | 3 | 2 |
| Remote Control | 3 | 1 | 1 | 3 | 3 | 1 |
| Agent of Empires | 4 | 5 | 5 (MIT) | 3 | 4 | 3 |
| agtx | 3 | 5 | 4 | 3 | 2 | 2 |
| agent-manager | 3 | 5 | 4 | 3 | 2 | 2 |
| agentapi | 2 | 5 | 4 | 1 | 1 | 1 |
| Nimbalyst | 3 | 4 | 5 | 3 | 1 | 2 |
(license 2 = vendor CLI, not OSS; fit-500: none of these show tree/hierarchy views for hundreds of tasks [inf].)

## C. Terminals

- **Ghostty 1.4.0 (2026-10-27)**; 1.3.0 on 2026-03-09 added AppleScript (preview), 1.3.1 follow-up [doc: ghostty.org release notes 1-3-0, 1-4-0].
  - 1.4: AppleScript `terminal` exposes `pid`, `tty`, `name override`; `send key`; Shortcuts entity; GTK CLI `+new-tab -e`, `+toggle-quick-terminal` [doc].
  - **OSC 7501 program-status protocol** (idle/working/done/blocked/failed) parsed by libghostty but GUI ignores it for now; `GHOSTTY_SURFACE_ID` env var [doc]. Worth emitting from cc-tree later, near-zero cost [inf].
  - OSC 9/777 desktop notifications work, OSC 9;4 progress; OSC 99 parsed only [doc]. Inside tmux needs passthrough [inf].
  - tmux -CC: not in GUI (see A). MIT [inf].
- **iTerm2 + tmux -CC**: the only mainstream native integration (macOS only) [rep]. Not available on the Linux box; Ghostty users give it up.
- **Warp**: client open-sourced 2026-04-28, AGPLv3 (UI crates MIT) [rep: itsfoss, i-programmer]. Oz = cloud agent orchestration (launched 2026-02-10); Oz/Drive cloud parts reportedly closed [rep, conflicting]. Runs Claude Code etc. with vertical tabs, unified notifications, a plugin for rich Claude notifications [rep: docs.warp.dev]. Vendor-hosted features, not a tmux host; skip, borrow notification UX only.

## D. Phone / push

### Web Push (PWA) on iOS
- iOS 16.4+ push only for Home Screen web apps; no push in a plain Safari tab; permission must follow a user gesture;
  no silent push (permission revoked if you don't show a notification) [rep: progressivewebapps.com, relaybell, synthetixis].
- iOS 26: every site added to Home Screen opens as web app by default (manifest no longer required); Declarative
  Web Push since iOS 18.4 (no service worker needed to display) [rep: above]. Still keep a manifest with `display: standalone` [inf].
- Tailnet-only origin: works. Delivery goes to Apple/FCM/Mozilla public push services; only the *origin* must be
  HTTPS with a real cert, which Tailscale Serve provides (`tailscale serve --https`, HTTPS certs enabled) [rep: sero#261, herdr-web, notify].
  Phone must be on the tailnet only when *opening* the page / fetching detail, not for receiving the notification [inf].
  Keep payload minimal (title, kind, session id) since it traverses Apple/Google [rep, sound practice].
- **Serve vs Funnel**: Serve = tailnet-only; Funnel = public internet (AoE steers users to Funnel for stable URLs [doc]). You do not need Funnel for push; avoid it given "anyone authenticated can type into your agents" [inf].
- One-tap approve: notification actions on iOS web push are limited (no custom action buttons in Safari Web Push [inf, unverified]); tap opens the PWA, then a second tap approves.

### ntfy
- Self-hostable, Apache/GPL dual [inf]. iOS cannot hold a background connection, so a self-hosted server must set
  `upstream-base-url: https://ntfy.sh`: it sends only a poll-request through ntfy.sh -> APNs/FCM, and the phone then
  fetches the message from your server (so your server must be reachable from the phone, e.g. over tailnet) [rep: ntfy docs config/ via search, GitHub issue #880].
  Without upstream, iOS delivery can be delayed by hours [rep].
- Action buttons (`X-Actions` header: view / http / broadcast) allow an HTTP POST to your endpoint on tap [rep, details not verified this session; official docs docs.ntfy.sh/publish/#action-buttons]. http action runs from the phone -> needs reachability to your endpoint (tailnet) [inf].

### Gotify / Pushover
- Gotify: self-hosted, Android app; iOS only via third-party igotify [rep]. Weak fit.
- Pushover: $4.99 one-time per platform, 30-day trial, 10k msgs/month, emergency priority with ack, iOS critical alerts [rep: capterra/softwaresuggest, checked 2026-11-08]. Supports supplementary URLs but not inline approve callbacks [inf]. Hosted; vendor risk low-ish.

### Telegram bot inline keyboards
- `InlineKeyboardMarkup` buttons with `callback_data` (<= 64 bytes); tap -> `callback_query` update; call
  `answerCallbackQuery` (text <= 200 chars) within ~30 s, then `editMessageText` to show result [doc: core.telegram.org/bots/api].
- Works over long polling from the box, so **no inbound port**, no tailnet needed for the tap, no PWA install; iOS
  Telegram delivers reliably via APNs; notifications show the buttons expanded in the message (tap = one step) [inf].
  You already have `notify-tomas-telegram` bots (CLAUDE.md), so incremental cost is a poller handling callback_query.
- Security: authenticate by chat id, put an opaque token (session+prompt id) in callback_data, expire it; since a tap
  maps to `tmux send-keys`, a stale tap must not answer a different prompt [inf].
- Vendor risk: Telegram is proprietary, bot API free; data visible to Telegram [inf].

### Scored (D)
| channel | maturity | self-host | license | vendor risk | fit-solo | fit-500 |
|---|---|---|---|---|---|---|
| Web Push PWA | 4 | 5 | 5 | 3 | 3 | 4 |
| ntfy (+ntfy.sh upstream) | 4 | 4 | 4 | 4 | 4 | 4 |
| Gotify | 3 | 5 | 5 | 4 | 1 | 3 |
| Pushover | 5 | 1 | 1 | 4 | 3 | 4 |
| Telegram inline kbd | 5 | 2 | 1 | 3 | 5 | 4 |
fit-solo weights one-tap approve/deny on iPhone with minimum new infra.

## E. What to borrow

| from | borrow |
|---|---|
| Agent View | state enum + `waitingFor`; per-turn one-line summaries; "N awaiting input" title; hook types `agent_needs_input`/`agent_completed` |
| herdr | integration-installed hooks as source of truth for state; socket "wait until blocked"; herd strip (dot per agent) in the web Tree |
| herdr-web | VAPID Web Push triggered on working->blocked transition; quick-keys bar for phones; serve via `tailscale serve` |
| AoE | PWA auth (passkey+passphrase, device revoke, Host/Origin gate); ACP-style structured approvals view |
| agtx / agent-manager | private tmux socket (`-L`) per tool to isolate; kanban phases; "answer blocked agent without attaching" |
| Ghostty | emit OSC 7501 and OSC 9 on state change; `GHOSTTY_SURFACE_ID` |

## F. Recommended build order (inferred)
1. Hook-driven state: Claude Code Notification hook -> `tmux set-option -p @cc_state blocked` (+ `waitingFor`). Retire capture-pane polling for state.
2. On transition to blocked: send Telegram message with [Approve][Deny][Open] buttons; poller handles callback_query -> `send-keys` (guarded by prompt-id token).
3. Web Tree: add VAPID Web Push for the same event (second channel, no extra account), Home Screen install on iPhone.
4. Optional: ntfy self-hosted with upstream ntfy.sh as non-Telegram fallback.
5. Revisit zellij/herdr only if tmux's lack of agent awareness keeps costing code after step 1.

## G. Unverified / gaps
- Did not read tmux 3.7/3.8 changelogs; AoE push doc and waiting-detection method unconfirmed; ntfy action-button docs and iOS Web Push notification actions not fetched; Conductor, Sculptor, unified-agent-manager, CLIshake not located; Agent View docs read only first 100k of 146k chars.

## Sources
- https://code.claude.com/docs/en/agent-view
- https://ghostty.org/docs/install/release-notes/1-4-0 , /1-3-0
- https://zellij.dev/news/remote-sessions-windows-cli/
- https://www.agent-of-empires.com/guides/web-dashboard/ ; https://github.com/njbrake/agent-of-empires (API)
- https://github.com/herdrdev/herdr (API) ; https://github.com/barnuri/herdr-web
- https://api.github.com/repos/tmux/tmux/releases/latest ; https://api.github.com/repos/zellij-org/zellij/releases/latest
- https://github.com/fynnfluegge/agtx ; https://github.com/YoanWai/agent-manager ; https://github.com/coder/agentapi
- https://core.telegram.org/bots/api ; https://github.com/binwiederhier/ntfy/issues/880 ; https://docs.ntfy.sh/config/
- https://progressivewebapps.com/background/ios-web-push/ ; https://github.com/sero-labs/sero/issues/261
- https://www.helpnetsecurity.com/2026/02/25/anthropic-remote-control-claude-code-feature/
- https://itsfoss.com/news/warp-goes-open-source/ ; https://github.com/stravu/crystal ; https://github.com/shell-pool/shpool
