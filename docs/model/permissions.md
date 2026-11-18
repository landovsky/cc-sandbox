---
type: analysis
status: draft
title: "Area B: permission grants for agents (subjects, inheritance, consent, the policy decision API)"
task: ai-tooling:#54bc (parent ai-tooling:#62b5, milestone ai-tooling:#6rrd)
date: 2026-11-18
inputs: docs/unified-session-model.md, docs/cc-flow-on-kubernetes.md, doc 68 in full (dotfiles c84519e,
  §3, §5, 25-dynamic-consent-resume.md), docs/model/orchestration.md (E), ccsp PROPOSAL rev 3,
  remote-control-security-stash.md, claude/settings/*.json, cc-flow.md "Autonomy levels", 68-node-settings.md
evidence: docs/model/evidence/permissions-survey.md (web survey, 2026-11-18)
---

# B. Permission grants for agents

**In one line.** A grant is a signed, expiring record "subject may do action on resource,
within constraints", where the subject is a place in the tree (milestone → task → session) so
grants flow down it. One **embedded Cedar evaluator** (`ccgrant`) answers every "may it?" for the
MCP gateway (A), the hub (C) and the orchestrator (E), offline, from files. **Ceilings are
`forbid` policies that no grant can override**, so most requests never reach a human. Enforcement
is always outside the fence: the PDP only decides, gateways and launchers act. On the `local` tier
there is no real consent (the agent is Tomas's uid and could answer its own ❓); consent becomes
enforceable from the `sandbox` tier up.

Tags: [documented] vendor docs or repo · [reported] third party · [inferred] mine · [observed] seen
on hp today. Survey detail and URLs: [evidence/permissions-survey.md](evidence/permissions-survey.md).

## 1. What Tomas has today

| # | Piece | Where | State |
|---|---|---|---|
| T1 | **Bypass everywhere.** Every `claude!` adds `--dangerously-skip-permissions`; base settings hold 95 `allow` rules and an **empty `deny`**, so under bypass the allowlist is decoration | `zsh/cc-flow.zsh:38`, `~/.dotfiles/claude/settings/base.json`, map §4 | live |
| T2 | **Autonomy as prose.** L1–L4 levels × gates (infra, spend, deploy), Tomas's words; 11 milestones in 5 trees carry an `AUTONOMY: L3/L4. May fix alone: <paths>… no infra, no spend; red-flag files via #2cgx; bigger → Tomas` block in their Outcome. That block *is* a grant set (allowed paths, forbidden gates, a routing rule), enforced by nobody | `cc-flow.md` "Autonomy levels"; `grep 'AUTONOMY: L' ~/cc-tree/*/tree.md` | live, prose only |
| T3 | **Deploy autonomy**: an agent may merge+push to `~/git/k3s` main when `scripts/deploy-scope` says `OWN`/`INERT`; `SHARED` stops. A real, fail-closed classifier, run by the agent itself | `~/git/k3s/docs/DEPLOY-AUTONOMY.md`, `scripts/deploy-scope` | live; doc 68 ❓6 recommends turning it off until a merge bot exists |
| T4 | **ccsp**: base-URL pin in root-owned managed settings, redaction, PreToolUse guards (upload, secret-loss write, self-protection). Decides about *content*, not about *who may do what* | `~/git/cc-secret-proxy` (PROPOSAL rev 3, D1–D19) | pin and hook live on hp [observed: `host-hp-ubuntu.json`, unified doc §4 ¹]; README still says "nothing installed", stale |
| T5 | **Consent contract, designed**: `cc-req` → `AccessRequest` (requester+generation, platform-set provenance, scope, ttl, idempotency key, phases, `scopeHash`, signed decision) → tiers auto/low/medium/high/never → ❓ card → enforcer applies with its own expiry; anti-patterns (self-approval, relayed authority, fatigue, TOCTOU, replay, injected request) each with a test; `AccessPolicy` YAML with `maxTTL {session: 24h, milestone: 7d}` | doc 68 §5, `68-k8s-blueprint/25-dynamic-consent-resume.md` | design; egress- and kube-centric |
| T6 | **❓ queue**: decision nodes in tree.md, answered as `'<id> <option>'` on the terminal or web Tree (128 ❓ in ai-tooling alone) | cc-tree, cc-tree-web | live; the answer path is reachable by every local agent |
| T7 | **Credentials**: 25 keys in every shell, gh OAuth, kubeconfigs, a Google service account for the SEO skill (`GOOGLE_APPLICATION_CREDENTIALS`), project-scoped MCP servers (whimsical, graphiti, gtm, magnific) plus the github plugin | `zsh/.zshrc`, `claude/skills/seo/scripts/google_auth.py`, `~/.claude.json` | live; doc 68 §3.4 lists the fixes |
| T8 | **Node settings**: `Model/Refresh/Hear/Dir/Agent` lines; only `Dir:` inherits; "Autonomy L1–L4" is a reserved slot | `68-node-settings.md` | `Model:` live, registry draft |
| T9 | **Artifacts** have stable ids `art:<5>` from a canonical locator (git path, URL, PR) and a node | doc 53 §5.3 | live (md), registry draft |
| T10 | **Rules as a tree** (#5sd4): rules are stored by *place*, agents are defined by role, instance, grant, harness, machine; "boundary enforcement belongs to #3bw3/#232x, 5sd4 owns the rule text" | `agent-instructions-observation.md`, #5sd4 checkpoints | observation; this doc is the enforcement half |

What is missing is not a policy language but **a subject**: nothing today can say "this session"
or "this milestone" to an enforcer. The tree has the addresses; no enforcer reads them.

## 2. What B must do in the unified model

**Object model.** Cedar entity types (schema in the evidence file); `in` means "inherits from".

```
Principals  Human::"tomas"                         (later: Keycloak users, tenant admins)
            Tree::"ai-tooling"
            Node::"ai-tooling:6rrd"   in Tree / parent Node      kind milestone|task|area, autonomy 1-4
            Session::"ai-tooling:54bc/g3"  in Node               attrs: harness, host, role, gen, billing, started_by
