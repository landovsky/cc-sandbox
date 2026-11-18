[Index](../../README.md) › [Unified model](../../unified-model.md) › [MCP gateway](../mcp-gateway.md) › mcp-gateway-survey-1

# MCP gateway survey 1 (2026-11-18)

Scope: gateways fronting ALL MCP servers for sandboxed coding agents (no internet, placeholder creds).
Required: outbound credential injection, OAuth refresh-token holding, stdio spawn in isolation,
per-session tool allowlist, tools/call audit, response redaction, tool-poisoning / rug-pull defence.
Tags: [documented] vendor docs/repo, [reported] third party, [inferred] my deduction.
Method caveat: facts came from WebFetch/WebSearch summaries of pages, not from reading source or running anything. Verify before relying.
Stars/dates are as seen on 2026-11-18.

## 1. Docker MCP Gateway (docker/mcp-gateway)
- Version: v0.44.3, 2026-10-26, flagged pre-release (v0.44.2 2026-10-21, v0.44.1 2026-09-23). [documented] https://api.github.com/repos/docker/mcp-gateway/releases
- ~1.6k stars, 1,063 commits, MIT, single vendor (Docker). [documented] https://github.com/docker/mcp-gateway
- Self-host: yes; CLI plugin, works on Docker CE (`DOCKER_MCP_IN_CONTAINER=1`, `docker mcp feature enable profiles`). [documented]
- Transports: stdio default, streaming HTTP via `--port 8080 --transport streaming`. Each catalog server runs in its own container; npx/uvx servers get minimal host privileges. [documented]
- Credential injection: `docker mcp secret set` stores secrets, injected into the server container at runtime, not env/config. Store is Docker Desktop secrets; headless-Linux backing store unclear. [documented]/[inferred]
- OAuth: `docker mcp oauth` built-in flows for catalog servers; refresh-token location not documented in what I read (Docker Desktop credential store [inferred]).
- Per-client tool allowlist: per-profile `docker mcp profile tools <id> --enable/--disable <server>.<tool>`. Caller identity is the profile you launch the gateway with, not a token mapped to a session. One gateway process per profile is the workable pattern [inferred]. [documented]
- Audit: `--log-calls` logs every tool call. [documented/reported] https://dasroot.net/posts/2026/01/docker-mcp-gateway-interceptors-security/
- Redaction: `--block-secrets` scans inbound and outbound payloads; blocks, may not mask. [reported]
- Poisoning/rug-pull: `--verify-signatures` covers server IMAGE signatures only. No description pinning found. Third-party comparison says "detects tool poisoning: No". Custom interceptors (scripts) are the extension point. [reported] https://ailearningguides.com/mcp-scan-vs-docker-mcp-gateway-2026/
- Limits: `--block-network`, `--cpus`, `--memory` per tool container. [reported] https://www.docker.com/blog/mcp-horror-stories-github-prompt-injection/
- Kubernetes: not covered in README; Docker-host product. [documented]
- CVEs: none found for the gateway itself. [reported]
- Best-in-class for stdio isolation; weakest on multi-user identity and OAuth for remote servers.

