// Tests for the pod builder and the contract check. The contract is what the k3s side's admission
// policy and the fence's own design require, so every rule has a test that damages a good pod in
// exactly that way and expects the refusal (a block twin), plus one that the good pod passes.
package k8s

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	fenceImg = "ghcr.io/landovsky/ccsbx-fence@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sideImg  = "ghcr.io/landovsky/ccsbx@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func goodSpec() Spec {
	cmd, err := SideArgv(SideInfo{Sid: "s0abcdefgh", Gen: 2, Label: "cc-sandbox:x", FullID: "tomas/future-two/s0abcdefgh/g2", Machine: "future-two",
		Scope: []byte(`{"manager":{"node":"cc-sandbox:#6b3s","machine":"hp-ubuntu"},"peers":[]}`)})
	if err != nil {
		panic(err)
	}
	return Spec{Sid: "s0abcdefgh", Gen: 2, Label: "cc-sandbox:x", OwnerUID: 1000, FenceImage: fenceImg, SideImage: sideImg, SideCmd: cmd,
		MemoryMB: 4096, CPU: 2, EphemeralMB: 20480, WorktreeMB: 10240}
}

func goodPod(t *testing.T) *Pod {
	t.Helper()
	p, err := PodFor(goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPodForMakesAContractPod(t *testing.T) {
	p := goodPod(t)
	if p.Metadata.Name != "ccsbx-s-s0abcdefgh-g2" || p.Metadata.Namespace != "cc-sandbox" || p.Metadata.Labels["cc-sandbox/role"] != "session" {
		t.Errorf("%+v", p.Metadata)
	}
	if p.Spec.PriorityClassName != "ccsbx-session" || p.Spec.RestartPolicy != "Never" || *p.Spec.TerminationGracePeriodSeconds != 30 {
		t.Errorf("%+v", p.Spec)
	}
	if len(p.Spec.InitContainers) != 1 || p.Spec.InitContainers[0].RestartPolicy != "Always" || len(p.Spec.Containers) != 1 {
		t.Error("expected a native sidecar and one fence container")
	}
	// the JSON the API server gets carries none of the fields the contract refuses
	b, _ := json.Marshal(p)
	for _, forbidden := range []string{"runtimeClassName", "finalizers", "valueFrom", "secret", "configMap", "hostPath", "serviceAccount", "ephemeralContainers", "preemptionPolicy", "hostNetwork"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("the pod JSON mentions %s", forbidden)
		}
	}
	// no credential anywhere in the spec: tokens arrive by exec stdin
	for _, c := range append(append([]Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		if len(c.Env) != 0 {
			t.Errorf("%s has env %v", c.Name, c.Env)
		}
	}
	if q := p.Spec.Containers[0].Resources; q.Limits["memory"] != "4Gi" || q.Requests["cpu"] != "2" || q.Limits["ephemeral-storage"] != "20Gi" {
		t.Errorf("%+v", q)
	}
	if h := p.Spec.HostUsers; h != nil {
		t.Error("hostUsers set without being asked")
	}
	s := goodSpec()
	s.HostUsersFalse = true
	if p, _ := PodFor(s); p.Spec.HostUsers == nil || *p.Spec.HostUsers {
		t.Error("hostUsers:false was not set")
	}
}

func TestPodForRefusesBadSpecs(t *testing.T) {
	for name, mod := range map[string]func(*Spec){
		"a sid with a slash":     func(s *Spec) { s.Sid = "s0/../x" },
		"an uppercase sid":       func(s *Spec) { s.Sid = "S0ABCDEFGH" },
		"generation 0":           func(s *Spec) { s.Gen = 0 },
		"too little memory":      func(s *Spec) { s.MemoryMB = 10 },
		"no cpu":                 func(s *Spec) { s.CPU = 0 },
		"more memory than 4Gi":   func(s *Spec) { s.MemoryMB = 4097 },
		"more cpu than 4":        func(s *Spec) { s.CPU = 5 },
		"more storage than 40Gi": func(s *Spec) { s.EphemeralMB = 40*1024 + 1 },
		"a worktree over 40Gi":   func(s *Spec) { s.WorktreeMB = 40*1024 + 1 },
		"an image by tag":        func(s *Spec) { s.FenceImage = "ghcr.io/landovsky/ccsbx-fence:latest" },
		"a foreign repository":   func(s *Spec) { s.FenceImage = "docker.io/evil/img@sha256:" + strings.Repeat("c", 64) },
		"a sidecar from elsewhere": func(s *Spec) {
			s.SideImage = "ghcr.io/landovsky/other@sha256:" + strings.Repeat("d", 64)
		},
		"an image with a space": func(s *Spec) { s.FenceImage = fenceImg + " --privileged" },
	} {
		s := goodSpec()
		mod(&s)
		if _, err := PodFor(s); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

type contractCase struct {
	name string
	mod  func(p *Pod)
	want string
}

func fenceC(p *Pod) *Container { return &p.Spec.Containers[0] }
func sideC(p *Pod) *Container  { return &p.Spec.InitContainers[0] }

// contractCases damages a good pod in the one way each rule forbids; the same table feeds the fake
// API server's own admission (backend_test.go), so the two are compared on every rule.
func contractCases() []contractCase {
	f, sd := fenceC, sideC
	vol := func(p *Pod, name string) *Volume {
		for i := range p.Spec.Volumes {
			if p.Spec.Volumes[i].Name == name {
				return &p.Spec.Volumes[i]
			}
		}
		panic("no volume " + name)
	}
	return []contractCase{
		{"name without the ccsbx-s- prefix", func(p *Pod) { p.Metadata.Name = "mypod" }, "ccsbx-s-"},
		{"a name over 63 characters", func(p *Pod) { p.Metadata.Name = "ccsbx-s-" + strings.Repeat("a", 60) }, "63"},
		{"a name that ends in a dash", func(p *Pod) { p.Metadata.Name += "-" }, "ccsbx-s-"},
		{"a name that is not derived from its labels", func(p *Pod) { p.Metadata.Labels["ccsbx.sid"] = "s0zzzzzzzz" }, "not the one derived"},
		{"another namespace", func(p *Pod) { p.Metadata.Namespace = "kube-system" }, "namespace"},
		{"role door", func(p *Pod) { p.Metadata.Labels["cc-sandbox/role"] = "door" }, "session"},
		{"a finalizer", func(p *Pod) { p.Metadata.Finalizers = []string{"x/y"} }, "finalizers"},
		{"another node", func(p *Pod) { p.Spec.NodeSelector["kubernetes.io/hostname"] = "hp-ubuntu" }, "nodeSelector"},
		{"an extra node selector", func(p *Pod) { p.Spec.NodeSelector["a"] = "b" }, "nodeSelector"},
		{"no toleration", func(p *Pod) { p.Spec.Tolerations = nil }, "tolerations"},
		{"a toleration of everything", func(p *Pod) { p.Spec.Tolerations = append(p.Spec.Tolerations, Toleration{Operator: "Exists"}) }, "tolerations"},
		{"a NoExecute toleration", func(p *Pod) { p.Spec.Tolerations[0].Effect = "NoExecute" }, "tolerations"},
		{"a PreferNoSchedule toleration", func(p *Pod) { p.Spec.Tolerations[0].Effect = "PreferNoSchedule" }, "tolerations"},
		{"an Exists operator with the right key", func(p *Pod) { p.Spec.Tolerations[0].Operator = "Exists"; p.Spec.Tolerations[0].Value = "" }, "tolerations"},
		{"no priority class", func(p *Pod) { p.Spec.PriorityClassName = "" }, "priorityClassName"},
		{"another priority class", func(p *Pod) { p.Spec.PriorityClassName = "system-node-critical" }, "priorityClassName"},
		{"a runtime class", func(p *Pod) { p.Spec.RuntimeClassName = ptr("gvisor") }, "runtimeClassName"},
		{"a grace period of 120 s", func(p *Pod) { p.Spec.TerminationGracePeriodSeconds = ptr(int64(120)) }, "terminationGracePeriodSeconds"},
		{"a negative grace period", func(p *Pod) { p.Spec.TerminationGracePeriodSeconds = ptr(int64(-1)) }, "terminationGracePeriodSeconds"},
		{"the service account token", func(p *Pod) { p.Spec.AutomountServiceAccountToken = ptr(true) }, "automountServiceAccountToken"},
		{"an unset service account token flag", func(p *Pod) { p.Spec.AutomountServiceAccountToken = nil }, "automountServiceAccountToken"},
		{"a shared process namespace", func(p *Pod) { p.Spec.ShareProcessNamespace = ptr(true) }, "shareProcessNamespace"},
		{"host network", func(p *Pod) { p.Spec.HostNetwork = true }, "host namespaces"},
		{"host pid", func(p *Pod) { p.Spec.HostPID = true }, "host namespaces"},
		{"host ipc", func(p *Pod) { p.Spec.HostIPC = true }, "host namespaces"},
		{"hostUsers true", func(p *Pod) { p.Spec.HostUsers = ptr(true) }, "hostUsers"},
		{"cluster DNS", func(p *Pod) { p.Spec.DNSPolicy = "ClusterFirst" }, "dnsPolicy"},
		{"another nameserver", func(p *Pod) { p.Spec.DNSConfig.Nameservers = []string{"8.8.8.8"} }, "dnsPolicy"},
		{"two nameservers", func(p *Pod) { p.Spec.DNSConfig.Nameservers = []string{"127.0.0.1", "8.8.8.8"} }, "dnsPolicy"},
		{"no seccomp", func(p *Pod) { p.Spec.SecurityContext.SeccompProfile = nil }, "seccomp"},
		{"an unconfined seccomp", func(p *Pod) { p.Spec.SecurityContext.SeccompProfile.Type = "Unconfined" }, "seccomp"},
		{"the pod not runAsNonRoot", func(p *Pod) { p.Spec.SecurityContext.RunAsNonRoot = ptr(false) }, "pod securityContext"},
		{"the fence not runAsNonRoot", func(p *Pod) { f(p).SecurityContext.RunAsNonRoot = ptr(false) }, "runAsNonRoot"},
		{"a second container", func(p *Pod) { p.Spec.Containers = append(p.Spec.Containers, Container{Name: "x"}) }, "exactly one container"},
		{"another container name", func(p *Pod) { f(p).Name = "app" }, "exactly one container"},
		{"no init container", func(p *Pod) { p.Spec.InitContainers = nil }, "init container"},
		{"a second init container", func(p *Pod) { p.Spec.InitContainers = append(p.Spec.InitContainers, Container{Name: "init2"}) }, "init container"},
		{"a sidecar that is a plain init container", func(p *Pod) { sd(p).RestartPolicy = "" }, "init container"},
		{"a volume that is not sized", func(p *Pod) { vol(p, "worktree").EmptyDir.SizeLimit = "" }, "sizeLimit"},
		{"a volume of 41Gi", func(p *Pod) { vol(p, "worktree").EmptyDir.SizeLimit = "41Gi" }, "sizeLimit"},
		{"a volume of 0Mi", func(p *Pod) { vol(p, "worktree").EmptyDir.SizeLimit = "0Mi" }, "sizeLimit"},
		{"a Memory volume of 41Gi", func(p *Pod) { vol(p, "tmp").EmptyDir.SizeLimit = "41Gi" }, "sizeLimit"},
		{"a volume that is not an emptyDir", func(p *Pod) { vol(p, "out").EmptyDir = nil }, "emptyDir"},
		{"no worktree volume", func(p *Pod) { vol(p, "worktree").Name = "wt"; fixMounts(p, "worktree", "wt") }, "no worktree volume"},
		{"a fence image by tag", func(p *Pod) { f(p).Image = "ghcr.io/landovsky/ccsbx-fence:latest" }, "digest"},
		{"a sidecar image from another repo", func(p *Pod) { sd(p).Image = "quay.io/x/y@sha256:" + strings.Repeat("e", 64) }, "digest"},
		{"the fence running the sidecar's image", func(p *Pod) { f(p).Image = sideImg }, "ccsbx-fence"},
		{"the sidecar running the fence's image", func(p *Pod) { sd(p).Image = fenceImg }, "ghcr.io/landovsky/ccsbx by"},
		{"fence as root", func(p *Pod) { f(p).SecurityContext.RunAsUser = ptr(int64(0)) }, "10001"},
		{"fence as the sidecar's uid", func(p *Pod) { f(p).SecurityContext.RunAsUser = ptr(int64(10002)) }, "10001"},
		{"fence with group 0", func(p *Pod) { f(p).SecurityContext.RunAsGroup = ptr(int64(0)) }, "10001"},
		{"sidecar as the fence's uid", func(p *Pod) { sd(p).SecurityContext.RunAsUser = ptr(int64(10001)) }, "10002"},
		{"a writable root filesystem", func(p *Pod) { f(p).SecurityContext.ReadOnlyRootFilesystem = ptr(false) }, "readOnlyRootFilesystem"},
		{"privilege escalation allowed", func(p *Pod) { f(p).SecurityContext.AllowPrivilegeEscalation = ptr(true) }, "privilege escalation"},
		{"privileged", func(p *Pod) { f(p).SecurityContext.Privileged = ptr(true) }, "privileged"},
		{"a capability added", func(p *Pod) { f(p).SecurityContext.Capabilities.Add = []string{"NET_ADMIN"} }, "capabilities"},
		{"capabilities not dropped", func(p *Pod) { f(p).SecurityContext.Capabilities.Drop = []string{"NET_RAW"} }, "capabilities"},
		{"capabilities dropped with another", func(p *Pod) { f(p).SecurityContext.Capabilities.Drop = []string{"ALL", "NET_RAW"} }, "capabilities"},
		{"no security context", func(p *Pod) { f(p).SecurityContext = nil }, "no securityContext"},
		{"memory over 4Gi", func(p *Pod) { f(p).Resources.Limits["memory"] = "5Gi"; f(p).Resources.Requests["memory"] = "5Gi" }, "memory"},
		{"cpu over 4", func(p *Pod) { f(p).Resources.Limits["cpu"] = "8"; f(p).Resources.Requests["cpu"] = "8" }, "cpu"},
		{"cpu 0", func(p *Pod) { f(p).Resources.Limits["cpu"] = "0"; f(p).Resources.Requests["cpu"] = "0" }, "cpu"},
		{"ephemeral storage over 40Gi", func(p *Pod) {
			f(p).Resources.Limits["ephemeral-storage"] = "41Gi"
			f(p).Resources.Requests["ephemeral-storage"] = "41Gi"
		}, "ephemeral-storage"},
		{"requests below limits (not Guaranteed)", func(p *Pod) { f(p).Resources.Requests["memory"] = "1Gi" }, "Guaranteed"},
		{"no limit at all", func(p *Pod) { delete(f(p).Resources.Limits, "cpu"); delete(f(p).Resources.Requests, "cpu") }, "Guaranteed"},
		{"the sidecar over its limits", func(p *Pod) { sd(p).Resources.Limits["memory"] = "9Gi"; sd(p).Resources.Requests["memory"] = "9Gi" }, "memory"},
		{"the fence mounting the sidecar's state", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "side-state", MountPath: "/s"})
		}, "alone"},
		{"the fence mounting the snapshot", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "snapshot", MountPath: "/snap"})
		}, "alone"},
		{"no fsGroup", func(p *Pod) { p.Spec.SecurityContext.FSGroup = nil }, "fsGroup"},
		{"fsGroup root", func(p *Pod) { g := int64(0); p.Spec.SecurityContext.FSGroup = &g }, "fsGroup"},
		{"fsGroup the fence's own group", func(p *Pod) { g := int64(FenceUID); p.Spec.SecurityContext.FSGroup = &g }, "fsGroup"},
		{"fsGroup the sidecar's own group", func(p *Pod) { g := int64(SideUID); p.Spec.SecurityContext.FSGroup = &g }, "fsGroup"},
		{"another fsGroup", func(p *Pod) { g := int64(WorkGID + 1); p.Spec.SecurityContext.FSGroup = &g }, "fsGroup"},
		{"the fence mounting the sidecar's journal", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "journal", MountPath: "/j"})
		}, "alone"},
		// M1: the two never share a /tmp
		{"the sidecar mounting the fence's tmp", func(p *Pod) {
			sd(p).VolumeMounts = append(sd(p).VolumeMounts, VolumeMount{Name: "tmp", MountPath: "/t"})
		}, "fence's alone"},
		{"the sidecar mounting the fence's home", func(p *Pod) {
			sd(p).VolumeMounts = append(sd(p).VolumeMounts, VolumeMount{Name: "home", MountPath: "/h"})
		}, "fence's alone"},
		{"the fence mounting the sidecar's tmp", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "side-tmp", MountPath: "/t"})
		}, "side's alone"},
		{"a mount of a volume that does not exist", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "host", MountPath: "/h"})
		}, "not a volume"},
		{"the fence able to write in/", func(p *Pod) { f(p).VolumeMounts[2].ReadOnly = false }, "read-only"},
		{"the fence able to write managed/", func(p *Pod) { f(p).VolumeMounts[3].ReadOnly = false }, "read-only"},
		{"the sidecar able to write out/", func(p *Pod) { sd(p).VolumeMounts[1].ReadOnly = false }, "read-only"},
		{"a port on the fence", func(p *Pod) { f(p).Ports = []Port{{ContainerPort: 22}} }, "no ports"},
		{"another port on the sidecar", func(p *Pod) { sd(p).Ports[0].ContainerPort = 2375 }, "forwarder port"},
		{"a credential in the environment", func(p *Pod) { sd(p).Env = []EnvVar{{Name: "SESSION_TOKEN", Value: "x"}} }, "credential"},
		{"a key in the environment", func(p *Pod) { f(p).Env = []EnvVar{{Name: "API_KEY", Value: "x"}} }, "credential"},
	}
}

