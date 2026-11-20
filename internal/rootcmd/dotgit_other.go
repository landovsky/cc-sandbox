// dotgit_other.go: the descriptor-based look at `.git` exists on Linux, where ccsbx-root runs;
// elsewhere it refuses, so a build for another system compiles and can never derive a gitdir.

//go:build !linux

package rootcmd

import (
	"fmt"
	"os"
)

func dotGit(*os.File) (string, []byte, *os.File, error) {
	return "", nil, nil, fmt.Errorf("linux only")
}
func readChild(*os.File, string) ([]byte, error) { return nil, fmt.Errorf("linux only") }
func hasRegularChild(*os.File, string) bool      { return false }
