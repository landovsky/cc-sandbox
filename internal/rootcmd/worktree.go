// worktree.go: `ccsbx-root worktree-check -- <path>` (R-H, docs/m2c-remote-start.md (4b)). In helper mode hostd runs
// as the service uid and cannot look inside the owner's home, so before it records a REMOTE start's workdir it asks
// root, read-only: is this a git worktree root that `ccsbx-root run` would accept? Root answers with the same
// rules `run` applies at launch (the allowed roots, no symlink, the owner's own directory, no hidden home
// directory, the worktree's own .git derived through a pinned descriptor), plus one `run` cannot see by itself:
// the directory does not equal, contain or sit inside another live session's workdir.
//
// It writes nothing, takes no lock, reads no stdin and no environment. stdout is one line, the resolved absolute
// path, on success only; stderr is one fixed line per refusal, with no path and no OS text (the path came
// from a remote caller and root's errors name files). Exit 0: a worktree root; 2: refused; 3: could not tell
// (a read error; hostd treats it as a refusal, never as "ok").
package rootcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// wcErr is a failure with its exit code and its one fixed line.
type wcErr struct {
	code int
	line string
}

func (w *wcErr) Error() string { return w.line }

var (
	wcUsage    = &wcErr{cli.ExitRefused, "refused: usage: worktree-check -- <path>"}
	wcCaller   = &wcErr{cli.ExitRefused, "refused: not for this caller"}
	wcRoot     = &wcErr{cli.ExitRefused, "refused: not a path inside an allowed root"}
	wcNotDir   = &wcErr{cli.ExitRefused, "refused: not a directory"}
	wcOwner    = &wcErr{cli.ExitRefused, "refused: not a login user's directory"}
	wcNoGit    = &wcErr{cli.ExitRefused, "refused: not a git worktree root"}
	wcOverlap  = &wcErr{cli.ExitRefused, "refused: overlaps a live session"}
	wcCannot   = &wcErr{3, "cannot tell"}
	wcDisabled = &wcErr{cli.ExitRefused, "refused: the verb is not enabled"}
)

const wcHelp = `  sudo -n /usr/local/sbin/ccsbx-root worktree-check -- /home/tomas/git/project
  Exactly one argument, after --; no option, no environment, no stdin. Prints the resolved absolute path and
  exits 0 when it is a git worktree root inside an allowed root that overlaps no live session's workdir.
  Exit 2: refused (one fixed line on stderr, never the path). Exit 3: could not tell (a read error).
  For the service user (hostd) and root only; it changes nothing and takes no lock.`

func (e *env) worktreeCheckCmd() *cli.Command {
	return &cli.Command{
		Name:    "worktree-check",
		Summary: "say whether a path is a git worktree root that run would accept (read-only; for hostd's remote starts)",
		Recipes: wcHelp,
		Raw:     true,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				fmt.Fprintf(stdout, "ccsbx-root worktree-check -- <path>: say whether a path is a git worktree root that run would accept (read-only)\n\nRecipes:\n%s\n", wcHelp)
				return cli.ExitOK
			}
			if len(args) != 2 || args[0] != "--" {
				fmt.Fprintln(stderr, "ccsbx-root worktree-check:", wcUsage.line)
				return wcUsage.code
			}
			path, err := e.worktreeCheck(args[1])
			if err != nil {
				we, ok := err.(*wcErr)
				if !ok {
					we = wcCannot
				}
				fmt.Fprintln(stderr, "ccsbx-root worktree-check:", we.line)
				return we.code
			}
			fmt.Fprintln(stdout, path)
			return cli.ExitOK
		},
	}
}

// worktreeCheck returns the resolved path of a worktree root, or the *wcErr that says why not.
func (e *env) worktreeCheck(p string) (string, error) {
	w, err := e.prelude("worktree-check")
	if err != nil {
		if _, ok := err.(*Refusal); ok {
			return "", wcDisabled
		}
		return "", wcCannot
	}
	if w.uid != w.service && w.uid != 0 {
		return "", wcCaller
	}
	rp, err := cleanPath(p) // clean, absolute, no mount-spec character, no symlink anywhere in it
	if err != nil {
		return "", wcRoot
	}
	if e.cfg.insideRoot(rp) == "" {
		return "", wcRoot
	}
	st, err := os.Lstat(rp)
	if err != nil {
		return "", wcCannot
	}
	if !st.IsDir() {
		return "", wcNotDir
	}
	owner, err := ownerUID(rp)
	if err != nil {
		return "", wcCannot
	}
	if owner < e.cfg.MinUID || owner == w.service {
		return "", wcOwner // a worktree belongs to a login user, never to root or a system user
	}
	home, err := e.d.HomeOf(owner)
	if err != nil {
		return "", wcOwner
	}
	wd, _, err := e.cfg.workdir(rp, owner, home) // the rules run applies: roots, owner, hidden home dirs, state and run dirs, fixed mounts
	if err != nil {
		return "", wcRoot
	}
	wpin, err := pin(e.d.OpenDir, wd, owner) // held open: what is checked is what is looked at
	if err != nil {
		return "", wcNoGit
	}
	defer wpin.close()
	gd, gp, err := e.gitdirOf(owner, wd, wpin, home)
	if err != nil {
		return "", wcNoGit // a .git that is a symlink, not host-owned, or a linked worktree that points elsewhere
	}
	gp.close()
	if gd == "" {
		return "", wcNoGit // no .git of its own: a plain directory, a subdirectory of a repository, a parent of repositories
	}
	live, err := e.liveWorkdirs(w)
	if err != nil {
		return "", wcCannot
	}
	for _, o := range live {
		if wd == o || strings.HasPrefix(wd, o+string(filepath.Separator)) || strings.HasPrefix(o, wd+string(filepath.Separator)) {
			return "", wcOverlap
		}
	}
	return wd, nil
}

// liveWorkdirs are the workdirs of the sessions that have not ended (root reads the daemon's records, as it does
// for every verb). A record it cannot read is an error: "could not tell" must not become "no overlap".
func (e *env) liveWorkdirs(w who) ([]string, error) {
	dir := filepath.Join(e.cfg.StateDir, "sessions")
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil // nothing was ever started here
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range ents {
		sid, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok || !start.ValidSid(sid) {
			continue
		}
		rec, err := loadRecord(e.cfg.StateDir, sid, w.service, e.cfg.TrustedUID)
		if err != nil {
			return nil, err
		}
		if rec.Ended == nil && rec.Backend != "k8s" && rec.Spec.Workdir.Path != "" {
			out = append(out, filepath.Clean(rec.Spec.Workdir.Path))
		}
	}
	return out, nil
}
