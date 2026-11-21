[Index](README.md)

# Fenced-day unknowns: what is decided, built and missing for one real fenced day on hp

What this is: the analysis for cc-sandbox:#32sa. It takes the five open unknowns named in the goal of
the milestone "Phased build" (cc-sandbox:#2f3e, Tomas, 2026-11-21) and, for each, says what is
decided, what is built and where, and what is still missing before C25 (cc-sandbox:#5bb2, a fenced
agent does a typical day's work) can pass fenced on hp with the real install.

Why it exists: the goal says agents work on the open unknowns, not on polish. Before anyone builds,
the manager needs to know which unknowns block C25, which can be written down as known gaps, and
which of Tomas's decisions are still missing. This doc changes no code. The ❓ proposals below are
for the manager to create as nodes.

**State read (2026-11-21):** cc-sandbox `main` at dd26a78 (pushed). dotfiles `n1-c` at c2118b1
(pushed, **not deployed**: hp runs dotfiles `main` b708469). The N2-F chain (`n2-f-c12` →
`n2-f-c16` 30b3796 → … → `n2-f-m14`) is **local only, not pushed**. The node's hash ae56a65 for
C16 is the same patch before a rebase. cc-secret-proxy `main` at 916e352. k3s PR #20 (M8) is not
merged. **Gate M has not run.** tomas is still in the docker group and `/run/ccsbx-api` doesn't
exist, so every fence result on hp so far is "not evidence" (runbook, invariant 14 waiver).

Status words: **PROVEN** = ran for real on hp with the real pieces. **BUILT-UNPROVEN** = the code
is merged or pushed, but has only run against a stub, a fake door or in direct mode.
**MISSING** = no code for it exists. Sizes follow the build plan: XS ≈ hours, S ≈ a day,
M ≈ several days.

## Summary

| # | Unknown | Status | Smallest step on hp that proves or closes it | Size | C25 |
|---|---|---|---|---|---|
| 0 | (found) the docker fence on hp has no model route in the real install | MISSING | `ccsbx-root run` mints the token through the signer and starts the forwarder container (P2) on the session's network, with a leg to the doors | S | **blocks** |
| 1 | model door on the subscription | MISSING | proves: the U4s spike (loopback proxy, dummy token in, setup-token injected, web search, revoke). Closes: U4 `ccsp-model` + the `subscription` billing on `dev-default` | XS + S | **blocks** |
| 2 | egress credentials (git, gh, APIs) | reads BUILT-UNPROVEN; writes MISSING | proves: one real R-WT fetch of a private repo on hp. Closes: a hostd `git.*` handler + a push and PR path (❓ C) | XS + S…M | **blocks** (push, PR) |
| 3 | approvals (solo low tier) | MISSING | a `grant.request` gets a clean `not_built` and the session runs on (inv 16); P7 itself later | XS (gap) / S+S (P7) | known gap (❓ D) |
| 4 | manager starts with the cockpit down (C16) | BUILT-UNPROVEN (stub only, unpushed branch) | after the cc-tree deploy: cockpit down, a manager tab's `cc-tree start` of a fenced worker runs; cockpit up, it shows once | XS | known gap if needed; not a blocker |
| 5 | open web fetch vs the exfiltration rule | MISSING (route) + docs disagree | the CONNECT route on the egress door + a fetch route in the forwarder, logged per session; fix the docs | S + XS | **blocks** (the fetch step) |

COMPANY-TIER items are marked in each section and collected at the end.

## 0. Found on the way: no door exists, and the hp docker fence gets no forwarder

These two facts sit under unknowns 1, 2 and 5, so they come first.

- **No door code exists in any repo.** `ccsp-model`, `ccsp-egress` and the doors' owner listener
  are not in cc-secret-proxy (it is still the local redaction proxy, with no door, CONNECT or
  grant code), not in cc-sandbox and not in k3s (`apps/cc-sandbox/doors/` holds namespace, quota,
  NetPol, RBAC and a PVC, no Deployment). The night-2 plan's "M1 absent, U15 code in ccsp repo"
  is still true. What does exist is the door's **front half**: `internal/token` (JWT + `cnf`
  proof-of-possession check, revoke), used by the C18 stub door (`internal/stub`). So a door
  starts from the stub's token check, not from zero.
- **The real-install docker path on hp gives the fence no forwarder.** `internal/rootcmd/verbs.go`
  `run` creates the `--internal` network and the fence, but mints no token and starts no `ccsbx
  fwd` container. `exec-token` is `notWired` ("needs the signer, U0b"). Only the pod path
  (`pod-create`) mints (`realMint`, `internal/rootcmd/pod.go`) and runs a forwarder (the sidecar,
  `internal/k8s/side.go`). The managed settings point the fence at `http://fwd:8080`, which in the
  real install is nobody. `e2e/07-workday` gets its real model step only because `e2e_fwd`
  (`e2e/lib.sh`) starts a forwarder by hand. Also: the forwarder sits on an `--internal` network,
  so even with a door it needs a second network leg to reach one. That design isn't written yet.
- **Also found by 07-workday, still open:** the docker backend doesn't create `out/requests`, so
  the git shim can't write a request until something else makes it (XS).

**❓ A (proposed): Where does C25's fence run?**
*Rec: docker on hp.* That is what Gate M installs and what the goal names. It needs P2 wired into
`ccsbx-root run` (S): mint through the signer as the pod path does, start the forwarder as uid
10002 on the session's network plus one door network, and stop it with the generation.
*Alternative:* a pod on future-two placed by hp. The sidecar already holds the forwarder, git and
relay, but the path needs M8 merged, M1 and a live F2. That is more of Tomas's gates and more of
k3s.

**❓ B (proposed): Where do the doors run for C25?**
*Rec: on hp, as host services under their own uid (not tomas, not ccsbx, not ccsbx-keys), the same
binaries the pods will run, reachable only from the forwarders' door network.* Invariant 12 allows
this: host keys live under a dedicated uid. It also takes k3s secrets-encryption (Q13) off the
critical path for the subscription token. Under 3e4g's rule the token may not go into the cluster
before Q13; on hp it never does, so the day can run on the subscription from the start, as Tomas
asked (Q16). The doors move to future-two (M1) after the day.
*Alternative:* the plan as written (pods in `cc-sandbox-doors`, M8 + M1, SHARED go), with the
first real run on the API key until Q13 is done.

## 1. The model door on the subscription

**Decided.** The subscription is used from day one for fenced sessions (Q16 #4wsw, which overrides
the rec). The terms reading and the token risks are accepted (#3e4g). The token must not go into
the cluster before k3s secrets-encryption (Q13); until then the API key is the fallback. Revocation
is to be confirmed in U4s first. There are no door budgets, only the $50/month Console cap on the
API key (Q12 D8).

**Built.**
- Fence side: `internal/settings` (U17) writes `ANTHROPIC_BASE_URL` = forwarder,
  `ANTHROPIC_AUTH_TOKEN=dummy`, nonessential traffic and telemetry off. The fence holds no
  credential (07-workday: real Claude Code → forwarder → stub, STUB-OK; direct mode, so not
  evidence).
- Forwarder: `internal/fwd` model routes (`POST /v1/messages`, `count_tokens`, `GET /v1/models`),
  token + proof per call.
- Door front half: `internal/token` (verify, `cnf` proof, revoke) and the stub door `internal/stub`.
- Profiles and tokens accept `billing = subscription` (`internal/profile`, `internal/token`).
- Runbook §13 (rotate the setup-token) exists as *planned*.

**Missing.**
- **U4s spike** (nothing in `spike/`). Unproven: which `anthropic-beta` value the injected OAuth
  bearer needs; whether a dummy-token client plus an injecting proxy works at all; whether web
  search (a server tool) works behind a base-URL gateway; whether and how fast a revoke in claude.ai
  takes effect. One weak hint: this hp's own unfenced sessions already reach the model through a
  loopback base URL (ccsp). But they carry their own login, which is not the door's case.
- **U4 `ccsp-model`**: strip inbound `Authorization`/`x-api-key`; inject by the token's `billing`;
  redaction; per-session audit; `upstream_limit` passed through. Code: none.
- **R5b** (Tomas): `claude setup-token`. With ❓ B = hp, it goes into the door uid's 0600 file, not
  into SOPS.
- **`profiles/dev-default.json` lists `billing: [api, stub]`.** A subscription session is refused
  as `billing_unavailable` even with a door (XS; the trusted list's `max_billing` for hp too).
- Unknown 0's forwarder in the docker path.

**Smallest step that proves it (XS, on hp, no fence needed):** U4s as the plan writes it, with a
fresh setup-token from Tomas. Step 1: a loopback logging proxy (header names and beta values only)
in front of an unfenced Claude Code. Step 2: a client with `ANTHROPIC_AUTH_TOKEN=dummy` through a
proxy that injects `Bearer <setup-token>` plus the beta value. Then one `say hi`, one web search,
and a revoke with a timing. The result goes in `spike/u4s.md`.

**Closes it:** U4 on the stub's token check (S) + the `dev-default` billing line (XS) + unknown 0.

**Blocks C25:** yes. Without a model door no fenced session can work (invariant 16's exception).

**COMPANY-TIER:** the terms reading (intermediating Claude.ai credentials) does not carry over to a
company. There, the API key or the company's own plan applies, and a real ruling is needed. Also
company-tier: a yearly-token rotation alert (O3b), and per-door audit shipped to Loki (O2).

## 2. Egress credentials: how a fenced agent gets git, gh and API access

**Decided.** No credential ever enters the fence (inv 2). Every way out is a door (inv 4); "the host
commits and pushes". Git reads on docker use the **owner's own** git credentials on the host,
never in the fence (R-WT scope). Pushes go through the egress door's git route (U16: ff only, the
token's branch only, path and size caps). The GitHub API goes through a method+path allowlist with
per-session GitHub App tokens of ≤ 1 h (U15, App R7). MCP goes through egress routes (N2).

**Built.**
- Reads: R-WT direct mode on main (`internal/worktree`, dd26a78) fetches into a bare clone cache as
  the owner and checks out a session worktree. In the fence: the git shim (`image/git-shim`) passes
  read verbs, and 07-workday shows log, diff and blame working. **Helper mode (the real install)
  keeps the repo form `not_built`**; the path form (an owner-made worktree under the root) works.
- Writes: in the fence, `git commit` and `git push` become `git.commit`/`git.push` requests in the
  outbox, and a force-push to main is refused in the shim (07-workday). On the host, H1
  (`internal/gitroute`) is a hardened runner that can commit. For pods, `internal/side/gitops.go`
  does clone, commit and push against a **fake** door (`e2e/08-sidecar`).
- Forwarder: GitHub API reads, `POST …/pulls`, `POST …/issues/*/comments` (`DefaultRules`). The git
  smart-HTTP routes go only with the sidecar's secret (`GitRules`).

**Missing.**
- **hostd answers every `git.*` request `not_built` on docker.** `cmd/ccsbx-hostd/main.go` handles
  only `msg.` and `ccf.`. Nothing on hp turns the outbox's commit or push into a commit or push.
- The egress door's git route (U16) and API route (U15): no code. The GitHub App R7: Tomas's; not
  recorded as done.
- **Opening a PR from the fence has no client path.** There is no `gh` in the image, and no ccf
  verb. The only route is `curl` to `http://fwd:8080/egress/github/repos/O/R/pulls`, which no
  brief or skill tells the agent about.
- Helper-mode repo form (a client-side `worktree-prepare`; "Not done" in r-wt-scope).
- MCP and other API routes (N2): none.

**Smallest step that proves the read half (XS):** this is also goal rule 2 ("one real fetch of a
private repo"). On hp in direct mode, run `ccsbx start` with `workdir.{repo,base,branch}` naming a
private repo. Check that the worktree appears under the root, the fence's `git log` shows its
history, and `grep -r` finds no credential in the fence's env, home or `/proc/*/environ`.

**❓ C (proposed): How does C25 push and open its PR?**
*Rec (solo, for the day): the host does it as the owner.* hostd's `git.commit` runs H1. Its
`git.push` pushes the session's own branch with a refspec the daemon fixes
(`HEAD:refs/heads/<session branch>`, never `+`, never `main`), using the owner's credentials, as the
R-WT fetch already does. A host verb opens the PR on the session's repo and branch only. The agent
names nothing but the message, title and body. Size S. This cuts U16, U15 and R7 off the critical
path. The cost: a host bug would push with Tomas's wide credentials, and "force-push refused"
becomes a host rule (GitHub's protection on main is the second gate). It also departs from
inv 4's "git through the egress door", so it needs Tomas's yes.
*Alternative:* build the egress door's git and API routes now (U15 + U16, S+S+S) and create R7.

**Blocks C25:** yes, for commit, push and PR. The read half doesn't block (direct mode works;
helper mode has the path form).

**COMPANY-TIER:** the egress door with per-session GitHub App tokens, U16's path and size rules,
and no owner credentials on a host. A company has no "owner's git" to borrow, and the host-push
shortcut must not ship there. Also MCP routes (N2), and package mirrors (U15b) as a real control.

## 3. Approvals: the solo low-tier path

**Decided (Q10 #3y55).** Solo path P7, low tier only (one egress route or one repo read, for one
generation). A decision needs Tomas's tailnet identity **and** a 6-digit code that the door sends
by Telegram, typed on the door's owner listener. No forwarder or session token can decide. Medium
and high are refused until the friends tier.

**Built.**
- `internal/fwd/routes.go` never routes `approve`, `decide`, `grants`, `owner` and so on
  (`T:approve-via-fwd`).
- hostd accepts the `grant.request` type and answers `error/not_built`.
- Questions reach the manager with no cockpit (C9b, a real run in direct mode).
- Runbook §14 exists as *planned*. `e2e/05-approval` is skips only.

**Missing:** all of P7. That is the door's owner listener, `ar-` records with `scopeHash`, the
Telegram code, the decision page, `grant.requested`/`grant.decided` events, the overlay grant on
routes, and C17's grant cards in cc-tree.

**❓ D (proposed): Does C25 need an answered grant?**
*Rec: no. Write it as a known gap for the first fenced day.* With web fetch open (Q19) and the
profile's routes fixed, a typical day needs no grant. The step becomes: "a `grant.request` gets a
clean `not_built` reply and the session works on" (inv 16, XS, in 07-workday). Build P7 (S, the
door half) and C17 (S) right after the day.
*Alternative:* build P7 and C17 before C25. They need the egress door from ❓ C's alternative,
because a grant widens a door route.

**Blocks C25:** no, if ❓ D takes the rec. Otherwise yes (S+S on top of the egress door).

**COMPANY-TIER:** the friends and company tiers: passkeys (U13), medium and high tiers, more than
one approver, an audit of decisions.

## 4. A manager starting workers with the cockpit down (C16 #6e64)

**Decided.** Cockpit down means visibility is lost, never progress (Tomas, 2026-11-19). Agents keep
working and messaging, and visibility comes back with the cockpit. Failover is manual (Q18).
Fenced sessions can't start sessions in this milestone.

**Built (BUILT-UNPROVEN).** dotfiles `n2-f-c16` 30b3796 (rebased from ae56a65, same patch), on
`n2-f-c12`, on `n1-c`. **Local only, not pushed, not deployed.** It went through 3 review rounds
plus the dev manager's should-fix. One launch id. The record is written at start and deduped on
(sid, gen). `rate_capped` exits 75. A node fenced anywhere stays fenced. The manager is limited
to its own milestone. A fenced start in `$HOME` or a main checkout is refused. Every test ran
against `tests/stub-ccsbx.sh`, not the real hostd. The daemon half it needs is on cc-sandbox main
(`/v1/start` on the local socket; `POST /v1/sessions` for the remote form, owner-only on the
tailnet, M2a).

**Missing.**
- Rule 6: nothing may build on it until it is pushed and merged.
- The cc-tree deploy (Q-N2-5, Tomas's gate). That deploy is needed for C25 anyway: the fenced
  start (C1) is on `n1-c`.
- ❓ #2q42 (TAINT_DEFAULT) is still open and blocks `n1-c` reaching main.
- No run against the real daemon.

**Smallest step that proves it (XS, after the cc-tree deploy):** with hostd up and the cockpit
down (`cc-tree cockpit down`, follower stopped), a manager tab runs `cc-tree start <worker>` for a
fenced worker under its milestone. Check that `ccsbx ls` shows it and the worker's
`ccf.question` reaches the manager. Then `cc-tree cockpit up` and check the session appears once
in the tree. A 4th start in a minute exits 75.

**Blocks C25:** no. C25's steps don't take the cockpit down. If the day runs with the cockpit up
throughout, C16 stays a written known gap. Cheap to fold in: take the cockpit down for ten
minutes mid-day.

**COMPANY-TIER:** automatic cockpit failover, cross-machine starts through the hub (M2b/B3,
signed), and fenced managers starting workers.

## 5. The open web fetch vs the exfiltration rule

**Decided (Q19 #6v2c).** Web fetch stays **open** for now, with no allowlist, and is listed as a
known limitation. The model doc (§9) and the build plan (§10 risks, U15, U17) were updated:
CONNECT to any public host on 443, never RFC 1918, the tailnet or node IPs; no credential
injected; every CONNECT logged with sid, host and bytes. The planned fix is a per-profile
allowlist, with other hosts through P7.

**Built.**
- `internal/settings` sets `HTTPS_PROXY` only when a fetch URL is given, and always sets
  `skipWebFetchPreflight`.
- `ccsbx-root.toml` has `fetch_url`, empty by default. So today a fenced WebFetch simply fails
  (the internal network has no route out, which is safe).
- The rule twins exist in `tests/rules.toml` (`webfetch-open-logged`, `webfetch-private-refused`)
  with `allow = block = "pending"`.

**Missing.**
- **The route itself.** The forwarder refuses CONNECT by design (`T:fwd-cannot-reach-non-door`,
  `internal/fwd/fwd_test.go`). So it needs a separate, narrow fetch listener that passes CONNECT
  only to the egress door, and the egress door needs the CONNECT handler with the address checks
  and the log. No code. Size S, plus XS for the forwarder side.
- **The docs disagree with the decision**, which is what "vs the exfiltration rule" means here:
  - Invariant 4 says "git and HTTP through the egress door … there's no other egress". Still
    true: the CONNECT route is on the egress door.
  - The tiers doc says "GET-only package mirrors **instead of open tunnels**". The open CONNECT
    route is an open tunnel.
  - `HTTPS_PROXY` is process-wide, so the route serves `curl`, `npm`, `pip` and `go`, not only
    Claude Code's WebFetch. While it is open, the mirrors' and the GitHub-API allowlist's exfil
    value is void (their credential rules still hold: nothing is injected on CONNECT). The plan's
    §10 risk row already says this; the tiers doc and invariant 4's wording don't.
  - The C25 node's own Accept text still says "web fetch (through the egress door, allowlisted)".
    The plan's C25 row says "open … logged". One of them must change.
- A side effect worth taking: while fetch is open, the build-and-test step's packages work through
  the same CONNECT route. So U15b mirrors aren't needed for C25. The mirrors become COMPANY-TIER.

**❓ E (proposed): What does "open" cover?**
*Rec: confirm that "open" covers every process in the fence, not only WebFetch.* Then fix the
three texts in one docs commit: the tiers row, an inv 4 footnote, and the C25 node's Accept
("open, logged"). Treat the mirrors as COMPANY-TIER.
*Alternative (cheap, if Tomas wants something now):* a small fixed deny list on the CONNECT host
(gist, paste sites, URL shorteners), as the plan's default already names. It doesn't change the
decision; it removes the most obvious drop points (XS on top of the route).

**Smallest step that proves it (after the route exists, XS):** in a fence on hp, WebFetch a public
docs page (it works and the door log has sid, host and bytes), and `curl` a `100.x` and a `10.x`
address through the proxy (refused). That is both twins of the two rules.

**Blocks C25:** yes, for the web fetch step and, under the rec, for build and tests through
packages. Web **search** depends on unknown 1 (U4s), not on this route.

**COMPANY-TIER:** the per-profile host allowlist with P7 grants for other hosts. The mirrors (U15b)
as the only package path. A rule that private code and an open fetch never meet in one session
(the trifecta). **This must change before a company install.**

## Proposed critical path to C25 (if ❓ A–E take the recs)

1. **Tomas:** Gate M (it is the "real install" and ends the evidence waiver), the cc-tree deploy
   (n1-c + the N2-F chain, after ❓ #2q42), and R5b (a setup-token).
2. U4s spike (XS), then the door on hp: `ccsp-model` (S) + `dev-default` billing (XS).
3. P2 in `ccsbx-root run`: the token through the signer, the forwarder on the session's network
   plus the door network, and `out/requests` made by the backend (S).
4. hostd's `git.commit`/`git.push` handler and the PR verb, host side as the owner (S).
5. The egress door with only the CONNECT fetch route + its forwarder listener (S).
6. Fill 07-workday's skips. The grant step becomes the `not_built` check, and the MCP step is a
   written gap (no MCP route without the egress door's inject routes).

That is about 4–5 S pieces after Tomas's gates, all on hp, none in k3s.

## Written known gaps (candidates for the C25 Outcome)

- No answered grant (P7, C17) [❓ D].
- No MCP tool through an egress route (N2).
- No package mirrors. Packages go through the open fetch route.
- Push and PR are made by the host as the owner, not through the egress door [❓ C].
- Cockpit-down start not shown on the day (if skipped) [C16].
- Helper-mode repo form (the path form only).
- Doors on hp, not future-two [❓ B]. future-two pods (F2, M1) are not part of the day.

## COMPANY-TIER (don't build now)

A real ruling on the subscription terms, or the API key only. The egress door with App tokens and
U16, and no owner credentials on hosts. Per-profile fetch allowlists, mirrors as the only package
path, and keeping the trifecta apart. Friends and company approval tiers (passkeys, medium/high).
Automatic cockpit failover and fenced managers. Doors in the cluster with secrets-encryption,
Loki audit (O2) and the door watch (O3b).

## Related

- [Build plan](cc-unified-model-build-plan.md): rows U4s, U4, U15, U15b, U16, P2, P7, C16, C17,
  C25; §6 catalogue; §10 risks.
- [The unified model](cc-unified-model.md): invariants, §9 known limitations.
- [R-WT scope](r-wt-scope.md), [runbook](runbook.md) §13–§14, [night-2 plan](night-2-plan.md) §2.
