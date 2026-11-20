# Night 1: what needs you (cc-sandbox:#2f3e, 2026-11-20)

Only what the manager can't do. Each item says what, why, and exactly how; nothing here should
need a question back. Risk / blast radius: High, Medium, Low.

| # | what you do | where | risk | blast |
|---|---|---|---|---|
| 1 | Push main (171 commits) and check the first fence-image and side-image Actions runs | hp, GitHub | Low | Medium (public repo, first package) |
| 2 | Decide the 6 P15 questions so the root installer stops refusing | this doc | Low | Low |
| 3 | Install the seccomp profile and run its docker test (Gate M) | hp, sudo | Medium | Medium |
| 4 | Run Gate M #4x65 (root install + reboot), moved to 2026-11-22 | hp, sudo | High | High |
| 5 | Say Arm! once Gate M passes | this tab | Low | Medium |

## 1 · Push main, then the first fence-image and side-image runs

Why: local main has PRs #2, #8–#14 and the new fence-image workflow merged (each reviewed, tests
green after every merge), 171 commits ahead. The workflow can only reach GitHub with your
credentials: my token, and the workers', lacks the `workflow` scope (GitHub refuses a push that
adds a workflow file without it), and `git push … main` is on the builders' deny list.

```
git -C ~/git/cc-sandbox push origin main
gh pr close 7 --comment "Merged into main in efa441e."
# the push touches image/** and the ccsbx sources, so BOTH workflows start (fence-image, side-image); watch each:
gh run watch "$(gh run list --repo landovsky/cc-sandbox --workflow fence-image.yml --limit 1 --json databaseId --jq '.[0].databaseId')" --repo landovsky/cc-sandbox
gh run watch "$(gh run list --repo landovsky/cc-sandbox --workflow side-image.yml --limit 1 --json databaseId --jq '.[0].databaseId')" --repo landovsky/cc-sandbox
```

Pass: the fence-image run is green and its summary shows `ghcr.io/landovsky/ccsbx-fence@sha256:…`; the side-image run is green and its summary shows `ghcr.io/landovsky/ccsbx@sha256:…` (the sidecar image). If it
fails at "Log in" or "Build and push" with a permission error: GitHub → landovsky/cc-sandbox →
Settings → Actions → General → Workflow permissions → "Read and write permissions", then
`gh workflow run fence-image.yml --repo landovsky/cc-sandbox` (and `side-image.yml`). Gate M steps 0 and 0b read the digests from
these runs; nothing to copy by hand.

Then one choice: hp must pull the image by digest at Gate M, and a new ghcr package is private.
**Rec:** make it public (the repo is public and the image test proves it holds no secrets):
GitHub → your profile → Packages → `ccsbx-fence` → Package settings → Change visibility → Public. The sidecar package `ccsbx` is a second new package: the k3s cluster pulls it, so it needs the same choice (public, or a pull secret on the k3s side).
The alternative is a `read:packages` token on hp (`docker login ghcr.io -u landovsky --password-stdin`).

## 2 · P15 questions

Why: the root installer refuses while P15 rows are open. How: reply in this doc with "recs" (all
my recommendations) or the numbers you want differently; stream D then marks the rows. Nothing to run.

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

## 3 · Seccomp profile + its docker test (Gate M, before step 8)

Why: `ccsbx-root run` refuses to start any fence without `/etc/ccsbx/seccomp.json`. The profile is
docker's own default for your engine version plus a rule that a fence can't set setuid/setgid
bits; the test proves docker enforces it. Both are denied to me (`curl https`, `docker run
--security-opt`, sudo).

Precondition: PR #8 is on main. Check: `ls ~/git/cc-sandbox/internal/seccomp` lists files (I merge
#8 today after its last small round; if the folder is missing, it isn't merged yet).

```
cd ~/git/cc-sandbox && git pull --ff-only
make build GO=$HOME/.local/go/bin/go                 # builds bin/ccsbx
V=$(docker version --format '{{.Server.Version}}')
curl -fsSL -o /tmp/default.json https://raw.githubusercontent.com/moby/moby/v$V/profiles/seccomp/default.json
bin/ccsbx seccomp-profile --base /tmp/default.json --out /tmp/seccomp.json
sudo install -d -o root -g root -m 0755 /etc/ccsbx
sudo install -o root -g root -m 0644 /tmp/seccomp.json /etc/ccsbx/seccomp.json
bin/ccsbx seccomp-profile --verify /etc/ccsbx/seccomp.json          # must print ok / exit 0
tests/seccomp-setuid-test.sh --base /tmp/default.json --image ccsbx-fence:dev --ccsbx bin/ccsbx
```

Pass: the last command exits 0 (chmod 755 works; chmod 4755, 2755, u+s and creating a 4755 file
are refused inside a fence). It only uses `docker run --rm`; nothing is removed. If the curl 404s,
your engine's tag has no profile at that path: tell me `$V`.

## 4 · Gate M

Moved to 2026-11-22. The installer's final review found it sound, but after the gate nothing
could start a fence yet: the client that calls `ccsbx-root` (U1c) isn't built, and running the gate
first would also cut every hp worker off from docker. A builds U1c by 11-21; D fixes two steps
in the Gate M list (go under sudo; the seccomp step moves into the table as 8b, so §3 above
becomes part of the gate). Merged and ready: the signer, `ccsbx-root`, the fence image.
What you pass and run at Gate M, beyond the old steps (all in the step table): step 0 and **0b** read the two image digests from the Actions runs; step 8 is
`sudo units/install-linux.sh install --yes --image "$DIGEST" --side-image "$SIDE" --hub-url http://100.101.219.96:4011 --hub-secret-from ~/.config/session-bus/secret`
(`--hub-url` is where the native hub listens and is required; re-read it from `ss -ltnH 'sport = :4011'` first; `--hub-secret-from` defaults to that path; the install refuses while the hub is still the docker container,
because its listener is root's, not yours); **8c** `sudo units/hub-probe --owner "$USER" --hub <the same url>` must print `POST 201`, `DELETE 200`, `hub secret OK` (401/403: stop and uninstall).
Not passed at Gate M: `--revoke-doors` (no doors before M1). Merge order: A's `ccsbx-root` change first (it accepts the non-loopback `hub_url` and the new keys).
Steps with commands: `docs/night-1-morning.md` (§ "Gate M, step by step"). The title of #4x65 says "ACL paste": that is step 11b there, a placeholder until the k3s PR number is known (`paste k3s PR <TBD>: steps follow`).

## 5 · Arm!

The milestone is NOT ARMED until Gate M passes. Type `Arm!` in this tab.

---

Low, whenever (each one command):
- 5 empty test networks: `docker network ls --filter name=ccsbx-net- --format '{{.Name}}'`, then
  `docker network rm <each name listed>`.
- Throwaway test image (fake string only): `docker rmi ccsbx-fence-leak:test`.
- C0c host test, on the Mac: `cd ~/git/cc-sandbox && spike/c0c.sh host`.
- messaging-hub: nothing needed. Before 01:20 only `messaging-hub-db-1` ran (the compose file says
  hub and sidecar run on the host via `bin/dev`); the db is back. The exited hub/sidecar
  containers came from the full `docker compose up`; remove them with
  `docker rm messaging-hub-hub-1 messaging-hub-sidecar-1` if you like.
