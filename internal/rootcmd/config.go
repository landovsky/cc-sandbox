// The configuration of ccsbx-root. It exists because the one NOPASSWD sudo target must take its
// policy (which directories may be mounted, which binaries run, whose records to believe) from a
// root-owned file and from constants, never from its arguments, its environment or the caller's
// files. The installer writes /etc/ccsbx/ccsbx-root.toml; everything else has a default here.
package rootcmd

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/revoke"
	"github.com/landovsky/cc-sandbox/internal/tomlmini"
)

// Verbs is every verb there is. The config's `verbs` lists the ones that are on (and `status = "off"`
// turns that one off by itself); nothing outside this list can be switched on.
var Verbs = []string{"run", "stop", "renew", "gc", "attach", "exec-token", "status", "pod-create", "pod-load", "pod-stop", "pod-status", "relay-mint"}

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
	StatusOff    bool // `status = "off"` in the file: the read-only status verb is disabled by itself
	// k8s sessions (F5): kubectl and the launcher's kubeconfig, run as LaunchUser (who holds the token).
	Kubectl      string // absolute kubectl binary, root-owned
	Kubeconfig   string // the launcher kubeconfig under ccsbx-launch: owned by LaunchUser or root, not writable by others
	K8sConn      string // root-owned JSON {server, ca_file, token_file}; the server and CA are passed to kubectl on its command line
	LaunchUser   string // the user that holds the kubeconfig and the launcher token
	K8sNamespace string
	HubURL       string // the session-bus hub, loopback only (relay-mint, relay revoke)
	HubSecret    string // the hub admin secret file: root-owned, 0600
	HubUser      string // the user the hub runs as: the process listening on hub_url's port must be theirs, or the admin secret is not sent
	TermUser     string // the credential-less user the owner-terminal side of a pod attach runs as (ccsbx-term)
	SignerUser   string // the signer's own user (ccsbx-keys): holds the machine key; the launch user must not be it
	// the pod launcher (F2w)
	K8sMachine      string // the machine name pod sessions are placed on (their record's spec.machine)
	K8sReadySeconds int    // how long pod-load waits for both containers (1..600)
	K8sSideImage    string // ghcr.io/landovsky/ccsbx@sha256:..., the sidecar image (required by pod-create)
	K8sCPU          int
	K8sEphemeralMB  int
	K8sWorktreeMB   int
	K8sTokenHours   int      // lifetime of the pod session's token (1..12)
	K8sAudiences    []string // the token's audiences
	SignerSocket    string   // the signer's unix socket, asked as the launch user
	K8sHubURL       string   // the operator egress Service to the hub, handed to the sidecar as --hub-url; empty until it exists
	K8sDoors        []string // NAME=URL doors the sidecar's forwarder may reach
	RevokeDoors     []string // base URLs of the doors' owner listeners that take a signed revoke at the end of a pod generation
	MinUID          int      // a session record must belong to a uid at least this large (never root or a system user)
	TrustedUID      int
}

// DefaultConfigPath is where the installer puts the file.
const DefaultConfigPath = "/etc/ccsbx/ccsbx-root.toml"

// Defaults are the values a key of the file overrides.
func Defaults() Config {
	return Config{
		Verbs:    append([]string(nil), Verbs...),
		StateDir: "/var/lib/ccsbx", RunDir: "/var/lib/ccsbx-run",
		Docker: "/usr/bin/docker", Tmux: "/usr/bin/tmux", TmuxSocket: "/run/ccsbx/tmux.sock",
		PinsFile: "/etc/ccsbx/pins.toml", SeccompFile: "/etc/ccsbx/seccomp.json", Kubectl: "/usr/local/bin/kubectl", Kubeconfig: "/var/lib/ccsbx-launch/launcher.kubeconfig", K8sConn: "/etc/ccsbx/k8s-conn.json", LaunchUser: "ccsbx-launch", K8sNamespace: "cc-sandbox", HubURL: "http://127.0.0.1:4011", HubSecret: "/etc/ccsbx/hub-admin.secret", TermUser: "ccsbx-term", SignerUser: "ccsbx-keys",
		K8sMachine: "future-two", K8sReadySeconds: 120, K8sCPU: 2, K8sEphemeralMB: 20480, K8sWorktreeMB: 10240, K8sTokenHours: 8,
		K8sAudiences: []string{"ccsbx-model", "ccsbx-egress"}, SignerSocket: "/run/ccsbx-signer/launch.sock",
		ServiceUser: "ccsbx", BaseURL: "http://fwd:8080", MinUID: 1000,
	}
}

