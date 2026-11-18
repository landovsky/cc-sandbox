# Web-attach surfaces for agent terminal sessions (as of 2026-11-18)

Tags: **[doc]** vendor docs/README/API, **[rep]** reported (blogs/search summaries), **[inf]** my inference.
Method: GitHub API + README fetches, vendor docs, web search. Stars are approximate (README page).

Requirement recap: web attach to tmux/pod session; READ-ONLY view + "send box"; human identity
(Tailscale now, Keycloak/OIDC later, <=500 users); recording/audit; no credential in sandbox; self-host.

## 1. ttyd — https://github.com/tsl0922/ttyd
- C, libwebsockets + xterm.js. Latest tagged release 1.7.7 (2024-03-30) [doc: api.github.com/repos/tsl0922/ttyd/releases/latest]; but commits continue: 2026-08-12 mobile viewport meta fix [doc: commits API]. ~12.7k stars. MIT. Self-host: yes.
- Read-only: **default** (clients read-only unless `-W`) [doc]. Gives a pure screen view for free.
- Auth: `-c user:pass` basic; `-H/--auth-header` trusts a reverse-proxy header (e.g. oauth2-proxy/Tailscale Serve identity headers); client certs [doc]. No native OIDC [inf: use oauth2-proxy/Keycloak gatekeeper in front].
- Recording: none built in [inf]; wrap command in `asciinema rec`/`script`.
- Mobile: xterm.js, WebGL2; viewport fix only merged Aug 2026; no key toolbar (Esc/Ctrl/arrows awkward) [inf].
- Pod attach: run `ttyd kubectl exec -it pod -- tmux attach` or sidecar ttyd in pod [inf]. No k8s story of its own.
- Recipe [inf]: viewer = `ttyd tmux attach -r -t S` (tmux `-r` read-only); writer = separate ttyd `-W` or a send-box endpoint doing `tmux send-keys`.
- Vendor risk: single maintainer, slow releases (2.5 yr since tag). Fit: solo great; 500 only as a component behind a proxy.

