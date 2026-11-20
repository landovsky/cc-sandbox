# The client layer in the fence (plan C3, C5, C6, H2)

What it is: dotfiles `ccf/` (`ccf-hook`, `ccf-statusline`, `ccf-mcp`, `ccfreq.py`, two managed fragments), stdlib-only Python 3. It is
how a fenced Claude Code reports (hooks into `out/events.jsonl`) and asks (the `ccf` MCP tools into `out/requests/`). It is used only
inside the fence; host sessions never load it.

How it gets to the fence, and why it is not in the base image:

1. `ccf/ccf-pack --out DIR` makes a reproducible tar (files at its root) and `candidate.json` with its sha256.
2. `image/build-ccf.sh --ccf-tar DIR/ccf-<digest12>.tar --base ccsbx-fence:dev` builds the CANDIDATE image: the fence image plus the tar at
   `/opt/ccf`, root-owned and read-only (`touch /opt/ccf/x` fails as the fence uid). It checks the digest, the label, the modes and that
   `ccf-hook` runs. The base image keeps `/opt/ccf` EMPTY (`tests/image-test.sh image-hardening`).
3. The candidate's digest is what H2's channel takes: `ccsbx pin-check --image <tag> --profile e2e-default` allows a candidate, a
   `dev-default` session refuses it; promotion to production is `ccsbx pin-promote` after a green full suite (not before gate M).
4. The base image carries `python3-minimal` and `libpython3.11-stdlib` at the versions in `image/pins.env` (python3-minimal alone has no
   `json`); `image/build.sh` passes them as build args and `tests/image-test.sh` checks them.

What the cc-sandbox side owns: the managed settings (`internal/settings`): the ONE list of hook events (`CCFHookEvents`), the `statusLine`,
the MCP command at `<CCFDir>/ccf-mcp` (the layer is flat), and `ENABLE_TOOL_SEARCH=false` (Claude Code otherwise defers MCP tools behind
ToolSearch, so a scripted or real session would not have the ten ccf tools in its first request). `TestTheDotfilesFragmentSaysWhatTheCompileSays`
(run with `CCF_FRAGMENT_DIR=<dotfiles>/ccf`) fails when the dotfiles fragment and the compile drift.

Daemon side, unchanged: `ccsbx-hostd` ingests `events.jsonl` (hook.* only, identity from its record) and queues each `ccf.*` request for the
realm's cockpit with the same stamp; `cc-tree cockpit up` and `cc-tree follow --write` apply them. In a pod the verb queue lives in the
in-cluster hostd (F4, not built), so the sidecar answers `ccf.*` `unsupported_in_pod`.

Try it: `e2e/run 03-verbs` (set `E2E_BASE_IMAGE` to a fence image built from the current pins, `E2E_CCF` / `E2E_CCTREE` to dotfiles checkouts).
