/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Names and paths the AgentHost controller reserves in a host Pod.
const (
	// AgentHostContainerName is the template container that runs the host daemon.
	AgentHostContainerName = "host"
	// AgentHostStateVolumeName mounts the host's PersistentVolumeClaim.
	AgentHostStateVolumeName = "workspace"
	// AgentHostStatusPort is the port of the Orka host agent sidecar.
	AgentHostStatusPort int32 = 7790
)

// AgentHostAccess selects how clients reach a host.
// +kubebuilder:validation:Enum=Relay;Standalone
type AgentHostAccess string

const (
	// AgentHostAccessRelay means the host registers with an outbound relay the
	// client already knows, so the cluster needs no inbound path.
	AgentHostAccessRelay AgentHostAccess = "Relay"
	// AgentHostAccessStandalone means the client port-forwards to the loopback
	// listener and presents the host's connection token.
	AgentHostAccessStandalone AgentHostAccess = "Standalone"
)

// AgentHostCredentialPolicy says whether a template's host receives the user's
// GitHub token.
// +kubebuilder:validation:Enum=None;Optional;Required
type AgentHostCredentialPolicy string

const (
	AgentHostCredentialNone     AgentHostCredentialPolicy = "None"
	AgentHostCredentialOptional AgentHostCredentialPolicy = "Optional"
	AgentHostCredentialRequired AgentHostCredentialPolicy = "Required"
)

// AgentHostEnvVar is a plain environment variable. Values are stored in the
// template in clear text; never put credentials here.
type AgentHostEnvVar struct {
	// Name is the variable name. Names starting with ORKA_ are reserved.
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('ORKA_')",message="environment variables starting with ORKA_ are reserved"
	Name string `json:"name"`

	// Value is the variable value.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Value string `json:"value,omitempty"`
}

