// Package gitroute is H1: every git command the host runs on an agent-writable worktree.
//
// The worktree is hostile input (invariants 24 and 25). A repo can carry hooks, a `.git` file
// redirecting to an attacker-chosen directory, `.gitattributes` naming filter drivers, and
// `.gitmodules` pointing at `ext::` transports. So the host never lets git discover anything:
//
//   - GIT_DIR and GIT_WORK_TREE are always set, so the worktree's `.git` is never read;
//   - the gitdir must be outside the worktree (it is host-owned, read-only to the fence);
//   - attributes are read from the empty tree, never from the worktree's .gitattributes;
//   - system and global config are off, and a short fixed list of `-c` overrides turns off hooks,
//     fsmonitor, external diff and credential helpers, and external/file transports;
//   - the environment is rebuilt from scratch, nothing from the caller's is inherited;
//   - only a short list of verbs runs: no push, fetch, clone, config, remote, submodule.
//
// Push happens only through the forwarder's fwd.sock (P2, not here).
package gitroute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The well-known empty trees, per object format.
const (
	emptyTreeSHA1   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	emptyTreeSHA256 = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
)

// Runner runs hardened git against one host-owned gitdir and one worktree.
type Runner struct {
	GitDir   string // host-owned gitdir, outside Worktree
	Worktree string
	// Home is the owner's home, for the config files steering-check reads and for "~" in them;
	// default os.UserHomeDir.
	Home string
	// AllowGitDirInWorktree is for a developer's own repo (steering-check on a normal checkout).
	// A fenced session's gitdir is never inside its worktree.
	AllowGitDirInWorktree bool
	Git                   string        // git binary, default /usr/bin/git, else git from PATH
	Author, Email         string        // commit identity; default "ccsbx" / "ccsbx@localhost"
	Path                  string        // PATH for git's own helpers, default /usr/bin:/bin
	Timeout               time.Duration // per git process; default 60s (a FIFO or a huge file must not hang the host)
	MaxFiles              int           // Commit refuses a worktree with more entries than this; default 200000
	MaxFileBytes          int64         // ... or a file larger than this; default 256 MiB
	empty                 string        // cached empty-tree id for this repo's object format
	probed                bool          // GIT_ATTR_SOURCE was seen to work with this git binary
}

// emptyTree is the empty tree of this repository's object format (SHA-1 or SHA-256), asked of git
// once with a clean environment. An unreadable answer falls back to SHA-1, which fails closed
// (git rejects a tree id that does not exist) rather than running with attributes enabled.
func (r *Runner) emptyTree() string {
	if r.empty != "" {
		return r.empty
	}
	r.empty = emptyTreeSHA1
	c := exec.Command(r.git(), "--git-dir="+r.GitDir, "rev-parse", "--show-object-format")
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=/usr/bin:/bin"}
	if out, err := c.Output(); err == nil && strings.TrimSpace(string(out)) == "sha256" {
		r.empty = emptyTreeSHA256
	}
	return r.empty
}

// ErrRefused is returned for a verb or flag the host will not run.
var ErrRefused = errors.New("refused")

// overrides are the fixed `-c` settings, placed before the verb. Command-line config beats the
// repo's own config, so a poisoned gitdir config can't undo them.
var overrides = []string{
	"core.hooksPath=/dev/null",
	"core.fsmonitor=false",
	"core.untrackedCache=false",
	// keys that decide what git NOTICES: a config the agent can write (developer-repo mode) must not
	// be able to make a change invisible (stat comparison, mode bits, sparse checkout)
	"core.checkStat=default",
	"core.trustctime=true",
	"core.fileMode=true",
	"core.symlinks=true",
	"core.sparseCheckout=false",
	"color.ui=never",
	"core.sshCommand=/bin/false",
	"core.pager=cat",
	"core.editor=/bin/false",
	"core.attributesFile=/dev/null",
	"core.excludesFile=/dev/null",
	"diff.external=",
	"diff.ignoreSubmodules=all",
	"status.submoduleSummary=false",
	"submodule.recurse=false",
	"credential.helper=",
	"protocol.ext.allow=never",
	"protocol.file.allow=never",
	"commit.gpgsign=false",
	// signature verification runs gpg/ssh-keygen/gpgsm over attacker-supplied bytes on the host
	"gpg.program=/bin/false",
	"gpg.ssh.program=/bin/false",
	"gpg.x509.program=/bin/false",
	"log.showSignature=false",
	"gc.auto=0",
	"maintenance.auto=false",
	"advice.detachedHead=false",
}