## 2. IBM ContextForge (IBM/mcp-context-forge)
- Version: v1.1.1, 2026-11-10 (v1.1.0 2026-10-20, v1.0.11 2026-09-28). [documented] https://api.github.com/repos/IBM/mcp-context-forge/releases
- ~4.9k stars, 3.4k commits, 731 open issues, Apache-2.0, IBM org project with DCO, no foundation. [documented] https://github.com/IBM/mcp-context-forge
- Self-host: yes (Docker, Podman, Helm).
- Transports: SSE, streamable HTTP, WebSocket, JSON-RPC. Stdio is wrapped via `mcpgateway.translate` (stdio to SSE/HTTP), so the gateway process runs the stdio child. Per-server container isolation is NOT built in [inferred]. Also REST/gRPC to MCP, A2A. [documented]
- Credential injection: per-gateway auth (header/bearer/OAuth) stored encrypted with `AUTH_ENCRYPTION_SECRET`. [documented]
- OAuth refresh tokens: stored per gateway AND per ContextForge user in the DB (`app_user_email`), auto-refresh near expiry; client secrets encrypted. 1.0.9 added pluggable token storage with HashiCorp Vault backend. All OSS. [documented] https://ibm.github.io/mcp-context-forge/manage/oauth/ ; [reported] https://www.speakeasy.com/blog/speakeasy-vs-ibm-contextforge
- Tool allowlist: "virtual servers" bundle chosen tools; teams + RBAC; caller identified by JWT (team-scoped tokens). A virtual server per session class works. [documented]/[inferred]
- Audit: structured logs, admin UI viewer/export; no dedicated tamper-evident audit subsystem seen. [documented]
- Redaction: plugin framework (40+ plugins): PII Filter (mask, tool_post_invoke), Secrets Detection, guardrail plugins. [reported] https://ibm.github.io/mcp-context-forge/using/plugins/plugins/
- Rug-pull/pinning: no description-change detection found. Open question. [not found]
- Kubernetes: Helm chart `charts/mcp-stack` with HPA, Redis HA, OTel, NetworkPolicy; cloud guides. [documented]
- CVEs 2026 (many): CVE-2026-53709 (9.8, hardcoded default JWT secret enabling forged admin tokens, fixed 1.0.2); CVE-2026-53710 (10.0, python_sandbox_server subproject RCE); CVE-2026-53708 (SSRF, fixed 1.0.3); CVE-2026-8355 (SSRF in OAuth registration token_url, affects <=1.0.5, 2026-10-20); more GHSAs (BOLA on plugin bindings, session auth ignoring JWT exp). [reported] https://github.com/IBM/mcp-context-forge/security , https://github.com/advisories/GHSA-6f4r-8687-5x9p
- Feature-richest OSS option, large attack surface and weak security track record; README itself says run untrusted servers sandboxed.

## 3. Microsoft MCP Gateway (microsoft/mcp-gateway)
- Version: v1.0.1, 2026-11-16, security fix (tool calls no longer follow HTTP redirects from tool pods; "all deployments should update"). [documented] https://api.github.com/repos/microsoft/mcp-gateway/releases
- ~923 stars, 69 commits, MIT, single vendor. [documented] https://github.com/microsoft/mcp-gateway
- Shape: reverse proxy/router + control plane for hosting MCP servers as K8s pods (StatefulSets, headless svc), metadata in Redis/Cosmos DB; Azure Bicep to AKS. Now requires MCP 2026-07-28 clients and is stateless (no session routing). [documented]
- Auth: Entra ID bearer + app roles; anonymous in dev. No outbound credential injection or OAuth-client/refresh-token store described (only managed/workload identity, shared `X-Gateway-Secret`). [documented]
- Tool allowlist: none gateway-wide; resource-level read/write roles. [documented]
- Audit, redaction, pinning: not described. [documented gap]
- Stdio spawn: not a stdio-spawner; it runs containerised HTTP servers on K8s. [inferred]
- CVE-2026-109492 Elevation of Privilege (CWE-639, CVSS 8.8, Important, 2026-11-10). [reported] https://msrc.microsoft.com/update-guide/vulnerability/CVE-2026-109492
- Not a fit: Azure/Entra-centric hosting router, none of the credential/OAuth/guardrail features.

