// sweep.go moves steering files (memory, settings, hooks, shell startup) out of a node's home between
// generations. It exists because a file an agent wrote there must be reviewed, not obeyed (invariant 25).

package backend

import (
	"os"
	"path/filepath"
	"strings"
)

// steeringHome are paths in the per-node home that steer a later session: user memory, harness
// settings, commands, agents, hooks, shell startup files and git/npm config. An agent-written one
// is input for review, never trusted instructions for the next generation (invariant 25).
// The transcript store (~/.claude/projects) is deliberately NOT here: resume needs it.
var steeringHome = []string{
	".claude/CLAUDE.md", ".claude/settings.json", ".claude/settings.local.json",
	".claude/commands", ".claude/agents", ".claude/hooks", ".claude/skills",
	".claude/output-styles", ".claude/plugins",
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile",
	".zshrc", ".zshenv", ".zprofile", ".gitconfig", ".npmrc", ".config",
}

// sweepScript moves each steering path out of /h into /q (same relative path) and reports it.
// Symlinks are moved as links, never followed. DIRECT MODE ONLY: it runs the session image as
// container root, which is acceptable only where the caller already holds docker. Helper mode
// (ccsbx-root) sweeps in Go, as root, with no container: see sweepHomeGo.
const sweepScript = `cd /h || exit 1
for p in "$@"; do
  if [ -e "$p" ] || [ -L "$p" ]; then
    mkdir -p "/q/$(dirname "$p")" && mv "$p" "/q/$p" && echo "moved $p"
  fi
done
exit 0`

// QuarantineDir is where a generation's swept steering files go: host side, never mounted.
func (s Spec) QuarantineDir() string { return filepath.Join(s.GenDir(), "quarantine") }

// SweepArgs is the `docker run` argv for the sweep. It runs as root inside a container with no
// network and one narrow capability set, so it can move files the fence (uid 10001) wrote into
// a home the host user cannot edit. It touches only the home and the quarantine mounts.
func (s Spec) SweepArgs() []string {
	if !s.NotEvid {
		panic("backend: SweepArgs is direct-mode only; helper mode sweeps in Go (sweepHomeGo)")
	}
	a := []string{"run", "--rm", "--network", "none", "--user", "0:0", "--read-only",
		"--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--cap-add", "CHOWN",
		"--security-opt", "no-new-privileges",
		"--name", s.Container() + "-sweep",
		"--mount", bind(s.HomeDir(), "/h", false),
		"--mount", bind(s.QuarantineDir(), "/q", false),
		"--entrypoint", "sh", s.Image, "-c", sweepScript, "sweep"}
	return append(a, steeringHome...)
}

// moved parses the sweep's output.
func moved(out string) []string {
	var l []string
	for _, ln := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(ln), "moved "); ok {
			l = append(l, p)
		}
	}
	return l
}

func (s Spec) prepareQuarantine() error {
	if err := os.Mkdir(s.QuarantineDir(), 0o700); err != nil {
		return err
	}
	return nil
}
