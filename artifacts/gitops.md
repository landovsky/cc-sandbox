---
app: cc-sandbox
platform: k3s
namespace: cc-sandbox                # plus cc-sandbox-doors (the doors); fences also run as docker on hp and the Mac
repo: landovsky/cc-sandbox
image: ghcr.io/landovsky/ccsbx       # also ccsbx-fence; doors are ghcr.io/landovsky/ccsp (built in the secret-proxy repo's own Actions)
prod_url: -                          # no public URL: the doors are tailnet-only
nodes: [future-two]                  # the only k3s node it uses. hp and the Mac run docker fences + hostd outside k3s (§2)
last_verified: 2026-11-20            # DRAFT: checked against the k3s team's live review of 2026-11-20 (not by a kubectl of ours)

stack:
  language: go
  database: none
  cache: none
  jobs: none
  error_tracking: none
  logshipping: loki                  # promtail ships all pod logs; door-decision lines are planned (O2)
  metrics_shipping: none
  analytics: none
  ci: github_actions                 # planned (R19): images built by Actions, pushed to ghcr
  cd: flux                           # Kustomization cc-sandbox; binaries on hp by ccsbx-deploy (NB5) or by hand
  tls: none                          # tailnet-only, TLS by the Tailscale operator
  backups: none                      # the hostd journal PVC is stateful and NOT backed up: see §7 (_GAP:_)

versions:
  database: "-"

credentials:
  dev: none                          # night 1 uses a dev key file in direct mode; no secrets in the repo
  prod: sops
---

# cc-sandbox: GitOps / operations

> **DRAFT (O1).** Written on night 1 from [the build plan](../docs/cc-unified-model-build-plan.md),
> then corrected against the k3s team's review of 2026-11-20 (devops:#3q6d). Statements marked
> **per k3s review 2026-11-20** come from that review of the live cluster; this stream has no
> `kubectl`, so it did not check them itself. `_GAP:_` rows are real unknowns, to be filled by the
> piece that makes them true. The final version comes after the first SHARED go (M1) and goes back
> to the k3s side for review (K7). Every manual procedure is in [`docs/runbook.md`](../docs/runbook.md)
> (O7); this file links to it and does not repeat it.
> This repo is public: tailnet names, IPs and hostnames beyond the machine names stay out.

## 1. At a glance

cc-sandbox runs Claude Code sessions inside fences (a docker container on hp and the Mac, a pod on
future-two) so CC Flow / cc-tree can drive them as it drives unfenced ones. A per-session
forwarder holds a per-generation-bound session token; the fence never sees it. One daemon per
machine (`ccsbx-hostd`) keeps the journal and stamps requests; the doors (`ccsp-model`,
`ccsp-egress`) are meant to run as pods on future-two in the `cc-sandbox-doors` namespace and be
the only way out for the model, git and packages.

**What is live and what is not** (per k3s review 2026-11-20):
- **Live, from the k3s side:** the Flux Kustomization `cc-sandbox` is Ready (at `d9286b9`); both
  namespaces exist; the watchdog CronJob runs every 5 minutes; the PVC `ccsbx-hostd-journal` (5Gi)
  is Bound.
- **Not live:** none of our own code. No door, no hostd, no fence pod, no launcher has been
  deployed; night 1 builds on branches and PRs only.

## 2. Topology

- **future-two** (Hetzner, the only k3s node used; `pool=shared:NoSchedule` taint; LUKS, so a
  reboot waits for a manual unlock: dropbear :2222, k3s `docs/HARDWARE-INVENTORY.md`, runbook §7):
  namespace `cc-sandbox` (fence + sidecar pods) and `cc-sandbox-doors` (the two doors and the
  in-cluster hostd; the VAP refuses role door/hostd in `cc-sandbox`, per k3s 2026-11-20). Pods are created by hp's launcher with a namespace-scoped kubeconfig and carry
  `cc-sandbox/role=session|door`, no SA token, no Secret refs, ≤ 4Gi / 4 CPU / 40Gi per container.
- **Admission and network** (per k3s review 2026-11-20): two ValidatingAdmissionPolicies,
  `cc-sandbox-pod-guard` and `cc-sandbox-subresource-guard`, pin pods and images (K11); the
  Tailscale ProxyClass `ccsbx-future-two` (approved) pins the operator's proxies to future-two; the
  doors are exposed by the operator with tag `tag:ccsbx` (the ACL is the k3s repo's
  `artifacts/tailscale-config.json`).