// AgentHostVolumeMount mounts the state volume or a declared scratch volume.
type AgentHostVolumeMount struct {
	// Name is workspace or the name of a template scratch volume.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// MountPath is the absolute path inside the container.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:MaxLength=1024
	MountPath string `json:"mountPath"`

	// ReadOnly mounts the volume read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// AgentHostContainer is the constrained container shape a template may declare.
// The controller adds the security context, so containers always run as the
// template's non-root user with no capabilities.
type AgentHostContainer struct {
	// Name is the container name. Names starting with orka- are reserved.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('orka-')",message="container names starting with orka- are reserved"
	Name string `json:"name"`

	// Image is a container image pinned by digest.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// Command overrides the image entrypoint.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Command []string `json:"command,omitempty"`

	// Args are the entrypoint arguments.
	// +kubebuilder:validation:MaxItems=128
	// +listType=atomic
	// +optional
	Args []string `json:"args,omitempty"`

	// WorkingDir is the container working directory.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// Env lists plain environment variables.
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []AgentHostEnvVar `json:"env,omitempty"`

	// Resources are the container's compute requests and limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// VolumeMounts mount the state volume and scratch volumes.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	VolumeMounts []AgentHostVolumeMount `json:"volumeMounts,omitempty"`
}

// AgentHostScratchVolume is a per-Pod emptyDir volume.
type AgentHostScratchVolume struct {
	// Name is the volume name. workspace and names starting with orka- are reserved.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self != 'workspace' && !self.startsWith('orka-')",message="workspace and names starting with orka- are reserved"
	Name string `json:"name"`

	// Medium selects node disk (empty) or memory.
	// +kubebuilder:validation:Enum="";Memory
	// +optional
	Medium corev1.StorageMedium `json:"medium,omitempty"`

	// SizeLimit bounds the volume.
	// +optional
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`
}

// AgentHostStorage sizes the host's state volume.
type AgentHostStorage struct {
	// Size is the requested capacity.
	Size resource.Quantity `json:"size"`

	// StorageClassName selects the storage class. Unset uses the cluster default.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// AgentHostEgressRule allows TCP egress to one CIDR in addition to DNS and
// public HTTPS.
type AgentHostEgressRule struct {
	// CIDR is the destination address range.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	CIDR string `json:"cidr"`

	// Ports are the allowed TCP destination ports.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	// +listType=set
	Ports []int32 `json:"ports"`
}

// AgentHostSecurityContext selects the non-root identity every container runs as.
type AgentHostSecurityContext struct {
	// RunAsUser is the uid of every template container.
	// +kubebuilder:validation:Minimum=1
	RunAsUser int64 `json:"runAsUser"`

	// RunAsGroup is the primary gid of every template container and the Pod fsGroup.
	// +kubebuilder:validation:Minimum=1
	RunAsGroup int64 `json:"runAsGroup"`
}

// AgentHostIdleSuspend suspends a host that has been idle for a while.
type AgentHostIdleSuspend struct {
	// After is how long the host must report idle before Orka suspends it,
	// from 1m to 720h.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('720h')",message="after must be a duration from 1m to 720h"
	After metav1.Duration `json:"after"`
}

// AgentHostTemplateSpec is the operator's definition of an agent host.
//
// The container named host runs the daemon. Orka adds to it, and promises
// nothing else:
//   - ORKA_HOST_ID: the AgentHost UID, stable for the host's life.
//   - ORKA_HOST_NAME: the AgentHost name, for display.
//   - ORKA_CONNECTION_TOKEN_FILE: a per-host random token in a memory-backed
//     volume mounted only into host. Read it once and delete it.
//   - ORKA_GITHUB_TOKEN_FILE: the user's GitHub token, read-only, when
//     githubToken allows it and the AgentHost names a token Secret.
//   - ORKA_STATUS_DIR: a directory shared with the Orka host agent. When
//     idleStatusFile is set, the host writes its idle status there as JSON
//     with at least idle (boolean) and idleSince (RFC 3339 or null), and
//     rewrites it at least every five minutes. Freshness is judged by
//     updatedAt (RFC 3339), or by the file's modification time without it.
//
// The daemon must serve the Agent Host Protocol on 127.0.0.1:listenPort, gated
// by the connection token.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="AgentHostTemplate spec is immutable; create a new template"
// +kubebuilder:validation:XValidation:rule="has(self.initContainers) == has(oldSelf.initContainers) && (!has(self.initContainers) || self.initContainers.map(c, c.name) == oldSelf.initContainers.map(c, c.name))",message="AgentHostTemplate spec is immutable; create a new template"
// +kubebuilder:validation:XValidation:rule="self.containers.all(c, oldSelf.containers.exists(o, o.name == c.name && has(c.env) == has(o.env) && (!has(c.env) || c.env.map(e, e.name) == o.env.map(e, e.name))))",message="AgentHostTemplate spec is immutable; create a new template"
// +kubebuilder:validation:XValidation:rule="!has(self.initContainers) || self.initContainers.all(c, has(oldSelf.initContainers) && oldSelf.initContainers.exists(o, o.name == c.name && has(c.env) == has(o.env) && (!has(c.env) || c.env.map(e, e.name) == o.env.map(e, e.name))))",message="AgentHostTemplate spec is immutable; create a new template"
// +kubebuilder:validation:XValidation:rule="self.containers.exists_one(c, c.name == 'host')",message="exactly one container must be named host"
// +kubebuilder:validation:XValidation:rule="self.containers.all(c, !has(c.volumeMounts) || c.volumeMounts.all(m, m.name == 'workspace' || (has(self.volumes) && self.volumes.exists(v, v.name == m.name))))",message="container volumeMounts must name workspace or a declared scratch volume"
// +kubebuilder:validation:XValidation:rule="!has(self.initContainers) || self.initContainers.all(c, !has(c.volumeMounts) || c.volumeMounts.all(m, m.name == 'workspace' || (has(self.volumes) && self.volumes.exists(v, v.name == m.name))))",message="init container volumeMounts must name workspace or a declared scratch volume"
// +kubebuilder:validation:XValidation:rule="!has(self.initContainers) || self.initContainers.all(i, self.containers.all(c, c.name != i.name))",message="init container and container names must be distinct"
// +kubebuilder:validation:XValidation:rule="!has(self.idleSuspend) || has(self.idleStatusFile)",message="idleSuspend requires idleStatusFile"
// +kubebuilder:validation:XValidation:rule="self.access != 'Relay' || self.githubToken == 'Required'",message="Relay access requires githubToken: Required"
type AgentHostTemplateSpec struct {
	// SecurityContext selects the non-root identity for every template container.
	SecurityContext AgentHostSecurityContext `json:"securityContext"`

	// InitContainers run in order before the containers, for example to copy
	// pinned vendor binaries into a scratch volume.
	// +kubebuilder:validation:MaxItems=4
	// +listType=map
	// +listMapKey=name
	// +optional
	InitContainers []AgentHostContainer `json:"initContainers,omitempty"`

	// Containers are the long-running containers. Exactly one is named host.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +listType=map
	// +listMapKey=name
	Containers []AgentHostContainer `json:"containers"`

	// Volumes declares per-Pod scratch volumes.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +optional
	Volumes []AgentHostScratchVolume `json:"volumes,omitempty"`

	// Storage sizes the state volume mounted as workspace.
	Storage AgentHostStorage `json:"storage"`

	// ListenPort is the loopback port the host serves the Agent Host Protocol on.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:validation:XValidation:rule="self != 7790",message="port 7790 is reserved for the Orka host agent"
	ListenPort int32 `json:"listenPort"`

	// Access selects how clients reach the host.
	Access AgentHostAccess `json:"access"`

	// GitHubToken says whether the host receives the user's GitHub token.
	// +kubebuilder:default=None
	// +optional
	GitHubToken AgentHostCredentialPolicy `json:"githubToken,omitempty"`

	// IdleStatusFile is the file name, inside ORKA_STATUS_DIR, where the host
	// writes its idle status.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]{1,128}$`
	// +kubebuilder:validation:XValidation:rule="self != '.' && self != '..'",message="idleStatusFile must be a plain file name"
	// +optional
	IdleStatusFile string `json:"idleStatusFile,omitempty"`

	// IdleSuspend suspends hosts that report idle for the given duration. Unset
	// keeps hosts running.
	// +optional
	IdleSuspend *AgentHostIdleSuspend `json:"idleSuspend,omitempty"`

	// RuntimeClassName selects a RuntimeClass, for example a gVisor or Kata
	// sandbox, for host Pods.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// AdditionalEgress allows TCP egress beyond DNS and public HTTPS.
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	// +optional
	AdditionalEgress []AgentHostEgressRule `json:"additionalEgress,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=orka,shortName=aht
// +kubebuilder:printcolumn:name="Access",type=string,JSONPath=`.spec.access`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentHostTemplate defines a long-lived, single-user agent host that
// AgentHosts in the same namespace may run. Selecting one requires the use
// verb on it.
type AgentHostTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentHostTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// AgentHostTemplateList contains a list of AgentHostTemplate.
type AgentHostTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentHostTemplate `json:"items"`
}

// AgentHostTemplateReference names an AgentHostTemplate in the AgentHost's namespace.
type AgentHostTemplateReference struct {
	// Name is the template name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AgentHostSecretKeyReference names one key of a Secret in the AgentHost's namespace.
type AgentHostSecretKeyReference struct {
	// Name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key is the data key holding the token.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// AgentHostSpec requests one host from a template.
// +kubebuilder:validation:XValidation:rule="self.templateRef == oldSelf.templateRef",message="templateRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.githubTokenSecretRef) || !has(self.githubTokenFrom)",message="githubTokenSecretRef and githubTokenFrom are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.githubTokenFrom) == has(oldSelf.githubTokenFrom) && (!has(self.githubTokenFrom) || self.githubTokenFrom == oldSelf.githubTokenFrom)",message="githubTokenFrom is immutable"
type AgentHostSpec struct {
	// TemplateRef selects an AgentHostTemplate in this namespace.
	TemplateRef AgentHostTemplateReference `json:"templateRef"`

	// Suspended stops the host's Pod and keeps its state volume and
	// credentials. Setting it back to false starts the host again.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// WakeRequestedAt wakes a host that Orka suspended for being idle when it is
	// later than status.idleSuspendedAt.
	// +optional
	WakeRequestedAt *metav1.Time `json:"wakeRequestedAt,omitempty"`

	// GitHubTokenSecretRef names the Secret holding the user's GitHub token,
	// for templates that use one. Orka copies it to the host and keeps the copy
	// current, so the host sees a rotated token.
	// +optional
	GitHubTokenSecretRef *AgentHostSecretKeyReference `json:"githubTokenSecretRef,omitempty"`

	// GitHubTokenFrom takes the user's GitHub token from their Connection to
	// an agent-host ConnectorProvider. Orka hands the host short-lived access
	// tokens from it and replaces them before they expire. Only the controller
	// sets it, when it creates the host for a Connection.
	// +optional
	GitHubTokenFrom *AgentHostTokenSource `json:"githubTokenFrom,omitempty"`
}

// AgentHostTokenSource names where a host's GitHub token comes from.
type AgentHostTokenSource struct {
	// ConnectionRef names the owner's Connection in this namespace.
	ConnectionRef LocalObjectReference `json:"connectionRef"`
}

// AgentHostGitHubAccount is the GitHub account a host's volume belongs to.
type AgentHostGitHubAccount struct {
	// ID is the account's numeric ID, which does not change when it is renamed.
	ID int64 `json:"id"`

	// Login is the account's login when the host was created.
	// +kubebuilder:validation:MaxLength=39
	Login string `json:"login"`
}

// AgentHostObjectReceipt records an object the controller created for a host.
// The nonce is recorded before creation and stamped on the object, so the
// controller adopts only objects it created.
type AgentHostObjectReceipt struct {
	// Kind is the object kind.
	Kind string `json:"kind"`

	// Namespace is the object's namespace. Empty means the host namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the object name.
	Name string `json:"name"`

	// Nonce is the creation nonce stamped on the object.
	Nonce string `json:"nonce"`

	// UID is the created object's UID, once observed.
	// +optional
	UID string `json:"uid,omitempty"`
}

// AgentHost phases.
const (
	AgentHostPhasePending      = "Pending"
	AgentHostPhaseProvisioning = "Provisioning"
	AgentHostPhaseReady        = "Ready"
	AgentHostPhaseSuspended    = "Suspended"
	AgentHostPhaseFailed       = "Failed"
	AgentHostPhaseDeleting     = "Deleting"
)

// AgentHostStatus reports the host's objects, lifecycle and readiness.
type AgentHostStatus struct {
	// ObservedGeneration is the most recent spec generation the controller observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase summarizes the host's lifecycle.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Suspended;Failed;Deleting
	// +optional
	Phase string `json:"phase,omitempty"`

	// HostNamespace is the namespace holding the host's objects, recorded at
	// first reconcile and used for cleanup.
	// +optional
	HostNamespace string `json:"hostNamespace,omitempty"`

	// PodName is the host's Pod while it runs.
	// +optional
	PodName string `json:"podName,omitempty"`

	// ConnectionSecretRef names the Secret in this namespace with the
	// connection details for Standalone access.
	// +optional
	ConnectionSecretRef *SecretReference `json:"connectionSecretRef,omitempty"`

	// Objects are receipts for the objects the controller created.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Objects []AgentHostObjectReceipt `json:"objects,omitempty"`

	// IdleSince is when the host last reported becoming idle.
	// +optional
	IdleSince *metav1.Time `json:"idleSince,omitempty"`

	// GitHub is the account a host fed by a Connection belongs to, pinned
	// from its first token. A token for another account fails the host.
	// +optional
	GitHub *AgentHostGitHubAccount `json:"github,omitempty"`

	// ObservedConnectionUID identifies the Connection whose grant the
	// controller last delivered to this host. It fences a recreated link.
	// +optional
	ObservedConnectionUID string `json:"observedConnectionUID,omitempty"`

	// ObservedGrantSequence is the consent grant last delivered from that
	// Connection. It contains no credential material.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ObservedGrantSequence int64 `json:"observedGrantSequence,omitempty"`

	// IdleSuspendedAt is when Orka suspended the host for being idle.
	// +optional
	IdleSuspendedAt *metav1.Time `json:"idleSuspendedAt,omitempty"`

	// Restarts counts consecutive replacements of a failed host Pod since it
	// was last ready. Replacements back off exponentially.
	// +optional
	Restarts int32 `json:"restarts,omitempty"`

	// LastRestartTime is when the controller last replaced a failed host Pod.
	// +optional
	LastRestartTime *metav1.Time `json:"lastRestartTime,omitempty"`

	// Conditions report readiness and problems.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orka,shortName=ah
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.templateRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentHost runs one long-lived, single-user agent host, such as an Agent Host
// Protocol daemon, from an AgentHostTemplate on Orka-owned Pods.
type AgentHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentHostSpec `json:"spec"`
	// +optional
	Status AgentHostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentHostList contains a list of AgentHost.
type AgentHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentHost `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentHostTemplate{}, &AgentHostTemplateList{}, &AgentHost{}, &AgentHostList{})
}