// fixMounts renames a volume in every mount, so a case that renames a volume breaks only the rule
// it is about.
func fixMounts(p *Pod, from, to string) {
	for _, list := range [][]Container{p.Spec.InitContainers, p.Spec.Containers} {
		for i := range list {
			for j := range list[i].VolumeMounts {
				if list[i].VolumeMounts[j].Name == from {
					list[i].VolumeMounts[j].Name = to
				}
			}
		}
	}
}

func TestCheckContractRefusesEachViolation(t *testing.T) {
	c := DefaultConfig()
	if err := c.CheckContract(goodPod(t)); err != nil {
		t.Fatalf("the good pod was refused: %v", err)
	}
	for _, tc := range contractCases() {
		p := goodPod(t)
		tc.mod(p)
		err := c.CheckContract(p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want a refusal mentioning %q)", tc.name, err, tc.want)
		}
	}
}

// Config values are checked before they go into a pod or a kubectl argv.
func TestConfigIsValidated(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"a namespace that starts with a dash": func(c *Config) { c.Namespace = "-n" },
		"an uppercase namespace":              func(c *Config) { c.Namespace = "CC" },
		"a nameserver that is not an IP":      func(c *Config) { c.DNS = "dns.example" },
		"a grace period over 60":              func(c *Config) { c.GraceSeconds = 61 },
		"no image repositories":               func(c *Config) { c.AllowedRepos = nil },
	} {
		c := DefaultConfig()
		mod(&c)
		if _, err := c.PodFor(goodSpec()); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// S2 helpers: the hash annotation is the pod's identity.
func TestPodCarriesItsSpecHash(t *testing.T) {
	p := goodPod(t)
	h := p.Metadata.Annotations[HashAnnotation]
	if len(h) != 64 {
		t.Fatalf("%q", h)
	}
	s := goodSpec()
	s.MemoryMB = 2048
	q, _ := PodFor(s)
	if q.Metadata.Annotations[HashAnnotation] == h {
		t.Error("two different specs have the same hash")
	}
	if p2, _ := PodFor(goodSpec()); p2.Metadata.Annotations[HashAnnotation] != h {
		t.Error("the hash is not deterministic")
	}
}

// What the server adds (its NoExecute tolerations) is tolerated in a fetched pod, and only that.
func TestCheckFetchedToleratesServerDefaultsOnly(t *testing.T) {
	c := DefaultConfig()
	s300 := int64(300)
	p := goodPod(t)
	p.Spec.Tolerations = append(p.Spec.Tolerations,
		Toleration{Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &s300},
		Toleration{Key: "node.kubernetes.io/unreachable", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &s300})
	if err := c.CheckContract(p); err == nil {
		t.Error("the strict check accepted extra tolerations")
	}
	if err := c.CheckFetched(p); err != nil {
		t.Errorf("a pod with the server's defaults was refused: %v", err)
	}
	p.Spec.Tolerations = append(p.Spec.Tolerations, Toleration{Key: "dedicated", Operator: "Exists"})
	if err := c.CheckFetched(p); err == nil {
		t.Error("an extra toleration of ours was accepted")
	}
}

// Each way of widening a "default" toleration is refused: another key, a wider operator, the effect,
// no seconds, other seconds, or a value.
func TestServerDefaultTolerationsAreExactlyTwo(t *testing.T) {
	c := DefaultConfig()
	s300, s9000 := int64(300), int64(9000)
	for why, tol := range map[string]Toleration{
		"another node.kubernetes.io key": {Key: "node.kubernetes.io/disk-pressure", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &s300},
		"effect NoSchedule":              {Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoSchedule", TolerationSeconds: &s300},
		"no seconds (forever)":           {Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoExecute"},
		"9000 seconds":                   {Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &s9000},
		"operator Equal":                 {Key: "node.kubernetes.io/not-ready", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &s300},
		"a value":                        {Key: "node.kubernetes.io/not-ready", Operator: "Exists", Value: "x", Effect: "NoExecute", TolerationSeconds: &s300},
	} {
		p := goodPod(t)
		p.Spec.Tolerations = append(p.Spec.Tolerations, tol)
		if err := c.CheckFetched(p); err == nil {
			t.Errorf("%s was let through as a server default", why)
		}
	}
}

// The cluster owns these values: a different Config changes the pod and the check together.
func TestConfigValuesAreNotConstants(t *testing.T) {
	c := DefaultConfig()
	c.PriorityClass, c.NodeHostname, c.Namespace = "other-class", "node-b", "ns2"
	p, err := c.PodFor(goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.PriorityClassName != "other-class" || p.Spec.NodeSelector["kubernetes.io/hostname"] != "node-b" || p.Metadata.Namespace != "ns2" {
		t.Errorf("%+v", p.Spec)
	}
	if err := DefaultConfig().CheckContract(p); err == nil {
		t.Error("the default contract accepted another class, node and namespace")
	}
	empty := DefaultConfig()
	empty.PriorityClass = ""
	if _, err := empty.PodFor(goodSpec()); err == nil {
		t.Error("a pod with no priority class was built")
	}
}

func TestPodNameFitsAlways(t *testing.T) {
	if n := PodName("s"+strings.Repeat("z", 16), 9999); len(n) > 40 || !podNameRe.MatchString(n) {
		t.Errorf("%q", n)
	}
}

// B's journal (up to 20000 records of ~300 B, about 6 MB) lives in a volume of its own, apart from the
// 8Mi side-state that holds the ledger: sidecar only, disk-backed (it must survive a container restart),
// 16Mi, mounted at /run/ccsbx/journal and nowhere else.
func TestThePodHasASidecarOnlyJournalVolume(t *testing.T) {
	p := goodPod(t)
	var vol *Volume
	for i := range p.Spec.Volumes {
		if p.Spec.Volumes[i].Name == "journal" {
			vol = &p.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.EmptyDir == nil || vol.EmptyDir.SizeLimit != "16Mi" || vol.EmptyDir.Medium != "" {
		t.Fatalf("journal volume: %+v", vol)
	}
	for _, c := range append(append([]Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if m.Name == "journal" && (c.Name != "side" || m.MountPath != "/run/ccsbx/journal" || m.ReadOnly) {
				t.Errorf("%s mounts the journal at %s", c.Name, m.MountPath)
			}
		}
	}
	for _, v := range p.Spec.Volumes {
		if v.Name == "side-state" && v.EmptyDir.SizeLimit != "8Mi" {
			t.Errorf("side-state is %s: the ledger's 8Mi stays separate", v.EmptyDir.SizeLimit)
		}
	}
	if volumeOwner["journal"] != "side" {
		t.Error("the journal is not in the sidecar-only allow-list")
	}
}

// F3c: the sidecar checks the repository out into /work and the fence writes it, so they share a group
// (the pod's fsGroup, neither one's primary group); the sidecar keeps what it fetched in a volume of its own,
// as large as the worktree, that the fence never sees.
func TestTheSharedWorktreeGroupAndTheSnapshotVolume(t *testing.T) {
	p := goodPod(t)
	if g := p.Spec.SecurityContext.FSGroup; g == nil || *g != WorkGID || WorkGID == FenceUID || WorkGID == SideUID {
		t.Errorf("fsGroup %v", g)
	}
	var snap *Volume
	for i := range p.Spec.Volumes {
		if p.Spec.Volumes[i].Name == "snapshot" {
			snap = &p.Spec.Volumes[i]
		}
	}
	if snap == nil || snap.EmptyDir == nil || snap.EmptyDir.Medium != "" || snap.EmptyDir.SizeLimit != miQty(10240) {
		t.Fatalf("snapshot volume %+v (disk-backed, as large as the worktree)", snap)
	}
	for _, c := range append(append([]Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if m.Name == "snapshot" && (c.Name != "side" || m.MountPath != "/run/ccsbx/side/snapshot" || m.ReadOnly) {
				t.Errorf("%s mounts the snapshot at %s", c.Name, m.MountPath)
			}
		}
	}
	if volumeOwner["snapshot"] != "side" {
		t.Error("the snapshot is not sidecar-only")
	}
	// it follows the worktree's size
	s := goodSpec()
	s.WorktreeMB = 2048
	q, err := PodFor(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range q.Spec.Volumes {
		if v.Name == "snapshot" && v.EmptyDir.SizeLimit != "2Gi" {
			t.Errorf("snapshot %s for a 2 GiB worktree", v.EmptyDir.SizeLimit)
		}
	}
}
