// spec.go builds the session pod and checks it against the live contract of the k3s side (plan
// §4.6). PodFor is the one place a pod is made; CheckContract is the same rules read the other way,
// so a test can hand it a damaged pod and see each rule refuse, and the backend runs it on every pod
// it is about to send. The values the cluster owns (namespace, node, priority class, pool) are
// Config, not constants: the k3s side can change them without a code change here.
package k8s

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Fixed by the design, not by the cluster.
const (
	FenceUID = 10001
	SideUID  = 10002
	// RoleLabel is the label the cluster's admission policy keys on.
	RoleLabel = "cc-sandbox/role"
	// SidePort is the one port the pod declares: the forwarder's, on the sidecar.
	SidePort = 8080
)

// Config is what the cluster decides.
type Config struct {
	Namespace     string   // cc-sandbox
	PriorityClass string   // ccsbx-session (required: launcher-created pods must name it)
	NodeHostname  string   // kubernetes.io/hostname value: future-two
	TolerationKey string   // pool
	TolerationVal string   // shared (effect NoSchedule)
	AllowedRepos  []string // image repositories, by digest only
	DNS           string   // dnsPolicy None, one nameserver: nothing resolves from the fence
	GraceSeconds  int64    // terminationGracePeriodSeconds, at most 60
}

// DefaultConfig is the live contract at k3s 1e76dc1/39cf29b.
func DefaultConfig() Config {
	return Config{
		Namespace: "cc-sandbox", PriorityClass: "ccsbx-session", NodeHostname: "future-two",
		TolerationKey: "pool", TolerationVal: "shared",
		AllowedRepos: []string{"ghcr.io/landovsky/ccsbx-fence", "ghcr.io/landovsky/ccsbx"},
		DNS:          "127.0.0.1", GraceSeconds: 30,
	}
}

// Spec is one generation.
type Spec struct {
	Sid, Label     string
	Gen            int
	OwnerUID       int
	FenceImage     string // repo@sha256:<64 hex>
	SideImage      string
	MemoryMB       int // the fence's; the sidecar's is fixed and small
	CPU            int // whole CPUs for the fence
	EphemeralMB    int // per container
	WorktreeMB     int // the emptyDir sizeLimit
	FenceCmd       []string
	SideCmd        []string
	HostUsersFalse bool
}

var (
	sidRe    = regexp.MustCompile(`^s[0-9a-z]{6,16}$`)
	digestRe = regexp.MustCompile(`^([a-z0-9][a-z0-9./_-]*)@sha256:[0-9a-f]{64}$`)
)

// PodName is the pod's name for a generation.
// The cluster's policy requires the prefix ccsbx-s-; with a sid of at most 17 characters and a
// generation of at most 4 digits the name is at most 31 characters, well inside a DNS label.
func PodName(sid string, gen int) string { return fmt.Sprintf("ccsbx-s-%s-g%d", sid, gen) }

var podNameRe = regexp.MustCompile(`^ccsbx-s-[a-z0-9][a-z0-9-]*$`)

// PodFor builds the pod with the default config.
func PodFor(s Spec) (*Pod, error) { return DefaultConfig().PodFor(s) }

func ptr[T any](v T) *T { return &v }

