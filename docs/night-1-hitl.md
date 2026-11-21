# What needs you (cc-sandbox:#2f3e, updated 2026-11-21)

Only what the manager can't do, for the goal set 2026-11-21: one real fenced working day (C25) on
hp before any new feature work. Each item says what, why, where and how. Risk / blast radius:
High, Medium, Low. Done items are dropped; git history has the old list.

| # | what you do | where | risk | blast |
|---|---|---|---|---|
| 1 | Answer the 6 P15 questions (§1) so the root installer stops refusing | reply to the manager | Low | Medium (hp host services) |
| 2 | Answer the five fenced-day ❓ (§2) | reply to the manager, by id | Low–Medium | up to Medium |
| 3 | Run Gate M #4x65 with [gate-m.md](gate-m.md) (root install + reboot, ~55 min) | hp, sudo; step 15 on the Mac | High | High (all of hp) |
| 4 | ❓ #2xq4: deploy the dotfiles branches at one gate after Gate M, with you present | hp + Mac, with the dev manager | Medium | High (every CC session) |
| 5 | R5b: `claude setup-token` for the model door, after ❓ 2bv6 and once the manager names the file | hp | High (uncapped 1-year credential) | High if leaked |
| 6 | Optional: push main (local commits since `dd26a78`) | hp | Low | Low |

## 1 · P15 questions

Why: the root installer refuses while P15 rows are open (Gate M step 3). How: reply "recs" (all
recommendations) or the numbers you want differently; stream D marks the rows. Nothing to run.

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

## 2 · Fenced-day decisions (rec: yes on all five)

From `docs/fenced-day-unknowns.md` (branch `n2-u-fenced-day-unknowns`): no model door exists yet,
so a fenced agent on hp has no model route. With these recs the path is your gates, then about
4–5 small pieces on hp, no k3s.

- **66bv** the fence runs in docker on hp (what Gate M installs). Risk Low, blast Low.
- **2bv6** the doors run on hp as host services under their own uid; the subscription token never
  enters the cluster, so k3s secrets-encryption leaves the path. Risk Medium, blast Medium.
- **4a3g** for the solo day the host pushes the session's own branch (fixed refspec, never `+`,
  never `main`) and opens its PR with your credentials; the egress door is COMPANY-TIER.
  Risk Medium, blast Medium (your GitHub account).
- **2rvs** approvals are a known gap on day 1 (`grant.request` gets a clean `not_built`); P7 and
  C17 right after. Risk Low, blast Low.
- **455v** "open" web covers every process in the fence, not only WebFetch; three docs get fixed.
  Risk Medium (an accepted exfiltration path for the solo day), blast Low–Medium.

## 3 · Gate M

**Follow [gate-m.md](gate-m.md):** 15 steps in order, exact commands with the expected output
and what to do when it differs, the current digests (main `dd26a78`), the private-repo fetch
before the install, the seccomp profile and its test (step 11), the `ccsbx-root worktree-check`
smoke, the first e2e start, and future-two's ephemeral-storage check. Bring: the P15 answers, a
private repo you can fetch, your sudo password, an admin kubeconfig on the Mac. No agents running
during the build step (tell the manager first). What the fenced day doesn't need is listed there
as LATER.

## 4 · Not blocking (defaults applied; answer any time)

- **2q42** normal start in a folder a sandbox wrote to: refuse only inside it (applied).
- **3cy2** sandbox relays type into host sessions: manager only (applied).
- **2dsc** tailnet mutations on another machine's daemon without a secret: local-only (applied).

## 5 · Parked by the goal

- k3s [PR #20](https://github.com/landovsky/k3s/pull/20) (manifests, do not merge) and
  [PR #21](https://github.com/landovsky/k3s/pull/21) (ACL paste): LATER in gate-m.md.
- C0c host test on the Mac (`cd ~/git/cc-sandbox && spike/c0c.sh host`): the Mac is out of scope.
- Arm! (verify days): after the fenced day.

---

Low, whenever (each one command):
- Throwaway test image (fake string only): `docker rmi ccsbx-fence-leak:test`.
- Two exited messaging-hub containers from a full `docker compose up` (24 h old):
  `docker rm messaging-hub-hub-1 messaging-hub-sidecar-1`.