// Env is the complete environment of every host git process. Nothing is inherited.
func (r *Runner) Env() []string {
	p := r.Path
	if p == "" {
		p = "/usr/bin:/bin"
	}
	name, email := r.Author, r.Email
	if name == "" {
		name = "ccsbx"
	}
	if email == "" {
		email = "ccsbx@localhost"
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
		"GIT_DIR=" + r.GitDir,
		"GIT_WORK_TREE=" + r.Worktree,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(r.Worktree),
		"GIT_ATTR_NOSYSTEM=1",
		// Attributes come from the empty tree, so a worktree .gitattributes can't select a filter
		// or diff driver that the host's own config happens to define (git-lfs, say).
		"GIT_ATTR_SOURCE=" + r.emptyTree(),
		"GIT_EXTERNAL_DIFF=/bin/false",
		"GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=/bin/false",
		"HOME=/nonexistent",
		"LC_ALL=C",
		"PATH=" + p,
	}
}

// flagSet is what one read verb accepts: exact flag names, a few value patterns, and whether
// positional arguments (revs and paths, never starting with '-' or ':') are allowed.
type flagSet struct {
	exact    []string
	patterns []string // anchored regexps for flags with a value
}

var (
	num        = `[0-9]{1,6}`
	commonDiff = []string{"--name-only", "--name-status", "--stat", "--numstat", "--shortstat", "--no-renames", "--no-color", "-z", "--quiet", "--exit-code", "--raw"}
	revFlags   = []string{"--oneline", "--no-merges", "--no-color", "--name-only", "--name-status", "--stat", "--no-renames", "-z"}
	valFlags   = []string{`--max-count=` + num, `-` + num, `--format=[^\x00]*`, `--pretty=format:[^\x00]*`, `--diff-filter=[ACDMRTUXB]+`}
)

// readVerbs are the only verbs Run accepts. Anything that writes (add, commit, branch,
// symbolic-ref, write-tree) is reachable only through the fixed-argument methods of Runner, and
// cat-file (which can run filters) isn't reachable at all. Flags match exactly: git accepts
// unambiguous abbreviations of long options (--fil= for --file), so a prefix match would let them
// through; here an abbreviation is just an unlisted flag.
var readVerbs = map[string]flagSet{
	"status":     {exact: []string{"--porcelain", "--porcelain=v1", "--porcelain=v2", "--short", "-s", "--branch", "-b", "--no-renames", "-z", "--untracked-files=no", "--untracked-files=normal", "--untracked-files=all"}},
	"diff":       {exact: append([]string{"--cached", "--staged", "--patch", "-p", "--no-ext-diff", "--no-textconv"}, commonDiff...), patterns: valFlags},
	"log":        {exact: append([]string{"--patch", "-p"}, revFlags...), patterns: valFlags},
	"show":       {exact: append([]string{"--patch", "-p"}, revFlags...), patterns: valFlags},
	"rev-parse":  {exact: []string{"--absolute-git-dir", "--is-inside-work-tree", "--show-toplevel", "--verify", "--short", "--abbrev-ref"}},
	"rev-list":   {exact: []string{"--count", "--no-merges", "--reverse", "--all"}, patterns: valFlags},
	"ls-files":   {exact: []string{"-z", "--cached", "-c", "--others", "-o", "--modified", "-m", "--deleted", "-d", "--stage", "-s", "--exclude-standard"}},
	"ls-tree":    {exact: []string{"-r", "-z", "--name-only", "--long", "-l", "-t"}},
	"merge-base": {exact: []string{"--is-ancestor"}},
}