## 2. gotty and forks — https://github.com/sorenisanerd/gotty
- Original yudai/gotty is unmaintained [rep, inferred from fork's existence]. sorenisanerd fork: **v1.11.0 released 2026-11-05** [doc: releases API], ~2.6k stars, MIT, active.
- Read-only default; `-w/--permit-write` enables input [doc]. Auth: basic (plain text warning), random URL `-r`, TLS client certs [doc]. No OIDC. No recording [doc: README silent].
- Same tradeoffs as ttyd but more actively released; ttyd has better UX/mobile polish [inf]. Fit: solo ok, 500 no (component only).

## 3. sshx — https://github.com/ekzhang/sshx
- Rust, E2E-encrypted (Argon2+AES) collaborative terminal sharing via sshx.io [doc]. Last release v0.4.1 2025-02-12; last commit 2026-09-21 (rustfmt/clippy fix) [doc: API] -> low activity. ~7.7k stars, MIT.
- Read-only link: `--enable-readers` gives separate read-only URL (since v0.3.0) [rep: release notes].
- **Self-hosting explicitly NOT supported** [doc: README]. Hosted relay = third party sees ciphertext only [inf]. No identity/OIDC (link-as-secret) [doc/inf]. No recording. Single-person project.
- Fit: solo toy only; fails self-host and identity requirements. Not for 500.

## 4. upterm — https://github.com/owenthereal/upterm
- Go; SSH-based terminal sharing with self-hostable `uptermd` (Helm, Docker Compose, systemd). Latest **v0.41.1 (2026-11-05)** [doc: API]. ~1.4k stars, Apache-2.0.
- `--read-only` guest mode [doc]; guest auth = SSH keys, allowlists incl. GitHub/GitLab/URL key lists [doc]. Host must approve commands/SFTP/forwarding.
- Guest is an SSH client, **not a browser** [doc/inf]: fails phone-browser requirement. No OIDC, no recording [inf]. Also needs an outbound connection from the host (credential-free but a relay hop).
- Fit: solo OK for laptop-only; 500 no.

## 5. Coder OSS — https://github.com/coder/coder
- Go, AGPL-3.0 core + Premium (commercial) features. Latest **v2.38.10 (2026-11-16)** [doc: API]. Very active; VC-backed single vendor.
- Web terminal: xterm.js over WebSocket, every workspace, reconnect via per-session UUID, agent buffers output; shell persists after browser closes [doc: coder.com/docs/user-guides/workspace-access/web-terminal]. Docs mention no tmux integration, no read-only mode, no recording [doc: page silent].
- Auth: OIDC free in OSS [rep: ssno.tax]; Keycloak works generically [inf]. Audit logs and connection logs are **Premium** [doc: coder.com/docs/admin/security/audit-logs/]. These record events, not terminal I/O [doc/inf].
- k8s: first-class (workspaces as pods via Terraform templates) [inf/known]. Heavy: control plane + DB + Terraform templates.
- AI features: **Tasks deprecated/removed** (removed from releases from v2.37, Sep 2026) in favour of Coder Agents (GA v2.37) [doc: coder.com/changelog/coder-2-37; docs/ai-coder/agents/tasks-to-chats-migration]. Coder Agents runs its **own Go agent loop in the control plane, not Claude Code**; LLM creds never enter workspace [doc: docs/ai-coder/agents/]. Claude Code only via "Agent Relay" (Premium-tagged, Sep 2026) [rep/doc].
- **coder/agentapi archived 2026-09-13, "deprecated and no longer maintained"** [doc: github.com/coder/agentapi]. Do not build on it. (HTTP API: /message, /messages, /status, /events SSE; nice idea, dead project.)
- Fit: solo = overkill; 500 = plausible platform (OIDC+RBAC+k8s) but you pay Premium for audit, and it does not give read-only/send-box for tmux sessions. Sources disagree on Tasks removal version (2.34 vs 2.37).

## 6. Tailscale SSH recording / k8s operator
- Tailscale identity is the auth: tailnet ACL/grants + SSH policy [doc]. Tailscale Serve identity headers can front ttyd [inf].
- **tsrecorder is deprecated**: "support for current users scheduled to end August 2027"; new users pointed to **Tailscale PAM** (beta) [doc: tailscale.com/kb/1246/tailscale-ssh-session-recording; docs/privileged-access-management/how-to/access-kubernetes]. An older tsrecorder page still describes it as current [rep: search result] — treat as stale.
- tsrecorder (legacy): asciinema .cast to disk/S3, output-only (no keystrokes), E2E encrypted, fail-open default, `enforceRecorder:true` = fail-closed [doc]. Also recorded k8s exec/attach/debug via operator `Recorder` CRD [doc].
- Operator API-server proxy: forwards with Tailscale identity as impersonated k8s user, standard RBAC; "auth" vs "noauth" mode [doc: tailscale.com/kb/1437/kubernetes-operator-api-server-proxy]. Page states recording covers kubectl exec/attach/debug, but plan/GA status unclear [doc: silent].
- Tailscale PAM: SSH/DB/RDP/HTTPS/K8s/S3 services, recorded replay, browser client, no kubeconfig creds distribution; **beta** [rep: search summary of docs]. Commercial, closed source.
- No browser terminal, no read-only; it is the identity+record **layer** under whatever attach tool you use. Vendor risk: closed, and churn in recording product (tsrecorder -> PAM in <2 yrs).
- Fit: solo excellent (identity); 500 weak for Keycloak-based clients (Tailscale identity != client's IdP unless SSO'd via OIDC to the tailnet) [inf].

## 7. Teleport — https://github.com/gravitational/teleport
- Latest **v18.11.10 (2026-11-16)** [doc: API]. Highly active; commercial vendor.
- License: since v16 (2024) Community **binaries are commercial**: free for <100 employees AND <$10M revenue, no resale/embed; repo stays AGPLv3, <16 was Apache-2.0 [doc: goteleport.com/blog/teleport-community-license/]. I found no further 2026 change [rep].
- Feature matrix [doc: goteleport.com/docs/feature-matrix/]: CE has k8s access, RBAC, session recording (playback + enhanced) — **but OIDC/SAML SSO is Enterprise-only; CE has GitHub SSO only**. Session sharing and **moderation are Enterprise-only**.
- Web UI has browser SSH terminal and live-session join [inf/known]; kubectl exec recorded and joinable (observer mode = read-only join, Enterprise-tier) [inf]. Mobile browser usability unverified.
- Security note: 18.10.7 (2026-08-19) fixed a moderated-session bypass via non-interactive exec [rep: changelog snippet].
- No credential in sandbox: short-lived certs, proxy-issued [inf/known].
- Fit: solo = over-heavy, and CE can't do Keycloak. 500-person *client* companies = strong, but need Enterprise (cost; and CE's <100 employee cap is exceeded). Best recording/audit story of the set.

