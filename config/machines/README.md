# config/machines: the trusted-key list (plan M9)

One `<machine>.json` per machine that may sign session tokens. Doors, hostd and the hub read this
list (shipped as the ConfigMap `ccsbx-machines`) and refuse any token whose key is not listed, or
whose owner, machine, profile or billing is outside that key's entry. The format and the rules are in
the header of `internal/machines/machines.go`; `ccsbx machines --help` has the recipes.

    ccsbx machines check          # read every file, refuse the first problem
    ccsbx machines fmt FILE       # print a file in the canonical form the loader requires
    ccsbx machines render         # print the ConfigMap; nothing is applied from here

The bytes must be canonical (a duplicate or wrong-case key is refused, never "last one wins"), each key
is `{"kid", "key"}` with the kid matching the PUBLIC key, and a file may sign for another machine only
if that machine has no file of its own.

This directory has no machine files yet: a file needs the machine's real PUBLIC key, which exists
only after Gate M (`ccsbx-signer --init`). Add `hp-ubuntu.json` then, in an OWN PR with k3s review.
Never put a private key or seed here: the kid check refuses one, but do not rely on it.

Only the stub reads the list so far (`ccsbx stub --machines-dir`). hostd and the hub still use their
own keyring path; wiring them is separate work, so M9 is not "done everywhere" yet.
