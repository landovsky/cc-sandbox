[Index](../../README.md) › [Unified model](../../unified-model.md) › [Permissions](../permissions.md) › permissions-survey

# Permissions / authz survey for agent grants (evidence)

Date of research: 2026-11-18. Web only, read-only. Tags: [documented] vendor docs/repo page fetched;
[reported] third-party or search-snippet only; [inferred] my deduction or background knowledge, not
re-verified today. "NOT VERIFIED" = could not confirm. Version/date facts marked documented come from
GitHub release pages fetched today.

## Cross-cutting findings (read first)
- No engine below has a native "session ends -> revoke subtree" primitive. Closest: SpiceDB expiring
  relationships + delete-by-filter, OpenBao/Vault lease revoke-by-prefix, Teleport cert TTL. [inferred]
- Only SpiceDB (native `expiration`, v1.40+) and OpenFGA (CEL condition + `current_time` in request)
  give data-level expiry in a ReBAC graph. Cedar/OPA/Cerbos need time passed in context, and expiry
  of the grant itself is a policy-store concern. [documented] see sections.
- Cedar and OPA are libraries/sidecars with no required DB: best fit for laptop/offline. SpiceDB and
  OpenFGA are servers that need a DB for production (memory mode for dev only). [documented]
- Standards in motion: MCP auth spec rev 2026-07-28; AuthZEN 1.0 final (Jan 2026); IETF ID-JAG draft
  -04; WIMSE AI-agent drafts adopted Sep 2026. All are about token issuance/PDP wire format, none
  define a grant tree. [documented/reported below]

---------------------------------------------------------------------------------------------------
## A. Policy / authorization engines

### A1. OpenFGA
- Version: v1.22.1, 2026-10-28; v1.22.0 2026-10-06 (adds MySQL read replicas). [documented] https://github.com/openfga/openfga/releases
- License: Apache-2.0. [documented] https://github.com/openfga/openfga
- Governance: CNCF Incubating (TOC move 2025-10-28; blog 2026-01-07); originated at Auth0/Okta, maintained by Okta + Grafana staff. [reported] https://openfga.dev/blog/incubation-announcement ; https://presentations.cncf.io/projects/openfga/
- Self-host: yes. Embedded vs server: server (HTTP 8080/gRPC 8081); "can also be embedded as a Go library" (pkg/server). [documented] https://github.com/openfga/openfga
- Datastores: in-memory default (dev only, lost on stop), Postgres 14+, MySQL 8, SQLite (beta). DB needed for anything durable. [documented] https://github.com/openfga/openfga
- Hierarchy: yes, Zanzibar ReBAC (parent relations, tuple-to-userset), so milestone->task->session inheritance is a native model. [documented-by-design] https://openfga.dev/docs/modeling ; [inferred] for our tree.
- Time-bound/conditional: CEL conditions on tuples; docs example `current_time < grant_time + grant_duration` with timestamp/duration types; stored tuple context wins over request context. NO native tuple TTL: tuple stays in store, caller must send current_time. Limits: 32KB tuple context, CEL cost 100. [documented] https://openfga.dev/docs/modeling/conditions
- 2026 news: "Dynamic Conditions" experimental in v1.21.0 (2026-09-20); weighted-graph resolution change. [documented] releases page above; [reported] https://openfga.dev/blog/archive
- Consistency/latency: no zookie-style token in my evidence; NOT VERIFIED today. [inferred] eventual with caching; check docs before relying.
- Agent/MCP features: none native; FGA is Auth0's hosted flavour with "FGA for RAG". [reported] https://auth0.com/blog/auth0-for-ai-agents-generally-available/
- Risks: low abandonment risk (CNCF); single-vendor contributor concentration (Okta/Grafana). Revocation = delete tuples (own job). [inferred]

