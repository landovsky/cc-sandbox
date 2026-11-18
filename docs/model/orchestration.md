---
type: analysis
status: draft
area: E — orchestration (CC Flow as the orchestrator)
task: ai-tooling:#2rxg (milestone ai-tooling:#6rrd)
date: 2026-11-18
inputs: docs/unified-session-model.md, docs/cc-flow-on-kubernetes.md (doc 68 summary),
  docs/coding-harness-control-surfaces.md, ~/.dotfiles/cc-flow (OVERVIEW, cc-flow.md "The boundary",
  ADR-0002..0007, docs 46, 62, 68-node-settings), bin/lib/cc_tree.py, jobs.yaml,
  docs/architecture/{ai-tooling-map,component-placement}.md
evidence: model/evidence/orchestration-survey.md (outside orchestrators, sources, scores)
---

[Index](../README.md) › [Unified model](../unified-model.md) › Orchestration

# E. Orchestration: CC Flow as the orchestrator

**In one line.** CC Flow keeps everything that decides **which session runs for which task,
where, under which harness, with which grants, and when**. It hands the *how* to four layers
under it (host launcher, harness adapter, hub, permissions). Nothing outside covers the tree,
the ❓ queue, managers and arming, so that stays CC Flow's product. What it can buy is underneath.

## 1. What Tomas has today

Live = used daily; draft = written, not built; idea = mentioned only. Paths are in `~/.dotfiles` unless noted.

| Piece | What it does for orchestration | Where | State |
|---|---|---|---|
| Tree + node address | `<tree>:<node>` exists before any process; Markdown in git, auto-committed, synced via GitHub | `~/cc-tree/<tree>/tree.md`, `nodes/<id>/` | live |
| Node settings | `Model:`, `Refresh:` (ADR-0003), `Hear:`, `Agent:`, `Done when:`, `Check:`, start dir; settings class proposed | `cc_tree.py:3101`, doc 68-node-settings | live / settings class draft |
| Launch | `launch()` builds `zsh -ic "claude! -N <ref> …"` → tmux window; `claude!` binds tab, names, colours, pins model, last line `cc-rc claude` | `cc_tree.py:4892`, `zsh/cc-flow.zsh:34` | live |
| Start a worker | `cc-tree start <ref> --brief …`; worktree per task (doc 16); brief = milestone + newest checkpoint | `cc_tree.py:5408`, `cc_tree_worktree.py` | live |
| Managers | ◆ milestone manager (starts workers, checks Outcome vs `Accept:`), ▣ workspace manager (intake) | `milestone-manager-role.md`, `claude/roles/workspace.md` | live |
| ❓ queue | decisions only Tomas answers; view, web, `/cc-task`; generated decisions page | `cc_tree_decisions.py`, doc 69 | live / doc 69 draft |
| Delivery | every channel calls `cc-tree deliver`, which reads `session_health` + `Refresh:` and picks message / fresh start / hold (ADR-0004) | `cc_tree_deliver.py`, `session_health` `cc_tree.py:4622` | live |
| Refresh | cold restart > 100k, roll at 40 % (ADR-0002); nudge timer auto-checkpoints before cache expiry | `cc-tree-nudge.sh` + timers | live |
| Arming | Arm! / Armed? / Disarm!; `verify-days` runner; 08:00 LATE/VOID Telegram | `claude/commands/cc-arm.md`, doc 62, `cc_tree_countdown.py` | partly live; doc 62 gaps draft |
| Scheduled agents | `Agent:` on a ◆, `cc-agents` tschedule job every 15 min, plan-limit gate (`cc-tree limits`), hp only | `cc_tree_agents.py`, `jobs.yaml:105` | live (experimental) |
| Unattended `claude -p` jobs | hriste, pharmacy ×19, hotdesk; bypass, host env | other repos' `jobs.yaml`, tschedule (`~/git/tschedule`) | live, **outside CC Flow's launch path** |
| Remote start | web ▶ Start; Mac long-polls hp (`cc-tree-agent`) | `bin/cc-tree-web`, `bin/cc-tree-agent` | live |
| Bus | `ccmsg`/`ccq`, Elixir hub on hp, native inbox / send-keys | `session-bus/` | live (area C owns its future) |
| Session host `sandbox` | `ccsbx run` in the pane; fence, placeholders, 0.45 s cold | `~/git/cc-sandbox/spike/bench.py` | spike; build items 1–7 in unified-session-model §3 |
| Harness adapters | only Claude Code; Codex / OpenCode capability table researched | harness doc | Claude live, others idea |
| Principle | mechanism over prompt (ADR-0007); "harness supplies signal and command, CC Flow decides when/for which session" | `cc-flow.md` "The boundary" | accepted |