## 4. agentgateway (Linux Foundation, now AAIF)
- Version: v1.7.0, 2026-10-27 (rc.1 the day before). [documented] https://api.github.com/repos/agentgateway/agentgateway/releases
- ~6.3k stars, 3.3k commits, Apache-2.0, Rust. Created by Solo.io, donated to LF 2025-08-25; docs say AAIF project in 2026. Solo sells a separate Enterprise edition. [documented] https://github.com/agentgateway/agentgateway ; https://agentgateway.dev/blog/2025-08-25-solo-contributes-agentgateway-to-lf/
- Self-host: yes, standalone binary (YAML) or K8s controller (Gateway API).
- Transports: stdio, SSE, streamable HTTP, OpenAPI-to-MCP; virtual/federated multiplexed backends with per-target policies. Stdio children run as plain processes, no per-server container [inferred]. [documented]
- Caller auth: JWT, API key, OAuth (MCP auth broker/proxy is OSS). Caller identified by JWT claims / API-key metadata, so a per-session token carrying claims maps well. [documented] https://docs.solo.io/agentgateway/latest/about/overview/
- Tool allowlist: CEL `mcpAuthorization` per tool/prompt/resource, `tools/list` filtered by the same rules. OSS. [documented] https://agentgateway.dev/docs/standalone/latest/mcp/
- Outbound credentials:
  - OSS: static secret injection and claim-based key mapping (per user/team) on backend auth. [reported] https://github.com/rvennam/agentgateway-auth-patterns
  - OSS RFC 8693 / RFC 7523 / Entra OBO token exchange announced 2026-07-12 as "upcoming release"; v1.7.0 inclusion unverified. [documented] https://agentgateway.dev/blog/2026-07-12-agentgateway-token-exchange-jwt-assertion-entra-obo/
- Refresh tokens: OSS holds NO persistent upstream token store (only an in-memory exchanged-token cache). Solo Enterprise's brokered STS stores per-user upstream tokens in Postgres (SQLite for dev) and does "double OAuth" (SSO, then upstream consent). Paid. Third-party SaaS MCP servers (Whimsical, Notion, Atlassian) therefore need Enterprise or a separate token broker. [reported]
- Audit/OTel: tracing and logs of MCP calls (OTel). [documented, details not verified]
- Redaction: guardrail processors, in-process CEL (reject/mask) or remote ExtMCP policy server that inspects/mutates requests and responses; regex, OpenAI moderation, Bedrock, Model Armor, webhooks. OSS. [documented] https://agentgateway.dev/docs/standalone/latest/mcp/guardrails/
- Poisoning/rug-pull: docs claim defence against tampering, shadowing, rug pull; Solo Enterprise adds tool-server fingerprinting and versioning. OSS description-hash pinning not confirmed (guardrails page does not name it). [reported] https://druce.ai/governance/wiki/vendors/agentgateway ; https://www.solo.io/press-releases/enterprise-agentgateway-mcp-labs
- Kubernetes: first-class (controller, Gateway API, Helm). [documented]
- CVEs: none found in my searches (not exhaustively checked). [not found]
- Strongest OSS policy engine and solo-to-K8s scaling; weak in OSS on refresh-token custody and stdio isolation. Needs a companion.

## 5. Envoy AI Gateway, now Agent Router (AAIF)
- Rename confirmed: announced 2026-09-09, effective 2026-09-10; moves from Envoy sub-project to standalone AAIF project; CRDs, `aigw` CLI and API group unchanged. [documented] https://aaif.io/blog/agent-router-joins-aaif ; https://github.com/aaif/project-proposals/issues/18
- Versions: release page lists v1.2.1 (2026-10-27, security/stability) and also v1.3.0 dated 2026-11-24, which is AFTER today; treat as page error or timezone/typo. Latest reliably dated: v1.2.1. [documented, inconsistent] https://theagentrouter.ai/release-notes/
- Apache-2.0; 9 maintainer seats shared by Bloomberg, Nutanix, AMD, Tetrate, Netflix, none majority; 132 contributors/21 orgs. Best governance of the five. Stars not retrieved. [documented]
- MCP: MCPRoute CRD (v0.4), OAuth 2.0 with JWKS validation, server multiplexing with tool prefixes, CEL-based MCP authorization (v0.5), stdio servers in standalone `aigw run` with Claude-Desktop-style mcpServers JSON (v0.5), tools/list filtered by authz (v0.7), per-backend header forwarding (v0.6). [documented]
- v1.2.0: MCP auth hardening, bare tool names, caller-supplied credentials passthrough; v1.2.1: MCP session ID bound to the MCPRoute that created it, session-encryption seeds kept out of pod specs/logs. [documented]
- Outbound credentials: upstream auth policies and per-request creds exist; no MCP OAuth-client/refresh-token store seen; token exchange on roadmap only. [documented]/[not found]
- Audit: per-request usage/latency/cost telemetry; tools/call-specific audit not confirmed. Guardrails: "ecosystem collaboration with guardrail engines". Pinning: not found.
- K8s-first (Envoy Gateway); standalone mode exists. Stdio runs as processes [inferred].
- CVEs: none seen; 1.2.1 is a security patch (details not read).
- Good K8s fabric for authn/z; thinner than agentgateway on MCP guardrails and credentials.