### A2. SpiceDB (AuthZed)
- Version: v1.57.1, 2026-11-10 (fixes for expiration, cache TTL); v1.57.0 2026-10-20. [documented] https://github.com/authzed/spicedb/releases
- License: Apache-2.0. [documented] https://github.com/authzed/spicedb
- Governance: AuthZed (single company) project, "community project" wording, DCO, no foundation; contributors incl. GitHub (MySQL), Netflix (caveats). [documented] https://github.com/authzed/spicedb
- Self-host: yes (binaries, containers). Embedded: no library mode documented; a memdb datastore exists (docker-compose.memdb.yaml in repo; `--datastore-engine memory` is background knowledge). Prod datastores: Spanner, CockroachDB, Postgres, MySQL. [documented] repo page; memory flag [inferred]
- Hierarchy: yes, Zanzibar schema language, recursive relations/arrows. [documented-by-design] https://authzed.com/docs/spicedb/concepts/schema ; [inferred] for our tree.
- Time-bound: first-class expiring relationships since v1.40.0 (released Jan 2025): `use expiration`, `with expiration`, `OptionalExpiresAt`, `zed --expiration-time`. Expiry judged by datastore clock. Expired rels stop counting immediately but are GC'd after default 24h window (Postgres/MySQL GC every 5 min). Caveats (CEL, context at check) also exist; caveat-expiry was never GC'd. [documented] https://authzed.com/docs/spicedb/concepts/expiring-relationships ; https://github.com/authzed/spicedb/releases/tag/v1.40.0
- Consistency: configurable per request (minimize_latency ... fully_consistent) with zedtokens; cache can still return a result for an expired rel within the cache window unless fully_consistent. [documented] expiring-relationships page above
- Agent/MCP: AuthZed publishes "How SpiceDB secures authorization for AI" (RAG-oriented); no MCP gateway product found. [reported] https://authzed.com/blog/spicedb-secures-authorization-for-ai
- Ops weight: heaviest of the ReBAC pair for prod (needs Postgres/CRDB); single binary for dev. [inferred]
- Risks: single-company stewardship; some features (e.g. enterprise) gated to AuthZed Dedicated/Enterprise (NOT VERIFIED which). Expiry works on OSS per docs. [documented] expiring page says OSS/Cloud/Dedicated/Enterprise.

### A3. Cedar (language) + cedar-agent + Amazon Verified Permissions
- Version: cedar-policy v4.15.0, 2026-11-16 (CLI binaries added). [documented] https://github.com/cedar-policy/cedar/releases
- License: Apache-2.0 (reported; AWS open-sourced 2023-05-10). [reported] https://www.infoq.com/news/2026/01/cedar-joins-cncf-sandbox/
- Governance: accepted to CNCF Sandbox 2025-10-08 (InfoQ article dated 2026-01), originated at AWS. Answer to "Cedar joining CNCF?": yes, Sandbox. [reported] https://infoq.com/news/2026/01/cedar-joins-cncf-sandbox/
- Embedded: yes, Rust library (also bindings), no server or DB required. [inferred from crate model] https://github.com/cedar-policy/cedar
- Hierarchy: entity hierarchy with `in` (parent groups) is core; grants inherit via entity parents supplied in the request/entity store. [documented-by-design] https://docs.cedarpolicy.com/ ; [inferred] for tree.
- Time-bound: no native grant expiry; time is a `context` value and policies use `when` conditions. RFC issue #112 discusses an `@expires` annotation and says expiry is a policy-store concern. [documented] https://github.com/cedar-policy/rfcs/issues/112 ; [reported] https://aws.amazon.com/blogs/security/migrating-from-open-policy-agent-to-amazon-verified-permissions/
- Dogwood: Cedar-compatible superset with session-history temporal operators (since/once/count_within); NOT wall-clock expiry. Supported in AgentCore Policy. [reported] https://aws.amazon.com/blogs/opensource/introducing-dogwood-runtime-verification-for-ai-agents/
- cedar-agent (Permit.io): standalone Cedar HTTP agent, Apache-2.0; repo shows 84 commits, no releases listed, not archived; last-commit date and Cedar version NOT VERIFIED. [documented] https://github.com/permitio/cedar-agent ; OPAL integration https://docs.opal.ac/tutorials/cedar/
- AWS Verified Permissions: managed Cedar PDP (AWS-only SaaS); reference design for multi-hop MCP agent chains 2026-07-06. [reported] https://aws.amazon.com/blogs/security/ (not individually fetched)
- AgentCore Policy (Cedar at MCP gateway): preview 2025-12-02, GA 2026-03-03, default-deny, forbid wins, JWT sub = principal, tool name = action. AWS-managed only. [reported] https://medium.com/@tolghn/amazon-bedrock-agentcore-policy-cedar-control-what-your-ai-agents-can-do-3313e6f3a2db ; docs https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/policy-core-concepts.html
- Related: NVIDIA OpenShell donated to CNCF 2026-11-10 and adopting Cedar (still YAML today). [reported] https://www.qovery.com/blog/openshell-cncf-agent-sandbox-vs-deployment-governance
- Latency: in-process evaluation, microseconds-scale (formal-verification/perf claims from AWS). NOT VERIFIED numerically today.
- Risks: needs own entity store + revocation/expiry layer; Sandbox maturity; Dogwood young. [inferred]

