// worktree.go: `ccsbx-root worktree-check -- <path>` (R-H, docs/m2c-remote-start.md (4b)). In helper mode hostd runs
// as the service uid and cannot look inside the owner's home, so before it records a REMOTE start's workdir it asks
// root, read-only: is this a git worktree root that `ccsbx-root run` would accept? Root answers with the same
// rules `run` applies at launch (the allowed roots, no symlink, the owner's own directory, no hidden home
// directory, the worktree's own .git derived through a pinned descriptor), plus one `run` cannot see by itself:
// the directory does not equal, contain or sit inside another live session's workdir.
//
// It writes nothing, takes no lock, reads no stdin and no environment. stdout is TWO lines on success, nothing
// otherwise: the resolved absolute path, and the gitdir `ccsbx-root run` will derive for it (hostd records it:
// run refuses a record whose gitdir is not the one it derives, so a path that passes here must come with it); stderr is one fixed line per refusal, with no path and no OS text (the path came
// from a remote caller and root's errors name files). Exit 0: a worktree root; 2: refused; 3: could not tell
// (a read error; hostd treats it as a refusal, never as "ok").
package rootcmd

import (
	"errors"
	"fmt"
	"github.com/landovsky/cc-sandbox/internal/backend"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
  Exactly two arguments: the -- and then one path; no option, no environment, no stdin. Prints the resolved
  absolute path, then its derived gitdir, and exits 0 when it is a git worktree root inside an allowed root that
  overlaps no live session's workdir.
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
			path, gitdir, err := e.worktreeCheck(args[1])
			if err != nil {
				we, ok := err.(*wcErr)
				if !ok {
					we = wcCannot
				}
				fmt.Fprintln(stderr, "ccsbx-root worktree-check:", we.line)
				return we.code
			}
			fmt.Fprintln(stdout, path)
			fmt.Fprintln(stdout, gitdir) // exactly what `run` derives from <path>/.git (empty: nothing to mount)
			return cli.ExitOK
		},
	}
}

// worktreeCheck returns the resolved path of a worktree root and the gitdir `run` will derive from it ("" when
// there is none to mount), or the *wcErr that says why not.
//
// The order is the point. First, rules that read nothing from the disk: the caller, the shape of the path, the
// allowed root, no hidden component. A path that fails them is refused without a single stat, so the service
// user (hostd) cannot use this verb to ask whether a file exists in the owner's ~/.ssh. Then everything that
// depends on the disk (absent, not a directory, another user's, no .git, a symlink) is ONE answer: "not a git
// worktree root". Only a lexical refusal and an overlap with a live session are told apart.
func (e *env) worktreeCheck(p string) (path, gitdir string, err error) {
	w, perr := e.prelude("worktree-check")
	if perr != nil {
		if _, ok := perr.(*Refusal); ok {
			return "", "", wcDisabled
		}
		return "", "", wcCannot
	}
	if w.uid != w.service && w.uid != 0 {
		return "", "", wcCaller
	}
	// lexical: no stat
	if backend.CheckPath(p) != nil || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", "", wcRoot
	}
	root := e.cfg.insideRoot(p)
	if root == "" {
		return "", "", wcRoot
	}
	for _, c := range strings.Split(strings.TrimPrefix(p, root+string(filepath.Separator)), string(filepath.Separator)) {
		if strings.HasPrefix(c, ".") {
			return "", "", wcRoot // a hidden directory under a root (.ssh, .config, .git itself ...) is never a workdir
		}
	}
	// from here the disk decides, and every "no" is the same line
	rp, cerr := cleanPath(p) // no symlink anywhere in it
	if cerr != nil {
		return "", "", fsRefusal(cerr)
	}
	st, serr := os.Lstat(rp)
	if serr != nil {
		return "", "", fsRefusal(serr)
	}
	if !st.IsDir() {
		return "", "", wcNoGit
	}
	owner, oerr := ownerUID(rp)
	if oerr != nil {
		return "", "", fsRefusal(oerr)
	}
	// the worktree belongs to a login user and lies in that user's own home: run will ask for the session's
	// owner, and that is the user whose home it is in, so "passes here" means "run accepts"
	home, herr := e.d.HomeOf(owner)
	if !loginUID(owner, e.cfg.MinUID, w.service) || herr != nil || home == "" ||
		!strings.HasPrefix(rp, filepath.Clean(home)+string(filepath.Separator)) {
		return "", "", wcNoGit
	}
	wd, _, werr := e.cfg.workdir(rp, owner, home) // the rules run applies: roots, owner, hidden home dirs, state and run dirs, fixed mounts
	if werr != nil {
		return "", "", fsRefusal(werr)
	}
	wpin, perr2 := pin(e.d.OpenDir, wd, owner) // held open: what is checked is what is looked at
	if perr2 != nil {
		return "", "", fsRefusal(perr2)
	}
	defer wpin.close()
	gd, gp, gerr := e.gitdirOf(owner, wd, wpin, home)
	if gerr != nil {
		return "", "", fsRefusal(gerr) // a .git that is a symlink, not host-owned, not a git directory, or a linked worktree that points elsewhere
	}
	gp.close()
	if gd == "" {
		return "", "", wcNoGit // no .git of its own: a plain directory, a subdirectory of a repository, a parent of repositories
	}
	live, lerr := e.liveWorkdirs(w)
	if lerr != nil {
		return "", "", wcCannot
	}
	for _, o := range live {
		if wd == o || strings.HasPrefix(wd, o+string(filepath.Separator)) || strings.HasPrefix(o, wd+string(filepath.Separator)) {
			return "", "", wcOverlap
		}
	}
	return wd, gd, nil
}

// fsRefusal turns an error from looking at the disk into the one answer for "it is not a worktree root"; only an
// error that is not a plain "no" (an I/O error) is "could not tell".
func fsRefusal(err error) *wcErr {
	if _, ok := err.(*Refusal); ok || os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EACCES) {
		return wcNoGit
	}
	return wcCannot
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

// loginUID: a worktree belongs to a login user: at or above min_uid, never the service user, never nobody (65534)
// or anything above it.
func loginUID(owner, min, service int) bool { return owner >= min && owner < 65534 && owner != service }
