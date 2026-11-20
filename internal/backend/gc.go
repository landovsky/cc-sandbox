// gc.go finds the containers and networks of generations whose `run` died, and removes them. It
// exists because a SIGKILL or a crash skips the deferred cleanup, which would leave a fence running
// with its outbox open and no generation owning it. Only what the daemon has a record of is touched.

package backend

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Orphan is a container or network of a generation whose `ccsbx run` is gone (SIGKILL, a hard
// pane kill, a crash): the deferred cleanup never ran, so the fence may still be running with
// its outbox open and no generation owning it.
// sidShape is the shape of a daemon-minted session id (the same as start.ValidSid; backend cannot
// import start). A docker label that is not one is not ours.
var sidShape = regexp.MustCompile(`^s[0-9a-z]{6,16}$`)

func validSid(s string) bool { return sidShape.MatchString(s) }

type Orphan struct {
	Kind string // container | network
	Name string
	Sid  string
	Gen  int
}

// writePID records which process owns a generation, so gc can tell a live run from a dead one.
func (s Spec) writePID() error {
	return os.WriteFile(filepath.Join(s.GenDir(), "PID"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// ownerAlive reports whether the `ccsbx run` that owns a generation still exists. On Linux it also
// checks that the pid is still a ccsbx process for this sid, so a reused pid doesn't keep an
// orphan alive.
func ownerAlive(stateDir, sid string, gen int) bool {
	b, err := os.ReadFile(filepath.Join(stateDir, "s", sid, "g"+strconv.Itoa(gen), "PID"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	if cl, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.Contains(string(cl), "ccsbx")
	}
	return true
}

// sessionsDir is where the daemon's session records are: <stateDir>/sessions unless the records
// live elsewhere than the generation directories (ccsbx-root: records are the daemon's, the
// generation directories are root's).
func (r *Runner) sessionsDir(stateDir string) string {
	if r.SessionsDir != "" {
		return r.SessionsDir
	}
	return filepath.Join(stateDir, "sessions")
}

// Orphans lists containers and networks labelled ccsbx.sid whose sid has a session record under
// stateDir and whose owning run is dead. Resources of unknown sids (another state dir, another
// tool) are never listed: gc only touches what this state dir created.
func (r *Runner) Orphans(stateDir string) ([]Orphan, error) {
	var out []Orphan
	for _, k := range []struct{ kind, list string }{
		{"container", "ps"},
		{"network", "network"},
	} {
		args := []string{"ps", "-a", "--filter", "label=ccsbx.sid", "--format", `{{.Names}}	{{.Label "ccsbx.sid"}}	{{.Label "ccsbx.gen"}}`}
		if k.kind == "network" {
			args = []string{"network", "ls", "--filter", "label=ccsbx.sid", "--format", `{{.Name}}	{{.Label "ccsbx.sid"}}	{{.Label "ccsbx.gen"}}`}
		}
		b, err := exec.Command(r.docker(), args...).Output()
		if err != nil {
			return nil, fmt.Errorf("docker %s: %w", args[0], err)
		}
		for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			f := strings.Split(ln, "\t")
			if len(f) != 3 || !strings.HasPrefix(f[0], "ccsbx-") {
				continue
			}
			gen, err := strconv.Atoi(f[2])
			if err != nil || gen < 1 || gen > 9999 || !validSid(f[1]) {
				continue
			}
			// the name must be the one this sid and gen derive: labels alone are not believed
			want := Spec{Sid: f[1], Gen: gen}.Container()
			if k.kind == "network" {
				want = Spec{Sid: f[1], Gen: gen}.Network()
			}
			if f[0] != want {
				continue
			}
			if _, err := os.Stat(filepath.Join(r.sessionsDir(stateDir), f[1]+".json")); err != nil {
				continue // not ours
			}
			if ownerAlive(stateDir, f[1], gen) {
				continue
			}
			out = append(out, Orphan{Kind: k.kind, Name: f[0], Sid: f[1], Gen: gen})
		}
	}
	return out, nil
}

// Reap removes the orphans (containers first, so their networks can go) and marks each
// generation closed.
func (r *Runner) Reap(stateDir string, os_ []Orphan) error {
	var errs []string
	for _, pass := range []string{"container", "network"} {
		for _, o := range os_ {
			if o.Kind != pass {
				continue
			}
			args := []string{"rm", "-f", o.Name}
			if o.Kind == "network" {
				args = []string{"network", "rm", o.Name}
			}
			if out, err := exec.Command(r.docker(), args...).CombinedOutput(); err != nil {
				errs = append(errs, fmt.Sprintf("%s %s: %v: %s", o.Kind, o.Name, err, strings.TrimSpace(string(out))))
			}
			sp := Spec{Sid: o.Sid, Gen: o.Gen, StateDir: stateDir}
			if r.Unstage != nil {
				r.Unstage(sp)
			}
			_ = sp.Close()
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
