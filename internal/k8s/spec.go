// spec.go builds the session pod and checks it against the live contract of the k3s side (plan
// §4.6). PodFor is the one place a pod is made; CheckContract is the same rules read the other way,
// so a test can hand it a damaged pod and see each rule refuse, and the backend runs it on every pod
// it is about to send. The values the cluster owns (namespace, node, priority class, pool) are
// Config, not constants: the k3s side can change them without a code change here.
package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
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

// HashAnnotation records the sha256 of the pod as built, so a pod that is already there under the
// same name is accepted only if it is this very one.
const HashAnnotation = "ccsbx.landovsky/spec-hash"

// Spec is one generation. Label and OwnerUID are the caller's own bookkeeping and are NOT written
// to the pod (the cluster has no business knowing them). FenceCmd and SideCmd must carry no
// credential: tokens and keys reach the sidecar by exec stdin, never in the spec.
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
	// KnownUID is the pod UID the session record holds for this generation ("" = none recorded:
	// the first create). It is not written to the pod. A pod that already exists under the name is
	// this generation's only if its UID is this one.
	KnownUID string
}

var (
	sidRe    = regexp.MustCompile(`^s[0-9a-z]{6,16}$`)
	digestRe = regexp.MustCompile(`^([a-z0-9][a-z0-9./_-]*)@sha256:[0-9a-f]{64}$`)
)

// PodName is the pod's name for a generation.
// The cluster's policy requires the prefix ccsbx-s-; with a sid of at most 17 characters and a
// generation of at most 4 digits the name is at most 31 characters, well inside a DNS label.
func PodName(sid string, gen int) string { return fmt.Sprintf("ccsbx-s-%s-g%d", sid, gen) }

var (
	podNameRe = regexp.MustCompile(`^ccsbx-s-[a-z0-9]([a-z0-9]|-[a-z0-9])*$`)
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

func atoiOr(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// validate checks the values the cluster owns before they go into a pod or a kubectl argv.
func (c Config) validate() error {
	if !dnsLabel.MatchString(c.Namespace) || len(c.Namespace) > maxName {
		return fmt.Errorf("namespace %q is not a DNS label", c.Namespace)
	}
	if net.ParseIP(c.DNS) == nil {
		return fmt.Errorf("dns %q is not an IP address", c.DNS)
	}
	if c.GraceSeconds < 0 || c.GraceSeconds > maxGrace {
		return fmt.Errorf("grace %d outside 0..%d", c.GraceSeconds, maxGrace)
	}
	if len(c.AllowedRepos) == 0 {
		return fmt.Errorf("no allowed image repositories")
	}
	return nil
}

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
		m := map[string]string{"memory": miQty(memMB), "cpu": strconv.Itoa(cpu), "ephemeral-storage": miQty(ephMB)}
		r := map[string]string{}
		for k, v := range m {
			r[k] = v
		}
		return Resources{Requests: m, Limits: r}
	}
	vol := func(name string, mb int, mem bool) Volume {
		v := Volume{Name: name, EmptyDir: &EmptyDir{SizeLimit: miQty(mb)}}
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
					{Name: "side-state", MountPath: "/run/ccsbx/side"},
					{Name: "side-tmp", MountPath: "/tmp"}, {Name: "journal", MountPath: "/run/ccsbx/journal"},
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
				vol("home", 512, false), vol("tmp", 512, true), vol("side-tmp", 32, true), vol("side-state", 8, true), vol("journal", 16, false),
			},
		},
	}
	if s.HostUsersFalse {
		p.Spec.HostUsers = ptr(false)
	}
	if err := c.CheckContract(p); err != nil {
		return nil, err
	}
	h, err := specHash(p)
	if err != nil {
		return nil, err
	}
	p.Metadata.Annotations = map[string]string{HashAnnotation: h}
	return p, nil
}

// miQty writes a size the way the API server stores it: whole GiB as "NGi", anything else as "NMi".
// The server canonicalises quantities (1024Mi comes back as 1Gi); a pod fetched after a restart is
// compared with the pod as built (the spec hash), so what we write must already be canonical.
func miQty(mb int) string {
	if mb > 0 && mb%1024 == 0 {
		return fmt.Sprintf("%dGi", mb/1024)
	}
	return fmt.Sprintf("%dMi", mb)
}

