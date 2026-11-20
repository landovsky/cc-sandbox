# Night 2 plan: what is left for Accept §9 items 1-3 (cc-sandbox:#3xx4)

**What this is.** A read-only plan of every piece still needed before go-live items 1-3 of the build
plan's §9 (fence, launch, events) pass on hp, the Mac and future-two. Night 2 in §7.1 (F2-F5, M2b,
C12-C16, first slice end to end on hp + future-two) has no tasks yet; this doc is the input for them.
**Why.** Go-live needs Gate M plus those three items. Without a per-piece list of what is built, what
can be tested before root, and who owns it, the nights get scheduled by piece titles, and titles
overstate what exists.

**How it was checked (2026-11-20, main 8493f66).** I listed `cmd/` and `internal/` and read the files
behind each row. I grepped for `k8s`, `kubectl`, `whois`, `downlink`. I read the dotfiles branch logs
(cc-tree, session-bus). I did NOT run anything. Every Night 1 fence result is "invariant 14 waived: not
evidence". **Size:** XS under half a day, S about a day, M about two. **Pre-M** = buildable and testable
with no root, no SHARED k3s merge and no live-tool deploy (direct mode, stub, fake API servers).

**Where the code is.** `cc-sandbox` main has the Go side. The cc-tree and session-bus side lives in
`~/.dotfiles`, **not deployed**, on branches `n1-c` (B2 B3 B5 B6c B9 B10 B11, M2b hub half), `n1-f`
(C1-C9, C22) and `n1-g-cc` (n1-f plus C10 C11 C13 C15). Streams: A platform/root, B token/fwd,
D ops/installer, G hostd/bus, k3s = a09b4c.

## 1. Already on main (do not re-plan)