## 2. What orchestration must do in the unified model

CC Flow is the **control plane outside every fence**. Per session it owns one **session record**
and four decisions. It never executes code, holds credentials or renders a conversation.

```
SessionRecord (one per generation; address is the identity, gen++ on every fresh start/move)
  address     ai-tooling:2rxg          gen 3
  role        worker | milestone-mgr | workspace-mgr | agent:<name> | job:<name>
  host        local | sandbox | host-uid | pod   (+ machine: hp-ubuntu | mac)
  harness     claude@2.1.327 | codex@x | opencode@y   harness_sid (verbatim, mapped)
  billing     subscription | api            (chosen by launcher, recorded; C4)
  grants      profile name + resolved ceiling id (area B owns content)
  workdir     worktree path (same absolute path inside the fence)
  responder   human | manager                (single approval responder, harness invariant)
  started_by  tomas | <manager address> | schedule:<job>
```

| # | Decision CC Flow owns | Today | In the model |
|---|---|---|---|
| D1 | **Which session for which task**: start, resume, fresh start, roll, hold | `start`, Enter, `deliver`, ADR-0002/3 | unchanged; `session_health` becomes host- and harness-neutral |
| D2 | **Placement**: host tier + machine + harness | implicit (local, claude, where you pressed Enter; agents hp only) | node settings `Host:` / `Harness:`, resolved at launch into the record; refuses a tier the node isn't allowed |
| D3 | **Grant request**: which grant profile the session starts with | none (host env, user settings) | `Grants: <profile>` setting → area B; CC Flow never decides a grant, it only asks and records |
| D4 | **Timing**: schedules, loops, arming, verify days, nudges | tschedule + `cc-agents` + nudge + `verify-days` | unchanged, plus unattended jobs move onto the same launch path (E9) |

**Invariants, applied to orchestration**

- **No secret inside the fence.** The orchestrator itself is outside it and must also not be a
  credential store for the fence: a launch spec carries profile names and placeholders, never a
  value (doc 68 MR-39: the tree service as a cross-session pivot). tree-core's own keys (GitHub
  push, Telegram) stay in tree-core and never enter a spec.
- **Sessions don't depend on the backend (B0).** Placement and launch must work with the hub, web
  and gateway's control API down: the launch spec is computed from the local tree clone, `local`
  and `sandbox` start offline. A `pod` start is the only one that needs a service (the launcher).
- **Consent by a human.** CC Flow is where consent is *asked* (❓) and *recorded*; area B is where
  it is *enforced*. A manager may start workers and decide inside the autonomy Tomas armed, but
  can't widen a grant ceiling or arm a milestone (doc 62 §4: only he arms).
- **tmux owns lifecycle on local/sandbox.** The orchestrator starts sessions and sends input; it
  never kills a session to change it. Fresh start / roll happen only at a turn boundary after a
  checkpoint (ADR-0002/3). On `pod`, the drain protocol is the only stop (C1 in the unified doc).
- **Mechanism over prompt (ADR-0007).** Placement, grants and the agent's door out are fields and
  commands, not lines in a brief.

