---
type: evidence
area: C
date: 2026-11-18
provenance: Sonnet subagent, web search and fetch only; context given: hub jobs, tree shape, invariants, tiers; no repo access, no external models
---

# Agent context/memory landscape survey (as of 2026-11-18)

Tags: [D] documented by vendor/primary source, [R] reported by third party (blogs, comparison
sites, arXiv preprints I did not verify), [I] my inference. Search-result based; I did not
read primary repos line by line. Many sources are vendor-adjacent blogs. Treat star counts,
dates and versions as [R]. Verify anything you act on.

Scoring: maturity 1-5 (5 = boring and stable). Fit solo / fit 500 = 1-5 for YOUR constraints
(offline-first, git/markdown truth, no secret in sandbox, later tenancy/GDPR/audit).

## 1. Letta (ex-MemGPT)
- Apache-2.0 core framework and Letta Code harness [R: promptquorum, chatforest, developersdigest].
  Self-host: `letta server` [R]. Seed $10M Felicis Sep 2024; no 2026 round found [R].
- Memory blocks (labelled, size-limited text in context); blocks can be attached to several
  agents = the only first-class cross-agent shared-state primitive among these tools [R].
  Conversations API (Jan 2026) lets agents share blocks [R].
- Context Repositories ("MemFS"), announced Feb 2026: agent memory is a git repo cloned to
  local filesystem, every change committed with message, subagents work on branches and
  merge; `/memfs` migrates blocks to files; legacy server memory tools (core_memory_replace)
  to be removed in favour of filesystem ops [D: https://www.letta.com/blog/context-repositories/ ,
  https://www.letta.com/blog/our-next-phase/ , https://www.letta.com/agent-sdk/].
- Notes: V1 server API reported retired, development moved to letta-code [R]; churn risk.
  Letta is an agent runtime, not a store you bolt onto Claude Code; adopting it means
  adopting its harness. But the *design* (memory = git repo, subagents branch/merge) is
  almost exactly what you already do.
- Provenance: git commit authorship only [I]. Permissions: none beyond who owns the agent [I].
- Maturity 3 | self-host yes | Apache-2.0 | vendor risk high (small startup, pivoting) |
  solo 2 | 500 2 | offline yes (local git clone) | git truth: yes for MemFS, blocks no.
- Verdict: validates the git-as-memory pattern; steal ideas, do not adopt the runtime.

## 2. Mem0 / OpenMemory
- Core lib Apache-2.0, self-hostable, can run fully local with Ollama [R: developersdigest,
  mem0 cookbook]. Scopes: user_id, agent_id, run_id on writes and search filters [R].
- OpenMemory MCP (the self-hostable MCP server): sunset announced Apr 2026, removed from
  repo 29 Jul 2026; mem0-mcp repo archived; supported MCP is hosted mcp.mem0.ai needing a
  Platform account [R: developersdigest, chatforest, coworker.ai; some older pages stale].
  => no first-party self-hosted MCP now.
- Graph memory: reported removed from OSS in the v3 pipeline; native only on managed
  platform since Oct 2026 [R]. Plans disagree across sources.
- Provenance: scope ids + metadata, no signed/bitemporal provenance [I]. Memory is LLM-extracted
  facts, i.e. lossy rewrites of what agents said [I]; bad for checkpoints/outcomes.
- Maturity 3 | self-host partial (library only) | Apache-2.0 core | vendor risk high
  (open-core retreat) | solo 2 | 500 2 | offline yes with Ollama | git truth no.
- Verdict: avoid; the open-core direction moved away from what you need.

## 3. Zep and Graphiti
- Graphiti: Apache-2.0 temporal knowledge graph library; backends Neo4j, FalkorDB, Neptune;
  Kuzu reported deprecated [R: vectorize.io, aiagentmemory.org; D license via GitHub field].
- Zep Community Edition deprecated 2 Apr 2025, moved to legacy folder, no support [R,
  quotes Zep FAQ]. Zep product = commercial cloud.
- Bi-temporal: edges carry valid_at/invalid_at (world time) and created_at/expired_at
  (system time); contradictions close the old interval rather than delete [D/R: Graphiti docs
  via mintlify.wiki mirror]. Episodes = raw inputs; every fact traces to its source episode
  [R]; MCP tool get_episode_entities [R]. MCP server experimental, FalkorDB default; whether
  search filters on valid_at exist depends on version (main README yes; v1.0.2 per a survey
  no) [R, conflicting: https://github.com/getzep/graphiti/blob/main/mcp_server/README.md,
  https://mnemoverse.com/docs/library/graph-memory-mcp-tools].
- Requires a graph DB server + LLM calls for extraction on every write [I].
- Maturity 3-4 | self-host yes (needs graph DB) | Apache-2.0 | vendor risk medium (Zep
  funds it, but CE abandonment shows priorities) | solo 2 | 500 3 | offline only with
  local LLM + local DB | git truth no (derived index only).
- Verdict: best-in-class *model* for "what did we believe when, from which source"; use as
  an optional derived index later, never as truth. Overkill now.

## 4. Cognee
- Apache-2.0 engine; cloud/on-prem enterprise commercial. $7.5M seed Feb 2026 (Pebblebed)
  [R]. Topoteretes UG, Berlin (EU: helpful for GDPR conversations) [R].
- Self-host: Docker Compose, embedded defaults (SQLite/LanceDB/Kuzu-type), optional
  Postgres/Neo4j [R]. MCP server (cognee-mcp: remember/recall/forget) [R]. v1.6.x Sept
  2026 can run on local models without LLM key [R]; MCP quickstart still wants OpenAI key.
- Ingests documents -> graph + vectors; good for "search over a pile of markdown".
  `forget` is a real erasure verb (GDPR-relevant) [R, inferred relevance].
- Provenance/permissions: dataset-level scoping reported; multi-user ACL maturing [R, weak].
- Maturity 3 | self-host yes | Apache-2.0 | vendor risk medium | solo 3 | 500 3 |
  offline yes with local models | git truth no (index over files is possible [I]).
- Verdict: only graph option worth a spike as retrieval layer (d); not for truth.

## 5. LangMem
- MIT SDK from LangChain, released 18 Feb 2025, still 0.0.x (0.0.30, Oct 2025); 2026
  commits mostly dependency bumps; LangChain's newer memory docs build directly on the
  LangGraph Store [R: rywalker.com/research/langmem, atlan.com, vectorize.io].
- No storage of its own: needs a LangGraph BaseStore (Postgres, in-memory, Mongo). Memory
  types: semantic, episodic, procedural. Namespaces provide scoping [R].
- Maturity 2 | self-host yes (library) | MIT | vendor risk medium | solo 1 | 500 2 |
  offline yes | git truth no. You are not on LangGraph. Verdict: skip.

## 6. Claude Code and Anthropic native
- CLAUDE.md hierarchy: managed policy (org-wide, IT-deployed), user (~/.claude), project
  (checked in), local; plus .claude/rules/, skills, subagent memory [D: code.claude.com
  docs; summarised R: https://dev.to/naman_here/claude-code-team-memory-how-to-share-what-your-agents-learn-189c].
- Auto memory: shipped v2.1.32 (5 Feb 2026), on by default; ~/.claude/projects/<proj>/
  memory/MEMORY.md + topic files; only first 200 lines / 25 KB of MEMORY.md load at start;
  machine-local, not shared across machines/cloud [R: archcore.ai, stele-ai.dev, dev.to].
  One team disables it because it applied stale decisions [R].
- Subagent memory: `memory: project` puts it in repo at .claude/agent-memory/<agent>/
  (git-shareable, merge conflicts possible); main-session auto memory is NOT loaded into
  subagents [R].
- No native team/shared dynamic memory as of mid-2026 [R: multiple; several sources are
  vendors selling shared memory, discount accordingly]. AGENTS.md: Claude Code can read a
  repo AGENTS.md in place of CLAUDE.md (reported Oct 2026) [R: aq.dev].
- Anthropic API memory tool `memory_20250818`: client-side; Claude emits view/create/
  str_replace/insert/delete/rename on a /memories dir, YOU implement the backend; docs say
  restrict to that directory (path traversal); pairs with context editing; reportedly GA now
  [D: https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool ]. This is
  the ideal hook: you control the backend, so scope checks and provenance stamping happen in
  YOUR code, backed by git/markdown [I].
- Agent teams (experimental, env flag): lead + teammates, shared task list (pending/
  in-progress/done, deps, claiming) and per-agent mailbox JSON files under ~/.claude/teams
  and ~/.claude/tasks; no shared conversation context; one machine; CLAUDE_CODE_TASK_LIST_ID
  shares a list across sessions [D: https://code.claude.com/docs/en/agent-teams ; paths R].
- Maturity 4 (CLAUDE.md), 3 (auto memory), 2 (teams) | self-host n/a (local files) |
  proprietary client, open file formats | vendor risk: low for formats, medium for features |
  solo 4 | 500 2 | offline yes | markdown truth yes.
- Verdict: the default substrate; keep it, but treat as per-machine cache, not the
  exchange layer.

## 7. Others
- Basic Memory (basicmachines-co): local-first MCP server, markdown notes are canonical,
  SQLite/Postgres is a rebuildable index; tools write_note/read_note/edit_note/search_notes/
  build_context; semantic search via FastEmbed; Obsidian-compatible; v1.0.0 reported
  2026-10-29; AGPL-3.0 (matters if you ever host it as a service for clients; use as a
  local tool is fine) [R: mnemoverse.com, glama.ai, neoneye atlas]. Optional paid cloud sync.
  Maturity 3 | self-host yes | AGPL-3.0 | vendor risk medium (small company) | solo 4 |
  500 2 (no ACL model, AGPL) | offline yes | markdown/git truth YES.
  Verdict: closest existing match to your truth model; good candidate for (d)/(b) at solo
  tier, or as design reference.
- MCP reference `memory` server (@modelcontextprotocol/server-memory): entities/relations/
  observations in one JSONL file via MEMORY_FILE_PATH; 9 tools; whole-file rewrite under
  lock per write; no scopes, no provenance, single file [D/R: npmjs.com/package/
  @modelcontextprotocol/server-memory ; chatforest]. Maturity 3 (as reference) | MIT |
  solo 2 | 500 1 | offline yes | git truth partial (JSONL diffs ok, merge conflicts bad).
  Verdict: toy; do not build on it.
- Beads (bd; steveyegge/beads, now gastownhall/beads, MIT): dependency-aware issue graph for
  agents; hash IDs avoid multi-agent collisions; `bd ready`, `bd update --claim` atomic;
  originally SQLite + git-tracked JSONL, now Dolt backend (version-controlled SQL, cell-level
  merge) synced via Dolt remotes on refs/dolt/data of a git remote; issues.jsonl is only an
  export now; git optional; v1.3.x with a schema migration at 1.2.2->; reported migration
  data-loss bug and Dolt perf concerns (Apr 2026, medium credibility) [R: github repo,
  beads.gascity.com, martianlee.github.io, tekai.dev]. Maturity 3 | MIT | vendor risk medium
  (one famous author, project reorganised) | solo 3 | 500 2 | offline yes | git truth NO
  any more (Dolt is truth). Verdict: strong ideas (claim, hash IDs, ready-queue) for task
  layer, but you already have a markdown tree; adopting means giving up markdown truth.
- Supermemory: engine NOT open source per official site; free local binary capped ~10k docs;
  self-host in your cloud only on Scale/Enterprise; SDKs MIT; third-party pages contradict
  [R, conflicting: supermemory.ai/blog/self-hosted-ai-memory-tradeoffs/ vs
  openapps.pro]. Maturity 3 | closed core | vendor risk high | solo 2 | 500 2 | offline
  binary only | git truth no. Verdict: skip.
- OpenAI/Codex memory: ~/.codex/memories/ local markdown summarising past chats, off by
  default, per machine; Chronicle (Apr 2026) / Computer History (Aug 2026) screen-derived
  memories stored as unencrypted markdown [R: helpnetsecurity.com 2026/04/21, codex.
  danielvaughan.com]. Confirms convergence on local markdown. Codex cannot read Claude's
  auto memory and vice versa [R]. Not an exchange layer.
- Graphlit: hosted platform (ingest, knowledge graph); MCP server MIT but needs a Graphlit
  account/JWT secret [R]. Fails "no backend dependency" and "no secret in sandbox". Skip.
- AGENTS.md: open convention (now under the Agentic AI Foundation per reports) for repo-level
  agent instructions; read by Codex, Cursor, and reportedly Claude Code [R]. Static
  instructions, not memory; cheap interoperability win [I].
- Other git-native: Letta MemFS (above); Claude subagent `memory: project`; Trail-code
  (ex-Graft) "coding agent memory" turned up in search, unevaluated [R: trailhq.com];
  Hindsight and mcp-memory-service appear in lists as self-hosted alternatives, unevaluated.
  [I] No widely adopted git-native, multi-agent, permissioned memory exists. The pattern
  is converging (Letta, Claude, Codex, Basic Memory all = markdown files + index) but
  nobody has the permission/provenance layer.

## 8. Artefact / file handoff and provenance
- MCP: tools can return `resource_link` content blocks (URI the client may fetch/subscribe);
  added in spec 2025-06-18; blocks support annotations (audience, priority, lastModified);
  links need not appear in resources/list. Nothing about content addressing in the spec;
  hash-URI schemes like scrape://cache/{hash} are app conventions [D: modelcontextprotocol
  spec tools page; R: GitHub issue]. A 2026-07-28 spec revision exists and reportedly
  drops resources/subscribe [R] - check before depending on subscriptions.
- A2A (Linux Foundation / AAIF; v1.0 early 2026 [R, dates conflict]): Task output =
  Artifacts made of Parts (text, raw bytes, url, data in v1.0; older FilePart/DataPart).
  v1.0 adds optional Signed Agent Cards (JWS over JCS-canonicalised card) proving who the
  *agent* is, not who made a given artifact. Extensions list includes Timestamp/Traceability
  but arXiv 2606.31498 says none of MCP/A2A/ACP can express governance [R]. Designed for
  network-separated agents; heavy for same-box handoff [I].
- Content-addressed storage (git objects, CAS/OCI/IPFS): git already gives you this: a
  commit/blob hash is the artefact ID and is verifiable offline [I].
- Signed provenance: in-toto statements/SLSA provenance + Sigstore (keyless OIDC, Rekor log)
  can wrap any artefact; GitHub Artifact Attestations emit them [D/R]. In-toto issue #604
  proposes an AI code-generation predicate (per-range human/AI attribution, agent identity,
  prompt hash); #588/#591 propose agent action/decision predicates - all PROPOSALS, not
  merged standards [R: github.com/in-toto/attestation/issues/604]. Fast.io and TraceSeal
  describe DSSE sidecars for agent output [R, vendor blogs]. arXiv 2609.28216 warns
  attestation proves identity/origin, not correctness or authority [R].
  C2PA: media-focused; I found no use for agent text/code artefacts [I].
- In practice: I found NO evidence of agent-artefact signing being routine anywhere;
  production uses plain git commit authorship + CI attestations on build outputs [I].
  Practical minimum for you: git commit (signed with per-agent key, SSH signing) +
  trailer fields (Agent-Id, Session-Id, Task, Parent-Brief-Hash) - verifiable offline,
  no backend [I].

## 9. Security: memory poisoning
- AgentPoison (2024): poisons RAG/memory with trigger-activated demos, high success at tiny
  poison rate, assumes write access. MINJA (2025): query-only injection of malicious
  reasoning records, reportedly >95% injection success, >70% attack success; evades
  input/output moderation [R: christian-schneider.net, vectorize.io, promptfoo].
- 2026 preprints [R, arXiv IDs unverified by me]: 2606.04329 (systematic study: existing
  prompt-injection defences cover memory poisoning incompletely; more aggressive
  writers/retrievers are more exploitable), 2608.21230 (content screening and provenance
  ranking have limits; undefended arm lost ~65% of memory value), 2606.30566 (provenance
  detector relying on observable tool calls misses implicit retrieval), 2608.01759 and
  2609.00523 (composed benign memories; transferable optimisation), "Hidden in Memory"
  reporting ~99.8% adversary-memory storage on a frontier model. OWASP Agentic Top 10 (2026)
  lists Memory and Context Poisoning as ASI06 [R].
- Mitigations recommended in sources [R]: provenance on every entry (signed source), TTL/
  decay, trust score with minimum-trust retrieval threshold, anomaly scan before write-back,
  write-path gating (detect "remember for future sessions / always prefer"), immutable
  ground truth for high-impact redirects, quorum before high-impact actions, lifecycle-wide
  controls.
- [I] For your setup the structural defences matter more than detectors, since detectors
  are shown weak: (1) memory writes by an agent go to a quarantine/proposed tier, promoted
  by a human or a higher-trust agent via PR/commit; (2) provenance tag on every entry
  (agent id, session id, source = own work | read-from-web | read-from-other-agent) and
  never auto-trust the last; (3) retrieved memory is injected as data with its tier labelled,
  never in system/CLAUDE.md position; (4) write scopes by path (agent may write only its
  task dir + its own memory dir; cross-scope writes via the manager); (5) git history gives
  you rollback and blame - a real advantage over DB stores; (6) persistent memory loaded
  into sessions that hold tools/credentials is the risky case; your no-secret-in-sandbox
  rule already caps blast radius.

## Scored table (1-5; higher better; "vendor" 5 = low single-vendor risk)
| Candidate         | Mat | Self-host | License   | Vendor | Solo | 500 | Offline | MD/git truth |
|-------------------|-----|-----------|-----------|--------|------|-----|---------|--------------|
| Letta             | 3   | yes       | Apache-2  | 2      | 2    | 2   | yes     | partial      |
| Mem0 (OSS lib)    | 3   | lib only  | Apache-2  | 2      | 2    | 2   | yes*    | no           |
| Graphiti          | 3-4 | yes+DB    | Apache-2  | 3      | 2    | 3   | w/ local LLM | no      |
| Zep CE            | 1   | dead      | Apache-2  | 1      | 1    | 1   | n/a     | no           |
| Cognee            | 3   | yes       | Apache-2  | 3      | 3    | 3   | yes*    | no           |
| LangMem           | 2   | lib       | MIT       | 3      | 1    | 2   | yes     | no           |
| Claude CLAUDE.md/ | 4   | local     | prop/open | 3      | 4    | 2   | yes     | yes          |
|  auto memory      |     |           | formats   |        |      |     |         |              |
| API memory tool   | 4   | you build | n/a       | 3      | 4    | 4   | yes     | yes (yours)  |
| Agent teams       | 2   | local     | prop      | 3      | 2    | 1   | yes     | no (JSON)    |
| Basic Memory      | 3   | yes       | AGPL-3    | 3      | 4    | 2   | yes     | YES          |
| MCP memory server | 3   | yes       | MIT       | 4      | 2    | 1   | yes     | partial      |
| Beads             | 3   | yes       | MIT       | 3      | 3    | 2   | yes     | no (Dolt)    |
| Supermemory       | 3   | binary    | closed    | 1      | 2    | 2   | binary  | no           |
| Graphlit          | 3   | no        | MIT shim  | 1      | 1    | 2   | no      | no           |
| A2A artifacts     | 3-4 | protocol  | Apache-2  | 4      | 1    | 3   | n/a     | no           |
| in-toto/Sigstore  | 4   | yes       | Apache-2  | 5      | 2    | 5   | in-toto yes; keyless no | yes (sidecar) |
* with Ollama/local models.

## Recommendation by sub-layer
(a) Structured task context (checkpoints/briefs/outcomes): BUILD ON WHAT YOU HAVE. Keep the
    markdown task tree in git as truth. Nothing evaluated fits: Beads gives claiming and
    ready-queues but moves truth into Dolt; Claude agent teams' task list is one-machine
    JSON. Add: front-matter schema (id, author_agent, session_id, parent_brief, status,
    trust_tier), per-agent signed commits, and a thin CLI that enforces path-scope writes.
    Steal Beads' hash IDs (no collisions across parallel agents) and atomic claim.
(b) Shared knowledge/memory store: BUILD (thin) on markdown + git, exposed through the
    Anthropic memory-tool-style interface / an MCP server of your own so scope checks and
    provenance stamping happen in your code. Reference designs: Letta MemFS (branch/merge
    memory) and Basic Memory (markdown canonical, rebuildable index). Entries: proposed ->
    accepted tiers. Do not adopt Mem0/Zep/Supermemory/Graphlit (backend dependency, lossy
    LLM rewrites, open-core retreat). At 500 people revisit: Postgres projection with
    row-level tenancy and erasure on top of the same files (git history makes GDPR erasure
    hard: plan per-tenant repos or encrypted blobs with key deletion [I]).
(c) Artefact handoff with provenance: BUILD ON GIT. Artefact = path + git blob/commit hash;
    handoff = brief containing the hash; provenance = signed commit + trailers; later add
    in-toto/DSSE attestations through Sigstore for the client tier. MCP resource_link is the
    transport to the agent; A2A only when agents are across network boundaries (k8s/clients).
    No agent-artefact signing standard is mainstream; in-toto predicates for AI are drafts.
(d) Retrieval/search: Tier 1 ripgrep + front-matter filters (free, offline). Tier 2 a
    rebuildable local index (SQLite FTS5 + embeddings) like Basic Memory's; Basic Memory
    itself is the cheapest adoptable (mind AGPL if you ever host it for clients). Tier 3
    spike Cognee or Graphiti as a derived index if cross-document/temporal questions
    become common. Index must be disposable and respect the same scope checks on read
    (filter before retrieval, not after) [I].

Overall: "build: nothing fits" for the permission + provenance + offline combination; no
evaluated product has path-scoped agent permissions with git as truth. The build is small
(schema, signed commits, scope-enforcing wrapper, proposal/promotion flow), reusing Basic
Memory/Letta ideas and Claude's native files.

## Caveats
- Vendor "alternatives" blogs dominate search results and contradict each other (Supermemory
  license, Mem0 graph, Beads storage). Primary docs for Letta, Anthropic memory tool, Claude
  agent teams, MCP spec and Graphiti were surfaced but not read in full by me.
- arXiv IDs 2606-2609 come from search snippets; I did not open the papers.
- Not covered: Hindsight, mcp-memory-service, Trail-code, Zep Cloud's ACLs, GDPR specifics
  of each tool. Secret handling: none of the hosted options fit "no secret in sandbox"
  without a credential-injecting proxy [I].
