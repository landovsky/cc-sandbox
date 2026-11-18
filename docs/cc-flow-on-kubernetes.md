[Index](README.md) › Inputs › cc-flow-on-kubernetes

CC Flow · proposal 68 · 6 November 2026 · design only, nothing built

# CC Flow on Kubernetes

**Kubernetes is the right fleet, not the reason you are safe.** It fits unattended agent jobs now and client cells later. Most of the protection for one person's own sessions needs no cluster at all.

For Tomas and the koperníci 20 research reports · 159 claims fact-checked Full doc: dotfiles · cc-flow/docs/proposed-changes/68-k8s-blueprint.md

- [Verdict](#verdict)
- [Map](#map)
- [One task's life](#life)
- [Ladder](#ladder)
- [Build vs buy](#buy)
- [The others](#others)
- [Risks](#risks)
- [Decide now](#decide)
- [Questions](#questions)
- [Next iteration](#next)

## The verdict

How CC Flow and CC Tree would run on Kubernetes with much stronger security, first for Tomas, then for client companies of up to 500 people.

- **Right shape for jobs and clients.** Kubernetes makes isolation declarative and reviewable: admission refuses a bad pod, network policy fences every pod alike, RBAC keeps the agent off the API, and a RuntimeClass puts agent code under gVisor or a VM.
- **Not sandboxed by nature.** A pod shares the host kernel and pod networking is open until you close it. Isolation is configuration plus a stronger runtime, and tests have to prove the configuration.
- **Most of your own protection needs no cluster.** Take secrets out of shells, let ccsp refuse keys the agent sends and add the real one itself, harden the bus, and run each agent as its own Unix user. That closes most holes on today's hosts.
- **Interactive sessions move into pods only after gate G1.** G1 passes when the terminal and gVisor spikes meet their numbers, when a client is near, or when the host route stops being safe. Until then sessions stay on hp, each under its own user, and you look again each quarter.
- **Buy the substrate, build the flow.** Buy gVisor, k3s, Cilium, Flux and the built-in admission now, and Kata, NATS, OpenBao and Keycloak for clients. Build what nobody ships: the tree, the ❓ queue, managers, the last hop into Claude Code, and ccsp's redaction and key pinning.

### The shape in five parts

### Session pods

One per live task. Claude Code runs inside gVisor in bypass mode and holds no secret, no API token and no DNS.

### Two gateways

ccsp-model and ccsp-egress: the only ways out, and the only holders of upstream credentials, added on the way out.

### Tree service, split in three

session-api (agents' only door, holds nothing), tree-core (holds keys, unreachable from pods), launcher (the only creator of pods).

### Your access

Ghostty and the Mac's tmux for the terminal, the web Tree on the phone, both through Tailscale. Remote Control is gone.

### GitOps and tests

The platform changes only by reviewed commits. Probes from a real agent identity run every day.

## The map

Today's parts in the target shape, for tenant zero after gate G1.

A session pod reaches exactly three Services, all through its sidecar. Upstream credentials exist only in ccf-gateway and are added on the way out (accent). You reach a session only through the Tailscale operator. Not drawn: unattended Jobs also call ccsp-model; tree-core pushes the tree repo to GitHub and sends Telegram notices; Flux pulls the reviewed ccf GitOps repo. A client cell keeps this picture and swaps backends: NATS for the hub, agent-sandbox for the StatefulSet, OpenBao for Secrets, Kata on bare metal, Keycloak for the tailnet.

**No arrow carries a credential into the sessions namespace.** Secrets mount only into the two gateways. The agent sees placeholders such as `GH_TOKEN=proxy-injected`.

**Agents never talk to each other.** Messages go to the hub in tree-core, which resolves one address, signs an envelope and keeps a durable inbox. The sidecar delivers between turns.

**The agent cannot change its own fence.** Settings, network policy and identity sit outside its reach. New reach is an expiring grant a human approved.

## One task's life

Twelve steps, from starting a task to the tests that keep proving the isolation.

1. **You start a task.** Enter on a node in the Mac's tree view, `cc-tree start`, or ▶ Start on the web Tree. A manager may start workers under its milestone, at most three a minute. The task's address `<tree>:<node>` exists before any pod does.
2. **The tree service creates the session, never the agent.** `tree-core` asks the `launcher`, the only account allowed to create session pods. An admission policy refuses a session pod from anyone else, or one with a secret in it.
3. **The pod starts, fenced in.** It runs under gVisor on a node that runs only session pods. No DNS, no internet, only three named Services. A persistent volume holds transcripts, worktree and CC Flow's state. The sidecar alone holds the pod's 10-minute identity token.Host route before G1: a systemd unit as user `agent-N`, whose root-owned firewall lets it reach only ccsp.
4. **The agent calls the model through ccsp.** ccsp checks who is calling, refuses any API key the agent sends itself, allows only Claude Code's own endpoints, removes known secret values and adds the real credential. Managed settings pin the base URL and set `disableRemoteControl: true`.
5. **The agent pushes to GitHub.** `git push` goes to `ccsp-egress` by an explicit route, with no TLS-breaking proxy. The gateway mints a one-hour GitHub App token for the tree's repos and accepts pushes only to the task's own branches.
6. **The agent needs a new host.** It runs `cc-req egress docs.example.com --ttl 4h --why "…"`. Hosts on the docs list are approved at once; anything else becomes a ❓ card you answer on the web Tree or the Mac. The grant applies within seconds and expires on its own. Only a human can approve.
7. **A manager messages a worker.** The hub finds exactly one current session for the address, checks the sender may write to it, removes secrets, signs the message and keeps it in a durable inbox. The worker's sidecar waits until Claude Code is between turns and nobody is typing, and reports `consumed` only when the prompt matches.
8. **You attach from the Mac, or look from the phone.** `ccattach the-tree:98mz` opens the session's inner tmux through Tailscale, with no cluster credential on the Mac. While you are attached, automated messages wait. The phone gets ❓ answers, a read-only screen and a send box.Host route: SSH into hp's tmux.
9. **The session idles, stops, then resumes or moves.** After two idle hours with a cold cache and nobody attached, the sidecar waits for a quiet moment, saves a WIP ref and the pod stops; its volume stays. A new pod resumes from the transcript, or starts fresh from the newest checkpoint if the transcript is cold and over 100k tokens. Each start is a new generation: old tokens and grants stop working.
10. **The Outcome is checked.** The worker writes its Outcome and can mark only its own node done. The manager checks it against `Accept:`. Code reaches `main` by PR or through a merge bot.
11. **The platform changes by GitOps.** Templates, policies and allowlists change by PR to the `ccf` repo. Your merge is the approval; Flux applies it within a minute. Sessions cannot push to that repo's main branch.
12. **The tests keep proving all of this.** Every control has a test that it blocks and a test that normal work still passes. Probes run daily from a real agent identity, fake secrets sit in four places for a red-team agent to go after, and weakened policies must turn the suite red. A threat with no test is a finding.

### What changes compared with today

- No agent holds a credential: no `credentials.env` in shells, no `SB_SECRET`, no OAuth file, no `.env` in worktrees.
- An agent cannot rewrite its own environment: settings, the network fence and the identity sit outside its reach.
- Egress is default-deny. New reach is an expiring grant you approved, not a line in a settings file.
- Messages carry a signed envelope, wait for a turn boundary and report `consumed`. There is no shared bus secret.
- Remote Control is gone. The phone gets the web Tree, a read-only screen and a send box.
- Restarts become routine and resume from the transcript. Background processes do not survive them.
- The web Tree, the bus hub and the timers stop depending on hp staying up.

## The ladder

A decision ladder, not a straight line. Every rung is useful alone, has a one-step rollback and an exit test.

1. ### 0 · Harden today, prove on a laptop days to 2 weeks

   Bus hardening, secrets out of every shell, ccsp on hp refuses foreign keys, deploy autonomy off on the production GitOps repo, the questions to Anthropic, a lab on the Mac with the first 15 tests.

   Closes the Cowork-class hole and takes the bus secret out of the agents, before any hardware.
2. ### 0b · Host boundary on hp 1–3 weeks

   A pool of agent users, sessions as sandboxed systemd units, root-owned settings, nftables egress only to ccsp, explicit egress routes for GitHub first.

   The age key, your home directory and every secret leave the agents' reach, with no cluster.
3. ### Gate 0 · The existing cluster in parallel

   Move it off Kubernetes 1.33 (end of life since 28 June 2026) one minor at a time, and patch runc 1.3.1 to 1.3.3 or later against the November 2025 container escapes.

   Client production gets security fixes again. Independent of CC Flow.
4. ### 1 · Cluster groundwork and spikes

   A small ccf cluster (cloud VM plus a dedicated node), k3s 1.36, gVisor, Cilium, default-deny, the admission policies, Flux, a daily probe job. Spikes S1–S6.

   A hardened empty sandbox with daily probes, and the unknowns that could change the design get answered.
5. ### 2 · Gateways and unattended jobs

   Both ccsp gateways, a Console key held only by ccsp, the GitHub App, and the unattended claude -p jobs moved into cluster Jobs.

   The riskiest traffic gets hard isolation and holds no secrets.
6. ### 3 · Tree service off hp, split, new hub contract

   session-api, tree-core and launcher; durable inbox, receipts, signed envelopes; the Elixir hub in shadow.

   An hp reboot no longer takes down the web Tree, feedback or messages.
7. ### G1 · Go or no-go for interactive pods

   Passes if the attach and gVisor spikes meet their numbers and a preview route exists, or a client is near, or incidents show attacks the host route does not contain.
8. ### 4 · One pilot tree in pods

   The session image, launcher, sidecar, ccattach, minimal consent, one low-stakes tree.

   One project fully sandboxed; delivery ≥ 99.5 %, start p95 ≤ 30 s, resume p95 ≤ 60 s.
9. ### 5 · All trees, hp retired, rotate every credential

   Rule #1 holds for every CC Flow session.
10. ### 6 · Company readiness

    Kata on bare metal, Keycloak and a leaver revoker, audit to object lock, an external pentest, one restore drill.
11. ### 7 · First client cell

    A dedicated cell for a developer-heavy client without special-category data; the client brings their own model account.

**Stop rule.** If G1 does not pass, stay at rung 3 and look again each quarter. CC Flow is experimental and cut by default; the platform gets the same discipline.

## Build vs buy, per layer

No platform can be adopted whole. Buy every layer that exists as a proven product; build the flow layer, the last hop and ccsp's policy.

| # | Layer | Verdict | Pick: solo → clients |
| --- | --- | --- | --- |
| 1 | Isolation runtime | buy | gVisor → plus Kata (runtime-rs) on bare metal |
| 2 | Sandbox lifecycle | buy | host user → StatefulSet → agent-sandbox (router not deployed) |
| 3 | Distribution | buy | k3s on 1.36 → Talos per client cell |
| 4 | CNI and egress fence | buy | Cilium → Cilium plus ClusterNetworkPolicy |
| 5 | Admission | buy | Pod Security plus ValidatingAdmissionPolicy → plus Kyverno image checks |
| 6 | Secrets store | buy | SOPS-fed Secrets → OpenBao per cell; broker deferred |
| 7 | Credential-injecting gateway | build policy | ccsp ×2 with explicit routes → ccsp in front of Agent Router for Bedrock/Vertex EU |
| 8 | Model gateway, redaction | build | ccsp |
| 9 | Workload identity | buy | projected service-account tokens plus a source-pod check |
| 10 | SSO | buy | Tailscale identity → a Keycloak realm per client |
| 11 | Messaging | build semantics | long poll plus durable inbox → NATS JetStream |
| 12 | Discovery | build | a scoped directory in the hub, A2A Agent Card shape |
| 13 | Tree, coordination, consent | build: the product | tree-core and the ❓ queue → plus AccessRequest objects |
| 14 | Web Tree, MD Canvas | build (keep) | behind Tailscale Ingress → oauth2-proxy plus Keycloak |
| 15 | Terminal access | buy core | kubectl exec via the Tailscale proxy → thin attach gateway with an SSH CA |
| 16 | GitOps | buy | Flux plus SOPS (and a small merge bot) |
| 17 | Observability, detection | buy | Prometheus, Loki, OpenTelemetry plus ccsp canaries → plus Falco |
| 18 | Backup | buy | restic plus etcd snapshots → Longhorn or Velero, off-site |
| 19 | Testing tools | buy tools | netassert, kdigger, Peirates, kubescape, Chainsaw; the plan is ours |
| 20 | Tenancy | buy | a cell (own cluster) per client |
| 21 | Storage | buy | local-path → Longhorn |

### Vanity to build

A runtime or VMM, a CNI or DNS firewall, a TLS-intercepting proxy with its own CA, a vault or a broker for one person, a message transport, SSO or a browser shell, a conversation renderer, scanners.

### The product to build

Sessions under tasks, checkpoints and briefs, the ❓ queue, managers and field ownership, the turn-aware last hop, the directory, ccsp's redaction and pinning, and the test plan. Budget: about six custom security components for solo.

## Who the others are

Hosted platforms are reference only. The buy candidates are open source or self-hostable.

**Why most big platforms keep sandboxes off plain Kubernetes.** Modal, Fly, Vercel, Cloudflare and AWS built their own schedulers for 10⁵–10⁶ sandboxes, sub-second starts and memory snapshots. Some do run on Kubernetes: E2B's supported self-host, Northflank with Kata, and Google's GKE Agent Sandbox. At CC Flow's scale (about 120 live sessions) only two of the big platforms' reasons apply: the shared kernel, answered by gVisor or Kata, and no pod hibernation, answered by volumes plus transcript resume plus checkpoints.

**The pattern everyone converged on** is the one ccsp already points at: a VM-grade wall around the agent, no secrets inside it, a proxy that adds credentials on the way out, and a control plane outside the sandbox.

### NVIDIA OpenShell

**Closest on security.** Could replace egress injection, the fence logic and consent ceilings.

Single-vendor and months old, on the most sensitive path. Worth a one-week spike.

### Coder OSS

**Closest on human access.** Attach gateway, web terminal and IDE access for client developers.

Leaves the whole agent side, Rule #1 included, to us. Postgres, OIDC, wildcard DNS; open-core drift.

### OpenSandbox

**Closest all-in-one.** Lifecycle, egress and a credential vault.

Its vault lives inside the sandbox pod, so it does not meet Rule #1 as is.

### Building blocks

**The recommendation.** Mostly proven code and the smallest exit cost.

The most custom security code, so it gets a component budget and a pentest.

On a scorecard corrected for bias, the build path is one point ahead of Coder OSS, so the score does not decide. The build path rests on sessions never depending on the tree service, staying terminal-first, nobody shipping the flow layer, and tenant zero dogfooding the company stack.

## The five risks that matter most

Out of 45 in the full register, sorted by severity.

1. **Legitimate paths out.** A push to the GitOps repo that Flux applies, or a workflow file that leaks CI secrets. Open today, and independent of Kubernetes.MR-06 · critical
2. **The credential gateway and our own security code.** ccsp will hold every credential, and custom code on the security path is the weakest layer, in Anthropic's own words.MR-08 · MR-11
3. **The tree service as a cross-session pivot.** Reachable by every agent, holding keys, creating pods and typing into sessions: the pattern behind LiteLLM's October breach. Hence the three-way split.MR-39 · new
4. **Operator patch lag.** The existing cluster runs an end-of-life Kubernetes with a runc that three container escapes affect.MR-33
5. **The model credential and its terms.** No documented way keeps the Max login behind a gateway without the token sitting in Claude Code. Injecting it at the gateway is unknown, and the terms are unasked.MR-04 · MR-45

## Decide now

If nothing is said: every recommendation applies, and Phase 0 starts.

| Q | Decision | Recommendation | Blocks |
| --- | --- | --- | --- |
| ❓ 39 | Order of work: the no-cluster wins first, then the ladder with G1 and its stop rule | yes | everything below |
| ❓ 6 | Deploy autonomy on the production GitOps repo | off now; back only after Flux tenancy lockdown and a merge bot, then for OWN/INERT apps only | MR-06, open today |
| ❓ 3 | Ask Anthropic in writing about a gateway-injected Max token and about client jobs on a personal plan; price two weeks of ccsp's log at API rates | ask now; a Console key at ccsp for every unattended role | Phase 2, G1 |
| ❓ 17 | Upgrade the existing cluster off 1.33 and patch runc | yes, now, one minor at a time | Gate 0, any client |
| ❓ 45 | Rung 0b, the host boundary on hp | yes, after Phase 0 | the evidence for G1 |
| ❓ 46 | The structural rule, the three-way tree-service split, envelope signing and the complexity budget | adopt all four | the Phase 3 design |
| ❓ 1 | Where the first cluster workloads run | own small ccf cluster on 1.36, after quotes, and measure session memory before sizing the agent node | Phase 1 |
| ❓ 10 | Move the hriste, pharmacy ×19 and /k3s-audit bypass jobs into cluster Jobs | yes, in Phase 2 | Phase 2 |

## Questions, answered

The questions from the brief, one short answer each.

Is Kubernetes the right fit, and is it "sandboxed by nature"?

It is the right fleet layer: for unattended jobs now, and for client cells, which need SSO, audit, quotas and GitOps across clients. It is not sandboxed by nature: pod networking is open by default, the kernel is shared, and in 2026 every runtime family had escape CVEs. Isolation is configuration (Pod Security, admission policies, network policy, zero RBAC, no secrets in the pod) plus gVisor or Kata, proven by tests. For your own sessions, a separate Unix user, root-owned settings, an nftables fence and injection at ccsp give the same properties on a host, which is why interactive pods wait for G1.

Can the environment change dynamically, and is there consent?

Yes. What an agent asks for mid-task changes live, in seconds: egress to a host, credentialed reach through a gateway, labels, CPU and memory, mounted config. Anything about the pod's shape needs a new pod at a turn boundary, which resume from the transcript makes cheap. Kubernetes has no consent primitive, so consent is built: the agent files an access request, a human approves on an authenticated surface, the gateway applies an expiring rule, and no service account can approve. Ceilings come before prompts, because users approve about 93 % of prompts.

Is a messaging hub still needed? Which native primitives? How do agents discover each other?

Yes, a smaller one. Kubernetes has no inbox, no addressing by task, no turn-aware delivery, no provenance and no message-level authorization, and that is where today's failures sit: 3.7 % failed sends, stale ids after restarts, truncated deliveries acked as delivered, fan-out to every matching pane. It does give identity (projected tokens), a registry (StatefulSet names), labels, Leases, fences, native sidecars and CronJobs. Agents never discover pods: pods do not talk to each other, and an agent asks a scoped directory through its sidecar. "See each other through NFS" was most likely dictation for DNS: Services and DNS serve infrastructure, network policy decides who may talk.

What happens when a pod restarts? Does the session stay itself?

A planned restart happens only between turns, after the sidecar has saved the work. The new pod resumes the same Claude Code session from its transcript, in the same directory. The task keeps its address across restarts and moves, and its messages follow it. Background processes die. A cold resume costs a cache write; above 100k tokens, a fresh start from the newest checkpoint is cheaper. Each pod start is a new generation, so old tokens and grants stop working.

What does a company of up to 500 people need?

The sandbox stays the same; people, tenancy and money change: a cell per client, a Keycloak realm brokering its identity provider, a leaver who loses everything within five minutes, client-owned model accounts with EU inference where the data needs it, an audit trail captured outside the pod, crypto-shredding for GDPR erasure, no per-person monitoring, and an operating team that can patch, staff a pager and sign a liability and NIS2 contract pack.

Where should it run, thinking Europe?

Tenant zero: a Hetzner Cloud VM for the control plane, gateways and tree service, a Hetzner dedicated server for the session pods, backups to EU object storage. Client cells: Hetzner dedicated workers for Kata, with OVHcloud or Scaleway as the tested second provider. The model can stay on Anthropic's API through ccsp; clients whose data must stay in the EU use Bedrock or Vertex in EU regions.

Who are the others, and build or buy?

Nobody ships CC Flow's layer: the Markdown tree in git, sessions under tasks, checkpoints, the ❓ queue and managers. What changed in 2026 is the layer underneath: agent-sandbox, OpenShell, OpenSandbox, E2B, Agent Router. Most are months old and single-vendor; the proven parts are gVisor, Kata, Firecracker, Envoy, Cilium and NATS. So buy the substrate, build the flow, and spike OpenShell as the one platform that could shrink our custom security code.

How do we test that isolation works?

As ccsp already does: fakes only, counts not values, a block and an allow test for every control, probes from the agent's real identity, every block an incident, mutation that must turn the suite red, the same test on the laptop, in CI and as a production job, and a threats × controls × tests map where an empty cell is a finding (224 test ids; 26 cells still empty). The suite proves the configuration and the alarms. It cannot prove the absence of unknown kernel bugs, bugs in our own proxy or leaks at the provider; the VM boundary, fuzzing, a pentest and contracts cover those.

## Next iteration: tiers and a stack lab

Brief in doc 69, written so an agent can pick it up cold.

Iteration 2 does not repeat this research. It picks one stock component per layer and designs the platform as **at most five cheap increments**: from a solo kit Tomas can hand to friends and family, to a company of 100 people each running about 100 agents. Each tier carries data and identities over and has its own cost in euros, operator effort, security coverage and acceptance tests.

CC Flow wires in at one seam: `cc_tree.py` sends every tmux call through a single function, so a session-host adapter (local tmux, host user, pod) lets it move between tiers without a rewrite. Then tier 1 is installed from scratch in a lab and the first 15 tests decide which components stay.

At about 10,000 agents some of the big platforms' scale arguments start to apply, so the top tier gets a capacity model, and the first question for Tomas is how many agents run at the same time.

**How this was made.** Three inventories of the repos, three web surveys, three independent blueprints (minimal, company platform, threat-first), four deep dives (testing, messaging hub, live changes and consent, company needs), a merge, a build-versus-buy scoring, a critique, a test-gap check and two fact-checks against primary sources: 159 claims, 135 confirmed, 19 corrected, 1 refuted, 4 unverifiable. Non-obvious claims in the full document carry evidence tags (documented, reported, observed, inferred, unknown).

Full blueprint and the 20 evidence reports: the dotfiles repository, cc-flow/docs/proposed-changes/68-k8s-blueprint.md and the folder next to it.