Resources   Repo, Ref (in Repo), App (GitOps), McpServer, McpTool (in McpServer),
            Artifact::"art:k3f9x" (in Node), GDoc::"<drive file id>", Host (egress),
            Namespace (k8s), CredentialRoute::"gh-push" (a gateway route name, never a value),
            Address (a node as a message target), Budget::"<milestone>"
Actions     mcp.call · git.push · git.merge · deploy · egress · credential.use · message.send ·
            context.read · artifact.read|write · gdoc.read|comment|edit · session.start|stop ·
            spend · grant.request · grant.approve (Human only, by schema)
```

**Requirements**, each with the invariant it serves:

| # | Requirement | Invariant |
|---|---|---|
| R1 | **Subjects are tree places.** A grant names a Session (dies with it), a task Node (survives fresh starts and rolls) or a milestone Node (covers every task and session under it). Harness and role are attributes the policy may test, not subjects | address is the identity (unified §1 rule 1) |
| R2 | **Inherit down, never up or sideways.** A milestone grant reaches its tasks' sessions; nothing a session asks for widens its node | consent by a human |
| R3 | **Ceilings before prompts.** `forbid` policies (the "never" tier, plus per-tree caps such as "no `deploy` to SHARED", "no domain-wide Google delegation") win over every `permit`; the auto tier is a `permit` set checked before any ❓ exists | consent by a human (doc 68 P6: ~93 % of prompts get approved) |
| R4 | **Every grant expires at the enforcer.** Caps: session ≤ 24 h or session end, task ≤ 7 d, milestone ≤ 30 d (doc 68 says 7 d, ❓ B-2); permanent = a reviewed policy PR (doc 68 P5). The PDP checks `expires` itself, so a dead controller can't extend a grant | — |
| R5 | **Revocation on session end and on generation bump.** SessionEnd / container exit / pod drain → `revoke --session`; a grant bound to `g3` is refused for `g4` even inside its TTL (doc 68 T-ID3) | tmux owns lifecycle (the pane's exit is the signal on local/sandbox) |
| R6 | **Only a human approves**, on an authenticated surface (D), with a signed decision bound to `scopeHash` and a nonce. No agent, manager or service account holds `grant.approve`; authority never travels as text (doc 68 P3, P4) | consent by a human |
| R7 | **Works offline.** The evaluator is a library + CLI reading policy and grant files from local disk; `local` and `sandbox` starts never wait for a service | sessions don't depend on the backend |
| R8 | **No credential in a grant.** A grant names a `CredentialRoute`; the gateway holds the value and mints the short-lived token (GitHub App, Google SA, OpenBao lease) | no secret inside the fence |
| R9 | **Audit**: every decision (counts and ids, never values) and every grant event; approval rate and prompts/day measured (alarm > 90 %, ≤ 5 prompts/day target, doc 68) | — |
| R10 | **Enforcement outside the fence.** Hooks inside a fence are a speed bump (bypass mode, Bash can do anything); the real PEPs are the MCP gateway, ccsp-egress / git route, hub, launcher | no secret inside the fence |
| R11 | **The prose becomes data.** Today's `AUTONOMY:` blocks map to grants: "may fix alone: <paths>" → `git.push` with `context.path_classes ⊆ {own}`; "no infra / no spend" → `forbid deploy` (SHARED) and `forbid spend`; "red-flag via #2cgx" → `path_class red_flag` ⇒ ask (the dev manager can't approve, only route). L1–L4 becomes `Node.autonomy`, inherited | mechanism over prompt (ADR-0007) |

**Tier honesty.** On `local` the agent runs as uid `tomas`, can edit `~/cc-tree`, call `cc-tree
answer` and reach every credential (T1, T7). There B is **recorded consent**: same decisions, same
❓, the audit flags any approval whose process tree descends from a Claude session, but nothing
stops a determined agent. On `sandbox` the grant store, policies and answer path aren't mounted
into the fence, the fence's only ways out are PEPs, and consent is **enforced**. That is a reason
to move workers to `sandbox` (E's ❓ E-1), not a reason to build less.

## 3. Outside solutions

The survey ran on 2026-11-18 as a Sonnet subagent with web access only. Its context was this
brief's requirements list and no repo files. Every row below is in the evidence file with a URL.
No external model (Gemini/GPT) was used.

**Scores** are 1–5 and higher is better. *Vendor* means low single-vendor risk. *Offline* means
it embeds or runs with no database on a laptop. *Fit* is solo → 500.

| Sub-layer | Candidate | State (2026-11) | Mat. | Offline | Licence | Vendor | Fit | Note |
|---|---|---|---|---|---|---|---|---|
| **Policy engine (PDP)** | **Cedar** | cedar-policy 4.15.0 (11-16); **cedar-go 1.9.3 (11-18)**, which has datetime/duration and schema validation but no templates [documented]; CNCF Sandbox 2025-10 [reported]; behind AWS AgentCore Policy at the MCP gateway (GA 2026-03, forbid wins, tool = action) [reported]; NVIDIA OpenShell is adopting it [reported] | 4 | 5 | Apache-2 | 3 | 5 | **Forbid always wins**, entity parents give tree inheritance, and an SMT analyser (Rust) can prove ceilings. You must bring your own grant store and expiry |
| | OpenFGA | 1.22.1 (10-28), CNCF Incubating, Okta+Grafana maintainers [documented/reported] | 5 | 4 | Apache-2 | 4 | 4 | Native ReBAC; ListObjects suits "what may X see". Expiry needs CEL plus your own reaper; production needs a DB. **Runner-up**, for when many humans share objects |
| | Cerbos | 0.57.0 (10-23), single company, open-core; Synapse + agentgateway for MCP; AuthZEN contributor [documented/reported] | 4 | 4 | Apache-2 | 2 | 3 | The best MCP story, but there is no hierarchy: the tree has to become attributes. In-process use needs Hub WASM (commercial) |
| | OPA / Rego | 1.22.4 (11-12), CNCF Graduated; creators went to Apple in 2025-08 and Styra wound down [documented] | 5 | 5 | Apache-2 | 3 | 3 | Fits anything, but nothing in it models grants or trees; you write a policy engine in Rego |
| | SpiceDB | 1.57.1 (11-10), AuthZed [documented] | 5 | 2 | Apache-2 | 2 | 4 | Has the only **native expiring relationships**, but it needs a server and a DB, and expired rows are GC'd after 24 h |
| | dropped | Oso (OSS archived 2026-10-22), Topaz (Aserto shut down), Ory Keto (OSS lagging Enterprise), cedar-agent (unverified), Permit.io (SaaS control plane) [documented/reported] | — | — | — | — | — | — |
| **Decision wire format** | **AuthZEN Access Evaluation 1.0** | final 2026-01 [documented] | 4 | — | open spec | 5 | 5 | `check` in §4 is AuthZEN-shaped (subject, action, resource, context), so A can swap in any engine or gateway that speaks it |
| **Grant store + consent workflow** | build | Nothing self-hostable ships request → human approve → expiring grant for *arbitrary* resources. Teleport Access Requests are Enterprise-only and for infra; OpenBao control groups cover secrets only; k8s has no AccessRequest CRD [documented/reported] | — | — | — | — | — | **Build: nothing fits, because the subject is a tree address.** Borrow CIBA's `binding_message` + `requested_expiry` (final spec) for the company IdP |
| **Credentials: GitHub** | **GitHub App installation tokens** | 1 h; narrowed to repos + permissions at mint, never widened [documented] | 5 | — | — | 3 | 5 | The gateway mints one per grant. Ref rules stay at the gateway plus GitHub rulesets |
| **Credentials: Google** | **service account + per-file share** | headless least privilege is to share exactly one file with the SA's email. `drive.file` covers only files the app created or the user picked in the Picker; **domain-wide delegation is far too broad** [documented] | 5 | — | — | 3 | 4 | Company tier: per-user OAuth `drive.file` + Picker. Per-permission `expirationTime` not verified |
| **Credentials: dynamic** | **OpenBao** (company tier) | 2.7.2 (10-26), MPL-2, LF/OpenSSF; leases, revoke-by-prefix, `control_group` approval [documented] | 3 | 3 | MPL-2 | 4 | 4 | Solo needs none (SOPS + gateway). Vault is BUSL/IBM: no |
| **Agent-identity products** | none | Auth0 for AI Agents (Token Vault, CIBA, FGA for RAG), Descope, Stytch (Twilio), Keycard, Arcade, WorkOS: SaaS. Aembit authz is cloud-only. Astrix belongs to Cisco since 2026-06. Entra Agent ID / Okta XAA are tenant-bound [documented/reported] | — | 1 | prop. | 1 | 2 | Rule them out for solo and for EU self-host. Okta XAA / ID-JAG (IETF draft -04) is worth watching only if a client brings Okta |

**RECOMMENDED 3RD PARTY, per sub-layer**
- **PDP: Cedar via cedar-go**, embedded in `ccgrant`. Run the Rust CLI and analyser (`cedar validate`, symcc) in CI only.
  Why: (1) forbid-wins is exactly the ceiling rule; (2) the tree maps onto entity parents with no
  graph store; (3) it works offline with policies as files in git; (4) we can *prove* things such as "no
  grant reaches `deploy` on SHARED". cedar-go has no templates, so each grant compiles to one
  generated policy (`permit(principal in Node::"…", action == …, resource == …) when
  { context.now < datetime("…") && principal.gen == 3 }`), which also keeps grants readable in review.
- **Wire: AuthZEN 1.0** shape for `check`.
- **Grant store, consent, revocation: build** (B1, B4–B6). It is small: an append-only JSONL file plus a fold.
- **Credentials:** GitHub App (solo, now), a Google SA with per-file shares (solo), and OpenBao (company).
- **Runner-up PDP: OpenFGA**, if the company tier ever needs relationship queries across thousands
  of human-shared objects. Grants stay records in our store, so switching changes only B2.

## 4. Contracts with the other four areas

One API, three transports: **library/CLI** (`ccgrant check …`, offline), **Unix socket**
`/run/ccf/ccgrant.sock` on the host (never mounted into a fence), **HTTP** on the cluster
(company tier). Callers are PEPs outside the fence; the fence reaches B only through `cc-req`
(E's agent-door spool) or the gateway.

```
check(principal, action, resource, context) -> {decision: allow|deny|ask, tier: auto|low|medium|high|never,
        grant_ids[], policy_ids[], cache_ttl_s (≤ 30, ≤ grant expiry), ask_hint: {scope, max_ttl, subject_choices[]}}
   context: {now, provenance: human|delivery|scheduler|untrusted (platform-set, never agent-set),
             gen, amount?, path_classes[]?, deploy_scope?: OWN|INERT|SHARED, args_digest?}
