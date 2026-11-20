// Command ccsbx-signer is the machine-key signer (U0b). It exists as its own binary so the process
// that holds the key is tiny and separate from hostd: hostd asks it to sign claims and never sees
// the key. All behaviour lives in internal/signer; run it with --help.
package main

import (
	"os"

	"github.com/landovsky/cc-sandbox/internal/signer"
)

func main() { os.Exit(signer.Main(os.Args[1:], os.Stdout, os.Stderr)) }
