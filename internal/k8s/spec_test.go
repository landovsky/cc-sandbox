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
	return Spec{Sid: "s0abcdefgh", Gen: 2, Label: "cc-sandbox:x", OwnerUID: 1000, FenceImage: fenceImg, SideImage: sideImg,
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
	if q := p.Spec.Containers[0].Resources; q.Limits["memory"] != "4096Mi" || q.Requests["cpu"] != "2" || q.Limits["ephemeral-storage"] != "20480Mi" {
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

// Every rule of the contract, damaged one at a time.
func TestCheckContractRefusesEachViolation(t *testing.T) {
	c := DefaultConfig()
	f := func(p *Pod) *Container { return &p.Spec.Containers[0] }
	sd := func(p *Pod) *Container { return &p.Spec.InitContainers[0] }
	cases := []struct {
		name string
		mod  func(p *Pod)
		want string
	}{
		{"name without the ccsbx-s- prefix", func(p *Pod) { p.Metadata.Name = "mypod" }, "ccsbx-s-"},
		{"a name over 63 characters", func(p *Pod) { p.Metadata.Name = "ccsbx-s-" + strings.Repeat("a", 60) }, "63"},
		{"another namespace", func(p *Pod) { p.Metadata.Namespace = "kube-system" }, "namespace"},
		{"role door", func(p *Pod) { p.Metadata.Labels["cc-sandbox/role"] = "door" }, "session"},
		{"a finalizer", func(p *Pod) { p.Metadata.Finalizers = []string{"x/y"} }, "finalizers"},
		{"another node", func(p *Pod) { p.Spec.NodeSelector["kubernetes.io/hostname"] = "hp-ubuntu" }, "nodeSelector"},
		{"an extra node selector", func(p *Pod) { p.Spec.NodeSelector["a"] = "b" }, "nodeSelector"},
		{"no toleration", func(p *Pod) { p.Spec.Tolerations = nil }, "tolerations"},
		{"a toleration of everything", func(p *Pod) { p.Spec.Tolerations = append(p.Spec.Tolerations, Toleration{Operator: "Exists"}) }, "tolerations"},
		{"no priority class", func(p *Pod) { p.Spec.PriorityClassName = "" }, "priorityClassName"},
		{"another priority class", func(p *Pod) { p.Spec.PriorityClassName = "system-node-critical" }, "priorityClassName"},
		{"a runtime class", func(p *Pod) { p.Spec.RuntimeClassName = ptr("gvisor") }, "runtimeClassName"},
		{"a grace period of 120 s", func(p *Pod) { p.Spec.TerminationGracePeriodSeconds = ptr(int64(120)) }, "terminationGracePeriodSeconds"},
		{"the service account token", func(p *Pod) { p.Spec.AutomountServiceAccountToken = ptr(true) }, "automountServiceAccountToken"},
		{"an unset service account token flag", func(p *Pod) { p.Spec.AutomountServiceAccountToken = nil }, "automountServiceAccountToken"},
		{"a shared process namespace", func(p *Pod) { p.Spec.ShareProcessNamespace = ptr(true) }, "shareProcessNamespace"},
		{"host network", func(p *Pod) { p.Spec.HostNetwork = true }, "host namespaces"},
		{"host pid", func(p *Pod) { p.Spec.HostPID = true }, "host namespaces"},
		{"hostUsers true", func(p *Pod) { p.Spec.HostUsers = ptr(true) }, "hostUsers"},
		{"cluster DNS", func(p *Pod) { p.Spec.DNSPolicy = "ClusterFirst" }, "dnsPolicy"},
		{"another nameserver", func(p *Pod) { p.Spec.DNSConfig.Nameservers = []string{"8.8.8.8"} }, "dnsPolicy"},
		{"no seccomp", func(p *Pod) { p.Spec.SecurityContext.SeccompProfile = nil }, "seccomp"},
		{"an unconfined seccomp", func(p *Pod) { p.Spec.SecurityContext.SeccompProfile.Type = "Unconfined" }, "seccomp"},
		{"a second container", func(p *Pod) { p.Spec.Containers = append(p.Spec.Containers, Container{Name: "x"}) }, "exactly one container"},
		{"another container name", func(p *Pod) { f(p).Name = "app" }, "exactly one container"},
		{"a second init container", func(p *Pod) { p.Spec.InitContainers = append(p.Spec.InitContainers, Container{Name: "init2"}) }, "init container"},
		{"a sidecar that is a plain init container", func(p *Pod) { sd(p).RestartPolicy = "" }, "init container"},
		{"a volume that is not sized", func(p *Pod) { p.Spec.Volumes[0].EmptyDir.SizeLimit = "" }, "sizeLimit"},
		{"a volume of 41Gi", func(p *Pod) { p.Spec.Volumes[0].EmptyDir.SizeLimit = "41Gi" }, "sizeLimit"},
		{"a Memory volume of 41Gi", func(p *Pod) { p.Spec.Volumes[5].EmptyDir.SizeLimit = "41Gi" }, "sizeLimit"},
		{"a volume that is not an emptyDir", func(p *Pod) { p.Spec.Volumes[1].EmptyDir = nil }, "emptyDir"},
		{"no worktree volume", func(p *Pod) { p.Spec.Volumes[0].Name = "wt" }, "worktree"},
		{"a fence image by tag", func(p *Pod) { f(p).Image = "ghcr.io/landovsky/ccsbx-fence:latest" }, "digest"},
		{"a sidecar image from another repo", func(p *Pod) { sd(p).Image = "quay.io/x/y@sha256:" + strings.Repeat("e", 64) }, "digest"},
		{"fence as root", func(p *Pod) { f(p).SecurityContext.RunAsUser = ptr(int64(0)) }, "10001"},
		{"fence as the sidecar's uid", func(p *Pod) { f(p).SecurityContext.RunAsUser = ptr(int64(10002)) }, "10001"},
		{"sidecar as the fence's uid", func(p *Pod) { sd(p).SecurityContext.RunAsUser = ptr(int64(10001)) }, "10002"},
		{"a writable root filesystem", func(p *Pod) { f(p).SecurityContext.ReadOnlyRootFilesystem = ptr(false) }, "readOnlyRootFilesystem"},
		{"privilege escalation allowed", func(p *Pod) { f(p).SecurityContext.AllowPrivilegeEscalation = ptr(true) }, "privilege escalation"},
		{"privileged", func(p *Pod) { f(p).SecurityContext.Privileged = ptr(true) }, "privileged"},
		{"a capability added", func(p *Pod) { f(p).SecurityContext.Capabilities.Add = []string{"NET_ADMIN"} }, "capabilities"},
		{"capabilities not dropped", func(p *Pod) { f(p).SecurityContext.Capabilities.Drop = []string{"NET_RAW"} }, "capabilities"},
		{"no security context", func(p *Pod) { f(p).SecurityContext = nil }, "securityContext"},
		{"memory over 4Gi", func(p *Pod) { f(p).Resources.Limits["memory"] = "5Gi"; f(p).Resources.Requests["memory"] = "5Gi" }, "memory"},
		{"cpu over 4", func(p *Pod) { f(p).Resources.Limits["cpu"] = "8"; f(p).Resources.Requests["cpu"] = "8" }, "cpu"},
		{"ephemeral storage over 40Gi", func(p *Pod) {
			f(p).Resources.Limits["ephemeral-storage"] = "41Gi"
			f(p).Resources.Requests["ephemeral-storage"] = "41Gi"
		}, "ephemeral-storage"},
		{"requests below limits (not Guaranteed)", func(p *Pod) { f(p).Resources.Requests["memory"] = "1Gi" }, "Guaranteed"},
		{"no limit at all", func(p *Pod) { delete(f(p).Resources.Limits, "cpu"); delete(f(p).Resources.Requests, "cpu") }, "Guaranteed"},
		{"the sidecar over its limits", func(p *Pod) { sd(p).Resources.Limits["memory"] = "9Gi"; sd(p).Resources.Requests["memory"] = "9Gi" }, "memory"},
		{"the fence mounting the sidecar's state", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "side-state", MountPath: "/s"})
		}, "sidecar's alone"},
		{"the fence mounting the relay credential", func(p *Pod) {
			f(p).VolumeMounts = append(f(p).VolumeMounts, VolumeMount{Name: "relay-cred", MountPath: "/r"})
		}, "sidecar's alone"},
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
	if err := c.CheckContract(goodPod(t)); err != nil {
		t.Fatalf("the good pod was refused: %v", err)
	}
	for _, tc := range cases {
		p := goodPod(t)
		tc.mod(p)
		err := c.CheckContract(p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want a refusal mentioning %q)", tc.name, err, tc.want)
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