### A4. OPA / Rego
- Version: v1.22.4, 2026-11-12 (Go HTTP/1 CVE rebuild); v1.22.3 11-10; v1.22.2 11-06. [documented] https://github.com/open-policy-agent/opa/releases
- License: Apache-2.0 (no change reported). [reported] https://druce.ai/governance/wiki/vendors/open-policy-agent
- Governance: CNCF Graduated. 2025-08-20: creators (Koponen, Hinrichs, Sandall) and many Styra staff joined Apple; framed as acqui-hire, not formal acquisition; Styra wound down, styra.com dead (as of 2026-07-27), Styra DAS sunset, EOPA/OPA Control Plane/Regal/SDKs donated to CNCF OPA org. [documented] https://www.openpolicyagent.org/blog/note-from-teemu-tim-and-torin-to-the-open-policy-agent-community-2dbbfe494371 ; [reported] https://cloudnativenow.com/features/apple-buys-styra-brains-opa-remains-open/ ; https://ttlab.hopto.org/blog/2026-07-17-opa-post-styra-apple-acquihire-reality?lang=en
- Embedded: yes (Go library, WASM, REST sidecar); no DB. [inferred] https://www.openpolicyagent.org/docs
- Hierarchy: not a data model; you encode trees as data documents/Rego rules. [inferred]
- Time-bound: `time.now_ns()` in Rego or data bundles; expiry of grants is your data's job. [inferred]
- Agent/MCP: ecosystem integrations (gateways call OPA); "Rego in the hook" pattern for Claude Code hooks. [reported] https://www.ajayk.sh/blog/policy-as-code-guardrails-ai-agents-2026/
- Risks: core maintainers now at Apple, community-driven roadmap; releases still frequent through Nov 2026. [reported] ttlab link above

### A5. Permit.io + OPAL
- OPAL: latest stable 0.9.9 (2026-08-27); 0.10.0-rc.6 on 2026-11-10 (pre-release). Apache-2.0. [documented] https://github.com/permitio/opal/releases ; license [reported] https://www.permit.io/blog/top-open-source-authorization-tools-for-enterprises-in-2026
- What it is: OPAL pushes policy+data updates to OPA/Cedar agents in real time; Permit.io is the commercial SaaS control plane (also "Nexus PDP" on SurrealDB/NATS/OPA, license NOT VERIFIED). [reported] https://www.permit.io/ ; https://docs.opal.ac/tutorials/cedar/
- Governance: single company (Permit.io). Embedded: sidecar/agent, not in-process. Hierarchy/time-bound: inherits from underlying engine. [inferred]
- Risks: vendor-led, SaaS upsell; useful mainly for the live-update push pattern. [inferred]

