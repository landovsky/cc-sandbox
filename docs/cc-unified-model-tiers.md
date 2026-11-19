---
type: design
status: draft
version: 2.2
title: "Agent fence: tier profiles"
task: ai-tooling:#6gqx
date: 2026-11-19
supersedes: unified-model.md v1 §1 tiers, §2 components, §3 third party
---

[Index](README.md) › Unified model › 2. Tiers

**Docs:** [1 · contract](cc-unified-model.md) · **2 · tiers** · [3 · CC Flow as a client](cc-unified-model-cc-flow.md) · [4 · working log](cc-unified-model-log.md)

# Tier profiles

Each tier lists only what it **adds** to the tier below and which threat (T1–T4 in
[doc 1](cc-unified-model.md#1-threats)) each addition covers. The contract and its invariants are
the same at every tier; only the enforcement gets stronger. **Solo can be built and understood
on its own**, before any higher-tier component exists.

**Every instance has exactly one owner.** Friends don't share Tom's setup: each person installs
their own instance with their own subscription or API key. So installing it must be easy for
someone who isn't Tom: one install path per machine type, with no step that needs Tom's
infrastructure.

## Unfenced (outside the contract)

A session running directly on the host is labelled `unfenced`: its consent is recorded but not
enforced (invariant 13). It's listed only so nobody mistakes it for a tier. Two invariants still
protect the contract from it: host keys under a dedicated uid (12), and no agent in the docker
group (14, which can't hold on a Mac; see doc 1 §9).

## Solo: about 3 components, on one or several machines

For: one owner on their own machines. **Tom's first build:** three machines that run sessions:
the Mac, the hp, and one Hetzner server that's already a k3s node (which one is open, see the
log N9). The cockpit runs on the Mac or the hp.

| Adds | Covers | Build / buy |
|---|---|---|
| **Fence**: `ccsbx run` on every machine. It starts the container, mints the identity and the session token with the machine's key, mounts the worktree, bakes the managed settings and runs the session daemon (outbox/inbox, events, minimal messaging). The host's git runs with hooks off, and the harness and image are pinned | T2, part of T1 | build on **docker** (runc, hardened flags; Docker Desktop on the Mac) |
| **Model door**: ccsp-model injects the credential the launcher picked (subscription or API key) and redacts outgoing secrets | T1 | build (extends today's ccsp) |
| **Egress door**: ccsp-egress with static routes, GitHub App tokens (1 h, downscoped) and a path-aware git route that refuses force-pushes and pushes to protected branches | T2, part of T3 | build |

**Several machines.**
- **The doors are central**, one copy of each on Hetzner, reached over Tailscale. That gives one
  set of keys, one spending cap and one audit log. The accepted trade-off is a single point of
  failure (doc 1 §9).
- **Each machine runs only the fence.** That's the launcher, the session daemon and the image,
  plus the host-key and docker-group hardening.
- **A portable cockpit.** The cockpit (CC Flow, for Tom) runs on whichever machine Tom is at,
  the Mac or the hp, and moving it is just starting it on the other one. It starts sessions on
  any machine over Tailscale SSH and subscribes to every daemon's events. Nothing in the
  platform names a cockpit machine.
- **Cockpit down loses visibility, never progress** (doc 1, invariant 28). Each daemon:
  - delivers messages to other machines' daemons directly, checking the scope rule it was
    given at launch;
  - keeps its event journal and replays it from the cockpit's last-seen position on reconnect;
  - queues requests meant for the cockpit (tree verbs) and delivers them in order later.
- **A fence host that's also a k3s node.** k3s has its own containerd, so docker runs beside
  it, not inside it. Keep docker off k3s's networks: pin docker's bridge range outside the pod
  and service CIDRs, check that docker's iptables rules (it sets FORWARD to DROP) don't break
  flannel traffic, and reserve CPU and memory for the kubelet so sessions can't starve pods. Fences never use the
  k3s socket. If the same server is also the central door host, the door uid and the session
  users are separate, and the doors listen only on the Tailscale address (invariant 27).

**Billing per session.** Tom's development work uses the Claude subscription, and ops work
(chatbot evaluation, pharmacy ops) uses an API key. Whether the model door may hold a
subscription login is open (#6f24). Until it's answered, subscription sessions run unfenced or
on a stub, and API-key sessions run fenced.

- **Grants at this tier** are static per-session config that the owner writes on the
  machine the cockpit runs on. No ask tier yet: anything not allowed is forbidden.
- **MCP at this tier.** No tool door in the first build. Remote MCP servers, if needed, are plain
  HTTPS routes in the egress door with an injected token, with no per-tool filtering.
- **Leaves open:** T3 beyond the git rules (no ask tier), T4 beyond reach limits (no trifecta
  enforcement, no outbound scan), library code (T2 left), door failover, docker-group
  separation on the Mac.

## Friends: adds the tool door and approvals

For: anyone running their own instance who wants MCP tools and approvals. It's the same
single-owner instance with more components.

| Adds | Covers | Build / buy |
|---|---|---|
| **Tool door**: agentgateway with session tokens, a fixed catalog, `tools/list` filtered by grants, hash pinning, response redaction, outbound scanning and audit | T2 (MCP), T4 | **buy** agentgateway OSS; **build** ccmg-policy |
| **Grant list + policy**: ccgrant, a simple grant list on an embedded Cedar engine, revoked when the session or generation ends, with rate counters | T3, T4 | **buy** cedar-go; **build** the store |
| **Approval channel**: requests published to any surface, first decision wins, proofs verified by policy; the platform's approval page with passkeys | T3, T4 | build |
| **Grant plan**: scout phase, one-tap plan approval, batched mid-task asks, profiles | T3, T4 (trifecta) | build (prompt + plan-to-bundle step) |
| **Notifications**: Telegram buttons for low risk only, Web Push for the rest | — (UX for approvals) | buy (have it) |

- **Leaves open:** no volume detection, the shared kernel, per-user document access (only a
  service account), Notion, durable messaging.

## Company: adds scale and per-user identity

For: a partner company's server or cluster, with many owners.

| Adds | Covers | Build / buy |
|---|---|---|
| **Full policy set**: profiles and ceilings proven in CI with the Cedar analyser, policy store on Postgres | T3, T4 | buy Cedar CLI; build the CI tests |
| **Per-user delegation**: OAuth custody (comes back from descoped), RFC 8693 token exchange, Google `drive.file` per user | T4, §8 of doc 1 | build in the egress door, or Pomerium |
| **Identity and secrets backends**: Keycloak, OpenBao | T1, T2 | buy |
| **Pods with stronger isolation**: k3s with gVisor, or Kata on bare metal; the same contract with a sidecar instead of the session daemon | T2 (kernel) | buy |
| **Signed message hub**: durable inbox on Postgres, signed envelopes, scoped directory | §7 rules at scale | build |
| **Audit pipeline** that someone actually watches | T4 (volume) | buy or build |
| **Door failover**, if needed (descoped until then) | availability | build |

## Third party, per tier

| Layer | Solo | Friends | Company | Main risk |
|---|---|---|---|---|
| Fence runtime | docker 29 + runc (Docker Desktop on the Mac) | same | k3s + gVisor / Kata | shared kernel; on the Mac the docker socket belongs to the user |
| Machine network | Tailscale (SSH + private doors) | same | same or company VPN | — |
| Tool door | — | agentgateway OSS 1.7.0 | agentgateway k8s controller | vendor moving features to paid |
| Policy | static config | cedar-go 1.9.3 | Cedar + analyser, Postgres store | cedar-go lacks templates |
| GitHub credential | GitHub App tokens | same | same | — |
| Google credential | service account + per-file share | same | per-user OAuth `drive.file` | access drifts from the owner's (solo) |
| Dynamic secrets | SOPS + doors | same | OpenBao 2.7.2 | — |
| Messaging | daemon + send-keys | same | Postgres inbox (SKIP LOCKED, RLS) | send-keys is not durable |
| Notifications | — | Telegram (low) + Web Push | Web Push | Telegram compromise (so low tier only) |
