# R-WT (#22d4): the target machine creates the worktree from {repo, base, branch}: scope note

What this is: the scoping note the manager asked for before R-WT is built. It exists so the docker-host path (the daemon)
and the pod path (the sidecar, F3) are split once and neither duplicates the other. Nothing here is built yet.

## What exists
`start.ParseSpec` validates `workdir.{repo,base,branch}` (github.com/<owner>/<repo>, plain ref names, no `..`/`//`/`.lock`/`@`)
and `Daemon.Start` refuses the form with `not_built`. H1 (`internal/gitroute`) is a hardened git RUNNER (always `GIT_DIR`/`GIT_WORK_TREE`,
hooks/config/fsmonitor off, `--end-of-options`); it has no clone or `worktree add` verb. The remote start (M2c) already carries the form end to end.

## Split
| Backend | Who creates the worktree | Why |
|---|---|---|
| docker (hp, Mac) | **the daemon**, as the owner user, before the pane starts (`internal/worktree`, new, called from `Daemon.Start` where `not_built` is now) | the daemon already owns the record and the `--worktree-root`; it fetches with the OWNER's own git credentials (ssh agent / credential helper of that user), which never enter the fence |
| k8s pod (future-two) | **the sidecar** (F3, N1-B), inside the pod, with the forwarder's token | a pod has no owner credentials and no host path; F3's "clone/commit/push with H1" is exactly this; R-WT provides only the shared argv rules (below) |
R-WT therefore builds the docker half and exports the rules both halves must follow; it does not touch the pod's volume or token handling.

## Docker half
- **Where clones live (DECIDED by the M2c review): the owner's STATE directory, never under `--worktree-root`.** One bare clone per repo at
  `<state>/clones/<owner>/<repo>.git` (0700, owner-owned, outside every root), fetched not re-cloned. A cache under a root would be mountable by a
  start naming it as `workdir.path` (a root that is not `$HOME` does not refuse hidden names): the fence would get refs, objects, config and hooks that
  the daemon later fetches into and every other session checks out from (cross-session poisoning). Each session gets `git worktree add` at
  `<worktree-root>/<label-slug>` on `refs/heads/<branch>` from the resolved base; only that worktree's own gitdir is ever mounted, read-only. If helper mode's
  gitdir rule (`CheckGitDirLexical`: the gitdir under a root) ever forces the cache under a root, the name is RESERVED: CheckWorkdir and ccsbx-root refuse it
  at any depth under any root. The location is pinned now because moving it later means re-pointing every worktree's `.git` file.
- **argv rules (contract C5):** every value is one argv element, after `--end-of-options`; `git check-ref-format --branch` on top of the charset; branch always `refs/heads/<branch>`;
  base resolved with `rev-parse --verify --end-of-options <base>^{commit}`; never a shell; run through `internal/gitroute.Runner` (hardened env), not bare `git`.
- **Auth (no token in the fence):** the fetch runs in the daemon as the owner user with that user's own credential helper/ssh agent; the fence never sees it. Private repos work iff the owner's
  host user can fetch them. `GIT_TERMINAL_PROMPT=0`: a missing credential is a refusal (`bad_spec`: repo not fetchable), never a prompt. No credentials are written into the clone's config.
- **Cleanup:** the worktree is removed with the session's record at end-of-life (`ccsbx stop`/ended record + a retention of N days, default 7, `--worktree-keep-days`), never while a session is live; a failed start
  removes what it created (no half-built worktree: the record is written only after `worktree add` succeeds). Bare clones are kept (they are the cache); `git gc` weekly.
- **Locking/limits:** one fetch per repo at a time (flock in the clone), a per-fetch timeout (default 120 s) and size cap on the bare clone (`--clone-max-mb`), so a hostile repo cannot fill the disk; submodules and LFS are off (as H1).

## Out of scope / open
Pushing the branch back (H1's push goes through `fwd.sock`, unchanged); a base that is not on the default remote (refused: `bad_spec`); more hosts than github.com (the charset allows only it).
Open question for the manager: is the clone cache under `--worktree-root` acceptable, or should it live in the state dir (hostd runs as a service uid and cannot enter the owner's home in helper mode, so in helper mode the CLIENT would have to create the worktree as the owner, the way it already resolves `workdir.path` and `gitdir`; then R-WT = a client verb `ccsbx worktree-prepare` + hostd shape checks)? My recommendation: build the direct-mode daemon path first, and the helper-mode client verb second.

## Status: the direct-mode daemon path is BUILT (internal/worktree, hooked into start.Daemon.Start)
- `worktree.Builder.Prepare(owner, label, repo, base, branch)`: bare clone `<state>/clones/<owner>/<owner-repo>.git` (fetch of the base branch only, a flock per clone,
  a size cap `MaxCloneMB` 2048 that deletes an oversized clone, 120 s fetch bound), then `git worktree add -b <branch> -- <root>/<label-slug>-<6hex> <resolved sha>`.
  Hostile-repo switches are on every command line (hooks path /dev/null, fsmonitor off, only https+ssh, submodules and LFS filters off, fsck objects, no prompts); the
  owner's own git configuration is how access works (their credential helper / agent; nothing of it is written to the clone or reaches the fence). Refusals are fixed
  text only. C5 rules: charset (ParseSpec), `check-ref-format`, `refs/heads/` qualified, base resolved by `rev-parse --verify --end-of-options`, only the commit id is passed on.
- `Daemon.Start`: with a builder (direct mode, an absolute worktree root) the repo form is built OUTSIDE the daemon lock, then started on the resulting path like any start
  (so the remote git-worktree-root rule, root check, overlap rule, pins and caps all apply); a retry already recorded builds nothing; a start refused after the build removes
  its worktree; a resume cannot name a repo. Helper mode and pods keep `not_built` (helper: the client prepares it later; pods: the sidecar, F3).
- Cleanup: `Daemon.PruneWorktrees` (hostd hourly, `--worktree-keep-days`, default 7): a worktree and its branch go when its session ended longer ago than that, or no record names it
  and it is untouched that long; a live session's, or a recently ended one's, stays. Bare clones are kept (they are the cache).
- Not done: `git gc` of the clones; per-repo auth hints for private repos beyond "the owner's git can fetch it"; helper-mode client verb.