// PodFor builds the session pod for s and checks it.
func (c Config) PodFor(s Spec) (*Pod, error) {
	if !sidRe.MatchString(s.Sid) || s.Gen < 1 || s.Gen > 9999 {
		return nil, fmt.Errorf("sid %q gen %d", s.Sid, s.Gen)
	}
	if s.MemoryMB < 256 || s.CPU < 1 || s.EphemeralMB < 256 || s.WorktreeMB < 64 {
		return nil, fmt.Errorf("resources: memory %d MB, cpu %d, ephemeral %d MB, worktree %d MB", s.MemoryMB, s.CPU, s.EphemeralMB, s.WorktreeMB)
	}
	fenceCmd := s.FenceCmd
	if len(fenceCmd) == 0 {
		fenceCmd = []string{"tmux", "-f", "/dev/null", "new-session", "-s", s.Sid, "claude"}
	}
	sec := func(uid int64) *ContainerSec {
		return &ContainerSec{
			RunAsUser: ptr(uid), RunAsGroup: ptr(uid), RunAsNonRoot: ptr(true), ReadOnlyRootFilesystem: ptr(true),
			AllowPrivilegeEscalation: ptr(false), Capabilities: &Capabilities{Drop: []string{"ALL"}},
		}
	}
	guaranteed := func(memMB, cpu, ephMB int) Resources {
		m := map[string]string{"memory": fmt.Sprintf("%dMi", memMB), "cpu": strconv.Itoa(cpu), "ephemeral-storage": fmt.Sprintf("%dMi", ephMB)}
		r := map[string]string{}
		for k, v := range m {
			r[k] = v
		}
		return Resources{Requests: m, Limits: r}
	}
	vol := func(name string, mb int, mem bool) Volume {
		v := Volume{Name: name, EmptyDir: &EmptyDir{SizeLimit: fmt.Sprintf("%dMi", mb)}}
		if mem {
			v.EmptyDir.Medium = "Memory"
		}
		return v
	}
	p := &Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: ObjectMeta{Name: PodName(s.Sid, s.Gen), Namespace: c.Namespace, Labels: map[string]string{
			RoleLabel: "session", "ccsbx.sid": s.Sid, "ccsbx.gen": strconv.Itoa(s.Gen),
		}},
		Spec: PodSpec{
			NodeSelector:                  map[string]string{"kubernetes.io/hostname": c.NodeHostname},
			Tolerations:                   []Toleration{{Key: c.TolerationKey, Operator: "Equal", Value: c.TolerationVal, Effect: "NoSchedule"}},
			PriorityClassName:             c.PriorityClass,
			RestartPolicy:                 "Never",
			TerminationGracePeriodSeconds: ptr(c.GraceSeconds),
			AutomountServiceAccountToken:  ptr(false),
			EnableServiceLinks:            ptr(false),
			ShareProcessNamespace:         ptr(false),
			DNSPolicy:                     "None",
			DNSConfig:                     &DNSConfig{Nameservers: []string{c.DNS}},
			SecurityContext:               &PodSecurity{RunAsNonRoot: ptr(true), SeccompProfile: &Seccomp{Type: "RuntimeDefault"}},
			InitContainers: []Container{{
				Name: "side", Image: s.SideImage, Command: s.SideCmd, RestartPolicy: "Always",
				Ports:           []Port{{Name: "fwd", ContainerPort: SidePort, Protocol: "TCP"}},
				Resources:       guaranteed(256, 1, 1024),
				SecurityContext: sec(SideUID),
				VolumeMounts: []VolumeMount{
					{Name: "worktree", MountPath: "/work"}, {Name: "out", MountPath: "/run/ccsbx/out", ReadOnly: true},
					{Name: "in", MountPath: "/run/ccsbx/in"}, {Name: "managed", MountPath: "/etc/claude-code"},
					{Name: "side-state", MountPath: "/run/ccsbx/side"}, {Name: "relay-cred", MountPath: "/run/ccsbx/relay"},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Containers: []Container{{
				Name: "fence", Image: s.FenceImage, Command: fenceCmd, TTY: true, Stdin: true,
				Resources:       guaranteed(s.MemoryMB, s.CPU, s.EphemeralMB),
				SecurityContext: sec(FenceUID),
				VolumeMounts: []VolumeMount{
					{Name: "worktree", MountPath: "/work"}, {Name: "out", MountPath: "/run/ccsbx/out"},
					{Name: "in", MountPath: "/run/ccsbx/in", ReadOnly: true}, {Name: "managed", MountPath: "/etc/claude-code", ReadOnly: true},
					{Name: "home", MountPath: "/home/agent"}, {Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []Volume{
				vol("worktree", s.WorktreeMB, false), vol("out", 64, false), vol("in", 16, false), vol("managed", 8, false),
				vol("home", 512, false), vol("tmp", 512, true), vol("side-state", 8, true), vol("relay-cred", 1, true),
			},
		},
	}
	if s.HostUsersFalse {
		p.Spec.HostUsers = ptr(false)
	}
	if err := c.CheckContract(p); err != nil {
		return nil, err
	}
	return p, nil
}

// quantity values in the units we write and the cluster limits in.
var qtyRe = regexp.MustCompile(`^([0-9]+)(m|Mi|Gi)?$`)

func qty(v string) (int64, string, bool) {
	m := qtyRe.FindStringSubmatch(v)
	if m == nil {
		return 0, "", false
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n, m[2], true
}

// toMi converts a memory or storage quantity to MiB; toMilli a CPU quantity to millicores.
func toMi(v string) (int64, bool) {
	n, u, ok := qty(v)
	switch {
	case ok && u == "Mi":
		return n, true
	case ok && u == "Gi":
		return n * 1024, true
	}
	return 0, false
}
func toMilli(v string) (int64, bool) {
	n, u, ok := qty(v)
	switch {
	case ok && u == "":
		return n * 1000, true
	case ok && u == "m":
		return n, true
	}
	return 0, false
}

const (
	maxMemMi  = 4 * 1024
	maxCPUm   = 4000
	maxEphMi  = 40 * 1024
	maxGrace  = 60
	sideOnlyA = "side-state"
	sideOnlyB = "relay-cred"
)

// CheckContract refuses a pod the cluster's admission policy would refuse, and one that departs from
// the fence's own design (uids, volume access, ports). It reads the pod, not the Spec, so it holds
// whatever built it.
func (c Config) CheckContract(p *Pod) error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	m, s := p.Metadata, p.Spec
	if !podNameRe.MatchString(m.Name) || len(m.Name) > 63 {
		bad("name %q must match ccsbx-s-<...> and fit 63 characters", m.Name)
	}
	if m.Namespace != c.Namespace {
		bad("namespace %q, want %q", m.Namespace, c.Namespace)
	}
	if m.Labels[RoleLabel] != "session" {
		bad("label %s must be session", RoleLabel)
	}
	if len(m.Finalizers) != 0 {
		bad("finalizers are refused")
	}
	if s.NodeSelector["kubernetes.io/hostname"] != c.NodeHostname || len(s.NodeSelector) != 1 {
		bad("nodeSelector must be exactly kubernetes.io/hostname=%s", c.NodeHostname)
	}
	tol := false
	for _, t := range s.Tolerations {
		if t.Key == c.TolerationKey && t.Value == c.TolerationVal && t.Effect == "NoSchedule" && t.Operator == "Equal" {
			tol = true
		}
	}
	if !tol || len(s.Tolerations) != 1 {
		bad("tolerations must be exactly %s=%s:NoSchedule", c.TolerationKey, c.TolerationVal)
	}
	if c.PriorityClass == "" || s.PriorityClassName != c.PriorityClass {
		bad("priorityClassName must be %q", c.PriorityClass)
	}
	if s.RuntimeClassName != nil {
		bad("runtimeClassName is refused")
	}
	if g := s.TerminationGracePeriodSeconds; g != nil && (*g < 0 || *g > maxGrace) {
		bad("terminationGracePeriodSeconds %d > %d", *g, maxGrace)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		bad("automountServiceAccountToken must be false")
	}
	if s.ShareProcessNamespace != nil && *s.ShareProcessNamespace {
		bad("shareProcessNamespace must not be true")
	}
	if s.HostNetwork || s.HostPID || s.HostIPC {
		bad("host namespaces are refused")
	}
	if s.HostUsers != nil && *s.HostUsers {
		bad("hostUsers, if set, is false")
	}
	if s.DNSPolicy != "None" || s.DNSConfig == nil || len(s.DNSConfig.Nameservers) != 1 || s.DNSConfig.Nameservers[0] != c.DNS {
		bad("dnsPolicy must be None with the single nameserver %s", c.DNS)
	}
	if s.SecurityContext == nil || s.SecurityContext.RunAsNonRoot == nil || !*s.SecurityContext.RunAsNonRoot ||
		s.SecurityContext.SeccompProfile == nil || s.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		bad("pod securityContext: runAsNonRoot and seccomp RuntimeDefault")
	}
	if len(s.Containers) != 1 || s.Containers[0].Name != "fence" {
		bad("exactly one container, named fence")
	}
	if len(s.InitContainers) != 1 || s.InitContainers[0].Name != "side" || s.InitContainers[0].RestartPolicy != "Always" {
		bad("exactly one init container, the native sidecar side (restartPolicy Always)")
	}
	vols := map[string]Volume{}
	for _, v := range s.Volumes {
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit == "" {
			bad("volume %s must be an emptyDir with a sizeLimit", v.Name)
		}
		if v.EmptyDir != nil {
			if n, ok := toMi(v.EmptyDir.SizeLimit); !ok || n <= 0 || n > maxEphMi {
				bad("volume %s: sizeLimit %q must be 1..40Gi (the Memory one too)", v.Name, v.EmptyDir.SizeLimit)
			}
		}
		vols[v.Name] = v
	}
	if _, ok := vols["worktree"]; !ok {
		bad("no worktree volume")
	}
	all := append(append([]Container{}, s.InitContainers...), s.Containers...)
	for _, ct := range all {
		uid := int64(FenceUID)
		if ct.Name == "side" {
			uid = SideUID
		}
		if !digestRe.MatchString(ct.Image) || !c.repoAllowed(digestRe.FindStringSubmatch(ct.Image)[1]) {
			bad("%s: image %q must be an allowed repository by sha256 digest", ct.Name, ct.Image)
		}
		sc := ct.SecurityContext
		switch {
		case sc == nil:
			bad("%s: no securityContext", ct.Name)
		default:
			if sc.RunAsUser == nil || *sc.RunAsUser != uid || sc.RunAsGroup == nil || *sc.RunAsGroup != uid {
				bad("%s: must run as %d:%d", ct.Name, uid, uid)
			}
			if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
				sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || (sc.Privileged != nil && *sc.Privileged) {
				bad("%s: runAsNonRoot, readOnlyRootFilesystem, no privilege escalation, not privileged", ct.Name)
			}
			if sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
				bad("%s: capabilities must drop ALL and add nothing", ct.Name)
			}
		}
		for _, k := range []string{"memory", "cpu", "ephemeral-storage"} {
			lim, req := ct.Resources.Limits[k], ct.Resources.Requests[k]
			if lim == "" || lim != req {
				bad("%s: %s needs limits == requests (Guaranteed QoS)", ct.Name, k)
				continue
			}
			var n, max int64
			var ok bool
			switch k {
			case "memory":
				n, ok = toMi(lim)
				max = maxMemMi
			case "ephemeral-storage":
				n, ok = toMi(lim)
				max = maxEphMi
			default:
				n, ok = toMilli(lim)
				max = maxCPUm
			}
			if !ok || n > max || n <= 0 {
				bad("%s: %s %q outside 1..%d", ct.Name, k, lim, max)
			}
		}
		for _, mnt := range ct.VolumeMounts {
			if _, ok := vols[mnt.Name]; !ok {
				bad("%s mounts %s, which is not a volume", ct.Name, mnt.Name)
			}
			if ct.Name == "fence" && (mnt.Name == sideOnlyA || mnt.Name == sideOnlyB) {
				bad("fence mounts %s, which is the sidecar's alone", mnt.Name)
			}
		}
		for _, e := range ct.Env {
			if strings.Contains(strings.ToLower(e.Name), "token") || strings.Contains(strings.ToLower(e.Name), "key") {
				bad("%s: env %s looks like a credential (they arrive by exec, never in the spec)", ct.Name, e.Name)
			}
		}
		if ct.Name == "fence" && len(ct.Ports) != 0 {
			bad("the fence declares no ports")
		}
		if ct.Name == "side" && (len(ct.Ports) != 1 || ct.Ports[0].ContainerPort != SidePort) {
			bad("the sidecar declares only the forwarder port %d", SidePort)
		}
	}
	// who may write what: the fence cannot write in/ or managed/, the sidecar cannot write out/
	need := map[string]map[string]bool{"fence": {"in": true, "managed": true}, "side": {"out": true}}
	for _, ct := range all {
		for _, mnt := range ct.VolumeMounts {
			if need[ct.Name][mnt.Name] && !mnt.ReadOnly {
				bad("%s must mount %s read-only", ct.Name, mnt.Name)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pod %s breaks the contract: %s", m.Name, strings.Join(errs, "; "))
	}
	return nil
}

func (c Config) repoAllowed(repo string) bool {
	for _, r := range c.AllowedRepos {
		if r == repo {
			return true
		}
	}
	return false
}