U5 token, P2 forwarder (fwd), C18 stub, U0b signer (PR #7), U1b docker backend + `ccsbx-root`
(run/stop/attach/gc, seccomp), U2 start call, U3a/U3b hostd + local delivery, M3 journal, M4 verb
queue + lease (daemonapi), U17 settings compile, H1 host git (gitroute), U1a image source and pins
(Dockerfile, pins.env, git-shim), U0/U0b/I0 `units/install-linux.sh` (plan, not run), e2e/00-07 against
the stub. **Not on main, on branches:** B1 (hostd on the hub) is on `n1-g-b1`, reviewed once.

## 2. Pieces still needed for §9 items 1-3

Item: 1 fence, 2 launch, 3 events. "Main" says what the code shows, not what the title says.

| Id | Plan row (short) | Main | Deps | Pre-M? | Size | Stream |
|---|---|---|---|---|---|---|
| U1a | fence image by digest, multi-arch | source and `pins.env` (base digest, Claude sha256 per arch) on main; no Actions-built ghcr digest yet; the night-1 report still lists the pins as Tomas's to confirm | R19, Tomas confirms pins | local build yes; ghcr digest after R19 | S | D |
| U17 | settings compile | compiler on main; `selftest` partial: needs the image | U1a | partly | XS | A |
| P15 | replace hp docker/sudo users | the checklist tool `units/p15` exists; its rows wait on Tomas's decisions (#4vwb) | Tomas | no: Tomas's call | M | D |
| Gate M | U0 + U0b live, reboot | scripts + tests exist; never run as root | P15, B9 | **no** | Tomas | D |
| U1b-live | fence on docker with real root | direct mode only | Gate M | after M | S | A |
| M8 | manifests (doors, hostd, NetPols) | **absent** (k3s repo OWN PR) | K1-K4, K10 | write yes, apply no | S | k3s + B |
| M9 | trusted-key list, `ccsbx-machines` | absent; keyring format with `pairs=` exists in token/ | U0b, M8 | code yes | S | B |
| M1 | doors live (`ccsp-model`, `ccsp-egress`) | absent; ccsp is another repo | M8, R5/R5b/R7, SHARED go | **no** | S | k3s |
| U15 | egress inject routes (GitHub, MCP, fetch) | fwd has door allowlist only; no egress door code here | M1 | code in ccsp repo; test after M1 | M | B + k3s |
| U4s | spike: subscription + web search via a door | absent | R5b | no (live subscription) | XS | B |
| F2 | k8s backend (to the live contract §4.6) | **absent** (only `docker.go`; no kubectl/REST code) | U1b, U5, U0b, R10b | code + fake API server yes; live after | M | A |
| F3 | sidecar `ccsbx side` (uid 10002) | **absent** (`internal/side/` is empty) | F2, P2, H1, B6c | code yes; pods after F2 live | M | B |
| F4 | in-cluster hostd | absent | F3, M2a, K13 | no | S | G |
| F5 | attach to a pod (`kubectl exec`) | docker attach only (`rootcmd/attach.go`) | F2 | code yes | XS | A |
| M2a | daemon on the tailnet, WhoIs caller classes | **absent**: api.go says "WhoIs classes are M2a"; unix socket only | R4 for live | code yes (fake WhoIs); live after R4 | S | G |
| M2c | remote start `POST /v1/sessions`, rate caps | absent (`/v1/start` local only) | M2a, U2 | yes | S | G |
| C1, C2 | placement gate, attach windows | `n1-f` | U2 | yes | done | (F) |
| C12 | remote start + remote worktree spec | absent | M2c, F5, C2 | yes (two local daemons) | S | F |
| C13 | cockpit up/down/steal | `n1-g-cc` | M4 | yes | done | (F) |
| C16 | manager starts with cockpit down | absent | C12, M2c | yes | S | F |
| C24 | web asks daemons; ▶ Start for future-two | absent | C12 | yes | S | F |
| M14 | three machines in CC Flow, alias F2 | absent | C4b, F5 | yes | S | F |
| C14 | hp to Mac attach | absent | C12, Mac sshd (Q14) | no: Mac sshd | XS | F |
| C3, C4a, C4c | client layer, follower, tab state | `n1-f` (C3's H2 channel stubbed) | M3 | yes | done | (F) |
| C4b | multi-machine follow, `away` | absent | C4a, M3 | yes | S | F |
| C6 | verb writer | `n1-f` | C4a, M4 | yes | done | (F) |
| B5 | fenced presence on the hub | `n1-c` | B6c | hub deploy is live-tool: after | done | (G) |
| B1 | hostd as hub client | `n1-g-b1` (not merged) | B11 | yes | merge | G |
| M2b | cross-machine msgs via hub | hub half in `n1-c`; daemon half absent | B1-B3, B5, B10, U3b | yes (local hub) | S | G |
| B7 | ACL tag:ccsbx to hub 4011 | requirements only | k3s K5, R4 | no (ACL paste) | XS | k3s |
| install-mac | `units/install-mac.sh`, `_ccsbx`, LaunchDaemon signer | **absent**; signer has no Mac unit | U0b | write yes; run needs Mac root | M | D |
| C19-C20 | e2e per hook point, cockpit kill | e2e/00-07 exist against the stub only; 02-events needs C4a deployed | C4a, C18 | stub yes | S | F |

"(F)" = cc-tree/ccf stream (not in the brief's list; I propose the manager names an owner).

## 3. What can be built before Gate M (the Night 2 queue)

In order: M2a, M2c, F2 with a fake API server, F5, F3, B1 merge, M2b daemon half, M9, C12, C16, C4b,
C24, M14, install-mac, M8 manifests as a PR. That is about 14 days across 4 streams; about 5 working
days of wall time. **Only after Gate M:** live fence runs (U1b-live), M1, F2/F3 on future-two, B9 live,
anything needing R4/R10d. First slice on hp + future-two needs M1 and F2 live, so it cannot be Night 2
night-one unless Gate M and the M1 SHARED go happen first.

## 4. Steps for Tomas (with the how)

1. **Gate M (hp root).** Steps and exact commands: `docs/night-1-morning.md` ("Gate M, step by step"),
   short list in `docs/night-1-hitl.md`. Dated 2026-11-22. Order matters; the reboot is step 9.
2. **Image pins (U1a, R19).** `gh workflow run fence-image.yml --repo landovsky/cc-sandbox`; Gate M step 0
   reads the digest. hp pulls by digest and a new ghcr package is private, so add a read-only
   `read:packages` token and `docker login ghcr.io --password-stdin` on hp and the Mac (plan §7.2 R19).
3. **Tailnet ACL (R4, M2a, B7).** Admin console, Access controls, paste the k3s repo's
   `artifacts/tailscale-config.json`, Save. On the Mac check `socketfilterfw --getglobalstate`; allow
   the signed hostd if the firewall is on.
4. **Door credentials and SHARED go (M1).** R5 capped API key, R5b `claude setup-token` on the Mac,
   R7 GitHub App `cc-sandbox-egress` (Contents RW, Metadata R, PRs RW, nothing else). Encrypt each with
   `read -rs` then `sops -e` into the k3s repo on a branch, never in a file or argv (exact text: plan
   §7.2 R5, R5b, R7). Merge the M8/M1 PR yourself after k3s review.
5. **Mac legs (only if Q12 says Mac is in).** `sudo units/install-mac.sh install --yes` once it exists
   (R10d); Remote Login on, key-only and tailnet-only for C14 (Q14); Docker Desktop settings (R13).
6. **future-two.** Up, no maintenance planned, and the LUKS unlock works from the phone (R11).
7. **Arm!** in the night manager's tab after Gate M (R16).

## 5. Open questions (each with my recommendation)

- **Q-N2-1. Is the Mac in Night 2?** Accept items 1-3 say all three machines. Q12 D7 says Mac legs: no
  for nights 1-2. *Rec: hp + future-two for Night 2; Mac after both are green, as its own night.*
- **Q-N2-2. First slice before or after M1?** M1 needs the SHARED go and your secrets. *Rec: split
  Gate M (root, reboot) from the M1 go; first slice runs on hp docker with the stub until M1 lands.*
- **Q-N2-3. Is F4 (in-cluster hostd) needed for go-live?** Items 1-3 need hp's hostd to serve future-two
  sessions; F4 is only for hp-down. *Rec: defer F4 to Night 3.*
- **Q-N2-4. Owner for the cc-tree/ccf stream.** None of A, B, D, G, k3s covers C12/C16/C24/M14/C4b/C19.
  *Rec: name stream F (the one that built n1-f).*
- **Q-N2-5. Who deploys the dotfiles branches** (n1-c, n1-f, n1-g-cc: not deployed)? Q6 says yes behind
  flags with a rollback. *Rec: one deploy gate after Gate M, S0 green first, you present.*
- **Q-N2-6. Doors enforce profile-billing pairs?** The signer enforces them; a stolen key is only
  limited if M9 carries `pairs=` and doors read it. *Rec: yes, put `pairs=` in the trusted list.*
- **Q-N2-7. Priority.** *Rec: M2a/M2c, F2/F5, M9 first (they unblock C12 and the doors); install-mac
  last (only matters if Q-N2-1 pulls the Mac in).*