### A6. Cerbos
- Version: v0.57.0, 2026-10-23; v0.56.0 09-30; v0.55.0 08-13. [documented] https://github.com/cerbos/cerbos/releases
- License: Apache-2.0 for the PDP. [documented] https://github.com/cerbos/cerbos
- Governance: single company (Cerbos), open-core; Cerbos Hub is the commercial control plane with an Embedded PDP (WASM) bundle for browser/edge. [documented] https://github.com/cerbos/cerbos
- Self-host/embedded: stateless PDP as sidecar, service or DaemonSet; policies on disk/git/object store/DB; no DB required. True in-process embedding only via Hub's WASM bundle. [documented] https://docs.cerbos.dev/cerbos/latest/deployment/
- Hierarchy: RBAC + derived roles + conditions on principal/resource attributes; trees must be modelled in attributes. [documented] repo page; [inferred] for grant inheritance.
- Time-bound: via conditions on request attributes (time passed by caller). [inferred]
- Agent/MCP: strongest PDP story: Cerbos Synapse (2026-03-20) plugs into agentgateway via Envoy ext_authz for model requests and MCP tool calls; Tailscale integration 2026-02-17; Claude Code tool-call interception write-ups; AuthZEN contributor. [reported] https://www.cerbos.dev/blog/governing-ai-agents-at-the-gateway-with-cerbos-and-agentgateway ; https://www.cerbos.dev/blog/mcp-authorization
- Risks: attribute-based (you supply grants as data per call), so it does not store grants itself. [inferred]

### A7. Topaz (Aserto)
- Company: Aserto announced wind-down 2025-04-27, SaaS sunset by 2025-05-31; unprofitable hosted model. [documented] https://www.aserto.com/blog/the-final-chapter-for-aserto
- Topaz: Apache-2.0, not archived, active: v0.33.28 on 2026-11-14 (bumps OPA 1.22.4). Helm chart stale (0.2.5, Apr 2025). [documented] https://github.com/aserto-dev/topaz/releases ; https://github.com/aserto-dev/topaz ; [reported] https://artifacthub.io/packages/helm/topaz/topaz
- What: OPA decisions + built-in Zanzibar-style directory (RBAC/ABAC/ReBAC). Single binary, embedded SQLite-like store (NOT VERIFIED which store). [documented] repo page
- Governance: no backing company; small maintainer group (bus-factor risk). Agent/MCP features: none found. [reported] https://idsync.com/directory/aserto

### A8. Oso
- Open-source Polar library: deprecated 2023-12-18; repo osohq/oso archived 2026-10-22 (read-only, Apache-2.0, "no new releases or bug fixes"). [documented] https://github.com/osohq/oso ; https://www.osohq.com/docs/oss/project/changelogs/2023-12-18.html
- Oso Cloud: proprietary SaaS (SDKs Apache-2.0), has "local authorization". Acquisition by Tiny Ltd reported complete 2026-10-01 with team joining Temporal (single dev.to source, NOT corroborated). [reported] https://www.osohq.com/post/local-authorization ; https://dev.to/authbyexample1/how-oso-polar-policies-map-to-rbac-rebac-and-abac-if-youre-planning-a-move-2ke4
- Verdict: not a candidate for self-hosted; both OSS dead and ownership in flux. [inferred]

### A9. Ory Keto
- Version: OSS repo latest release v26.2.0, 2026-03-20; Ory changelog shows v26.3.5 (2026-08) and v26.4.16 (2026-11-12) shipped to Ory Network/Enterprise License first "and will be part of the next Ory Open Source release". [documented] https://github.com/ory/keto/releases ; https://changelog.ory.com/announcements/ory-hydra-ory-kratos-ory-keto-ory-polis-ory-elements-v26-4-16-released
- License: Apache-2.0 OSS; Ory Enterprise License for self-hosted prod with SLAs. [reported] https://www.ory.com/keto
- Governance: single company (Ory). Zanzibar tuples + OPL; SQL datastores (Postgres/CRDB/Yugabyte/SQLite/MySQL). No native expiry/caveats found. [reported] changelog link ; [inferred] no expiry
- Risk: OSS lagging enterprise (open-core drift); a SQL-injection advisory GO-2026-4800 via forged pagination tokens listed on pkg.go.dev (affected range NOT VERIFIED). [reported] https://pkg.go.dev/github.com/ory/keto

