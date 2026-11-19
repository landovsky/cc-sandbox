---
app: cc-sandbox
platform: k3s
namespace: cc-sandbox                # plus cc-sandbox-doors (the doors); fences also run as docker on hp and the Mac
repo: landovsky/cc-sandbox
image: ghcr.io/landovsky/ccsbx       # also ccsbx-fence; doors are ghcr.io/landovsky/ccsp (built in the secret-proxy repo's own Actions)
prod_url: -                          # no public URL: the doors are tailnet-only
nodes: [future-two, hp-ubuntu]       # future-two runs the pods; hp runs docker fences, hostd and the hub. The Mac is optional (Q12)
last_verified: 2026-11-19            # DRAFT written on night 1 from the plan; not yet checked against a running system

stack:
  language: go
  database: none
  cache: none
  jobs: none
  error_tracking: none
  logshipping: loki                  # planned (O2): one JSON line per door decision
  metrics_shipping: none
  analytics: none
  ci: github_actions                 # planned (R19): images built by Actions, pushed to ghcr
  cd: flux                           # doors + pods' manifests via apps/cc-sandbox/ in the k3s repo; binaries on hp by ccsbx-deploy (NB5) or by hand
  tls: none                          # tailnet-only, TLS by the Tailscale operator
  backups: none                      # stateless doors; see §7

versions:
  database: "-"

credentials:
  dev: none                          # night 1 uses a dev key file in direct mode; no secrets in the repo
  prod: sops
---

# cc-sandbox: GitOps / operations

> **DRAFT (O1), written on night 1 from [the build plan](../docs/cc-unified-model-build-plan.md)
> before anything is deployed.** Rows marked `_GAP:_` are real unknowns, to be filled by the
> piece that makes them true. The final version comes after the first SHARED go (M1) and goes to
> the k3s side for review (K7). Every manual procedure is in [`docs/runbook.md`](../docs/runbook.md)
> (O7); this file links to it and does not repeat it.
> This repo is public: tailnet names, IPs and hostnames beyond the machine names stay out.

## 1. At a glance

cc-sandbox runs Claude Code sessions inside fences (a docker container on hp and the Mac, a pod on
future-two) so CC Flow / cc-tree can drive them as it drives unfenced ones. A per-session
forwarder holds a per-generation-bound session token; the fence never sees it. One daemon per
machine (`ccsbx-hostd`) keeps the journal and stamps requests; the doors (`ccsp-model`,
`ccsp-egress`) run as pods on future-two in the `cc-sandbox-doors` namespace and are the only way
out for the model, git and packages. Nothing is deployed yet (night 1 builds on branches).

## 2. Topology

- **hp-ubuntu** (always on, the cockpit): `ccsbx-hostd :7480` (uid `ccsbx`), `ccsbx-root` (the only
  sudo target), the `ccsbx-keys` uid (machine key, k8s kubeconfig, age key), the signer process,
  tmux server `-L ccsbx`, the session-bus hub `:4011`. hp's kubeconfig is read-only
  (`audit-reader`); only hp's 6443 is open to the cluster.
- **future-two** (Hetzner, k3s agent, taint `pool=shared:NoSchedule`, LUKS): namespace
  `cc-sandbox` (fence + sidecar pods, in-cluster hostd) and `cc-sandbox-doors` (the two doors).
  Pods are created by hp's launcher with a namespace-scoped kubeconfig; pods carry
  `cc-sandbox/role=session|door`, no SA token, no Secret refs, ≤ 4Gi / 4 CPU / 40Gi per container.
  The Tailscale operator exposes the doors (tag `tag:ccsbx`); the ACL is `artifacts/tailscale-config.json`
  in the k3s repo. Pod Security `restricted` + the VAP (K11) pin images to our three ghcr repos.
- **The Mac** (optional, Q12): Docker Desktop fences, `_ccsbx` signer, hostd. Not required by any gate.
- **Network:** daemons talk by pull/direct; the hub is a door too. PVCs: the in-cluster hostd's
  journal (K13). Worktrees on future-two are `emptyDir` with a size limit (ephemeral: push early).
- Full picture: plan §1 and §5.

## 3. Deployment

- **Source of truth:** `main` of this repo; manifests under `apps/cc-sandbox/` in the k3s repo
  (own PRs; the k3s reviewer merges OWN paths, Tomas merges SHARED ones and the ACL).
- **Images:** built by GitHub Actions from `main`, by digest only (ghcr, multi-arch). Production
  digests are listed in `pins.toml` and promoted by hand: runbook §8.
- **Binaries on hp:** the root parts (`ccsbx-root`, the signer, `ccsbx-keys`) are installed by
  Tomas with `units/install-linux.sh` (runbook §3). Other binaries by `ccsbx-deploy` if Q12 = yes
  (runbook §5), otherwise by hand.
- **The night:** gates and auto-start: runbook §1, §2, §4. Preflight: `bin/ccsbx-preflight`;
  report: `bin/ccsbx-night-report`.
- **Live tools the project changes** (session-bus, cc-tree/cc-flow, ccsp): behind flags,
  with a rollback to the `pre-ccsbx-2026-11-19` tag; strict modes flip at a human gate: runbook §9–§11.
