# config/machines: the trusted-key list (plan M9)

One `<machine>.json` per machine that may sign session tokens. Doors, hostd and the hub read this
list (shipped as the ConfigMap `ccsbx-machines`) and refuse any token whose key is not listed, or
whose owner, machine, profile or billing is outside that key's entry. The format and the rules are in
the header of `internal/machines/machines.go`; `ccsbx machines --help` has the recipes.

    ccsbx machines check          # read every file, refuse the first problem
    ccsbx machines render         # print the ConfigMap; nothing is applied from here

This directory has no machine files yet: a file needs the machine's real PUBLIC key, which exists
only after Gate M (`ccsbx-signer --init`). Add `hp-ubuntu.json` then, in an OWN PR with k3s review.
Public keys are not secret; never put a private key or seed here.
