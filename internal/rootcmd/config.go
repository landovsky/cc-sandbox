// The configuration of ccsbx-root. It exists because the one NOPASSWD sudo target must take its
// policy (which directories may be mounted, which binaries run, whose records to believe) from a
// root-owned file and from constants, never from its arguments, its environment or the caller's
// files. The installer writes /etc/ccsbx/ccsbx-root.toml; everything else has a default here.
package rootcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/tomlmini"
)

// Verbs is every verb there is. The config's `verbs` can only switch some of them off.
var Verbs = []string{"run", "stop", "renew", "gc", "attach", "exec-token", "status"}

// Config is the policy. TrustedUID is who must own the files root believes; it is 0 in production
// and is never read from the file.
type Config struct {
	AllowedRoots []string // resolved, absolute; a workdir must be strictly inside one
	Verbs        []string // enabled verbs
	StateDir     string   // the daemon's: <StateDir>/sessions/<sid>.json (read, never written)
	RunDir       string   // root's: generation directories and homes (created by ccsbx-root)
	Docker, Tmux string   // absolute binaries
	TmuxSocket   string   // the service uid's tmux socket
	PinsFile     string
	SeccompFile  string // the fence's seccomp profile (Docker's default plus the setid rule), root-owned
	ServiceUser  string // the uid hostd and the panes run as
	Machine      string
	BaseURL      string // the forwarder's model route written into managed settings
	FetchURL     string
	MinUID       int // a session record must belong to a uid at least this large (never root or a system user)
	TrustedUID   int
}

// DefaultConfigPath is where the installer puts the file.
const DefaultConfigPath = "/etc/ccsbx/ccsbx-root.toml"

// Defaults are the values a key of the file overrides.
func Defaults() Config {
	return Config{
		Verbs:    append([]string(nil), Verbs...),
		StateDir: "/var/lib/ccsbx", RunDir: "/var/lib/ccsbx-run",
		Docker: "/usr/bin/docker", Tmux: "/usr/bin/tmux", TmuxSocket: "/run/ccsbx/tmux.sock",
		PinsFile: "/etc/ccsbx/pins.toml", SeccompFile: "/etc/ccsbx/seccomp.json", ServiceUser: "ccsbx", BaseURL: "http://fwd:8080", MinUID: 1000,
	}
}

var configKeys = map[string]bool{
	"allowed_roots": true, "verbs": true, "state_dir": true, "run_dir": true, "docker": true, "tmux": true,
	"tmux_socket": true, "pins_file": true, "service_user": true, "machine": true, "base_url": true, "fetch_url": true, "min_uid": true, "seccomp_profile": true,
}