### A10. Agent/MCP-authorization standards (2025-2026)
- MCP authorization: current revision 2026-07-28. MCP server = OAuth 2.1 resource server; RFC 9728 PRM required; RFC 8707 `resource` required; CIMD (draft-03) preferred; DCR deprecated; RFC 9207 issuer check; step-up via 403 insufficient_scope; stdio transports should NOT use it (use env credentials); auth is OPTIONAL. Enterprise-Managed Authorization extension (June 2026, ext-auth repo). [documented] https://modelcontextprotocol.io/specification/draft/basic/authorization ; extension [reported] https://scalekit.com/blog/mcp-authorization-guide
- Implication: spec covers token issuance to one MCP server, not per-tool or per-argument grants, not human-consent grant trees. [inferred; Cerbos makes the same argument] https://www.cerbos.dev/blog/mcp-authorization-standards
- AuthZEN Authorization API: final Jan 2026 (PDP wire format: subject/action/resource/context); COAZ-MCP profile drafts dated 2026-02-13. Good fit for our "policy decision API called by MCP gateway". [reported] https://www.cerbos.dev/blog/mcp-authorization-standards
- IETF: draft-ietf-wimse-aims-00 (agent auth framework); draft-klrc-aiagent-auth adopted by WIMSE 2026-09-09 (reported; -03); WIMSE interim 2026-06-03; OAuth WG rechartered June 2026 with agents-on-behalf-of in scope; draft-niyikiza-oauth-attenuating-agent-tokens (attenuation = ceilings idea); draft-sweeney-wimse-credential-delegation (delegation server issuing/revoking tokens). All are drafts, not RFCs. [reported] https://datatracker.ietf.org/doc/draft-klrc-aiagent-auth/01/ ; https://datatracker.ietf.org/doc/html/draft-ietf-wimse-aims-00 ; https://datatracker.ietf.org/doc/draft-sweeney-wimse-credential-delegation/
- ID-JAG (Okta XAA basis): IETF OAuth WG draft -04 dated 2026-05-21, expires 2026-11-22, not an RFC. [reported] https://guptadeepak.com/ciam-compass/changelog/cross-app-access-converges-2026/
- OpenID Foundation: AI Identity Management CG whitepaper "Identity Management for Agentic AI" published 2025-10-07 (no 2026 edition found); presented at ITU-T 2026-03-31. [reported] https://openid.net/new-whitepaper-tackles-ai-agent-identity-challenges/ ; https://arxiv.org/abs/2510.25819
- CIBA (OpenID, final 2021-09-01): poll/ping/push, `requested_expiry`, `binding_message`. [documented] https://openid.net/specs/openid-client-initiated-backchannel-authentication-core-1_0.html

---------------------------------------------------------------------------------------------------
## B. Credential side

### B1. OpenBao (vs HashiCorp Vault)
- OpenBao: v2.7.2, 2026-10-26 (security patch); v2.7.0 2026-09-23; v2.6 announced 2026-08-06. MPL-2.0. [documented] https://github.com/openbao/openbao/releases ; https://openbao.org/community/release-notes/2-7-0/ ; license https://github.com/openbao/openbao
- Governance: Linux Foundation / OpenSSF project (Sandbox), open governance docs (GOVERNANCE/CHARTER/TSC). [documented] https://github.com/openbao/openbao ; https://openbao.org/community/release-notes/2-7-0/
- Features relevant: dynamic credentials, leases, renewal, revoke incl. whole trees (`revoke-prefix`); v2.7 adds `control_group` ACL stanza (second-party approval for a path = human-in-the-loop), per-namespace sealing (2.6), Postgres read-scaling, PebbleDB single-node backend (file backend removed), strong-consistency headers. [documented] repo + release notes above
- Weight: server process, `bao server -dev` for laptop (in-memory-ness NOT VERIFIED); prod wants Raft/Postgres + unseal. [documented] repo; [inferred] ops
- Agent features: none native. Risks: young fork, plugin extraction churn (cloud seals, LDAP moved to external plugins), breaking Go module path. [documented] release notes
- Vault: BUSL-1.1 since v1.15 (2023-08-10); IBM completed HashiCorp acquisition 2025-02-27, LICENSE now "IBM Corp"; no reversal found. Version unclear: sources disagree (2.1.0 on 2026-09-01 vs 2.2.2 on 2026-11-12) NOT VERIFIED. HCP Vault Secrets EOL 2026-07-01. [reported] https://startwithidentity.com/vendors/machine-identity/hashicorp-vault/ ; https://guptadeepak.com/top-5-secrets-management-tools-hashicorp-vault-aws-doppler-infisical-and-azure-key-vault-compared/
- Lease facts (Vault/OpenBao shared lineage): default_ttl/max_ttl per role, 32-day system default if unset, revoke-prefix. [reported] same Vault sources.