allowed(principal, action, resource_type) -> [resource ids]        # filters MCP tools/list, hub targets
grant.resolve(profile, address, role) -> ceiling_id                # E calls at launch (orchestration.md §4)
grant.request(AccessRequest) -> {id: "ar-…", phase: AutoApproved|Pending|Denied, q_node?}   # doc 68 shape
grant.decide({request_id, scopeHash, decision, narrowed_to?, subject, ttl, approver, channel, nonce, sig,
        proof: {kind: header|otp|webauthn, assertion}}) -> grant_id     # proof per surfaces.md §4
revoke({grant_id} | {session, gen} | {node}) ;  freeze() / thaw()  # break-glass: deny all but auto
events: grant.applied | grant.expired | grant.revoked | decision.denied   -> PEPs drop caches
```

| Area | What it calls on B | What B calls on it | Data crossing |
|---|---|---|---|
| **A. MCP gateway** | `allowed(session, mcp.call, McpTool)` at session connect to shape `tools/list`; `check` per `tools/call`; `grant.request` when a call is `ask` | `register_catalog(server, tools[{name, annotations, pinned_hash}])` so tools become entities with a default tier | tool risk default from MCP annotations (`readOnlyHint` → auto, `destructiveHint` → medium, unregistered server → never); A pins the descriptions (rug-pull defence is A's) |
| **C. Hub / context** | `check(session, message.send, Address)` per envelope; `check(session, context.read, Artifact|Node)` per read | none | default policy: worker → own manager and siblings in its milestone; manager → its workers; anyone → Tomas; cross-tree or cross-milestone = grant |
| **D. Surfaces** | render ❓ grant cards from `grant.request`; post `grant.decide` with a `proof` B checks against the tier: `header` (Tailscale identity + nonce) only for **low** and only for fenced requesters; **medium and high need `webauthn`** (or a Telegram `otp` for medium), as surfaces.md asks; Keycloak later | none | the card: exact scope, subject choices (this session / task / milestone, narrowest preselected), TTL, the agent's reason **labelled agent text**, provenance in red when untrusted |
| **E. Orchestration** | `grant.resolve` at launch (`Grants: <profile>` → `grants_ref` in the LaunchSpec); `grant.request` from the agent door; `revoke({session, gen})` on SessionEnd / pane exit / drain; `check(session, session.start, Node)` for manager starts (≤ 3/min, own milestone) | `resolve(address) -> SessionRecord {gen, role, host, harness, started_by}` and the node's parent chain + `autonomy` (CC Flow is the entity source; B caches) | E owns the ❓ node as UI and log; **B owns the authoritative grant record** |
| **Gateways (ccsp-model / -egress, git route)** | `check(session, git.push, Ref, {path_classes, deploy_scope})`, `check(…, egress, Host)`, `check(…, credential.use, CredentialRoute)` | none | B returns *which route and scope*; the gateway mints the token (GitHub App downscoped to the grant's repos and permissions, 1 h) |

## 5. Build list delta (on top of unified-session-model §3 items 0–7 and orchestration E1–E9)

| # | Piece | Size | Order / needs | Works when |
|---|---|---|---|---|
| B1 | **Grant record + store**: append-only `<tree>/grants-<machine>.jsonl` in the tree repo (git = history, syncs Mac↔hp, offline), folded into an active set; fields: id, subject, action, resource, constraints, expires, request_id, decision{approver, channel, at, nonce, sig}, events | S | first | `ccgrant list --node ai-tooling:54bc` shows a hand-written grant; an expired one is listed as expired, not active |
| B2 | **`ccgrant` PDP**: Cedar evaluator (CLI + host socket); policy dir split `never/` (forbid), `auto/`, `ask-{low,medium,high}/`, `profiles/`; entities built from tree.md parent chains + live SessionRecords; `check`, `explain`, `allowed`, `--help` with recipes | S–M | after B1 | `ccgrant check session:ai-tooling:54bc/g1 git.push ref:cc-sandbox/main` → deny + the forbid policy id; offline, < 5 ms |
| B3 | **Policy tests in CI**: one allow and one block test per policy; Cedar's analyser proves "no permit reaches a `never` action"; a mutation (drop a forbid) must turn the suite red | S | with B2 | deleting `never/deploy-shared.cedar` fails `make test` |
| B4 | **Session-end revocation**: SessionEnd hook + `ccsbx` exit trap + E's generation bump call `revoke` | XS | after B2, with unified item 3 | after `/exit`, `ccgrant list --session …` shows every session grant `revoked:session-end` |
| B5 | **`cc-req` + ❓ grant card**: agent writes an AccessRequest to E's door spool; host side runs ceilings → auto grant or a ❓ node under the task (`cc-tree add --kind ❓`); `cc-tree answer` / web Tree → `grant.decide` | S | after B2, E4 | a sandboxed worker's `cc-req mcp.call github/create_pull_request --ttl 4h` shows a ❓; `'<id> 1'` creates the grant; the next call passes |
| B6 | **Decision proof**: cc-tree-web signs decisions with a host key the fence can't read, binding the login and the `proof` (D's D4 passkey); terminal answers count as `header` proof, so they can approve **low** only (a local agent shares Tomas's ssh-agent); B2 refuses unsigned or under-proved grants on `sandbox`+ | S | after B5, with D4 | a grant line edited by hand is refused with `bad-signature`; one from the web passes |
| B7 | **First PEPs**: the PreToolUse hook calls `ccgrant check` for `mcp__*` tools and `git push` (speed bump on local); the git route in ccsp-egress checks `git.push` with `path_classes` from the diff (own / red-flag / other) and `deploy_scope` from `scripts/deploy-scope` | S + S | after B2; the git route after unified item 5 | a push touching a red-flag file returns `ask`; an OWN deploy passes; a SHARED one is refused without a prompt |
| B8 | **Prose → data**: convert the 11 `AUTONOMY:` blocks into profiles + node grants; `Autonomy:` and `Grants:` join the node-settings registry, inherited milestone → task | S | after B2; Tomas reviews each conversion | `ccgrant explain` for each milestone reproduces its prose block line by line |
| B9 | **Settings compile**: `ccgrant compile --session` writes the session's `permissions.deny` into its managed settings at spawn (sandbox: baked into the read-only file), so tool-level denials hold even in bypass mode ² | S | after B2, unified item 1 | a denied MCP tool is refused by Claude Code itself in a sandbox |
| B10 | **Google per-file route**: one CC Flow service account, key only at the gateway; Tomas shares a file with it in Drive (that share is the human consent), plus a `GDoc` grant per node; DWD forbidden by a `never` policy | S | after A's gateway | an agent edits exactly the shared doc; a second doc in the same folder is refused at the gateway |
| B11 | **Audit digest**: decisions log (no values), approval rate, prompts/day, top asked scopes; "same scope approved 3× in 14 d" proposes a profile PR | S | after B5 | the daily digest line appears in ccsp's report |
| B12 | **Company tier**: grant store → Postgres behind the same API, Keycloak humans, tenant admins approve (koperníci never do), OpenBao for dynamic credentials | L | at a client | out of scope now |

**Minimum for "a fenced agent asks and Tomas grants"**: B1 + B2 + B4 + B5 (S + S–M + XS + S),
on top of unified items 1–3 and E4. **First real enforcement**: B7's git route (needs item 5).

² Verify first: that `permissions.deny` and PreToolUse denials still apply under
`--dangerously-skip-permissions` [inferred from Claude Code's settings docs, not tested here].

## 6. Risks and conflicts

**Top 3 risks**

1. **Consent theatre on `local`.** Same uid, bypass mode, a writable tree: an injected agent can
   file, approve and use its own grant. Mitigation: say so on every card ("recorded, not
   enforced"), flag approvals from agent process trees, and treat `sandbox` as the tier where B
   counts; B6's signatures make forged grants detectable even on local.
2. **Our own security code on the path** (doc 68 MR-08/MR-11): a wrong policy or a fold bug
   grants too much, silently. Mitigation: Cedar (a verified, analysable language) instead of
   bespoke YAML evaluation; forbid-wins ceilings; B3's block/allow/mutation tests; a small PDP
   with no network listener on the fence side.
3. **PEPs drift apart.** Five enforcers (gateway, egress, hub, launcher, hook) with caches can
   disagree after a revoke. Mitigation: one API, cache ≤ 30 s and ≤ grant expiry, revocation
   events, and the PDP checks `expires` itself; T-AR3-style test (controller down, grant still
   expires).

**CONFLICTS**

> Merged and resolved (or turned into ❓) in [unified-model.md §4](../unified-model.md#4-conflicts); build items renumbered in §5.

- **Doc 68 `AccessPolicy` YAML → Cedar.** Doc 68 (25 §2.3) has its own `autoApprove / deny /
  requireHuman / stepUp / limits` YAML and a grant controller that reads it. I keep the
  `AccessRequest` contract, the tiers and the anti-pattern tests as they are. I replace the YAML
  evaluator with Cedar policy sets (`never/`, `auto/`, `ask-*/`). The YAML's `limits` (rates,
  prompts/day) stay as counters in `ccgrant`, because Cedar is stateless. I also widen doc 68's
  scope kinds (egress, credential, resources, config, toolchain, template, kube) with mcp.call,
  git.push, deploy, message.send, context.read, artifact, gdoc and spend.
- **Doc 68 TTL caps.** Doc 68 caps a milestone grant at 7 d and I propose 30 d. Tasks keep 7 d.
  A milestone like scratchpad (#22td) runs for weeks, and re-asking every 7 d is the fatigue that
  P6 warns about. ❓ B-2.
- **Doc 68 enforcement is network-only.** Its grants apply at egress or kube. Mine also need
  *path-aware* git checks (`path_classes`, `deploy_scope`) at the git route. Doc 68 already has an
  explicit git route in ccsp-egress (K12), so this adds a check on that route and no new component.
- **orchestration.md (E) §2: "CC Flow is where consent is asked and recorded".** Agreed for the
  ❓ node. The **authoritative** grant record and its signature must be B's (`grants-*.jsonl`),
  because E's history line is writable by the session's own uid on `local`. The ❓ node is UI and
  log. E's `grant.decide(…, by=human)` needs `scopeHash, nonce, sig, proof` (surfaces.md says the
  same thing). E also has `grant.resolve` return a single `ceiling_id`. I return a ceiling id
  *plus* the profile's grant ids, so A can shape `tools/list` without a second call.
- **surfaces.md (D): medium tier.** I take D's correction: medium needs a passkey or an OTP. My
  first draft asked for WebAuthn only on high.
- **ccsp PROPOSAL rule "zero modules".** cedar-go is a module, so the PDP is its own binary
  (`ccgrant`), not part of `ccsp serve`. ccsp-egress calls it over the socket.
- **Harness doc invariant "a single designated approval responder per session".** It still holds
  for *tool prompts* inside the harness. *Grant* approval is always a human on a surface, never the
  manager, even when the manager is the session's responder.
- **A (mcp-gateway.md)** was not written when I committed (only its two surveys were). I
  assumed A shapes `tools/list` per session and pins tool descriptions. If A puts a policy engine
  in the gateway itself (Cerbos via agentgateway, Pomerium policy), that engine must call
  `ccgrant` or be fed from it, so that tool grants have one source.

## 7. Decisions for Tomas (❓ nodes under ai-tooling:#54bc)

1. **B-1 Policy engine** (#55ca). (1) Cedar via cedar-go, embedded in `ccgrant`, with the grant store built by us (rec). (2) OpenFGA server with SQLite/Postgres. (3) Cerbos sidecar.
2. **B-2 Milestone grant TTL cap** (#6dvg). (1) 30 d (rec). (2) 7 d as in doc 68. Tasks stay at 7 d and sessions at 24 h in both options.
3. **B-3 `local` tier** (#6a6q). (1) Run grants there as recorded, unenforced consent, labelled on every card, with the audit flagging approvals made from agent process trees (rec). (2) Refuse any non-auto grant on `local`, so asking moves the work to `sandbox`. (3) Wait for host-uid.
4. **B-4 Autonomy prose → grants** (#4srd). (1) After B2, convert the 11 `AUTONOMY:` blocks into profiles and node grants. You review each one, and the prose stays as the rationale (rec). (2) Leave them as prose.