// ErrRefused marks a refusal; Run wraps it with the reason.
func checkArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no git verb", ErrRefused)
	}
	fs, ok := readVerbs[args[0]]
	if !ok {
		return fmt.Errorf("%w: git %s is not a host verb", ErrRefused, args[0])
	}
	var pats []*regexp.Regexp
	for _, p := range fs.patterns {
		pats = append(pats, regexp.MustCompile("^"+p+"$"))
	}
	dashdash := false
	for _, a := range args[1:] {
		switch {
		case strings.ContainsRune(a, 0):
			return fmt.Errorf("%w: NUL in argument", ErrRefused)
		case dashdash:
			// pathspecs after --: no magic signatures (":(attr:...)", ":(top)") and no flags
			if strings.HasPrefix(a, ":") {
				return fmt.Errorf("%w: pathspec magic %q", ErrRefused, a)
			}
			if err := insideWorktree(a); err != nil {
				return err
			}
		case a == "--":
			dashdash = true
		case strings.HasPrefix(a, "-"):
			if !contains(fs.exact, a) && !matchAny(pats, a) {
				return fmt.Errorf("%w: flag %s is not allowed for git %s", ErrRefused, a, args[0])
			}
			if gpgPlaceholder.MatchString(a) {
				// %G? %GS %GK ... (also %+G? %-GS and "% G?") make git run gpg on the host
				return fmt.Errorf("%w: signature placeholders in a format run gpg", ErrRefused)
			}
		case strings.HasPrefix(a, ":"):
			return fmt.Errorf("%w: pathspec magic %q", ErrRefused, a)
		default:
			// A positional is a revision or a path. Either way it must stay inside the repo:
			// git diff switches to --no-index IMPLICITLY when a path lies outside the worktree,
			// and would print any host file it can read.
			if err := insideWorktree(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// insideWorktree refuses an argument that names a place outside the worktree: absolute paths,
// "~", and any ".." path segment. A range such as "a..b" or "a...b" is fine (the dots are not a
// segment on their own); so is "HEAD~1" and "HEAD@{1}".
func insideWorktree(a string) error {
	if strings.HasPrefix(a, "/") || strings.HasPrefix(a, "~") {
		return fmt.Errorf("%w: %q is outside the worktree", ErrRefused, a)
	}
	// split on '/' and on ':' (rev:path) so HEAD:../x is caught as well
	for _, seg := range strings.FieldsFunc(a, func(r rune) bool { return r == '/' || r == ':' }) {
		if seg == ".." {
			return fmt.Errorf("%w: %q leaves the worktree", ErrRefused, a)
		}
	}
	if i := strings.IndexByte(a, ':'); i >= 0 && strings.HasPrefix(a[i+1:], "/") {
		return fmt.Errorf("%w: %q is outside the worktree", ErrRefused, a)
	}
	return nil
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, r := range res {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// minGit is the first git release with GIT_ATTR_SOURCE (2.41.0; its release notes introduce
// "git --attr-source=<tree>" and the variable it is equivalent to; 2.40 has neither). Older git ignores it silently, and then a
// worktree .gitattributes is honoured again, so refuse rather than run unprotected.
var minGit = [2]int{2, 41}

// gpgPlaceholder matches %G and its modified forms (%+G, %-G, "% G"; modifiers may stack).
var gpgPlaceholder = regexp.MustCompile(`%[-+ ]*G`)

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

func (r *Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 60 * time.Second
}

func (r *Runner) git() string {
	if r.Git != "" {
		return r.Git
	}
	if _, err := os.Stat("/usr/bin/git"); err == nil {
		return "/usr/bin/git" // not resolved through the caller's PATH
	}
	return "git"
}

// checkRepo validates the gitdir and worktree. It runs before every command.
func (r *Runner) checkRepo() error {
	if !filepath.IsAbs(r.GitDir) || !filepath.IsAbs(r.Worktree) {
		return fmt.Errorf("%w: gitdir and worktree must be absolute", ErrRefused)
	}
	// first: every later check shells out to this git
	if err := r.checkGitVersion(); err != nil {
		return err
	}
	gd, err := filepath.EvalSymlinks(r.GitDir)
	if err != nil {
		return err
	}
	wt, err := filepath.EvalSymlinks(r.Worktree)
	if err != nil {
		return err
	}
	if !r.AllowGitDirInWorktree && (gd == wt || strings.HasPrefix(gd, wt+string(filepath.Separator))) {
		return fmt.Errorf("%w: gitdir %s is inside the worktree, so the agent could write it", ErrRefused, r.GitDir)
	}
	if st, err := os.Stat(filepath.Join(gd, "HEAD")); err != nil || st.IsDir() {
		return fmt.Errorf("%w: %s is not a git directory", ErrRefused, r.GitDir)
	}
	// Host-owned and not writable by anyone else: git's own safe.directory idea. A gitdir the
	// fence ever had write access to could carry a config with filter drivers or include paths.
	for _, p := range []string{gd, filepath.Join(gd, "config"), filepath.Join(gd, "config.worktree"), filepath.Join(gd, "info"), filepath.Join(gd, "hooks")} {
		if err := hostOwned(p); err != nil {
			return err
		}
	}
	if cd, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
		c := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gd, c)
		}
		for _, p := range []string{c, filepath.Join(c, "config"), filepath.Join(c, "info")} {
			if err := hostOwned(p); err != nil {
				return err
			}
		}
		gd = c
	}
	// include.path / includeIf in a gitdir config pull in a file the agent may control (a relative
	// include resolves into the worktree in developer-repo mode), which hands it every key the
	// overrides do not set. No includes, in the gitdir config or the common one.
	for _, dir := range []string{r.GitDir, gd} {
		for _, f := range []string{"config", "config.worktree"} {
			if err := r.noIncludes(filepath.Join(dir, f)); err != nil {
				return err
			}
		}
	}
	// $GIT_DIR/info/attributes is read whatever GIT_ATTR_SOURCE says, so it must not exist.
	for _, a := range []string{filepath.Join(r.GitDir, "info", "attributes"), filepath.Join(gd, "info", "attributes")} {
		if st, err := os.Stat(a); err == nil && st.Size() > 0 {
			return fmt.Errorf("%w: %s exists; info/attributes can select filter drivers", ErrRefused, a)
		}
	}
	return nil
}

// noIncludes refuses a config file with any include.* or includeIf.* key.
func (r *Runner) noIncludes(cfg string) error {
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		return nil
	}
	c := exec.Command(r.git(), "config", "--file", cfg, "--no-includes", "--name-only", "--get-regexp", `^include`)
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s has include settings (%s); includes can pull in a file the agent controls", ErrRefused, cfg, strings.Join(strings.Fields(string(out)), ", "))
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return nil // no match
	default:
		return fmt.Errorf("%w: cannot read %s: %v", ErrRefused, cfg, err)
	}
}

