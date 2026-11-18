[Index](../../README.md) › [Unified model](../../unified-model.md) › [MCP gateway](../mcp-gateway.md) › mcp-gateway-survey-2

# MCP gateway survey 2 (researched 2026-11-18)

Scope: gateways for "agent in no-internet/no-credential sandbox -> one gateway -> all MCP servers".
Tags: [documented] vendor/spec docs, [reported] third-party/search summary, [inferred] my reading.
Caveat: WebFetch summarised pages with a small model; items marked "not stated" were absent from the page
text I got, not proven absent. Release dates for several repos were not visible (GitHub release pages empty in capture).

## 0. Verdict (short)
- No candidate does everything. Best fit per requirement:
  - Credential injection + OAuth refresh held outside sandbox + per-user tool policy + tool-call audit, HTTP MCP only: **Pomerium** (OSS-licensed core) or **Obot** (MIT, also hosts stdio).
  - Hosting isolated stdio servers on k8s with per-user/group tool grants and snapshot pinning: **Obot**.
  - Generic HTTP credential injection for the sandbox (not MCP-aware): **OpenShell** (placeholders + injection + refresh in gateway).
  - Kong ai-mcp-proxy and Traefik Hub MCP: strong policy, but commercial; no stdio; no upstream OAuth refresh holding [documented].
  - Lasso: only guardrail plugins + scanner on stdio proxy; not a credential/OAuth broker. MetaMCP: archived 2026-11-12 + 9.8 CVE.
- Nobody found does description hash-pinning/rug-pull blocking on `tools/list` out of the box; Obot snapshot pinning is closest (catalog-level). [inferred]
- Likely composition: OpenShell-style egress/credential proxy + MCP-aware gateway (Pomerium or Obot) + own tools/list hash-pin layer.

## 1. Spec: MCP authorization (latest = 2026-07-28; earlier 2025-06-18, 2025-11-25)
Source: https://modelcontextprotocol.io/specification/draft/basic/authorization (page example shows MCP-Protocol-Version 2026-07-28),
https://blog.modelcontextprotocol.io/posts/2026-07-28/ , https://www.descope.com/blog/post/july-2026-mcp-revision
- Authorization optional; HTTP transports SHOULD follow it; stdio SHOULD NOT (credentials from environment). [documented]
- Server = OAuth resource server; MUST implement RFC 9728 Protected Resource Metadata; clients MUST use it for AS discovery. [documented]
- Client registration priority: Client ID Metadata Documents (SHOULD), pre-registration, DCR (MAY, now deprecated, >=12-month removal window). [documented]
- RFC 8707 `resource` param MUST be sent in authorization and token requests, canonical MCP server URI. [documented]
- Server MUST validate token audience; "MCP servers MUST NOT accept or transit any other tokens"; clients MUST NOT send tokens not issued by that server's AS. [documented]
- Token passthrough is an explicit anti-pattern ("MUST NOT accept any tokens that were not explicitly issued for the MCP server"). [documented] https://modelcontextprotocol.io/docs/tutorials/security/security_best_practices
- Proxy/gateway language: "MCP proxy server" = MCP server fronting a third-party API with ONE static client ID. MUST do per-client consent, exact redirect_uri match, __Host- cookies, single-use state (confused deputy). [documented]
- Refresh tokens: clients MUST keep them confidential in storage/transit; MUST NOT assume issued; MAY add offline_access. [documented]
- 2026-07-28 additions: RFC 9207 `iss` validation vs mix-up (SEP-2468), credentials bound to issuer (SEP-2352), stateless core (no initialize/session), `Mcp-Method`/`Mcp-Name` headers so gateways can route/authorize without parsing bodies, Enterprise Managed Authorization extension, formal deprecation policy (Roots, Sampling, Logging, DCR). [reported: truefoundry/4sysops/WorkOS via search]
- stdio-proxy risk: a proxy that spawns stdio servers is an RCE escalation path if its auth token leaks; spec says sandbox spawned processes, log all stdio use, require extra authz for dangerous commands. [documented] (directly relevant to our "gateway spawns stdio" design)
- Implication for our design: the gateway is simultaneously an OAuth *client* upstream (holds refresh tokens, own tokens per upstream resource, RFC 8707 per upstream) and an MCP *server* to the sandbox with its own audience-bound token (session token). Never forward the sandbox token upstream, never forward upstream tokens to the sandbox. [inferred]

