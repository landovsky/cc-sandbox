# What needs you (cc-sandbox:#2f3e, updated 2026-11-21)

Only what the manager can't do, for the goal set 2026-11-21: one real fenced working day (C25) on
hp before any new feature work. Done items are dropped; git history has the old list.

Each item: what, why it's needed, why the manager can't do it, where and how, risk and blast
radius (High / Medium / Low), and the scope (the domain it changes). Risk is how likely it goes
wrong; blast radius is what is hit if it does.

Shortest unblock: reply "P15 recs, 66bv 2bv6 4a3g 2rvs 455v yes", then Gate M, then 2xq4, then R5b.

## Now: answers only

### 1 · P15: six host questions

- **What:** reply "recs" (all recommendations below) or the numbers you want differently; stream D
  marks the rows. Nothing to run.
- **Why:** the root installer refuses while P15 rows are open (Gate M step 3).
- **Why not the manager:** they decide how your host keeps working (CI runners' docker, container
  restarts, backup jobs, sudoers, Home Assistant and neo4j).
- **Where:** reply to the manager.
- **Risk:** Low to answer. **Scope:** hp host services.
- **Blast radius:** Medium (CI runners, messaging-hub-db, memory-store backups).

1. The three `github-runner@1..3` units keep docker through their own unit after you leave the
   docker group. **Rec:** a dedicated `ghrunner` uid (no sudo, no keys, no home) with docker only
   via those units now; move CI to the k3s ARC runners later.
2. Containers with `restart=no` die at the reboot (`messaging-hub-db-1`, worktree `redis-*`).
   **Rec:** `docker update --restart unless-stopped messaging-hub-db-1` only; let the worktree
   redis go with their worktrees.
3. claude-memory-store's backup/review/daily jobs use `docker exec` and break silently at U0.
   **Rec:** run them by hand with sudo after the reboot; a small root-owned wrapper later.
4. dokku in your NOPASSWD sudoers file: nothing uses it. **Rec:** remove it (the installer does).
5. Home Assistant and neo4j stacks: keep running, operated with password sudo. **Rec:** accept.
6. reboot-check, backup-verify, pg-test, db-up, deploy-assets, weekly-upstream-deploy: **Rec:**
   `sudo docker` by hand for now; move the two GHCR pushes to a runner after 1.

### 2 · Five fenced-day decisions (rec: yes on all five)

- **What:** answer each by id.
- **Why:** `docs/fenced-day-unknowns.md` (branch `n2-u-fenced-day-unknowns`) found no model door,
  so a fenced agent on hp has no model route. With these recs the path is your gates, then about
  4–5 small pieces on hp, no k3s.
- **Why not the manager:** each sets security policy or departs from a documented invariant.
- **Where:** reply to the manager.

| id | decision | risk | blast radius | scope |
|---|---|---|---|---|
| 66bv | the fence runs in docker on hp (what Gate M installs) | Low | Low | cc-sandbox only |
| 2bv6 | the doors run on hp as host services under their own uid; the subscription token never enters the cluster, so k3s secrets-encryption leaves the path | Medium | Medium | hp host: a new service uid holding the subscription token |
| 4a3g | for the solo day the host pushes the session's own branch (fixed refspec, never `+`, never `main`) and opens its PR with your credentials; departs from inv 4; the egress door is COMPANY-TIER | Medium | Medium | your GitHub account, the session's branch only |
| 2rvs | approvals are a known gap on day 1 (`grant.request` gets a clean `not_built`); P7 and C17 right after | Low | Low | cc-sandbox only |
| 455v | "open" web covers every process in the fence, not only WebFetch; three docs get fixed | Medium (an accepted exfiltration path for the solo day) | Low–Medium | whatever is in the fence (repo content) |

## Then: your hands, in order

### 3 · Gate M #4x65: root install + reboot