// hostOwned requires p, if it exists, to be owned by this uid and not group/world writable.
func hostOwned(p string) error {
	st, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrRefused, p)
	}
	if st.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w: %s is world-writable", ErrRefused, p)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		if int(sys.Uid) != os.Getuid() {
			return fmt.Errorf("%w: %s is not owned by the host user", ErrRefused, p)
		}
		// group-writable is fine when the group is the user's own (a private group, umask 002);
		// it is not when some other group, such as the one a fence runs as, can write.
		if st.Mode().Perm()&0o020 != 0 && int(sys.Gid) != os.Getgid() {
			return fmt.Errorf("%w: %s is writable by group %d", ErrRefused, p, sys.Gid)
		}
	}
	return nil
}

func (r *Runner) checkGitVersion() error {
	out, err := exec.Command(r.git(), "--version").Output()
	if err != nil {
		return err
	}
	if err := versionOK(string(out)); err != nil {
		return err
	}
	return r.probeAttrSource()
}

// versionOK refuses a "git version X.Y..." line older than minGit.
func versionOK(out string) error {
	m := gitVersionRe.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("%w: cannot read git version from %q", ErrRefused, out)
	}
	var maj, min int
	fmt.Sscanf(m[1]+" "+m[2], "%d %d", &maj, &min)
	if maj < minGit[0] || (maj == minGit[0] && min < minGit[1]) {
		return fmt.Errorf("%w: git %d.%d is older than %d.%d (GIT_ATTR_SOURCE, which keeps a worktree .gitattributes from selecting filters, arrived in %d.%d); install a newer git and pass it with --git / CCSBX_GIT", ErrRefused, maj, min, minGit[0], minGit[1], minGit[0], minGit[1])
	}
	return nil
}

