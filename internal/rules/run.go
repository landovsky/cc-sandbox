// run.go executes the twins of selected rules and tallies pass, fail and pending. It exists so
// `ccsbx selftest` and the nightly job share one runner and one definition of red.

package rules

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Status of one twin.
const (
	Pass    = "pass"
	Fail    = "fail"
	Pend    = "pending"
	Skipped = "skipped"
)

// Result is one twin's outcome.
type Result struct {
	ID      string `json:"id"`
	Suite   string `json:"suite"`
	Twin    string `json:"twin"` // allow | block
	Backend string `json:"backend"`
	Status  string `json:"status"`
	Ms      int64  `json:"ms"`
	Output  string `json:"output,omitempty"` // tail of the twin's output, failures only
}

// Run executes both twins of every rule. dir is the repo root (twins run from there). Each twin
// gets CCSBX_BACKEND so one script can serve several backends.
func Run(rs []Rule, backend, dir string, timeout time.Duration) []Result {
	var out []Result
	for _, r := range rs {
		for _, tw := range []struct{ name, cmd string }{{"allow", r.Allow}, {"block", r.Block}} {
			res := Result{ID: r.ID, Suite: r.Suite, Twin: tw.name, Backend: backend}
			if strings.TrimSpace(tw.cmd) == Pending {
				res.Status = Pend
				out = append(out, res)
				continue
			}
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			c := exec.CommandContext(ctx, "sh", "-c", tw.cmd)
			c.Dir = dir
			c.Env = append(os.Environ(), "CCSBX_BACKEND="+backend, "CCSBX_RULE="+r.ID, "CCSBX_TWIN="+tw.name)
			b, err := c.CombinedOutput()
			cancel()
			res.Ms = time.Since(start).Milliseconds()
			if err != nil {
				res.Status = Fail
				res.Output = tail(string(b), 800)
				if ctx.Err() != nil {
					res.Output = "timeout after " + timeout.String() + "\n" + res.Output
				}
			} else {
				res.Status = Pass
			}
			out = append(out, res)
		}
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// Counts tallies results by status.
func Counts(rs []Result) map[string]int {
	m := map[string]int{}
	for _, r := range rs {
		m[r.Status]++
	}
	return m
}