var configKeys = map[string]bool{
	"allowed_roots": true, "verbs": true, "state_dir": true, "run_dir": true, "docker": true, "tmux": true,
	"tmux_socket": true, "pins_file": true, "service_user": true, "machine": true, "base_url": true, "fetch_url": true, "min_uid": true, "seccomp_profile": true, "status": true,
	"kubectl": true, "kubeconfig": true, "k8s_conn": true, "launch_user": true, "k8s_namespace": true,
	"k8s_machine": true, "revoke_doors": true, "k8s_hub_url": true, "k8s_doors": true, "hub_url": true, "hub_secret": true, "hub_user": true, "term_user": true, "signer_user": true, "k8s_ready_seconds": true, "k8s_side_image": true, "k8s_cpu": true, "k8s_ephemeral_mb": true, "k8s_worktree_mb": true,
	"k8s_token_hours": true, "k8s_audiences": true, "signer_socket": true,
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
		"tmux_socket": &c.TmuxSocket, "pins_file": &c.PinsFile, "seccomp_profile": &c.SeccompFile, "service_user": &c.ServiceUser, "kubectl": &c.Kubectl, "kubeconfig": &c.Kubeconfig, "k8s_conn": &c.K8sConn, "launch_user": &c.LaunchUser, "k8s_namespace": &c.K8sNamespace, "k8s_machine": &c.K8sMachine, "k8s_hub_url": &c.K8sHubURL, "hub_url": &c.HubURL, "hub_secret": &c.HubSecret, "hub_user": &c.HubUser, "term_user": &c.TermUser, "signer_user": &c.SignerUser, "k8s_side_image": &c.K8sSideImage, "signer_socket": &c.SignerSocket, "machine": &c.Machine, "base_url": &c.BaseURL, "fetch_url": &c.FetchURL} {
		if err := str(k, dst); err != nil {
			return nil, err
		}
	}
	if v, ok := top["status"]; ok {
		sv, _ := v.(string)
		if sv != "off" && sv != "on" {
			return nil, fmt.Errorf("config %s: status must be \"on\" or \"off\"", path)
		}
		c.StatusOff = sv == "off"
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
	for k, dst := range map[string]*int{"k8s_ready_seconds": &c.K8sReadySeconds, "k8s_cpu": &c.K8sCPU, "k8s_ephemeral_mb": &c.K8sEphemeralMB, "k8s_worktree_mb": &c.K8sWorktreeMB, "k8s_token_hours": &c.K8sTokenHours} {
		if v, ok := top[k]; ok {
			n, isInt := v.(int64)
			if !isInt {
				if i, ok2 := v.(int); ok2 {
					n, isInt = int64(i), true
				}
			}
			if !isInt || n < 1 || n > 1<<31 {
				return nil, fmt.Errorf("config %s: %s must be a positive number", path, k)
			}
			*dst = int(n)
		}
	}
	if rd, ok, err := list("revoke_doors"); err != nil {
		return nil, err
	} else if ok {
		for _, u := range rd {
			if err := revoke.CheckURL(u); err != nil {
				return nil, fmt.Errorf("config %s: %v", path, err)
			}
		}
		c.RevokeDoors = rd
	}
	if ds, ok, err := list("k8s_doors"); err != nil {
		return nil, err
	} else if ok {
		c.K8sDoors = ds
	}
	if as, ok, err := list("k8s_audiences"); err != nil {
		return nil, err
	} else if ok {
		c.K8sAudiences = as
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
	for name, p := range map[string]string{"state_dir": c.StateDir, "run_dir": c.RunDir, "docker": c.Docker, "tmux": c.Tmux, "tmux_socket": c.TmuxSocket, "pins_file": c.PinsFile, "seccomp_profile": c.SeccompFile, "signer_socket": c.SignerSocket, "hub_secret": c.HubSecret, "kubectl": c.Kubectl, "kubeconfig": c.Kubeconfig, "k8s_conn": c.K8sConn} {
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
	if c.LaunchUser == "" || strings.ContainsAny(c.LaunchUser, ":/ \n") || c.LaunchUser == c.ServiceUser {
		return fmt.Errorf("launch_user %q must be a user name other than the service user", c.LaunchUser)
	}
	if !nsRe.MatchString(c.K8sNamespace) {
		return fmt.Errorf("k8s_namespace %q is not a DNS label", c.K8sNamespace)
	}
	if err := checkHubURL(c.HubURL); err != nil {
		return err
	}
	if c.HubUser != "" && (strings.ContainsAny(c.HubUser, ":/ \n") || len(c.HubUser) > 64) {
		return fmt.Errorf("hub_user %q", c.HubUser)
	}
	if c.K8sReadySeconds < 1 || c.K8sReadySeconds > 600 {
		return fmt.Errorf("k8s_ready_seconds %d must be 1..600", c.K8sReadySeconds)
	}
	if c.K8sTokenHours < 1 || c.K8sTokenHours > 12 {
		return fmt.Errorf("k8s_token_hours %d must be 1..12", c.K8sTokenHours)
	}
	if len(c.K8sAudiences) == 0 {
		return fmt.Errorf("k8s_audiences is empty")
	}
	if c.K8sMachine == "" || strings.ContainsAny(c.K8sMachine, "/ :\n") || c.K8sMachine == c.Machine {
		return fmt.Errorf("k8s_machine %q must be a name other than this machine", c.K8sMachine)
	}
	if _, err := k8s.SideArgv(k8s.SideInfo{Sid: "s0config00", Gen: 1, Label: "config-check", FullID: "o/m/s0config00/g1", Machine: "m", Scope: []byte("{}"), GitRepo: "o/n", HubURL: c.K8sHubURL, Doors: c.K8sDoors}); err != nil {
		return fmt.Errorf("k8s_hub_url / k8s_doors: %v", err)
	}
	if c.K8sSideImage != "" && !sideImageRe.MatchString(c.K8sSideImage) {
		return fmt.Errorf("k8s_side_image %q must be ghcr.io/landovsky/ccsbx@sha256:<64 hex>", c.K8sSideImage)
	}
	if c.Machine == "" {
		return fmt.Errorf("machine is empty (set machine = \"...\")")
	}
	sort.Strings(c.Verbs)
	return nil
}

// Enabled reports whether a verb is switched on.
func (c *Config) Enabled(v string) bool {
	if v == "status" {
		// read-only. Listed explicitly, it is on; with `status = "off"` it is off whatever else is
		// listed; otherwise it is on wherever stop is, so a config written before the verb existed
		// (the installer's explicit list) keeps working.
		return !c.StatusOff && (c.enabled("status") || c.enabled("stop"))
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

var nsRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

var sideImageRe = regexp.MustCompile(`^ghcr\.io/landovsky/ccsbx@sha256:[0-9a-f]{64}$`)

// tailnetNet is Tailscale's address range (CGNAT): the hub on hp listens on hp's own tailnet address.
var tailnetNet = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// checkHubURL: the hub's admin secret never leaves the machine that runs the hub, so the address is plain
// HTTP to an IP literal of this machine: loopback, or its own tailnet address (the hub may listen only on
// that), with no userinfo, path or query. A name is refused: the listener check compares addresses.
func checkHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("hub_url %q must be http://IP:PORT (loopback or this machine's tailnet address)", raw)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !(ip.IsLoopback() || tailnetNet.Contains(ip)) {
		return fmt.Errorf("hub_url host %q must be an IP literal, loopback or in 100.64.0.0/10: the hub's admin secret never leaves its machine", u.Hostname())
	}
	return nil
}