## Other candidates bumped into [reported only, not verified]
- Obot (MIT): gateway + stdio servers hosted as containers + managed MCP OAuth + shared creds + audit logs + group access. Free tier capped at 100 users; enterprise auth providers need a licence. Closest to the whole brief; vendor-written comparisons, verify. https://obot.ai/blog/the-13-best-mcp-gateways-for-enterprise-teams/
- MCPJungle (MPL-2.0, v0.4.6): light single binary, tool groups per client, static bearer tokens; stdio unsandboxed. https://tulimoa.com/blog/open-source-mcp-gateways
- MetaMCP (MIT): namespaces + middleware, thin docs. Bifrost (per-user OAuth with refresh; Maxim AI sources, biased). Lasso (local proxy, secret masking). Lunar MCPX (sandbox for evaluating servers).
- Separate tool for rug-pull: MCP-Scan (description-change detection) as sidecar to any gateway. [reported]

## Comparison table
Legend: Y yes OSS, P partial, E enterprise/paid only, N no/not found, ? unverified.

| Capability | Docker | ContextForge | MS gateway | agentgateway | Agent Router |
|---|---|---|---|---|---|
| Latest (date) | 0.44.3 (10-26) | 1.1.1 (11-10) | 1.0.1 (11-16) | 1.7.0 (10-27) | 1.2.1 (10-27) |
| Stars | 1.6k | 4.9k | 0.9k | 6.3k | ? |
| License | MIT | Apache-2 | MIT | Apache-2 | Apache-2 |
| Governance | Docker | IBM | Microsoft | LF/AAIF (Solo-led) | AAIF, multi-vendor |
| stdio spawn | Y | P (translate) | N | Y | Y (standalone) |
| Per-server container isolation | Y | N | Y (K8s pods, HTTP) | N | N |
| Static header injection | Y (secrets) | Y | N | Y | P |
| OAuth client for remote MCP | P | Y | N | E (OSS: exchange only) | N |
| Refresh-token store | ? Desktop | Y DB/Vault | N | E (Postgres STS) | N |
| Per-session tool allowlist | P (profile) | Y (virtual server+JWT) | N | Y (CEL on JWT) | Y (CEL) |
| tools/call audit | Y | P | N | Y (OTel) | P |
| Response redaction | P (block) | Y (plugins) | N | Y (guardrails/ExtMCP) | ? |
| Description pinning/rug-pull | N | N | N | E / ? | N |
| Signed server images | Y | N | N | N | N |
| Kubernetes | N | Y Helm | Y AKS | Y | Y |
| 2025-26 CVEs | none found | many incl. 9.8/10.0 | 1 (8.8) | none found | none found |

## Reading for the design (inferred)
- No single candidate meets every requirement in OSS. Gaps: refresh-token custody (only ContextForge OSS; agentgateway paid), rug-pull pinning (none OSS confirmed), stdio isolation (only Docker).
- Plausible split: agentgateway or Agent Router as policy/identity front (token to session, CEL tool allowlist, guardrails, audit), Docker MCP Gateway or per-server containers as stdio runner, plus a token broker (ContextForge or custom) for OAuth refresh; add own description-hash pinning in a guardrail/ExtMCP hook.
- Open items to verify next: whether v1.7.0 shipped OSS token exchange; ContextForge "1.1.x" fixes for CVE-2026-8355; Docker secret backend on headless Linux; Agent Router MCP stdio isolation and v1.3.0 date.