### B2. GitHub
- GitHub App installation tokens expire 1 hour; request-time narrowing via `repositories` (<=500) / `repository_ids` and `permissions`; can only narrow, never widen; omitted = all installation repos and all app permissions. Authenticated by app JWT. [documented] https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
- Token format change: stateless `ghs_APPID_JWT` rollout began 2026-04-27 (breaks 40-char assumptions). [documented] same page
- Revocation of a token before expiry: `DELETE /installation/token` (page defers to other doc; NOT re-verified today). [inferred] https://docs.github.com/en/rest/apps/installations
- Fine-grained PAT: bound to one owner, selected repos, expiry; up to 50 per user; recommended only for one-off/personal tooling. [reported] https://nhimg.org/github-apps-vs-pats-vs-oauth-apps-guide
- Best fit: one GitHub App per deployment; our broker mints a per-session token scoped to one repo + minimal permissions, TTL <=1h, re-mint to extend. [inferred]

### B3. Google Drive / Docs, least privilege for exactly one document
- `drive.file` scope (non-sensitive): per-file access to files the app created or the user opened/picked via Google Picker; works with all Drive REST resources. Per-user grant, so sharing a file with someone else doesn't extend the app's token. [documented] https://developers.google.com/workspace/drive/api/guides/api-specific-auth
- Broad scopes `drive`, `drive.readonly`, `drive.metadata` are restricted (security assessment for public apps). [documented] same page
- Service account: own identity and own Drive, not a Workspace member; sees only what is shared with its email. Sharing the single file to the SA email is the least-privilege headless route (role reader/commenter/writer; revocation = delete permission via Drive permissions API). [reported] https://discuss.google.dev/t/google-drive-sheets-api-with-drive-file-scope-on-shared-folder-files/402266 ; revocation call [inferred]
- Domain-wide delegation: SA impersonates any user within admin-approved scopes, "without their direct consent", propagation up to 24h; far broader than needed. [documented] https://developers.google.com/identity/protocols/oauth2/service-account
- No true folder- or file-scoped OAuth token minted per grant in Drive; forum answer: folder sharing is the closest. [reported] discuss.google.dev link above
- Recommendation (inferred): per-doc grant = Drive permission for the SA (or a per-task SA) with `drive.file`-class scopes where possible; expire by a reaper that deletes the permission (Drive permissions support `expirationTime`, background knowledge, NOT re-verified today). Docs API needs its own `documents` scope in addition (not covered in the page fetched).

---------------------------------------------------------------------------------------------------
## C. Agent-specific products

### C1. Auth0 for AI Agents (Okta)
- GA announced 2025-11-19: User Auth, Token Vault (35+ integrations, refresh handled), Async Authorization (CIBA; Guardian push live, email/SMS "coming soon" at launch), FGA for RAG. Auth for MCP in Early Access at launch; reported GA 2026-05-21 with OBO token exchange. XAA requesting-app half in early access 2026-08-31. [documented] https://auth0.com/blog/auth0-for-ai-agents-generally-available/ ; later dates [reported] https://www.agentmail.to/blog/ai-agent-authentication-platforms-compared-2026 ; https://guptadeepak.com/ciam-compass/changelog/cross-app-access-converges-2026/
- SaaS-only: yes (Auth0 tenant; FGA self-host path is OpenFGA). Pricing from third-party lists only. [reported]

