// Package gitname holds the one definition of what a repository, a branch and a base may be called, for
// everything that takes them from a record or a spec and everything that acts on them: the launcher's
// sidecar flags (internal/k8s), the start path (internal/start) and `ccsbx side serve`. They were written twice,
// once on each side, and the two drifted (a branch the launcher accepted was refused by the sidecar, which
// then failed as "pod not ready" minutes later). One definition makes a bad name a refusal at the start.
//
// The rules are the sidecar's: a repository is owner/name, a branch is a plain ref name of at most 100
// characters that is never main, master, HEAD or trunk (in any case) and never under refs/, and a base is a
// branch name or a full commit id.
package gitname

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	hexRe    = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	// protected branches are never pushed, whatever the door would say
	protected = map[string]bool{"main": true, "master": true, "head": true, "trunk": true}
)

// Repo checks owner/name.
func Repo(r string) error {
	if !repoRe.MatchString(r) || strings.Contains(r, "..") || strings.HasSuffix(strings.Split(r, "/")[1], ".git") {
		return fmt.Errorf("repository %q must be owner/name", r)
	}
	return nil
}

func plain(b string) bool {
	return branchRe.MatchString(b) && !strings.Contains(b, "..") && !strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".lock") &&
		!strings.Contains(b, "//") && !strings.Contains(b, "/.")
}

// Branch checks the session branch: required, a plain name, not protected.
func Branch(b string) error {
	if b == "" {
		return fmt.Errorf("a session branch is required")
	}
	if !plain(b) || protected[strings.ToLower(b)] || strings.HasPrefix(strings.ToLower(b), "refs/") {
		return fmt.Errorf("branch %q must be a plain name of at most 100 characters, never main, master, HEAD or trunk", b)
	}
	return nil
}

// Base checks where the branch starts from: a branch name or a full commit id. Empty means the sidecar's
// default (main) and is allowed here.
func Base(b string) error {
	if b == "" || hexRe.MatchString(b) || plain(b) {
		return nil
	}
	return fmt.Errorf("base %q is neither a branch name nor a full commit id", b)
}

// Pair checks the three together: each on its own, and the branch is not the base.
func Pair(repo, base, branch string) error {
	if err := Repo(repo); err != nil {
		return err
	}
	if err := Branch(branch); err != nil {
		return err
	}
	if err := Base(base); err != nil {
		return err
	}
	if base == branch {
		return fmt.Errorf("the session branch must differ from the base")
	}
	return nil
}