// specHash is the sha256 of the pod's canonical JSON without what the server or a person adds after
// creation: annotations, UID, deletionTimestamp, status and the server's two default tolerations.
// The same function hashes the pod as built and the pod as fetched, so a fetched pod hashes equal
// only if every field we model is what we wrote.
func specHash(p *Pod) (string, error) {
	q := *p
	q.Metadata.Annotations = nil
	q.Metadata.UID = ""
	q.Metadata.DeletionTimestamp = nil
	q.Status = nil
	q.Spec.Tolerations = withoutServerDefaults(p.Spec.Tolerations)
	b, err := json.Marshal(&q)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// isServerDefaultToleration is exactly what the API server's DefaultTolerationSeconds admission adds:
// not-ready or unreachable, operator Exists, effect NoExecute, 300 s. Nothing wider is let through.
func isServerDefaultToleration(t Toleration) bool {
	return (t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable") &&
		t.Operator == "Exists" && t.Value == "" && t.Effect == "NoExecute" &&
		t.TolerationSeconds != nil && *t.TolerationSeconds == 300
}

func withoutServerDefaults(in []Toleration) []Toleration {
	var out []Toleration
	for _, t := range in {
		if !isServerDefaultToleration(t) {
			out = append(out, t)
		}
	}
	return out
}

// CheckFetched is CheckContract for a pod the server returned: the server's two default tolerations
// are not ours to refuse, and nothing else is let through.
func (c Config) CheckFetched(p *Pod) error {
	q := *p
	q.Spec.Tolerations = withoutServerDefaults(p.Spec.Tolerations)
	return c.CheckContract(&q)
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
	maxMemMi = 4 * 1024
	maxCPUm  = 4000
	maxEphMi = 40 * 1024
	maxGrace = 60
	maxName  = 63
)

// volumeOwner is the allow-list of who may mount what. Volumes both containers may mount are the
// few that carry the worktree and the two directions of the outbox; everything the sidecar keeps
// secrets or scratch in is the sidecar's alone, and the fence's scratch and home are the fence's. In
// particular the two never share a /tmp: a world-writable directory the agent can write is not a
// place for the process that holds the token.
var volumeOwner = map[string]string{
	"worktree": "both", "out": "both", "in": "both", "managed": "both",
	"side-state": "side", "side-tmp": "side", "journal": "side",
	"home": "fence", "tmp": "fence",
}

// CheckContract refuses a pod the cluster's admission policy would refuse, and one that departs from
// the fence's own design (uids, volume access, ports). It reads the pod, not the Spec, so it holds
// whatever built it.
func (c Config) CheckContract(p *Pod) error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	m, s := p.Metadata, p.Spec
	if !podNameRe.MatchString(m.Name) || len(m.Name) > maxName {
		bad("name %q must match ccsbx-s-<...> and fit 63 characters", m.Name)
	}
	if want := PodName(m.Labels["ccsbx.sid"], atoiOr(m.Labels["ccsbx.gen"])); m.Name != want {
		bad("name %q is not the one derived from its sid and generation labels (%q)", m.Name, want)
	}
	if err := c.validate(); err != nil {
		bad("config: %v", err)
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
		if mm := digestRe.FindStringSubmatch(ct.Image); mm == nil || !c.repoAllowed(mm[1]) || mm[1] != c.repoFor(ct.Name) {
			bad("%s: image %q must be %s by sha256 digest", ct.Name, ct.Image, c.repoFor(ct.Name))
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
			switch who := volumeOwner[mnt.Name]; {
			case who == "":
				bad("%s mounts %s, which is not a volume of this design", ct.Name, mnt.Name)
			case who != "both" && who != ct.Name:
				bad("%s mounts %s, which is %s's alone", ct.Name, mnt.Name, who)
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
		if ct.Name == "side" && len(ct.Command) > 0 {
			if len(ct.Command) < 3 || ct.Command[0] != "ccsbx" || ct.Command[1] != "side" || ct.Command[2] != "serve" || len(ct.Command) > 64 {
				bad("the sidecar runs ccsbx side serve and nothing else (at most 64 arguments)")
			}
			for _, a := range ct.Command {
				if len(a) > maxScope+256 || strings.ContainsRune(a, 0) {
					bad("a sidecar argument is too long or holds a NUL")
				}
			}
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

// repoFor is the repository each container runs from: the fence from the fence image, the sidecar
// from the ccsbx binary image (the first and second of AllowedRepos).
func (c Config) repoFor(container string) string {
	if container == "side" && len(c.AllowedRepos) > 1 {
		return c.AllowedRepos[1]
	}
	if len(c.AllowedRepos) > 0 {
		return c.AllowedRepos[0]
	}
	return ""
}

func (c Config) repoAllowed(repo string) bool {
	for _, r := range c.AllowedRepos {
		if r == repo {
			return true
		}
	}
	return false
}
