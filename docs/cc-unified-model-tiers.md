---
type: design
status: draft
version: 2.3
title: "Agent fence: tier profiles"
task: ai-tooling:#6gqx
date: 2026-11-19
supersedes: unified-model.md v1 §1 tiers, §2 components, §3 third party
---

[Index](README.md) › Unified model › 2. Tiers

**Docs:** [1 · contract](cc-unified-model.md) · **2 · tiers** · [3 · CC Flow as a client](cc-unified-model-cc-flow.md) · [4 · working log](cc-unified-model-log.md) · [5 · build plan](cc-unified-model-build-plan.md)

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
the Mac, the hp, and `future-two`, a Hetzner server that's already a k3s node. **On future-two,
sessions and doors run as k3s pods** (Tom's decision, v2.3); the doors sit in their own namespace.
The cockpit runs on the Mac or the hp. The phased plan is [doc 5](cc-unified-model-build-plan.md).

| Adds | Covers | Build / buy |
|---|---|---|
| **Fence**: `ccsbx run` on every machine, behind one backend interface (docker on the Mac and the hp, a pod on future-two). It starts the container and a per-session **forwarder** that holds the token (the token never enters the fence), mints the identity and the token with the machine's key, mounts the worktree with a read-only gitdir, bakes the managed settings and runs the session daemon (outbox/inbox, events, verb queue, messaging). The host commits with hooks off, and the harness and image are pinned | T2, part of T1 | build on **docker** (runc, hardened flags; Docker Desktop on the Mac) and **k3s pods** (future-two) |
| **Model door**: ccsp-model injects the credential the launcher picked (subscription or API key) and redacts outgoing secrets | T1 | build (extends today's ccsp) |
| **Egress door**: ccsp-egress with static routes (each with a method and path allowlist), GET-only package mirrors instead of open tunnels, GitHub App tokens (1 h, downscoped) and a path-aware git route that refuses force-pushes and pushes to protected branches | T2, part of T3 | build |
| **Message carrier**: the existing session-bus hub routes signed stamps between daemons; it's treated as a door (per-principal credentials, fail-closed auth); daemons fall back to delivering directly | doc 1 §7 | adapt (session-bus) |

**Several machines.**
- **The doors are central**, one copy of each as pods on future-two, reached over Tailscale. That gives one
  set of keys, one spending cap and one audit log. The accepted trade-off is a single point of
  failure (doc 1 §9).
- **Each machine runs only the fence.** That's the launcher, the session daemon and the image,
  plus the host-key and docker-group hardening. future-two has no launcher or key of its own: the
  hp's launcher creates its pods and signs their tokens, within that key's scope, and an
  in-cluster daemon keeps their journal.
- **A portable cockpit.** The cockpit (CC Flow, for Tom) runs on whichever machine Tom is at,
  the Mac or the hp, and moving it is just starting it on the other one. It starts sessions on
  any machine through that machine's daemon API (not Tailscale SSH, whose check mode needs a
browser) and subscribes to every daemon's events. Nothing in the
  platform names a cockpit machine.
- **Cockpit down loses visibility, never progress** (doc 1, invariant 28). Each daemon:
  - delivers messages to other machines' daemons via the hub (directly if the hub is down),
    checking the scope rule it was given at launch;
  - keeps its event journal and replays it from the cockpit's last-seen position on reconnect;
  - queues requests meant for the cockpit (tree verbs) and delivers them in order later.
- **`future-two`, a fence host that's also a k3s node: pods** (decided, v2.3). Docker beside
  k3s was rejected: the kubelet can't see it, its iptables bypass ufw and clash with flannel,
  and it isn't GitOps. The fence is a pod with a hardened securityContext and a sidecar (the
  forwarder, outbox relay and git). NetworkPolicy is per pod and IP-only, so per-host allowlists
  live in the door code. The doors run in their own namespace so the launcher's credentials can't
  reach their Secrets (invariant 12), and they expose a session listener that ignores identity
  headers plus an owner listener reachable only through the Tailscale proxy (invariant 27).

**Billing per session.** Tom's development work uses the Claude subscription, and ops work
(chatbot evaluation, pharmacy ops) uses an API key. Whether the model door may hold a
subscription login is open (#6f24). Until it's answered, subscription sessions run unfenced or
on a stub, and API-key sessions run fenced.

- **Grants at this tier** are static per-session profiles, kept on each launcher (not on the
  cockpit machine) and approved per machine. **A small low-tier ask exists** (v2.3, amends N5):
  one extra egress route or repo read for one generation, decided by the owner with a one-time
  code sent to the phone. Anything else not allowed is forbidden.
- **MCP at this tier.** No tool door in the first build. Remote MCP servers, if needed, are plain
  HTTPS routes in the egress door with an injected token, with no per-tool filtering.
- **Leaves open:** T3 beyond the git rules (only the low-tier ask), T4 beyond reach limits (no trifecta
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
| Messaging | daemon + session-bus hub (signed stamps) + direct fallback | same | Postgres inbox (SKIP LOCKED, RLS) | not durable; the hub on one machine |
| Notifications | — | Telegram (low) + Web Push | Web Push | Telegram compromise (so low tier only) |
