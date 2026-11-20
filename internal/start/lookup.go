// Which session, if any, owns a worktree path. Tools that read git metadata from a worktree ask
// here first, because a fenced session's worktree must never be read through its own `.git` (the
// agent can rewrite it). The answer comes from the daemon's session records, not from the worktree.
package start

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// SessionForWorkdir reports the session whose record names workdir (compared after resolving
// symlinks). Tools that read git metadata from a worktree use it to tell a fenced session's
// worktree from a developer's own checkout: the first must never be read through its own .git.
func SessionForWorkdir(stateDir, workdir string) (string, bool) {
	want, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		want = filepath.Clean(workdir)
	}
	ents, err := os.ReadDir(filepath.Join(stateDir, "sessions"))
	if err != nil {
		return "", false
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(stateDir, "sessions", e.Name()))
		if err != nil {
			continue
		}
		var r struct {
			Sid  string `json:"sid"`
			Spec struct {
				Workdir struct {
					Path string `json:"path"`
				} `json:"workdir"`
			} `json:"spec"`
		}
		if json.Unmarshal(b, &r) != nil || r.Spec.Workdir.Path == "" {
			continue
		}
		got, err := filepath.EvalSymlinks(r.Spec.Workdir.Path)
		if err != nil {
			got = filepath.Clean(r.Spec.Workdir.Path)
		}
		if got == want {
			return r.Sid, true
		}
	}
	return "", false
}