### C2. Descope
- Agentic Identity Hub 2.0 (2026-01-26), 2.5 (2026-06-09: step-up via CIBA, time-bound tokens, headless agents, standalone MCP auth), 3.0 reported 2026-10-15 (gateway-embedded MCP auth, ID-JAG; single source). XAA/ID-JAG validation 2026-09-01. [documented] https://www.descope.com/press-release/agentic-identity-hub-2.5 ; https://www.helpnetsecurity.com/2026/01/27/descope-agentic-identity-hub-2-0/ ; 3.0 [reported] https://forkast.news/identity-as-the-moat-descope-embeds-mcp-native-auth-into-the-gateway-layer/
- SaaS-only: appears so (self-host NOT VERIFIED).

### C3. Stytch
- Acquired by Twilio: announced 2025-10-30, closed 2025-11-14; no immediate contract/API changes. Connected Apps (OAuth 2.1/OIDC authorization server for agents and MCP servers incl. XAA, consent UI, revocation) still updated: 2026-09-25 changelog for MCP spec 2026-07-28. [reported] https://changelog.stytch.com/ ; https://workos.com/blog/best-mcp-server-authentication-providers
- SaaS-only: yes. Risk: roadmap under Twilio (reported by competitor WorkOS; treat as biased).

### C4. Aembit
- IAM for Agentic AI GA; MCP Identity Gateway (data plane) managed or self-hosted on Linux; MCP Authorization Server is managed in Aembit Cloud only (not self-hostable); gateway 1.34.6034 (2026-10-07) supports MCP rev 2026-07-28. Secretless credential exchange. [documented] https://docs.aembit.io/ai-guide/mcp/identity-gateway/concepts-mcp-gateway/ ; https://docs.aembit.io/changelog/
- Mostly SaaS control plane. Self-host partial.

### C5. Astrix
- Cisco announced intent to acquire 2026-05-04, closed 2026-06-29 (~$400M reported by Calcalist, undisclosed by Cisco); standalone new-license sales ended 2026-06-30. [reported] https://securityboulevard.com/2026/05/cisco-fortifies-ai-defenses-with-acquisition-of-astrix-security/ ; https://blogs.cisco.com/news/cisco-announces-intent-to-acquire-astrix-security
- Enterprise discovery/governance SaaS; not a grant engine. Effectively unavailable standalone: not a candidate. [reported]

### C6. Others
- Keycard: runtime auth for agents, per-task short-lived scoped tokens, policy-gated, integrates Claude/Codex/MCP; out of stealth 2025-10, raised $38M (reported); free tier 5k transactions/mo. SaaS (self-host NOT VERIFIED). [reported] https://www.keycard.ai/ ; https://www.helpnetsecurity.com/2025/10/22/keycard-ai-agents-identity-access-platform/
- Arcade.dev: per-user OAuth token vault, hosted tool execution, governed MCP gateways; hosted (self-host NOT VERIFIED). [reported] https://www.arcade.dev/blog/best-ai-agent-authentication-platforms/ (vendor-biased)
- WorkOS AuthKit for MCP: OAuth 2.1 AS in front of your MCP server, SSO/SCIM, fine-grained authz; SaaS. [reported] https://www.firecrawl.dev/blog/best-ai-agent-authentication-platforms
- Okta Cross App Access (XAA, ID-JAG): Agent SSO GA 2026-08-24; IdP-policy-mediated app-to-app token for agents; SaaS/IdP-bound. [reported] https://guptadeepak.com/ciam-compass/changelog/cross-app-access-converges-2026/
- Microsoft Entra Agent ID: GA (Learn says "now generally available"; April 2026 per third party); agent identity = special service principal created from a blueprint; Entra-tenant only; not ID-JAG. 2026-11-04 default setting lets all users create blueprints (LazyAdmin). [documented] https://learn.microsoft.com/en-us/entra/agent-id/whats-new-agent-id ; https://learn.microsoft.com/en-us/entra/agent-id/agent-identities ; [reported] https://lazyadmin.nl/office-365/microsoft-entra-lets-users-create-agent-blueprints-by-default/
- Teleport: MCP access via app-access agent, role-based tool allow-lists (`mcp.tools`), tbot machine identity, no long-lived secrets; JIT Access Requests are Enterprise; whether MCP/tool-level requests work, and audit/recording for MCP, NOT VERIFIED (older blog says server-level only). [documented] https://goteleport.com/docs/machine-workload-identity/access-guides/mcp/ ; [reported] https://goteleport.com/blog/granular-seamless-jit-access-with-teleport/
- Pomerium: OSS identity-aware proxy; per-tool `mcp_tool` policy criterion; service-account JWT for headless agents; MCP support described as experimental mid-2026; no approval workflow. [reported] https://www.pomerium.com/docs/capabilities/mcp/delegate-mcp-to-llm ; https://www.pomerium.com/blog/top-5-agentic-gateways-for-securing-mcp-tool-calls-in-2026 (vendor)
- Which are SaaS-only: Auth0, Descope, Stytch, Keycard, Arcade (likely), WorkOS, Okta XAA, Entra, Astrix. Self-hostable/partial: Aembit gateway, Teleport, Pomerium. [inferred from above]