## 2. Tool poisoning / rug pull / toxic flows
- Invariant Labs 2025-04-01: tool poisoning (hidden instructions in descriptions), rug pull (description changed after approval), shadowing (one server's description steers use of another's tools). Defence: pin server+tool versions, hash/checksum descriptions, cross-server dataflow controls, show full descriptions. https://invariantlabs.ai/blog/mcp-security-notification-tool-poisoning-attacks [documented]
- mcp-scan (2025-04-11) -> Snyk acquired Invariant (2025-06); now snyk/agent-scan, `uvx snyk-agent-scan@latest`, needs SNYK_TOKEN; toxic-flow codes W015-W020, poisoning E001, shadowing E002. [reported] https://appsecsanta.com/mcp-scan ; toxic flows: https://invariantlabs.ai/blog/toxic-flow-analysis1
- One source says the scanner's hash-based pinning was dropped in 2026 and no major MCP client pins by default. [reported, unverified]
- MCPTox (arXiv 2508.14925): 45 live servers, 353 tools, 20 agents; mean attack success 36.5%, up to 72.8%; best refusal (Claude 3.7 Sonnet) <3%; more capable models often more susceptible. https://arxiv.org/pdf/2508.14925 [reported]
- Cross-framework study arXiv 2611.03555: 22.9% overall ASR, 12.1% verified secret exfiltration. [reported]
- Limits of hashing (practitioner guidance): hash the whole canonicalised tool definition (name, description, inputSchema, annotations), not just top-level description; tools/call result text and code-only changes (postmark-mcp) are not covered by a tools/list snapshot. [reported]
- Claude Code itself: docs describe `list_changed` refetch of tools/prompts/resources and keep prior tools if refresh fails; no description-change re-approval or hash pinning documented. [documented/inferred] https://code.claude.com/docs/en/mcp
- Gateway-side implementation needed: store hash per (server, tool) at approval; on tools/list and on list_changed compare; on mismatch hide tool + alert; scan outputs for injection; label untrusted. [inferred]

