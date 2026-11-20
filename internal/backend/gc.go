package backend

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Orphan is a container or network of a generation whose `ccsbx run` is gone (SIGKILL, a hard
// pane kill, a crash): the deferred cleanup never ran, so the fence may still be running with
// its outbox open and no generation owning it.
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
			if err != nil {
				continue
			}
			if _, err := os.Stat(filepath.Join(stateDir, "sessions", f[1]+".json")); err != nil {
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
			_ = Spec{Sid: o.Sid, Gen: o.Gen, StateDir: stateDir}.Close()
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