## 8. Newer "web terminal for AI agent sessions / tmux in browser" (2026)
All found via one search [rep]; mostly tiny, young, solo-maintained; none verified by me beyond README.
- **axoinsights/tmux-terminals** — MIT, 0 stars, 28 commits [doc]. tmux-client + xterm.js, token on every route, loopback bind, origin check, QR, PWA, mobile key toolbar and compose bar (send-box-like), Claude Code/Codex configs. No read-only web mode, no OIDC, no k8s [doc].
- **Ark0N/Codeman** — self-hosted mission control for Claude Code/OpenCode/Codex etc., xterm.js, touch UI, tmux-backed [rep].
- **samoylenkodmitry/webmux** — tiny tmux-for-phones web terminal, Tailscale-oriented, one-tap answers to agent prompts [rep].
- **windmill webmux** — dashboard for parallel agents, Docker sandboxing, Svelte+xterm.js [rep: windmill.dev/blog/webmux].
- **zalo/terminal**, **jaiporterscott/RemoteCode**, **QuivrHQ/247-claude-code-remote** — phone-oriented tmux web terminals [rep].
- Search found **no** project with a built-in read-only mode plus send-box [rep]. Read-only is DIY: `tmux attach -r` or disable stdin in xterm.js [inf]. Mobile clipboard/OSC52 on xterm.js is flaky [rep: dev.to].
- Vendor risk: very high (hobby projects, churn) — use as design reference, not dependency [inf]. wetty (Node, xterm.js) is the older alternative; not re-verified.

## Synthesis
- No product does "read-only screen + send box + OIDC + recording" out of the box [inf]. Closest is composing:
  - **View**: ttyd/gotty running `tmux attach -r` (read-only by default, MIT, tiny).
  - **Send box**: small HTTP endpoint -> `tmux send-keys -l`/`kubectl exec ... tmux send-keys` with the authenticated user logged (own code, ~100 lines) [inf].
  - **Identity**: Tailscale Serve identity headers (solo) -> oauth2-proxy + Keycloak (500) in front of both [inf].
  - **Recording**: tmux `pipe-pane`/asciinema in the sandbox is trusted-by-sandbox; for tamper-proof recording record at the proxy/gateway (Teleport, Tailscale PAM, or your own broker) [inf].
  - **No credential in sandbox**: the web endpoints run in a broker outside the pod; the pod holds no keys; kubectl exec uses the broker's SA, not the user's [inf].
- Teleport is the only off-the-shelf option covering identity + recording + moderated/observe for k8s exec at company scale, but CE lacks OIDC.

## Scores (1-5, higher better; vendor risk 5 = low risk; fit = as the web-attach component)
| Option            | Maturity | Self-host | License | Vendor risk | Read-only | Auth | Fit-solo | Fit-500 |
|-------------------|---------:|----------:|--------:|------------:|----------:|-----:|---------:|--------:|
| ttyd              |        4 |         5 |       5 |           3 |         5 |    2 |        5 |       2 |
| sorenisanerd gotty|        3 |         5 |       5 |           3 |         5 |    2 |        4 |       2 |
| sshx              |        3 |         1 |       5 |           2 |         4 |    1 |        2 |       1 |
| upterm            |        3 |         4 |       5 |           3 |         4 |    2 |        3 |       1 |
| Coder OSS         |        5 |         5 |       3 |           3 |         1 |    4 |        2 |       4 |
| Tailscale (SSH/k8s/PAM)|     3 |         2 |       1 |           2 |         1 |    4 |        4 |       3 |
| Teleport          |        5 |         4 |       2 |           3 |         4 |    3 |        2 |       4 |
| 2026 tmux-web hobby tools|   1 |         5 |       5 |           1 |         1 |    1 |        3 |       1 |
