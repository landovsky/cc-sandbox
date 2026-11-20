// types.go is the subset of the v1 Pod JSON that ccsbx writes and reads. It is our own typed
// subset, not client-go's (no third-party modules), and it has only the fields the contract names:
// a field that is not here cannot be set by mistake, and CheckContract can say "nothing else".
package k8s

// Pod is a v1 Pod (the fields we use).
type Pod struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
	Status     *PodStatus `json:"status,omitempty"`
}

type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Finalizers  []string          `json:"finalizers,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// DeletionTimestamp is set by the server on a pod being deleted; it is only read.
	DeletionTimestamp *string `json:"deletionTimestamp,omitempty"`
	UID               string  `json:"uid,omitempty"`
}

type PodSpec struct {
	NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
	Tolerations                   []Toleration      `json:"tolerations,omitempty"`
	PriorityClassName             string            `json:"priorityClassName,omitempty"`
	RuntimeClassName              *string           `json:"runtimeClassName,omitempty"`
	RestartPolicy                 string            `json:"restartPolicy,omitempty"`
	TerminationGracePeriodSeconds *int64            `json:"terminationGracePeriodSeconds,omitempty"`
	AutomountServiceAccountToken  *bool             `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks            *bool             `json:"enableServiceLinks,omitempty"`
	ShareProcessNamespace         *bool             `json:"shareProcessNamespace,omitempty"`
	HostNetwork                   bool              `json:"hostNetwork,omitempty"`
	HostPID                       bool              `json:"hostPID,omitempty"`
	HostIPC                       bool              `json:"hostIPC,omitempty"`
	HostUsers                     *bool             `json:"hostUsers,omitempty"`
	DNSPolicy                     string            `json:"dnsPolicy,omitempty"`
	DNSConfig                     *DNSConfig        `json:"dnsConfig,omitempty"`
	SecurityContext               *PodSecurity      `json:"securityContext,omitempty"`
	InitContainers                []Container       `json:"initContainers,omitempty"`
	Containers                    []Container       `json:"containers"`
	Volumes                       []Volume          `json:"volumes,omitempty"`
}

type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
	// TolerationSeconds is only ever set by the server, on its default NoExecute tolerations.
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

type DNSConfig struct {
	Nameservers []string `json:"nameservers,omitempty"`
}

type PodSecurity struct {
	RunAsNonRoot   *bool    `json:"runAsNonRoot,omitempty"`
	SeccompProfile *Seccomp `json:"seccompProfile,omitempty"`
	// FSGroup makes every volume of the pod group-owned by this gid (with the setgid bit on its directories),
	// and adds it to every container's groups: how the fence (10001) and the sidecar (10002) share /work.
	FSGroup *int64 `json:"fsGroup,omitempty"`
}

type Seccomp struct {
	Type string `json:"type"`
}

type Container struct {
	Name            string        `json:"name"`
	Image           string        `json:"image"`
	Command         []string      `json:"command,omitempty"`
	Args            []string      `json:"args,omitempty"`
	RestartPolicy   string        `json:"restartPolicy,omitempty"` // "Always" on an init container makes it a native sidecar
	Ports           []Port        `json:"ports,omitempty"`
	Env             []EnvVar      `json:"env,omitempty"`
	Resources       Resources     `json:"resources"`
	SecurityContext *ContainerSec `json:"securityContext,omitempty"`
	VolumeMounts    []VolumeMount `json:"volumeMounts,omitempty"`
	TTY             bool          `json:"tty,omitempty"`
	Stdin           bool          `json:"stdin,omitempty"`
}

type Port struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"` // literal values only: no valueFrom, so no Secret reference can be written
}

type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type ContainerSec struct {
	RunAsUser                *int64        `json:"runAsUser,omitempty"`
	RunAsGroup               *int64        `json:"runAsGroup,omitempty"`
	RunAsNonRoot             *bool         `json:"runAsNonRoot,omitempty"`
	ReadOnlyRootFilesystem   *bool         `json:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool         `json:"allowPrivilegeEscalation,omitempty"`
	Privileged               *bool         `json:"privileged,omitempty"`
	Capabilities             *Capabilities `json:"capabilities,omitempty"`
}

type Capabilities struct {
	Drop []string `json:"drop,omitempty"`
	Add  []string `json:"add,omitempty"`
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Volume is an emptyDir and nothing else: there is no field for a Secret, a ConfigMap, a hostPath
// or a claim, so the pod cannot name one.
type Volume struct {
	Name     string    `json:"name"`
	EmptyDir *EmptyDir `json:"emptyDir"`
}

type EmptyDir struct {
	Medium    string `json:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type PodStatus struct {
	Phase             string            `json:"phase,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	Message           string            `json:"message,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

type ContainerStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	State struct {
		Running    *struct{} `json:"running,omitempty"`
		Terminated *struct {
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason,omitempty"`
		} `json:"terminated,omitempty"`
	} `json:"state"`
}