## 3. Claude Code controls (docs fetched 2026-11-18)
Sources: https://code.claude.com/docs/en/managed-mcp.md , /mcp.md , /settings.md , /env-vars.md
- `managed-mcp.json` paths: macOS `/Library/Application Support/ClaudeCode/managed-mcp.json`; Linux/WSL `/etc/claude-code/managed-mcp.json`; Windows `C:\Program Files\ClaudeCode\managed-mcp.json`. Same format as `.mcp.json`. Cannot be delivered via server-managed settings. [documented]
- **Exclusive: yes.** Claude Code loads only: servers in the file; `managedMcpServers`; in-process app servers (VS Code/desktop); mod-added tools server; Claude in Chrome only if `allowClaudeInChromeWithManagedMcp`. Users cannot add/modify others, incl. plugin servers and `--mcp-config` (startup exits: "You cannot dynamically configure MCP servers when an enterprise MCP config is present"). `--strict-mcp-config` with the file -> exits. Empty `{"mcpServers": {}}` disables MCP. `claude mcp add` fails "enterprise MCP configuration is active and has exclusive control". [documented]
- The file suppresses claude.ai connectors unless managed setting `"allowAllClaudeAiMcps": true` (admin tiers only; user/project ignored). [documented]
- `allowedMcpServers` / `deniedMcpServers`: arrays of single-key objects: `serverUrl` (exact or `*` wildcard; hostname-written port rule: default port only unless `:*`), `serverCommand` (array, exact command+args, env not compared), `serverName` (exact label; docs WARN it is not a security control). Denylist always wins and merges from all scopes; empty allowlist `[]` = nothing allowed; unset = all allowed. `allowManagedMcpServersOnly: true` (managed only) makes the managed allowlist authoritative (v2.1.273+ for cross-source). managed-mcp.json servers skip the allowlist unless they use `${VAR}` outside `env`. [documented]
- `managedMcpServers` (managed settings, v2.1.259+): https-only http/sse entries, no command/env/headersHelper/`${VAR}`; user can't remove. `strictPluginOnlyCustomization` with `mcp`; `disableSideloadFlags`. [documented]
- claude.ai connectors: setting `disableClaudeAiConnectors: true` (any scope, any-source-true, honored even when managed says false) and env `ENABLE_CLAUDEAI_MCP_SERVERS=false` ("Enabled by default for logged-in users"). Only affects connectors Claude Code fetches itself; cloud/desktop-delivered ones are governed in claude.ai org settings. Per-connector: `deniedMcpServers` `{"serverName":"claude.ai Slack"}` or serverUrl. [documented] (No-login/API-key-only sandbox sidesteps connectors anyway. [inferred])
- `headersHelper` (per-server in `.mcp.json`/`claude mcp add-json`): shell command, stdout = JSON object of string headers, 10 s timeout, runs on every connection (no caching), after folder trust for project/local scope; overrides static `headers`; on 401/403 re-runs and retries once; if it emits `Authorization`, no OAuth fallback. Env given to helper: `CLAUDE_CODE_MCP_SERVER_NAME`, `CLAUDE_CODE_MCP_SERVER_URL`. [documented] -> fits "session token" header injection: helper prints `{"Authorization":"Bearer <placeholder/session token>"}`.
- Static `headers` and `${VAR}` expansion also supported in `.mcp.json`. Other env: `MCP_CLIENT_SECRET`, `MCP_OAUTH_CALLBACK_PORT`, `CLAUDE_CODE_MCP_ALLOWLIST_ENV=1` (stdio servers get only baseline env), `CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH` (default 4096 chars), `MCP_TIMEOUT`, `MAX_MCP_OUTPUT_TOKENS`. [documented]
- OAuth settings per server: `oauth.clientId`, `callbackPort`, `authServerMetadataUrl` (https), `scopes`; discovery RFC 9728 then RFC 8414. [documented]
- MCP OAuth token storage: official docs say only "stored securely ... system keychain (macOS) or a credentials file"; per-endpoint. `claude mcp logout <name>` revokes before `claude mcp remove`. Third-party/issue reports: Linux `~/.claude/.credentials.json` (0600, plaintext) with `mcpOAuth` object next to `claudeAiOauth`; macOS Keychain service "Claude Code-credentials". [reported] https://secretspec.dev/blog/claude-code-stores-oauth-tokens-in-plaintext/ ; issues anthropics/claude-code #93331 (unbounded mcpOAuth growth), #69749, #43000. Server config for user/local scope lives in `~/.claude.json`. [documented]
- Telemetry: `OTEL_LOG_TOOL_DETAILS=1` puts MCP server/tool names in OTel events. [documented]
- Design consequence: in the sandbox, ship `/etc/claude-code/managed-mcp.json` with exactly one entry (the gateway URL, `headersHelper` or static placeholder header) + `deniedMcpServers`/`allowedMcpServers` `serverUrl` pin to the gateway, and `ENABLE_CLAUDEAI_MCP_SERVERS=false`/`disableClaudeAiConnectors`. Claude Code then holds no upstream OAuth state. [inferred]

## 4. Candidates (summary matrix)
| | Lasso | Pomerium | Kong AI GW | Traefik Hub | Obot | MetaMCP | OpenShell |
|---|---|---|---|---|---|---|---|
| License | MIT | Apache-2.0 source; official builds "modified Apache 2.0" community license | Enterprise-only for MCP plugins | commercial (Hub) | MIT, all editions same image | MIT | Apache-2.0 |
| Stdio hosting | proxy spawns stdio | no | no | no | yes, container/k8s | yes, in-container | n/a |
| Upstream OAuth refresh held | no | yes | no (docs silent) | no | yes | partial | yes (generic OAuth2) |
| Per-user tool allowlist | no | yes (`mcp_tool`) | yes (consumer ACL) | yes (TBAC) | yes (vMCP, group) | namespace-level | no (L7 only) |
| Tool-call audit | xetrack | yes | yes (log_audits) | OTel only | yes | no | no MCP |
| Redaction | basic/presidio/lasso | no (not stated) | not supported in proxy | Content/LLM Guard | MCP/webhook filters | no | no |
| Status | quiet since 2026-01 | active | active | active | active | archived 2026-11-12 | active, CNCF sandbox |

## 5. Per-candidate detail