// probeAttrSource checks, once per Runner, that this git binary really ignores a worktree
// .gitattributes when GIT_ATTR_SOURCE is the empty tree. A version string is a claim; this is the
// behaviour, and a vendor build that lacks the variable fails here instead of failing open.
func (r *Runner) probeAttrSource() error {
	if r.probed {
		return nil
	}
	d, err := os.MkdirTemp("", "ccsbx-attr-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(d)
	if err := os.WriteFile(filepath.Join(d, ".gitattributes"), []byte("x filter=probe\n"), 0o600); err != nil {
		return err
	}
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=/usr/bin:/bin", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(d), "GIT_ATTR_NOSYSTEM=1"}
	run := func(extra []string, args ...string) (string, error) {
		c := exec.Command(r.git(), args...)
		c.Dir, c.Env = d, append(env, extra...)
		out, err := c.Output()
		return string(out), err
	}
	if _, err := run(nil, "init", "-q"); err != nil {
		return fmt.Errorf("%w: attribute probe: %v", ErrRefused, err)
	}
	empty := emptyTreeSHA1
	if o, err := run(nil, "rev-parse", "--show-object-format"); err == nil && strings.TrimSpace(o) == "sha256" {
		empty = emptyTreeSHA256
	}
	out, err := run([]string{"GIT_ATTR_SOURCE=" + empty}, "check-attr", "filter", "x")
	if err != nil || !strings.Contains(out, "unspecified") {
		return fmt.Errorf("%w: this git does not honour GIT_ATTR_SOURCE (check-attr said %q): a worktree .gitattributes could select filter drivers; use git >= %d.%d (--git / CCSBX_GIT)", ErrRefused, strings.TrimSpace(out), minGit[0], minGit[1])
	}
	r.probed = true
	return nil
}

// Cmd builds the process for a read verb without starting it (tests inspect it).
func (r *Runner) Cmd(args ...string) (*exec.Cmd, error) {
	if err := checkArgs(args); err != nil {
		return nil, err
	}
	return r.cmd(args...)
}

// cmd builds any verb. Only fixed-argument methods call it for write verbs.
func (r *Runner) cmd(args ...string) (*exec.Cmd, error) {
	return r.build(context.Background(), args...)
}

func (r *Runner) build(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if err := r.checkRepo(); err != nil {
		return nil, err
	}
	argv := []string{}
	for _, o := range overrides {
		argv = append(argv, "-c", o)
	}
	argv = append(argv, args[0])
	// --no-ext-diff keeps diff drivers off even if a config override were missed.
	switch args[0] {
	case "diff", "show", "log":
		argv = append(argv, "--no-ext-diff", "--no-textconv", "--ignore-submodules=all")
	case "status":
		// diff.ignoreSubmodules is only a default: .gitmodules in the worktree (agent-written) can
		// say ignore=none and make git run inside an agent-made nested repo. Only the command line
		// beats it. Callers cannot pass their own (the allowlist has no such flag).
		argv = append(argv, "--ignore-submodules=all")
	case "commit":
		argv = append(argv, "--no-verify", "--no-gpg-sign")
	}
	argv = append(argv, args[1:]...)
	c := exec.CommandContext(ctx, r.git(), argv...)
	c.Dir = r.Worktree
	c.Env = r.Env()
	// own process group, so a timeout kills filters and helpers git started, not only git
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	return c, nil
}

// Run runs a READ verb with allowlisted flags and returns stdout. Arguments from outside the
// package (and, one day, from the fence) can only reach git through here.
func (r *Runner) Run(args ...string) (string, error) {
	if err := checkArgs(args); err != nil {
		return "", err
	}
	return r.exec(args...)
}

// RunForFence is Run for output that goes back to the fence. It adds one rule: the answer may
// only be computed from committed objects. A command that reads the WORKTREE (status, ls-files,
// diff without two revisions or with --cached) is refused, because while the fence runs the
// worktree changes under git (see the package comment of commit.go), and what git reads there can
// be a host file. Nothing calls it yet; the F3/C22 transport must use this and never Run.
func (r *Runner) RunForFence(args ...string) (string, error) {
	if err := checkArgs(args); err != nil {
		return "", err
	}
	switch args[0] {
	case "status", "ls-files":
		return "", fmt.Errorf("%w: git %s reads the worktree; not for the fence", ErrRefused, args[0])
	case "diff":
		revs := 0
		for _, a := range args[1:] {
			if a == "--" {
				break
			}
			if a == "--cached" || a == "--staged" {
				return "", fmt.Errorf("%w: git diff --cached reads the index; not for the fence", ErrRefused)
			}
			if !strings.HasPrefix(a, "-") {
				revs += 1 + strings.Count(a, "..")
			}
		}
		if revs < 2 {
			return "", fmt.Errorf("%w: git diff needs two revisions here; one revision or none diffs the worktree", ErrRefused)
		}
	}
	return r.exec(args...)
}

// exec runs without the allowlist: for the fixed argument lists of this package only.
func (r *Runner) exec(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
	defer cancel()
	c, err := r.build(ctx, args...)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("git %s: timed out after %s", args[0], r.timeout())
		}
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