// LoadConfig reads and validates the file. The file and its directory must be owned by trustedUID
// and not writable by group or others, or nothing is believed.
func LoadConfig(path string, trustedUID int) (*Config, error) {
	if err := trustedFile(path, trustedUID, true); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c := Defaults()
	c.TrustedUID = trustedUID
	if h, err := os.Hostname(); err == nil {
		c.Machine = strings.ToLower(h)
	}
	// the installer writes top-level keys; tomlmini wants tables
	doc, err := tomlmini.Parse("[[top]]\n" + string(b))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if len(doc) != 1 || len(doc["top"]) != 1 {
		return nil, fmt.Errorf("config %s: only top-level keys are allowed", path)
	}
	top := doc["top"][0]
	for k := range top {
		if !configKeys[k] {
			return nil, fmt.Errorf("config %s: unknown key %q", path, k)
		}
	}
	str := func(k string, dst *string) error {
		if v, ok := top[k]; ok {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("config %s: %s must be a string", path, k)
			}
			*dst = s
		}
		return nil
	}
	list := func(k string) ([]string, bool, error) {
		v, ok := top[k]
		if !ok {
			return nil, false, nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil, true, fmt.Errorf("config %s: %s must be an array of strings", path, k)
		}
		var out []string
		for _, x := range arr {
			s, ok := x.(string)
			if !ok {
				return nil, true, fmt.Errorf("config %s: %s must be an array of strings", path, k)
			}
			out = append(out, s)
		}
		return out, true, nil
	}
	for k, dst := range map[string]*string{"state_dir": &c.StateDir, "run_dir": &c.RunDir, "docker": &c.Docker, "tmux": &c.Tmux,
		"tmux_socket": &c.TmuxSocket, "pins_file": &c.PinsFile, "seccomp_profile": &c.SeccompFile, "service_user": &c.ServiceUser, "machine": &c.Machine, "base_url": &c.BaseURL, "fetch_url": &c.FetchURL} {
		if err := str(k, dst); err != nil {
			return nil, err
		}
	}
	if v, ok := top["min_uid"]; ok {
		n, isInt := v.(int64)
		if !isInt {
			if i, ok2 := v.(int); ok2 {
				n, isInt = int64(i), true
			}
		}
		if !isInt || n < 1 || n > 1<<31 {
			return nil, fmt.Errorf("config %s: min_uid must be a number from 1", path)
		}
		c.MinUID = int(n)
	}
	roots, _, err := list("allowed_roots")
	if err != nil {
		return nil, err
	}
	if vs, ok, err := list("verbs"); err != nil {
		return nil, err
	} else if ok {
		c.Verbs = vs
	}
	if err := c.validate(roots); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate(roots []string) error {
	if len(roots) == 0 {
		return fmt.Errorf("allowed_roots is empty: nothing could be mounted")
	}
	for _, r := range roots {
		if err := backend.CheckPath(r); err != nil {
			return err
		}
		if !filepath.IsAbs(r) || filepath.Clean(r) != r || r == "/" {
			return fmt.Errorf("allowed root %q must be a clean absolute path and not /", r)
		}
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			return fmt.Errorf("allowed root %q: %w", r, err)
		}
		if rr == "/" {
			return fmt.Errorf("allowed root %q resolves to /", r)
		}
		c.AllowedRoots = append(c.AllowedRoots, rr)
	}
	known := map[string]bool{}
	for _, v := range Verbs {
		known[v] = true
	}
	for _, v := range c.Verbs {
		if !known[v] {
			return fmt.Errorf("unknown verb %q", v)
		}
	}
	for name, p := range map[string]string{"state_dir": c.StateDir, "run_dir": c.RunDir, "docker": c.Docker, "tmux": c.Tmux, "tmux_socket": c.TmuxSocket, "pins_file": c.PinsFile, "seccomp_profile": c.SeccompFile} {
		if err := backend.CheckPath(p); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%s %q must be a clean absolute path", name, p)
		}
	}
	if c.StateDir == c.RunDir || strings.HasPrefix(c.RunDir, c.StateDir+"/") || strings.HasPrefix(c.StateDir, c.RunDir+"/") {
		return fmt.Errorf("state_dir and run_dir must be separate trees (the daemon can write the first, only root the second)")
	}
	if c.ServiceUser == "" || strings.ContainsAny(c.ServiceUser, ":/ \n") {
		return fmt.Errorf("service_user %q", c.ServiceUser)
	}
	if c.Machine == "" {
		return fmt.Errorf("machine is empty (set machine = \"...\")")
	}
	sort.Strings(c.Verbs)
	return nil
}

// Enabled reports whether a verb is switched on.
func (c *Config) Enabled(v string) bool {
	if v == "status" { // read-only: allowed wherever stop is, so a config written before it existed still works
		return c.enabled("status") || c.enabled("stop")
	}
	return c.enabled(v)
}

func (c *Config) enabled(v string) bool {
	for _, x := range c.Verbs {
		if x == v {
			return true
		}
	}
	return false
}

// trustedFile requires path (and its parent directory) to be owned by uid and not writable by
// group or others. A symlink is followed only to a target that passes the same test, and its
// directory too. Fails closed when the owner cannot be read.
func trustedFile(path string, uid int, wantRegular bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not absolute", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	for _, p := range []string{real, filepath.Dir(real)} {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no owner information, refusing", p)
		}
		if int(sys.Uid) != uid {
			return fmt.Errorf("%s is owned by uid %d, not %d", p, sys.Uid, uid)
		}
		if st.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by group or others (%v)", p, st.Mode().Perm())
		}
		if p == real && wantRegular && !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
	}
	return nil
}
