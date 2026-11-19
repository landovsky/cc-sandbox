package gitroute

import (
	"fmt"
	"regexp"
	"strings"
)

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9/_.:-]{1,120}$`)

// ChangedFiles lists files changed between base and HEAD (three-dot: since the merge base).
func (r *Runner) ChangedFiles(base string) ([]string, error) {
	if strings.HasPrefix(base, "-") {
		return nil, fmt.Errorf("%w: base %q", ErrRefused, base)
	}
	out, err := r.Run("diff", "--name-only", "--no-renames", "-z", base+"...HEAD", "--")
	if err != nil {
		return nil, err
	}
	return splitZ(out), nil
}

// Commit stages everything and commits with the Fenced-Session and Steering-Files trailers. The
// trailers come from the host's view of the staged change, never from the agent's message.
// No hooks run, the identity is the host's, and nothing is pushed.
func (r *Runner) Commit(session, message string) (string, error) {
	if !sessionRe.MatchString(session) {
		return "", fmt.Errorf("%w: session id %q", ErrRefused, session)
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("%w: empty commit message", ErrRefused)
	}
	// An agent-written message may try to forge a trailer; drop any trailer-looking line for ours.
	message = stripForgedTrailers(message)
	if _, err := r.exec("add", "-A", "--", "."); err != nil {
		return "", err
	}
	out, err := r.Run("diff", "--cached", "--name-only", "--no-renames", "-z", "--")
	if err != nil {
		return "", err
	}
	if len(splitZ(out)) == 0 {
		return "", fmt.Errorf("nothing to commit")
	}
	st := Steering(splitZ(out))
	if _, err := r.exec("commit", "-m", message, "-m", Trailers(session, st)); err != nil {
		return "", err
	}
	sha, err := r.Run("rev-parse", "HEAD")
	return strings.TrimSpace(sha), err
}

var forgedRe = regexp.MustCompile(`(?mi)^(Fenced-Session|Steering-Files):.*\n?`)

func stripForgedTrailers(m string) string { return strings.TrimSpace(forgedRe.ReplaceAllString(m, "")) }

func splitZ(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