---------------------------------------------------------------------------------------------------
## D. Human-in-the-loop approval with expiring grant: established patterns
- CIBA: agent-initiated back-channel request to human's device, `binding_message`, `requested_expiry`, poll/ping/push. Used by Auth0 Async Authorization and Descope step-up. IETF WIMSE agent draft recommends CIBA step-up. [documented] https://openid.net/specs/openid-client-initiated-backchannel-authentication-core-1_0.html ; [reported] https://datatracker.ietf.org/doc/draft-klrc-aiagent-auth/01/
- MCP step-up: 403 insufficient_scope + WWW-Authenticate scope, client re-authorizes; no consent record/expiry semantics beyond the token. [documented] https://modelcontextprotocol.io/specification/draft/basic/authorization
- Teleport Access Requests: request role/resource, reviewers approve, short-lived certs, request TTL default 1h, reviewer can shorten; Enterprise feature; per-pod RFD 101 creates temporary Role/RoleBinding and removes at expiry. [reported] https://goteleport.com/docs/identity-governance/access-requests/access-request-configuration/ ; https://fossies.org/linux/teleport/rfd/0101-pod-rbac.md
- OpenBao control groups (2.7): second-party approval gate on a path. [documented] https://openbao.org/community/release-notes/2-7-0/. Vault Enterprise has the equivalent under the same name (background; NOT re-verified).
- Kubernetes: no native AccessRequest CRD found. Expiry-only operator exists (RBAC Expiry Operator, no approval). Pattern to adopt: own CRD `AccessRequest` -> controller -> creates RoleBinding with expiry, reaps on TTL/session end. [reported] https://nikogura.com/projects/rbac-expiry-operator/ ; design [inferred]
- SpiceDB-native flavour: approval writes a relationship with `expiration`; session end deletes by filter. [inferred from] https://authzed.com/docs/spicedb/concepts/expiring-relationships
- Just-in-time access from other vendors (Boundary-style brokers, cloud PAM): not researched (a term in the brief was removed by the secret-redaction proxy). NOT VERIFIED.

---------------------------------------------------------------------------------------------------
## E. Fit notes for our requirements (inferred, for design discussion)
- Laptop/offline + embedded: Cedar (crate), OPA (lib/WASM), OpenFGA (Go lib, memory/SQLite) are strongest; SpiceDB needs a process (memdb for dev); Cerbos works as a local sidecar binary with disk policies.
- Native expiry in the engine: SpiceDB (best), OpenFGA (via CEL + request time; needs reaper to delete tuples).
- Tree inheritance (milestone->task->session): ReBAC engines (SpiceDB/OpenFGA/Keto/Topaz) native; Cedar via entity parents; Cerbos/OPA via attributes/data.
- Ceilings: no engine has "child grant <= parent grant" as a primitive; model as a forbid/intersection check or attenuation (cf. draft-niyikiza-oauth-attenuating-agent-tokens). 
- PDP API to MCP gateway: AuthZEN is the standard wire format; Cerbos Synapse and AgentCore show the pattern.
- Avoid: Oso (OSS archived), Aserto SaaS (shut), Astrix standalone, Vault (BUSL/IBM) when OpenBao suffices.