### Lasso Security mcp-gateway
- Version/activity: 40 commits, no GitHub releases, 403 stars/45 forks [documented https://github.com/lasso-security/mcp-gateway]; one third-party page says PyPI 1.2.1 + last commit 2026-01-22 [reported https://vdf.ai/blog/mcp-gateway-comparison/]. Created 2025-04. MIT. Governance: single vendor (Lasso Security); commercial upsell via `lasso` plugin (hosted API, needs key).
- Self-host: local Python proxy/CLI (`mcp-gateway -p basic -p presidio`), Docker. Transport: examples are all command/args (stdio children); SSE/HTTP not mentioned [documented]. Reads an existing `mcp.json`.
- Credentials: none brokered; `LASSO_API_KEY` env only; upstream secrets are just env of children. No OAuth/refresh storage. [documented]
- Allowlist/identity: none per session; scanner writes per-server passed/blocked/skipped into mcp.json. [documented]
- Audit: `xetrack` plugin logs tool calls to SQLite/DuckDB/file. [documented]
- Redaction: `basic` masks GitHub/AWS/GCP/Azure/JWT/Slack/HF tokens; `presidio` PII; `lasso` hosted (PII, secrets, prompt injection). Request+response sanitising via `run_tool`. [documented]
- Rug pull: `--scan` reputation (Smithery/npm/GitHub, threshold 30) + hidden-instruction detection at load; no continuous description hashing described. [documented]
- K8s: none. CVEs: none found [reported, search only]. Verdict: useful as a guardrail/redaction plugin source, not as the gateway.

### Pomerium MCP
- Docs "latest (main)", stable v0.34 (v0.33.0 added UpstreamAuthHandler, DCR for upstream). https://www.pomerium.com/docs/capabilities/mcp ; /mcp-upstream-oauth ; https://newreleases.io/project/github/pomerium/pomerium/release/v0.33.0 . 5.1k stars [documented].
- License: source Apache-2.0; official release binaries/images under a "modified Apache 2.0 / Pomerium Community License" https://github.com/pomerium/pomerium [documented]. Docs do not say whether MCP features are Enterprise-gated [not stated].
- Self-host yes; Kubernetes Ingress Controller with annotation `mcp_server_upstream_oauth2_secret` [reported]. Transport: HTTP(S) upstreams only; stdio not mentioned. Wrap stdio servers externally (e.g. mcp-proxy) [inferred].
- Credentials: acts as OAuth client upstream: discovers PRM, uses CIMD (default), DCR fallback, or pre-registered creds; acquires, caches, auto-refreshes per-user per-route tokens; injects `Authorization: Bearer` upstream; client only holds a Pomerium-issued External Token; service accounts can reuse a user's provisioned upstream tokens. SSRF allowlists `mcp_allowed_client_id_domains`, `mcp_allowed_as_metadata_domains`. [documented] Tokens cached server-side in databroker; rotating = clear databroker state [reported]. Encryption at rest not stated.
- Allowlist/identity: PPL `mcp_tool` (exact/prefix/suffix/list) combined with user/group claims; identity is the authenticated user/service-account. Session-token-per-sandbox = service account JWT [inferred]. Whether `tools/list` is filtered: not stated.
- Audit: every tool call logged with method, tool name, parameters [documented]. Redaction/guardrails: none stated. Rug pull: none stated.
- CVEs: none found for Pomerium MCP [reported, search only].

### Kong AI Gateway MCP
- Plugins `ai-mcp-proxy` (3.12+; tool ACLs 3.13+; 403 on ACL deny since 3.14; MCP 2026-07-28 upstream support reported) and `ai-mcp-oauth2`. https://developer.konghq.com/plugins/ai-mcp-proxy/ ; /ai-mcp-oauth2/ ; https://konghq.com/blog/product-releases/mcp-tool-acls-ai-gateway
- License: "only available as part of AI Gateway Enterprise" for both plugins [documented]. Kong Gateway OSS (Apache-2.0) core lacks them [inferred]. Self-host: on-prem hybrid/DB-less or Konnect.
- Modes: passthrough-listener (front existing servers), conversion-listener (REST->MCP tools), conversion-only, listener. Streamable HTTP only; no stdio, WebSocket, gRPC. No consumer scoping of the plugin; no AI Guardrails with it; no tool-change notifications; no cross-instance session sharing. [documented]
- Credentials upstream: docs silent; token exchange mentioned in ai-mcp-oauth2 changelog; no refresh-token vault described [documented/inferred]. OAuth2 plugin: introspection/JWT validation, RFC 8707, maps principals to consumers/groups. 
- ACL: default_acl + per-tool ACL, consumer/group, CEL `allow_when`/`deny_when`, OAuth claims (2026-04); `tools/list` filtered by default ACL. [documented/reported]
- Audit: `logging.log_audits: true` logs all allowed/denied attempts + Kong logging plugins. Redaction: not in this plugin. Rug pull: none.
- CVEs: ACL decision-cache collision fixed 3.14.0.9 (2026-07); tools/list leak to denied consumers fixed; auth-bypass hardening in 3.13 [reported].
- K8s: Kong Ingress Controller / Gateway API (general Kong) [inferred].

### Traefik Hub MCP Gateway
- Early access Oct 2025, now documented as MCP-2026-07-28 aware; docs updated 2026-11-18. https://doc.traefik.io/traefik-hub/mcp-gateway/mcp ; https://traefik.io/solutions/mcp-gateway
- License: commercial Traefik Hub (Helm `hub.mcpgateway.enabled=true`); not in OSS Traefik Proxy [documented/inferred]. Self-host/air-gapped yes.
- Streamable HTTP only; stdio not mentioned. Resource-server role only (RFC 9728 metadata, JWT validation); upstream: `forwardAuthorization` (OBO pass-through of caller's header) and `forwardHeaders` claim->header. No upstream OAuth client, no refresh vault, no token swap described [documented] -> fails "gateway holds refresh tokens" requirement.
- TBAC: deny-by-default `match` expressions on `mcp.*` body and `jwt.*` claims (tool name, arguments); `listPolicies` filter tools/prompts/resources lists by claims. Session identity = JWT claims. [documented]
- Guardrails: chain Content Guard / LLM Guard to mask PII in args and results [documented]. Audit: OTel metrics/traces (tool args optional via `traces.genAI.toolCallArguments`), access logs; no dedicated audit log. Rug pull: none stated.
- K8s: CRDs `Middleware` (plugin.mcp, plugin.jwt), `IngressRoute`, `TraefikService` (HRW affinity for pre-2026-07-28 sessions). 
- CVEs: none for Hub MCP; OSS Traefik 2026 CVEs (ForwardAuth CVE-2026-54764, mTLS/SNI bypasses fixed 2.11.58/3.7.14) are relevant if used as ingress [reported].

### Obot (obot-platform/obot)
- Latest seen v0.27.0 (docs v0.28.0); ~1.2k stars; MIT; one vendor (Obot AI), editions differ only by license key (Community 100 users/devices; registered Community adds SSO; Enterprise = no caps + support). Keygen validation online; air-gapped offline licensing "coming". https://docs.obot.ai/enterprise/overview/ [documented]
- Self-host: Docker (dev/single-tenant only, mounts docker socket) or Kubernetes (recommended prod). [documented https://github.com/obot-platform/obot]
- Hosts npx/uvx/container MCP servers as Docker containers or k8s workloads outside the server process; optional domain egress rules via network-policy provider; OpenAPI->MCP. Remote servers proxied. Filters run as separate MCP-style servers. [documented]
- Credentials: upstream OAuth credentials stay in Obot; clients get Obot tokens; per-user/shared credentials; token exchange (RFC 8693) via per-server shim so secrets are not visible to the MCP server. Storage backend/encryption: field encryption OFF by default, detail on /security/credentials (not fetched). [documented/partly stated]
- Allowlist/identity: vMCPs bundle selected tools; grants by user/IdP group; per-component tool selection, rename, description override; tool search. Session id/token scheme: not stated. Obot Sentry (beta) installs hooks into Claude Code/Codex etc. and can enforce tool-call allowlists (experimental). [documented]
- Audit: correlated logs across gateway, hosted workloads, devices; filter by user/server/tool/session/API key; export to S3/GCS/Azure (v0.24.0). [documented/reported]
- Redaction/guardrails: MCP filters and webhooks can inspect/reject/modify requests and responses. Rug pull: **snapshot pinning** of catalog entries in vMCPs since v0.26.0 (catalog edits do not change vMCPs until explicitly updated) [reported]; issue #8104 asks for alerts on tool-definition drift, so live `tools/list` drift detection likely absent [inferred]. https://github.com/obot-platform/obot/issues/8104
- CVEs: CVE-2026-130766 (0.26.x <0.27.0: resource identifier in PRM not validated -> malicious remote server can steal tokens; 2026-11-18); CVE-2026-130767 (command injection via file-type server config; <0.27.0); CVE-2026-103758 (authz bypass via `/mcp-connect-composite/`, 0.21.1-0.24.1). [reported: strix.ai, vuldb] -> frequent auth-surface bugs; pin >=0.27.0.

### MetaMCP (metatool-ai/metamcp)
- **Archived by owner 2026-11-12** (read-only); community fork Umbrella-IT-Group/metamcp. Last release shown v2.4.22 (2025-12-19); unmaintained 2026-02 to 2026-06 per fork [reported]. MIT, 2.8k stars. https://github.com/metatool-ai/metamcp
- Self-host Docker; stdio servers run inside the app container (admin-only in multi-tenant per PR #391). Transports: SSE/streamable HTTP/OpenAPI out; `sk_mt_` API keys or OAuth/OIDC in. Env `${VAR}` interpolation for secrets. No refresh-token vault for upstreams described. Namespace-level enable/disable tools and description overrides; no audit log ("future idea"), no redaction. [documented]
- CVE-2026-79538 (CVSS 9.8, <=2.4.22): inspector proxy `/mcp-proxy/server/stdio` spawns arbitrary command from query string; plus SSRF, no ownership check (fix PR #382). PR #391 fixed OAuth token theft/cross-user access. [reported: opencve, GitHub]
- Verdict: do not adopt.

### NVIDIA OpenShell
- Apache-2.0, 0.1.x line, 21.8k stars, donated to CNCF as Sandbox project 2026-11-10 (moving to ai-openshell org); Helm gateway, CNI must enforce NetworkPolicy. https://github.com/NVIDIA/OpenShell ; https://docs.nvidia.com/openshell/how-it-works/providers/overview [documented]; "alpha" per one review [reported].
- Model: gateway (control plane) + supervisor + sandbox. Sandbox env contains opaque placeholders `openshell:resolve:env:...` (fail closed when unknown/expired); the egress proxy substitutes real values only when (1) network policy allows binary+destination, (2) credential endpoint binding covers host/port/path, (3) placeholder is in declared location (header, Basic auth, query, URL path, SigV4 re-sign; body/WebSocket text opt-in). Mismatch -> 403 `credential_endpoint_mismatch` + finding without secret. [documented]
- OAuth: gateway mints from OAuth2 refresh-token, client-credentials, Google SA JWT, AWS STS; refresh tokens/client secrets gateway-only, never returned to sandbox; writes current access token back to provider record. Storage backend/encryption not stated. [documented]
- No response scrubbing ("does not automatically scrub credential values from upstream responses"). Requires L7 HTTP handling; raw TLS tunnels stay opaque. No MCP feature in docs; no per-tool allowlist, no tools/call audit [documented/inferred].
- Fit: closest to our "sandbox holds placeholders only" requirement for HTTPS egress, incl. Claude Code API auth. For MCP it would need an MCP-aware gateway behind it (or be the transport for it). [inferred]
- CVEs: none found for OpenShell. (One blog cites 3 CVEs in the official Git MCP server as motivation.)

## 6. Other MCP-gateway incidents 2025-2026 (context) [reported]
mcp-remote CVE-2025-6514 (9.6); MCP Inspector CVE-2025-49596; MCPJam Inspector CVE-2026-23744 (9.8); mcp-use inspector CVE-2026-81091; IBM ContextForge DNS-rebinding CVE-2026-77822/18905; Docker mcp-gateway CVE-2026-55887 (command injection, <0.42.2); Amazon mcp-gateway-registry CVE-2026-14471. Pattern: gateways/inspectors that spawn stdio or proxy URLs from request input are the recurring RCE/SSRF class. Track: https://github.com/mcp-security-project/mcp-cve-project , https://vulnerablemcp.info/

## 7. Gaps / next steps
- Verify Pomerium OSS-vs-Enterprise split for MCP (license file, pricing page) and `tools/list` filtering.
- Read Obot /security/credentials, /security/isolation, /security/audit-data; check whether Obot can issue per-session bearer tokens/API keys scoped to a vMCP.
- Test Claude Code with a single managed-mcp.json gateway entry + headersHelper in the sandbox; confirm 401 re-run behaviour and that no other egress exists.
- Decide where hash-pinning lives (own thin proxy in front of whichever gateway). 
