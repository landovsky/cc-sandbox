# The client layer in the fence (plan C3, C5, C6, H2)

What it is: dotfiles `ccf/` (`ccf-hook`, `ccf-statusline`, `ccf-mcp`, `ccfreq.py`, two managed fragments), stdlib-only Python 3. It is
how a fenced Claude Code reports (hooks into `out/events.jsonl`) and asks (the `ccf` MCP tools into `out/requests/`). It is used only
inside the fence; host sessions never load it.

How it gets to the fence, and why it is not in the base image:

1. `ccf/ccf-pack --out DIR` makes a reproducible tar (files at its root) and `candidate.json` with its sha256.
2. `image/build-ccf.sh --ccf-tar DIR/ccf-<digest12>.tar --base ccsbx-fence:dev` builds the CANDIDATE image: the fence image plus the tar at
   `/opt/ccf`, root-owned and read-only (`touch /opt/ccf/x` fails as the fence uid). It refuses a tar that is not EXACTLY the six layer files
   as plain files (or not the digest given with `--digest`), then checks the label, the modes, that no setid file or credential pattern is
   under `/opt/ccf`, that `/opt/ccf` holds nothing else, and that `ccf-hook` runs; a refused layer leaves no image. The base image keeps `/opt/ccf` EMPTY (`tests/image-test.sh image-hardening`).
3. The candidate's digest is what H2's channel takes: `ccsbx pin-check --image <tag> --profile e2e-default` allows a candidate, a
   `dev-default` session refuses it; promotion to production is `ccsbx pin-promote` after a green full suite (not before gate M).
4. The base image carries `python3-minimal` and `libpython3.11-stdlib` at the versions in `image/pins.env` (python3-minimal alone has no
   `json`); `image/build.sh` passes them as build args and `tests/image-test.sh` checks them.

What is protected, and what is not: the files in `/opt/ccf` cannot be edited by the fence. What RUNS is protected by the settings: every
layer process is started as `/usr/bin/python3 -I /opt/ccf/<script>` (no user site-packages, no PYTHON* env, no PATH lookup) and managed
env pins `CCF_OUT`, `CCF_IN`, `CCF_WAIT`, `PATH`, `BASH_ENV`, `ENV` and `PYTHONNOUSERSITE`, because the agent owns its HOME and a
project `.claude/settings.json` can add env keys. `e2e/03-verbs` plants a `.pth`, a `json.py` on PYTHONPATH, a hostile PATH and
BASH_ENV and shows the hooks still report and nothing of the agent's ran.

A session gets the hooks, statusLine and ccf server only if its image carries the layer: `ccsbx run` (and root's `run`) read the image's
`org.ccsbx.ccf.digest` label (`internal/backend/layer.go`). Without it the settings have none of them, because a hook whose script is missing
makes python exit 2 and Claude Code treats a hook exit 2 as blocking, which would refuse every prompt of a base-image session.

What the cc-sandbox side owns: the managed settings (`internal/settings`): the ONE list of hook events (`CCFHookEvents`), the `statusLine`,
the MCP command at `<CCFDir>/ccf-mcp` (the layer is flat), and `ENABLE_TOOL_SEARCH=false` (Claude Code otherwise defers MCP tools behind
ToolSearch, so a scripted or real session would not have the ten ccf tools in its first request). `TestTheDotfilesFragmentSaysWhatTheCompileSays`
(run with `CCF_FRAGMENT_DIR=<dotfiles>/ccf`) fails when the dotfiles fragment and the compile drift.

Daemon side, unchanged: `ccsbx-hostd` ingests `events.jsonl` (hook.* only, identity from its record) and queues each `ccf.*` request for the
realm's cockpit with the same stamp; `cc-tree cockpit up` and `cc-tree follow --write` apply them. In a pod the verb queue lives in the
in-cluster hostd (F4, not built), so the sidecar answers `ccf.*` `unsupported_in_pod`.

The layer's own tests run against the installed copy with dotfiles `tests/ccf-in-image-e2e.sh` (`CCF_CANDIDATE_IMAGE=<tag>`).

Try it: `e2e/run 03-verbs` (set `E2E_BASE_IMAGE` to a fence image built from the current pins, `E2E_CCF` / `E2E_CCTREE` to dotfiles checkouts).
