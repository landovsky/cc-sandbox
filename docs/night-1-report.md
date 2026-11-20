# Night 1 report (cc-sandbox:#2f3e, 2026-11-20 07:00)

```
NIGHT 1 cc-sandbox #2f3e: 55/63 done, 5 blocked, 3 open
piece  state    test  why
------------------------------------------------------------------
U1a    blocked  -     image pins are yours (digest, CC sha256)
U17    open     part  selftest partial; needs the U1a fence image
H1     open     pass  PR #3, git 10/10; MUSTs closed, needs merge
B9     blocked  -     native hub live test is yours
P15    blocked  -     your decisions in #4vwb
C0a    open     fail  spikes miss ACCEPT; C0c host arm on the Mac
C21    blocked  -     needs 3 green nights in a row
C25    blocked  -     needs the fence image (U1a)
------------------------------------------------------------------
stream done open  note
A         6    3  main 52f102d, not pushed; PRs #3 #5 #6
B         3    0  PR #1 (U5 P2 C18), waits on review
C         9    1  dotfiles n1-c pushed, not deployed
D         7    1  root install PR #2, not run as root
F        13    0  dotfiles n1-f, not deployed
G         9    1  PR #4 (U3a U3b M3 M4 C9b), retitle it
H         4    2  C21 C25 blocked
R      done       review on local branch n1-r (not pushed)
------------------------------------------------------------------
review  MUST 6: 5 closed by re-check, B6c-M1 fixed, re-check open
        SHOULD 28, NIT 37
incident 01:20: a test cleanup removed ALL hp containers;
        none restored (HA, hub, memory store, db). Hub is down.
```

For you this morning (in order):
1. Restore the hp containers first: Home Assistant (homeassistant, otbr, matter-server), then the session-bus hub, claude-memory-store, messaging-hub db, pg-test. A test cleanup with an empty filter removed them all at about 01:20. Nobody restored anything.
2. Push main (52f102d, ahead of origin). Then merge PRs #1, #3, #5, #6 and #4 (retitle #4: it carries U3a U3b M3 M4 and the daemon API).
3. U1a image pins: the debian-slim digest and the Claude Code version with sha256 for amd64 and arm64. U17 and C25 wait on them.
4. Before Gate M #4x65 (2026-11-21): read the root-install PR #2 and the review findings (docs/night-1-review.md on the local branch n1-r, not pushed because it has exploit detail). The Gate M steps are in docs/night-1-morning.md.
5. k3s check 5, on hp as ccsbx-keys: the block is in docs/night-1-morning.md step 11. Send the output to the k3s manager a09b4c.
6. P15: the decisions in #4vwb.
7. B9: run the native hub live test (session-bus/test/native_hub.test.sh).
8. Decide: test scripts can run commands the workers' shell denies (docker rm, sb-deploy-hub). My recommendation: let tests/ clean up only their own labelled resources, and keep sb-deploy-hub denied for workers.
9. Smaller: remove the 5 empty ccsbx-net-* networks. Check the 3 'password' redactions in G's go test output (cmd/ccsbx-hostd); they are probably test names, rotate only if one is real. C0c host arm: spike/c0c.sh host on the Mac.
10. The milestone is NOT ARMED (blocker: Gate M). Say Arm! after Gate M.