- Preconditions of the first deploy: the SHARED merges (K1, K3, K4, K8, K11), the ACL paste,
  the keys (R5, R5b, R7), and hp's root install and reboot.

## 4. Rollback

- **Per piece:** images and `pins.toml` revert to the previous digest (runbook §8); the manifests
  revert through the k3s repo and Flux.
- **Per live tool:** to its `pre-ccsbx-2026-11-19` tag: runbook §9 (cc-tree/cc-flow, session-bus
  hub, ccsp). Hub after B9: previous release directory (§10).
- **hp root install:** `units/install-linux.sh uninstall` restores the backup (runbook §3).
- **Kill switch for a night:** `cc-tree disarm cc-sandbox:2f3e`; nothing a night writes is deployed.

## 5. Configuration & Secrets

- **Config:** `config/machines/<m>.json` (the trusted-key list → ConfigMap `ccsbx-machines`,
  M9); `profiles/`; `pins.toml`; every tunable is a flag plus a `CCSBX_*` env var and listed in
  `--help`. `etc/preflight.toml` configures the preflight.
- **Secrets (SOPS in the k3s repo, `apps/cc-sandbox/`):**
  `secrets-cc-sandbox-model.env` (the capped Anthropic API key),
  `secrets-cc-sandbox-model-sub.env` (the subscription setup-token),
  `secrets-cc-sandbox-egress.env` (the GitHub App), the launcher kubeconfig (age-encrypted to the
  key under `ccsbx-keys`). Tomas's offline age master is the recovery. Until Q13 (k3s
  secrets-encryption) is done, only test keys go in.
- **Rotation** (runbook §12–§13): model and GitHub App keys every 90 days; setup-token yearly
  (created: `_GAP:_ date, set when R5b is done`; O3 warns 30 days before expiry); machine keys
  yearly or on loss; session tokens ≤ 12 h; launcher token every 30 days (self-renewed).
- **Spend:** only the Console workspace's monthly cap on the API key ($50, hard; Q12). Recorded
  limit: `_GAP:_ set when R5 is done`. Subscription sessions have only the plan's own limits.

## 6. Services & Dependencies

| Dependency | Used for | If it is down |
|---|---|---|
| `ccsp-model` door | every fenced session's model calls | fenced sessions stop thinking; unfenced work is unaffected |
| `ccsp-egress` door | git push, package mirrors, MCP routes, web fetch (open for now, logged: a known limitation) | fenced sessions can read and edit, not push |
| session-bus hub (hp) | messages between daemons | direct daemon-to-daemon delivery (M2d); same-machine messages never use it |
| the cockpit (cc-tree on hp) | the tree view | visibility only: fenced work, messages and grants carry on (plan §8) |
| future-two (LUKS) | the pods and the doors | hp docker fences carry on without the doors only for stub work; unlock: runbook §7 |
| Telegram (`notify-tomas-telegram`) | approval codes, the morning table, O3 alerts | approvals can't be given; grants wait |
| GitHub | the egress door's App, ghcr | pushes and image pulls fail |

## 7. Backups & Restore

_N/A — the doors are stateless and the fence is ephemeral. Config and static grants are in git;
keys are SOPS in git plus Tomas's offline age master; the audit log is in Loki (`_GAP:_ retention
not set until O2); daemon journals are pruned 14 days after the cursor passes them (cockpit
requests are never pruned); future-two worktrees are ephemeral, so branches must be pushed._

## 8. Debugging / Runbook

- **Is it healthy?** `bin/ccsbx-preflight --morning --post-M1` (read-only; one line per check);
  `ccsbx status` `_GAP:_ the command is U2's, confirm the name`; the doors' owner `/health`.
- **Logs:** door decisions: Grafana `{namespace="cc-sandbox-doors"}` (O2). hostd: journal under
  the hostd state dir `_GAP:_ path from U3a`. Hub: `sb-deploy-hub logs` today.
- **Common diagnostics:** `kubectl get pods -n cc-sandbox` needs the Mac's kubeconfig (hp's is
  read-only). A fence won't start: `ccsbx start --json` prints a refusal code (§4.1 of the plan).
- **Procedures (all in the runbook):** the evening and morning gates (§1, §2); cockpit failover
  (§6); future-two unlock (§7); pin promote (§8); rollback of a live tool (§9); hub redeploy (§10);
  strict-flag flips (§11); key and token rotation (§12, §13); the approval code (§14).
- **Known issues:** night 1 runs with invariant 14 waived (tomas is in the docker group): every
  fence result is "not evidence". Three `github-runner@N` units keep docker access through their
  own unit even after tomas leaves the group (P14 inventory, decision pending).

## 9. Maintenance

- **Scheduled (tschedule, hp):** nightly selftest 02:30 (P6); door/node watchdog every 5 min (O3);
  the weekly harness-bump job (O6); the nightly e2e (C21); the night auto-start (NB4).
  `_GAP:_ none of these exist yet.`
- **Journals:** pruned 14 days after the cursor passes them.
- **Certs:** `_N/A — tailnet TLS is the Tailscale operator's._`
- **Migrations:** `_N/A — no database._`
