// Tests for `ccsbx-root worktree-check` (R-H). Each refusal is a block twin: it exits 2 with one fixed line that
// holds neither the path nor any OS text; a good worktree exits 0 with exactly one line; the check writes nothing.
package rootcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/start"
)

// wcFx is newFx with hostd as the caller and a worktree root at f.wd (a plain repository).
func wcFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(svcUID)
	now := time.Now()
	f.rec.Ended = &now // the fixture's own session is over: the worktree is free
	f.writeRecord()
	mk(t, filepath.Join(f.wd, ".git"), 0o755)
	put(t, filepath.Join(f.wd, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	return f
}

func (f *fx) check(args ...string) (int, string, string) {
	f.t.Helper()
	var out, errb strings.Builder
	code := App(f.cfg, f.d, func(k string) string { return f.env[k] }).Main(append([]string{"worktree-check"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func (f *fx) refusedWC(why string, path string) {
	f.t.Helper()
	code, out, errb := f.check("--", path)
	if code != 2 || out != "" || strings.Count(errb, "\n") != 1 || strings.HasPrefix(errb, "ccsbx-root worktree-check: refused:") == false {
		f.t.Errorf("%s: exit %d stdout %q stderr %q", why, code, out, errb)
	}
	// one fixed line: the path (it came from a remote caller) and OS text never appear in it
	for _, leak := range []string{path, "no such file", "permission", "stat ", "lstat", "operation not"} {
		if leak != "" && leak != "." && strings.Contains(errb, leak) {
			f.t.Errorf("%s: stderr leaks %q: %q", why, leak, errb)
		}
	}
}

func TestAGoodWorktreeRootIsAnsweredWithItsPathAndNothingElse(t *testing.T) {
	f := wcFx(t)
	code, out, errb := f.check("--", f.wd)
	if code != 0 || out != f.wd+"\n" || errb != "" {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errb)
	}
	// a linked worktree (.git is a file naming a host-owned gitdir inside a root) is a worktree root too
	g := wcFx(t)
	os.RemoveAll(filepath.Join(g.wd, ".git"))
	g.linkGitdir()
	if code, out, errb := g.check("--", g.wd); code != 0 || out != g.wd+"\n" || errb != "" {
		t.Errorf("a linked worktree: exit %d stdout %q stderr %q", code, out, errb)
	}
	// root by login may ask too
	r := wcFx(t)
	delete(r.env, "SUDO_UID")
	if code, _, _ := r.check("--", r.wd); code != 0 {
		t.Errorf("root: exit %d", code)
	}
}

func TestEveryRefusalIsOneFixedLineWithNoPath(t *testing.T) {
	f := wcFx(t)
	outside := filepath.Join(f.dir, "elsewhere")
	mk(t, filepath.Join(outside, ".git"), 0o755)
	os.Symlink(outside, filepath.Join(f.roots, "git", "linkout"))
	f.refusedWC("a symlink out of the root", filepath.Join(f.roots, "git", "linkout"))
	f.refusedWC("a symlink out, below it", filepath.Join(f.roots, "git", "linkout", "sub"))
	// a sibling whose name starts with the root's
	evil := f.roots + "-evil"
	mk(t, filepath.Join(evil, "p", ".git"), 0o755)
	f.refusedWC("a sibling prefix of the root", filepath.Join(evil, "p"))
	f.refusedWC("the root itself", f.roots)
	f.refusedWC("a parent of repositories", filepath.Join(f.roots, "git"))
	// a plain directory, and a subdirectory of a repository
	mk(t, filepath.Join(f.roots, "git", "plain"), 0o755)
	f.refusedWC("a plain directory", filepath.Join(f.roots, "git", "plain"))
	mk(t, filepath.Join(f.wd, "pkg", "sub"), 0o755)
	f.refusedWC("a subdirectory of a repository", filepath.Join(f.wd, "pkg"))
	// a .git that is a symlink
	os.RemoveAll(filepath.Join(f.roots, "git", "plain"))
	mk(t, filepath.Join(f.roots, "git", "linkgit"), 0o755)
	os.Symlink(filepath.Join(outside, ".git"), filepath.Join(f.roots, "git", "linkgit", ".git"))
	f.refusedWC("a .git that is a symlink", filepath.Join(f.roots, "git", "linkgit"))
	// a .git file whose target is not a git directory, and one that points at another worktree's gitdir
	mk(t, filepath.Join(f.roots, "git", "badfile"), 0o755)
	put(t, filepath.Join(f.roots, "git", "badfile", ".git"), "gitdir: "+filepath.Join(f.roots, "git", "nowhere")+"\n", 0o644)
	f.refusedWC("a .git file naming nothing", filepath.Join(f.roots, "git", "badfile"))
	mk(t, filepath.Join(f.roots, "git", "other"), 0o755)
	put(t, filepath.Join(f.roots, "git", "other", ".git"), "gitdir: "+filepath.Join(f.wd, ".git")+"\n", 0o644)
	f.refusedWC("a .git file naming another worktree's gitdir", filepath.Join(f.roots, "git", "other"))
	// not a directory, not there, a hidden directory of the home
	put(t, filepath.Join(f.roots, "git", "afile"), "x", 0o644)
	f.refusedWC("a regular file", filepath.Join(f.roots, "git", "afile"))
	f.refusedWC("a path that does not exist", filepath.Join(f.roots, "git", "ghost"))
	f.refusedWC("a relative path", "git/wt")
	f.refusedWC("an unclean path", f.wd+"/../wt")
	f.refusedWC("a path with a comma (a mount-spec character)", filepath.Join(f.roots, "git", "a,b"))
	// the state and run directories are never a workdir
	mk(t, filepath.Join(f.state, ".git"), 0o755)
	f.refusedWC("the daemon's state directory", f.state)
}

func TestAWorktreeThatOverlapsALiveSessionIsRefused(t *testing.T) {
	// another live session works in the same directory, inside it, or contains it
	for _, why := range []string{"the same directory", "a directory inside it", "a parent"} {
		g := wcFx(t)
		mk(t, filepath.Join(g.wd, "nested", ".git"), 0o755)
		other, target := g.wd, g.wd
		switch why {
		case "a directory inside it":
			other = filepath.Join(g.wd, "nested")
		case "a parent":
			other, target = g.roots+"/git", filepath.Join(g.wd, "nested")
		}
		g.rec.Sid, g.rec.Ended, g.rec.Spec.Workdir.Path = "s0otherses1", nil, other
		g.writeRecord()
		g.refusedWC("overlap: "+why, target)
	}
	// an ended session, a pod session and an unrelated directory do not count
	g := wcFx(t)
	now := time.Now()
	g.rec.Sid, g.rec.Ended = "s0endedses1", &now
	g.writeRecord()
	k := *g.rec
	k.Sid, k.Ended, k.Backend, k.Spec.Workdir.Path = "s0podsess01", nil, "k8s", g.wd
	b, _ := json.Marshal(&k)
	put(t, filepath.Join(g.state, "sessions", "s0podsess01.json"), string(b), 0o600)
	if code, _, errb := g.check("--", g.wd); code != 0 {
		t.Errorf("an ended and a pod session must not block: exit %d %q", code, errb)
	}
	// a record that cannot be read is "could not tell" (3), never "no overlap"
	h := wcFx(t)
	put(t, filepath.Join(h.state, "sessions", "s0brokenxx1.json"), "{not json", 0o600)
	if code, out, errb := h.check("--", h.wd); code != 3 || out != "" || !strings.Contains(errb, "cannot tell") {
		t.Errorf("a broken record: exit %d %q %q", code, out, errb)
	}
}

func TestWorktreeCheckTakesExactlyOneArgumentAfterTheDashes(t *testing.T) {
	f := wcFx(t)
	for why, args := range map[string][]string{
		"no arguments": nil, "no dashes": {f.wd}, "dashes only": {"--"}, "two paths": {"--", f.wd, f.wd}, "an option": {"--x", f.wd},
		"an option after the dashes' path": {"--", f.wd, "--verbose"}, "an option before": {"-v", "--", f.wd}, "an empty path": {"--", ""},
	} {
		if code, out, errb := f.check(args...); code != 2 || out != "" || strings.Count(errb, "\n") != 1 {
			t.Errorf("%s: exit %d %q %q", why, code, out, errb)
		}
	}
	// --help prints one usage line, runs nothing and answers 0
	if code, out, _ := f.check("--help"); code != 0 || !strings.Contains(out, "sudo -n /usr/local/sbin/ccsbx-root worktree-check") {
		t.Errorf("--help: %d %q", code, out)
	}
}

func TestWorktreeCheckIsForHostdAndRootOnlyAndOnlyWhenEnabled(t *testing.T) {
	f := wcFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(os.Getuid()) // the session's owner
	if code, out, _ := f.check("--", f.wd); code != 2 || out != "" {
		t.Errorf("the owner: %d %q", code, out)
	}
	f = wcFx(t)
	f.writeConfig("verbs = [\"run\"]\n")
	f.env["SUDO_UID"] = fmt.Sprint(svcUID)
	if code, out, _ := f.check("--", f.wd); code != 2 || out != "" {
		t.Errorf("the verb off: %d %q", code, out)
	}
	f = wcFx(t)
	f.d.Euid = 1000 // not root
	if code, out, _ := f.check("--", f.wd); code != 2 || out != "" {
		t.Errorf("not root: %d %q", code, out)
	}
}

func TestWorktreeCheckWritesNothing(t *testing.T) {
	f := wcFx(t)
	snap := func() string {
		var b strings.Builder
		for _, root := range []string{f.state, f.run, f.wd, f.roots} {
			filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
				if err == nil {
					fmt.Fprintf(&b, "%s %v %d %v\n", p, i.Mode(), i.Size(), i.ModTime().UnixNano())
				}
				return nil
			})
		}
		return b.String()
	}
	before := snap()
	f.check("--", f.wd)
	f.check("--", filepath.Join(f.roots, "git"))
	if after := snap(); after != before {
		t.Errorf("the check changed something:\n%s\n---\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(f.run)); err == nil {
		if ents, _ := os.ReadDir(f.run); len(ents) != 0 {
			t.Errorf("the run dir was written: %v", ents)
		}
	}
}

var _ = start.ValidSid
