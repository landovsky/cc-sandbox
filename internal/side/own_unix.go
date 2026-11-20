// own_unix.go says whether a file belongs to the running uid. It exists so the control directory check
// is one function on Unix and a clear refusal elsewhere.
//go:build !windows

package side

import (
	"os"
	"syscall"
)

func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