- **What:** run [gate-m.md](gate-m.md): 15 steps, about 55 minutes if nothing fails.
- **Why:** it is the real install the goal requires; nothing counts as proven without it.
- **Why not the manager:** it needs root, your sudo password and a reboot of hp.
- **Where:** hp, a terminal in `~/git/cc-sandbox`; step 15 on the Mac (admin kubeconfig). No agents
  running during the build step (tell the manager first so it pauses them).
- **How:** `cd ~/git/cc-sandbox && less docs/gate-m.md`, then from step 1. Bring: the P15
  answers, a private repo you can fetch (step 6), your sudo password.
- **Risk:** High (rewrites sudoers, takes you out of the docker group, installs services as root;
  the lockout guards are step 8, before the install). **Scope:** the whole hp host.
- **Blast radius:** High (every container, runner, service and Claude session on hp during the
  reboot). Undo: `sudo units/install-linux.sh uninstall --yes`.

### 4 · ❓ #2xq4: deploy the dotfiles branches

- **What:** say yes, then be present while the dev manager deploys n1-c, n1-f, n1-g-cc and N2-F's
  chain (once reviewed) at one gate after Gate M.
- **Why:** the fenced day needs a deployed cc-tree.
- **Why not the manager:** it deploys CC Flow, which another milestone owns and every session uses.
- **Where:** hp and the Mac. Diff: https://github.com/landovsky/dotfiles/compare/main...n1-c
- **Risk:** Medium. **Scope:** CC Flow (cc-tree, session bus, daemon) on both machines.
- **Blast radius:** High (every Claude session and manager on hp and the Mac).

### 5 · R5b: the subscription setup-token

- **What:** `claude setup-token`, after ❓ 2bv6 and once the manager names the target file.
  Never paste the token into chat.
- **Why:** the model door (and its first spike, U4s) needs it.
- **Why not the manager:** an OAuth login to your account in a browser.
- **Where:** hp.
- **Risk:** High (an uncapped one-year credential for the whole subscription).
  **Scope:** your Anthropic subscription.
- **Blast radius:** High if leaked; revoking it in claude.ai is the remedy (the spike measures how
  fast that takes effect). Rotation: runbook §13.

### 6 · Optional: push main

- **What:** `git -C ~/git/cc-sandbox push origin main` (local commits since `dd26a78`), or say
  "cmush".
- **Why:** keeps GitHub current; not on the critical path.
- **Why not the manager:** it pushes only when you ask.
- **Where:** hp.
- **Risk:** Low. **Scope:** the cc-sandbox repo plus one CI image build. **Blast radius:** Low.

## Not blocking (defaults applied; answer any time)

Why not the manager: each is a security policy choice for CC Flow. Where: reply by id.

| id | question | default applied | risk | blast radius | scope |
|---|---|---|---|---|---|
| 2q42 | normal start in a folder a sandbox wrote to | refuse only inside it | Low | Low | session start in CC Flow |
| 3cy2 | sandbox relays typing into host sessions | manager only | Low | Medium | the bus between sessions |
| 2dsc | tailnet mutations on another machine's daemon without a secret | local-only | Low | Medium | the daemon on every machine |

## Parked by the goal

- k3s [PR #20](https://github.com/landovsky/k3s/pull/20): cc-sandbox manifests, DO NOT MERGE
  until digests set, replicas 0 until M1; off the path if 2bv6 is yes. Risk Low, blast Low,
  scope its own namespace.
- k3s [PR #21](https://github.com/landovsky/k3s/pull/21): Tailscale ACL paste, only needed when
  the cluster reaches hostd (F4). Risk Medium, blast High, scope the whole tailnet policy.
- C0c host test on the Mac (`cd ~/git/cc-sandbox && spike/c0c.sh host`): the Mac is out of scope.
- Arm! (verify days): after the fenced day.

---

Low, whenever (each one command, on hp):
- Throwaway test image (fake string only): `docker rmi ccsbx-fence-leak:test`.
- Two exited messaging-hub containers from a full `docker compose up` (24 h old):
  `docker rm messaging-hub-hub-1 messaging-hub-sidecar-1`.