- **Namespace defaults** (per k3s review 2026-11-20): the default ServiceAccount carries
  `imagePullSecrets: ghcr-pull`; `cc-sandbox` has a quota of 40Gi memory, 20 pods and 0 PVCs
  (so the hostd's PVC is not here: it is in `cc-sandbox-doors`, per k3s 2026-11-20; see §7).
- **hp-ubuntu** is not a k3s node. It runs the docker fences, `ccsbx-hostd :7480` (uid `ccsbx`),
  `ccsbx-root` (the only sudo target), the `ccsbx-keys` uid with the machine key, the signer, tmux
  server `-L ccsbx`, and the session-bus hub `:4011`. How hp reaches the cluster: hp reaches the k3s control plane's API over the tailnet admin grant, and the launcher kubeconfig uses that server (per k3s 2026-11-20). The address is deliberately not written here: this repo is public.
- **The Mac** (optional, Q12): Docker Desktop fences, `_ccsbx` signer, hostd. Not required by any gate.
- **Worktrees on future-two** are `emptyDir` with a size limit (ephemeral: push early).
- Full picture: plan §1 and §5.

## 3. Deployment

- **Source of truth:** `main` of this repo; manifests under `apps/cc-sandbox/` in the k3s repo.
  Flux Kustomization `cc-sandbox` (`clusters/production/apps/cc-sandbox.yaml`, path
  `./apps/cc-sandbox`, `prune` and `wait` on) applies them. The k3s reviewer merges OWN paths;
  Tomas merges SHARED ones.
- **SHARED files** (outside `apps/cc-sandbox/`, so a Tomas merge each; per k3s review 2026-11-20):
  the two VAPs, the Namespaces, the ProxyClass, the NetworkPolicies in `tailscale`, the
  `flux-system` Role, and the root-list entry for the Kustomization.
- **Deploy check:** `flux get kustomization cc-sandbox` (Ready, and the revision is the merge you
  expected). We have no kubectl in a night; the k3s side or the Mac runs it.
- **Images:** built by GitHub Actions from `main`, by digest only (ghcr, multi-arch). Production
  digests are listed in `pins.toml` and promoted by hand: runbook §8.
- **Binaries on hp:** the root parts (`ccsbx-root`, the signer, `ccsbx-keys`) are installed by
  Tomas with `units/install-linux.sh` (runbook §3). Other binaries by `ccsbx-deploy` if Q12 = yes
  (runbook §5), otherwise by hand.
- **The night:** gates and auto-start: runbook §1, §2, §4. Preflight: `bin/ccsbx-preflight`;
  report: `bin/ccsbx-night-report`.
- **Live tools the project changes** (session-bus, cc-tree/cc-flow, ccsp): behind flags,
  with a rollback to the `pre-ccsbx-2026-11-19` tag; strict modes flip at a human gate: runbook §9–§11.
- Preconditions of the first deploy of our own code: the SHARED merges, the ACL paste, the keys
  (R5, R5b, R7), and hp's root install and reboot.

## 4. Rollback

- **Manifests:** `git revert` the merge in the k3s repo and push; Flux prunes what the revert
  removes (per k3s review 2026-11-20). **Not Flux's, so not undone by a revert:** the session pods
  the launcher created, and the watchdog's state ConfigMap `cc-sandbox-watchdog-state`. Delete
  those by hand if they matter.
- **Per piece:** images and `pins.toml` revert to the previous digest (runbook §8).
- **Per live tool:** to its `pre-ccsbx-2026-11-19` tag: runbook §9 (cc-tree/cc-flow, session-bus
  hub, ccsp). Hub after B9: previous release directory (§10).
- **hp root install:** `units/install-linux.sh uninstall` restores the backup (runbook §3).
- **Kill switch for a night:** `cc-tree disarm cc-sandbox:2f3e`; nothing a night writes is deployed.

## 5. Configuration & Secrets

- **Config:** `config/machines/<m>.json` (the trusted-key list → ConfigMap `ccsbx-machines`,
  M9, planned); `profiles/`; `pins.toml`; every tunable is a flag plus a `CCSBX_*` env var and
  listed in `--help`. `etc/preflight.toml` configures the preflight.
- **Secrets that exist** (SOPS in the k3s repo, per k3s review 2026-11-20):
  `apps/cc-sandbox/secrets-cc-sandbox-watchdog.env` (the watchdog's Telegram access),
  `ghcr-pull.sops.yaml` (the image pull secret, in `cc-sandbox` and the doors namespace),
  `launcher.kubeconfig.sops.yaml` (the launcher's kubeconfig; **not applied by Flux**: it is
  decrypted on hp, and its token is a TokenRequest bound to the anchor).
- **Secrets that do not exist yet** (`_GAP:_ each is created by the R-step named; none is in the
  k3s repo today`): `secrets-cc-sandbox-model.env` (the capped Anthropic API key, R5),
  `secrets-cc-sandbox-model-sub.env` (the subscription setup-token, R5b, waits for Q13),
  `secrets-cc-sandbox-egress.env` (the GitHub App, R7). Tomas's offline age master is the
  recovery. Until Q13 (k3s secrets-encryption) is done, only test keys go in.
- **Rotation** (runbook §12–§13): model and GitHub App keys every 90 days; setup-token yearly
  (created: `_GAP:_ date, set when R5b is done`; a 30-day-before warning is `_GAP:_` too, see §9);
  machine keys yearly or on loss; session tokens ≤ 12 h; launcher token every 30 days (self-renewed).
- **Spend:** only the Console workspace's monthly cap on the API key ($50, hard; Q12). Recorded
  limit: `_GAP:_ set when R5 is done`. Subscription sessions have only the plan's own limits.

## 6. Services & Dependencies

| Dependency | Used for | If it is down |
|---|---|---|
| `ccsp-model` door | every fenced session's model calls | fenced sessions stop thinking; unfenced work is unaffected |
| `ccsp-egress` door | git push, package mirrors, MCP routes, web fetch (open for now, logged: a known limitation) | fenced sessions can read and edit, not push |
| session-bus hub (hp) | messages between daemons | direct daemon-to-daemon delivery (M2d); same-machine messages never use it |
| the cockpit (cc-tree on hp) | the tree view | visibility only: fenced work, messages and grants carry on (plan §8) |
| future-two (LUKS) | the pods and the doors | after a reboot it waits for a manual unlock (runbook §7); hp docker fences carry on without the doors only for stub work |
| Telegram (`notify-tomas-telegram`) | approval codes, the morning table | approvals can't be given; grants wait |
| Telegram (the k3s devops notifier) | the watchdog's alerts (§9) | no alert |
| GitHub | the egress door's App, ghcr | pushes and image pulls fail |

## 7. Backups & Restore

The hostd journal lives on a PVC, so this app is **stateful**.

- **What exists:** the PVC `ccsbx-hostd-journal` in `cc-sandbox-doors` (5Gi, local-path, on future-two), Bound (verified live, per k3s 2026-11-20). The journal is pruned 14 days after the cursor passes it; cockpit requests
  are never pruned.
- _GAP: the PVC is not backed up, and local-path storage lives on one node's disk. Decide: back it
  up (where, how often, encrypted with what), or record "not backed up on purpose, rebuilt from X"
  with the rebuild step. Plan §8 says a hostd restarted with a wiped journal starts a new epoch
  and the next outcome still lands (test C20 step 3b); events not yet followed into the tree repo
  would be lost. Until decided, treat the journal as expendable and push early._
- **In git, so recoverable:** config and static grants; the tree repo's node history.
- **Keys:** SOPS in git plus Tomas's offline age master.
- **Audit log:** in Loki. _GAP: retention is not set._
- _N/A — the doors have no state of their own, and the fence is ephemeral (future-two worktrees
  are `emptyDir`; push early)._

## 8. Debugging / Runbook

- **Is it healthy?** `bin/ccsbx-preflight --morning --post-M1` (read-only; one line per check);
  the doors' owner `/health`; `flux get kustomization cc-sandbox`.
  `ccsbx status`: `_GAP:_ the command is U2's, confirm the name.`
- **Logs** (per k3s review 2026-11-20): promtail ships all pod logs to Loki, so
  `{namespace="cc-sandbox-doors"}` should work in Grafana; `_GAP:_ mark it live once someone has
  run it against a real door.` Door decision lines are O2's (planned). hostd's journal path:
  `_GAP:_ from U3a`. Hub today: `sb-deploy-hub logs`.
- **Cluster diagnostics:** the read path for agents is the `ccsbx-ops` ServiceAccount's RBAC
  (`rbac-ops.yaml`, k3s amendment A6: read plus a pod-delete restart), not the Mac's cluster-admin
  kubeconfig. With it: `kubectl get pods -n cc-sandbox -o wide`;
  `kubectl -n cc-sandbox create job --from=cronjob/cc-sandbox-watchdog wd-now` runs the watchdog
  once, on demand (both per k3s review 2026-11-20).
- **Procedures (all in the runbook):** the evening and morning gates (§1, §2); cockpit failover
  (§6); future-two unlock (§7); pin promote (§8); rollback of a live tool (§9); hub redeploy (§10);
  strict-flag flips (§11); key and token rotation (§12, §13); the approval code (§14).
- **A fence won't start:** `ccsbx start --json` prints a refusal code (plan §4.1).
- **Known issues:** night 1 runs with invariant 14 waived (tomas is in the docker group): every
  fence result is "not evidence". Three `github-runner@N` units keep docker access through their
  own unit even after tomas leaves the group (P14 inventory, decision pending).

## 9. Maintenance

- **Watchdog (k3s side, live; per k3s review 2026-11-20):** CronJob `cc-sandbox/cc-sandbox-watchdog`
  (manifests `apps/cc-sandbox/watchdog/`, Flux), every 5 minutes. It watches for **Pending or
  failing pods** and sends a Telegram through the devops notifier; its state is the ConfigMap
  `cc-sandbox-watchdog-state`; its secret is SOPS (`secrets-cc-sandbox-watchdog.env`). Node
  pressure and eviction alerts come from the shared `k8s-event-notifier`, not from us.
- **`_GAP:_` what O3 in the plan promises and nothing does yet:** a check of future-two's `bootID`
  (a reboot) with the unlock command in the message; door `/health` checks; a run on hp's tschedule;
  the 30-day warning before the setup-token expires.
- **Scheduled on hp (tschedule), planned, none exists yet:** nightly selftest 02:30 (P6); the weekly
  harness-bump job (O6); the nightly e2e (C21); the night auto-start (NB4).
- **Journals:** pruned 14 days after the cursor passes them.
- **Certs:** _N/A — tailnet TLS is the Tailscale operator's._
- **Migrations:** _N/A — no database._