**The gap the sandbox opens (new finding).** A fenced worker can't reach `~/cc-tree`, tmux or the
bus spoke. Today these CC Flow verbs run *inside* the session and write directly:
`/cc-task` (outcome, done, rename, add), `/checkpoint` storage via `cc-tree-hook.sh`, `/subtask`,
adding a ❓, a manager's `cc-tree start` / `deliver` / `ccmsg`. In a sandbox every one of them
fails (or, if `~/cc-tree` were mounted, the agent could rewrite any tree, which breaks the fence).
So the model needs an **agent door**: a request spool the host applies with the *pane's* bound
address, never the address the agent claims (§4, E5). It is the solo version of doc 68's
`session-api`, and it should be built with the hook bridge (build item 3), because it's the same
mount and tailer.

**Role → default tier** (rec, ❓ E-1): workers that code → `sandbox` first; managers stay `local`
until the agent door carries `start`/`deliver`/`ccmsg` (they mostly need CC Flow verbs, not
code); scheduled agents and unattended jobs → `sandbox` with `billing: api` once the gateway
(item 5) exists, because they run with nobody watching.

## 3. Outside solutions

Full survey with every claim tagged and sourced: [evidence/orchestration-survey.md](evidence/orchestration-survey.md)
(a Sonnet subagent; context given: CC Flow's shape and invariants, the candidate list, no repo access).
Tags: **[D]** documented by the vendor, **[R]** reported by third parties, **[I]** inferred here.

**What the survey found.**
- **The wrapper layer is high-churn [R].** Vibe Kanban is sunsetting and its company shut down in April 2026. Crystal was deprecated in February. Conductor is joining Meta, and its cloud closes on 2026-11-20. Claude Squad is AGPL and quiet.
- **Closest designs to CC Flow [D]:**
  - [agtx](https://github.com/fynnfluegge/agtx): tmux plus worktrees, a board over MCP, trust prompts left to the human.
  - [Agent of Empires](https://github.com/agent-of-empires/agent-of-empires): tmux owns the session.
  - [CLIshake](https://github.com/Clishakehq): an adapter contract, approval gates and an audit log, but only about 3 stars.
- **No candidate covers four things [I]:** a human-only ❓ queue across sessions, a Markdown-in-git tree as the record, scheduled TUI sessions on a subscription, and offline start with gateway-held secrets.
- **Claude Code's own features:**
  - [Agent teams](https://code.claude.com/docs/en/agent-teams) are experimental and session-scoped, and a teammate can't approve for Tomas [D].
  - Agent View's daemon owns the PTY, which conflicts with tmux owning the lifecycle [D/R].
  - Both are useful only as signals (`claude agents --json`, the `agent_needs_input` hook) and as in-session tools.
- **LangGraph, CrewAI and the OpenAI Agents SDK own their own agent loop on API billing [D].** They would replace the harness, not orchestrate it.
- **ACP's Claude adapter drives the Agent SDK, not the TUI [R]** ([wire analysis](https://leanzero.net/blog/claude-code-in-zed-vs-the-terminal-what-the-acp-wire-actually-carries)). Using it would lose the pane and the subscription path.
- **Billing [R]:** Anthropic announced in May 2026 that `claude -p` and Agent SDK use would move to API-priced credits, then reversed that on June 15. Third-party use of subscription tokens is still restricted. That favours driving the real TUI, as CC Flow does. Re-check before E8.
- **[Temporal](https://temporal.io/blog/temporal-agent-harness-durable-agent-infrastructure) [D]** is MIT, mature and self-hostable. Its signals and `wait_condition` map onto the ❓ queue, and heartbeats map onto stale-session detection. Someone still has to write the activity that drives tmux.

**Shortlist** (1–5; vendor risk 5 = low risk):

| Candidate | Maturity | Self-host | License | Vendor risk | Fit | Use for CC Flow |
|---|---|---|---|---|---|---|
| Temporal | 5 | 4 | 5 MIT | 4 | 3 | durable engine at client tier |
| agtx | 3 | 5 | 5 Apache | 3 | 4 | ideas: board-over-MCP, artifact-file phase done, idle detection |
| Agent of Empires | 3 | 5 | 5 MIT | 4 | 3 | optional viewer / phone dashboard reference |
| CLIshake | 1 | 5 | 5 MIT | 2 | 3 | adapter-contract vocabulary |
| Claude agent teams / Agent View | 2–3 | 3 | 1 | 1 | 2–3 | in-session tool, signal source only |
| ACP | 4 | 5 | 4 | 4 | 2 | permission-request vocabulary; Codex/OpenCode headless later |
| OpenHands (Enterprise) | 4 | 4 | 3 | 4 | 2 | sovereign client host tier |
| GitHub Agent HQ / Copilot agent | 3 | 1 | 1 | 1 | 2 | GitHub-shop client host tier |
| LangGraph / CrewAI / OpenAI SDK | 3–4 | 2–5 | 4–5 | 2–3 | 1–2 | no |

**Recommended per sub-layer**

| Sub-layer | Solo (tenant zero) | 500-person client | Why |
|---|---|---|---|
| Flow: tree, ❓, managers, arming, placement | **build** (keep) | build | nothing ships it [I]; it is the product (doc 68 layer 13) |
| Durable timing / workflows | **tschedule + node fields** (keep) | **RECOMMENDED: Temporal**, only once a cell needs audit, retries and SLOs | at solo scale, Temporal adds a cluster with Postgres for about 70 timers; ADR-0007's "state is data" already gives durability through git |
| Session manager / multiplexer | **tmux + cc-tree** (keep) | same; attach is area D's | the wrapper layer churns; tmux is the invariant |
| Harness adapter protocol | **build a thin contract** (§4); borrow ACP's permission schema | ACP for owned headless Codex / OpenCode workers | ACP over Claude means the SDK, not the TUI [R] |
| Task / decision UI | **build** (web Tree + view) | build, plus SSO from area D | agtx and Vibe Kanban are references only |
| Hosted cloud agents as a host tier | not now (idea: Claude Code on the web for a phone-only task) | **RECOMMENDED: GitHub Agent HQ** for GitHub shops; **OpenHands Enterprise** for on-prem | "task in, PR out" behind a `Host: cloud:<vendor>` adapter; CC Flow keeps the node and the Outcome; vendor-neutral per the boundary rule |


## 4. Contracts with the other four areas

Arrows are calls; *data* is the shape crossing. All calls from inside a fence go through the
gateway or the spool; nothing inside calls CC Flow's host process directly.

**→ Session host / launcher (`ccsbx run`, unified doc build items 1–2)**: CC Flow calls it.
```
ccsbx run --spec <file|->  (pane process)      LaunchSpec {address, gen, role, host, machine,
                                                harness, harness_args[], workdir, home_volume,
                                                billing, grants_ref, spool_dir, env_placeholders{}}
```
`claude!` stays the host-side wrapper; its last line becomes `exec ccsbx run --spec …` when
`host != local`. The pod launcher accepts the same spec (doc 68 step 2), so the spec is the tier
seam, one level above `tmux()`.

**→ Harness adapters**: CC Flow calls them in-process (Python, `bin/lib/cc_tree_harness.py`, new).
```
Adapter.capabilities() -> {send_live, own_headless, interrupt, approve, subagents_addressable, resume}
Adapter.spawn_args(spec) -> argv      Adapter.observe_source(spec) -> hooks|sse|jsonrpc
Adapter.send(rec, text) -> via hub    Adapter.turn_state(rec) -> idle|busy|asking|unknown
```
No pretended parity (harness doc). `claude` = today's code moved behind it; `codex` / `opencode`
later. ACP is an option for *owned headless* sessions only (§3).

**→ A. MCP gateway**: CC Flow calls nothing at runtime; at launch it puts `grants_ref` in the spec
and the gateway resolves which MCP servers the session sees. **A calls CC Flow** for one MCP
server, `ccflow`, served at the gateway and scoped to the caller's address (identity from the
gateway, not the agent): `outcome.set`, `question.add`, `subtask.add`, `checkpoint.store`,
`start` (managers), `deliver` (managers). It is the pod-tier twin of the spool door (E5): same
verbs, same authorisation table.

**→ B. Permissions**: CC Flow calls B at launch (`grant.resolve(profile, address, role) ->
ceiling_id`), when an agent asks for more (`grant.request(address, what, ttl, why) -> request_id`,
which CC Flow turns into a ❓ node), and on answer (`grant.decide(request_id, approve|deny,
by=human)`). **B calls CC Flow** for `who is this address / role / milestone / armed autonomy`.
CC Flow never stores or evaluates a grant; B never decides placement.

**→ C. Cooperation (hub, context)**: CC Flow calls `hub.deliver(address, envelope)` from
`cc-tree deliver` (ADR-0004 stays the only entry) and `hub.presence(address)`. **C calls CC
Flow** for `resolve(address) -> current SessionRecord (gen, host, harness)`: the session
registry is CC Flow's, the hub caches it. Checkpoints and briefs stay CC Flow's (node data); C
owns transport and any shared context store. *Data*: `Envelope {to: address, from: address,
kind: message|tick|answer|work, body, sig}`.

**→ D. Surfaces**: surfaces call CC Flow verbs only: `start`, `answer ❓`, `arm/disarm`, `fresh
start`, `deliver`, `place` (change `Host:`/`Harness:`). CC Flow returns the one `session_health`
line (ADR-0005) extended with `host · harness · grants` so every surface shows where a session
runs. Attach (`ccattach`, tmux over ssh) is D's; CC Flow gives it `SessionRecord.machine/host`.

## 5. Build list delta (on top of unified-session-model §3 items 0–7)

| # | Piece | Size | Order / needs | Works when |
|---|---|---|---|---|
| E1 | **Session record**: write `host, harness@version, billing, grants_ref, gen, role, started_by` into each node's `history-<machine>.jsonl` launch line; `cc-tree show` / view / web line shows `host · harness` | XS | with item 2 | Enter on a `Host: sandbox` node; `cc-tree show` says `sandbox · claude@2.1.327 · subscription` |
| E2 | **`Host:` and `Harness:` as node settings** (doc 68-node-settings registry), resolved milestone → task only for these two (❓ E-2, #2s5y), validated against the role's allowed tiers | XS | after doc 68-node-settings step A | `cc-tree describe x --host sandbox` refused for a ▣ if managers are pinned local |
| E3 | **LaunchSpec**: `launch()` writes one JSON spec, `claude!` passes it to `ccsbx run --spec`; `--help` lists every field | S | with item 1 | the same spec file starts `local` and `sandbox`; `docker inspect` matches the spec, no secret in either |
| E4 | **Agent door v1 (spool)**: in the fence `/run/ccf/requests.jsonl`; host tailer applies `outcome / question / subtask / checkpoint / rename / done(self)` *as the pane's `@node`*; refuses other addresses; answers on `/run/ccf/replies.jsonl` | S | with item 3 (same mount, same tailer) | a sandboxed worker's `/cc-task outcome …` lands in tree.md; a request naming another node is refused and logged |
| E5 | **Agent door v2**: manager verbs `start` (under own milestone, ≤ 3/min, plan-limit gate), `deliver`, `ccmsg`; then managers may go `sandbox` | S | after E4; ❓ E-1 | a sandboxed manager starts a sandboxed worker; a start outside its milestone is refused |
| E6 | **Harness adapter interface**: move Claude specifics (spawn args, hook observation, send, turn state) behind `cc_tree_harness.py` | M | after E3; no behaviour change | all cc-tree e2e tests green with the adapter in between |
| E7 | **Grants profile**: `Grants:` setting → `grants_ref` in the spec; grant requests become ❓ nodes under the task | XS (CC Flow side) | after B's resolver exists | `cc-req egress …` from a worker shows a ❓ on the web Tree; approving it calls B |
| E8 | **Unattended jobs on the launch path**: tschedule `claude -p` jobs call `cc-tree job run <name>` → same spec, `host: sandbox, billing: api`, an address under a jobs tree | S | after item 5 (gateway) | a pharmacy job runs fenced, its run is a node with an Outcome; no host env inside |
| E9 | **Second harness: Codex** adapter (app-server, `--remote` TUI in the pane) | M | after E6; when a task needs it | Enter on a `Harness: codex` node opens a Codex TUI in a bound tab; `ccmsg` reaches it |

Minimum for "sandboxed workers do real tree work": E1 + E3 + E4 (XS + S + S) on top of items 1–3.

## 6. Risks and conflicts

**Top 3 risks**

1. **The agent door becomes the pivot** (doc 68 MR-39). Any request path from the fence into the
   tree is the attack surface. Mitigation: the address comes from the pane / gateway identity,
   never from the request; a fixed verb table per role; no free-form `cc-tree` passthrough; a
   block test and an allow test per verb.
2. **Two orchestrators.** Claude Code's own agent teams / Agent View, or a bought platform,
   start competing for lifecycle and task state (harness doc C8). Mitigation: CC Flow treats
   in-harness subagents and teams as annotations on the parent session, never as nodes.
3. **`cc_tree.py` (12k lines) absorbs the adapter, the spec and the door** and grows harder to
   split (gap 16). Mitigation: E3/E4/E6 go into new modules with their own tests from day one.

**CONFLICTS**

> Merged and resolved (or turned into ❓) in [unified-model.md §4](../unified-model.md#4-conflicts); build items renumbered in §5.

Neighbours' docs (A–D) weren't published when this was written; only evidence files exist. I'll check again before the integrator merges.

| # | With | Conflict | Position |
|---|---|---|---|
| X1 | doc 68 (`session-api` as the agents' only door) | Doc 68 puts the agent door in a split tree service, after rung 3. The sandbox tier needs it **now**, at item 3. | Build the spool door (E4) as the solo form of session-api, with the same verb table, so session-api later replaces only the transport. Doc 68 doesn't cover the gap before pods. |
| X2 | harness doc invariant "the manager never terminates/resumes to change a session" | E2 lets `Host:`/`Harness:` change on a node with a live session. | A change applies at the **next** generation only (fresh start or roll at a turn boundary); never by restarting a live session. |
| X3 | doc 68 "a manager may start workers, ≤ 3/min" | A sandboxed manager can't reach tmux. | Managers stay `local` until E5 (❓ E-1, #2sw2); the rate limit moves into the door's verb table. |
| X4 | area C (expected) | Who owns the session registry (address → current generation)? | **CC Flow** (it creates generations); the hub caches it and never mints sessions. If C claims it, the integrator decides. |
| X5 | area B (expected) | Arm! autonomy vs "only a human grants". | Arm! may set a **ceiling Tomas approves at Arm time**; inside it a manager approves nothing in B; it only picks among pre-granted profiles. |
| X6 | harness doc "input = Channel server per session" | Vendor-specific last hop. | Agree with the unified doc's C3: send-keys, then the native socket. The adapter's `send` hides the choice. |


## Decisions for Tomas (❓ under ai-tooling:#2rxg)

1. **#2sw2 E-1: which sessions go into the sandbox first.** Rec: workers that code. Managers stay local until E5.
2. **#2s5y E-2: where `Host:` and `Harness:` are set.** Rec: on the milestone, inherited by its tasks, applied at the next fresh start.
3. **#2sfs E-3: Temporal.** Rec: not for solo; consider it at the first client cell.
4. **#65cb E-4: hosted cloud agents as a host tier.** Rec: keep a slot for the adapter in the design and build it for the first client.

---

Related: [MCP gateway](mcp-gateway.md), [Permissions](permissions.md), [Cooperation](cooperation.md), [Surfaces](surfaces.md). Evidence: [orchestration-survey](evidence/orchestration-survey.md).
