/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/k8scaps"
)

const (
	LabelHostUID       = "orka.ai/agent-host-uid"
	LabelHostName      = "orka.ai/agent-host-name"
	LabelHostNamespace = "orka.ai/agent-host-namespace"
	LabelComponent     = "app.kubernetes.io/component"
	ComponentValue     = "agent-host"

	AnnotationNonce       = "orka.ai/agent-host-nonce"
	AnnotationTemplateUID = "orka.ai/agent-host-template-uid"
	AnnotationHost        = "orka.ai/agent-host"
	AnnotationGitHubToken = "orka.ai/agent-host-github-token"
)

const (
	HostIDEnv              = "ORKA_HOST_ID"
	HostNameEnv            = "ORKA_HOST_NAME"
	ConnectionTokenFileEnv = "ORKA_CONNECTION_TOKEN_FILE"
	GitHubTokenFileEnv     = "ORKA_GITHUB_TOKEN_FILE"
	StatusDirEnv           = "ORKA_STATUS_DIR"
	HostAgentEnv           = "ORKA_HOST_AGENT"
)

const (
	TokenKey               = "token"
	ConnectionNamespaceKey = "namespace"
	ConnectionPodKey       = "pod"
	ConnectionPortKey      = "port"
)

const (
	hostAgentContainer = "orka-host-agent"
	stageContainerName = "orka-stage"
	hostAgentBinary    = "/orka-host-agent"
	statusPortName     = "orka-status"

	connectionTokenVolume       = "orka-connection-token"
	connectionTokenSecretVolume = "orka-connection-token-secret"
	githubTokenVolume           = "orka-github-token"
	statusVolume                = "orka-status"
	hostAgentBinVolume          = "orka-host-agent-bin"

	connectionTokenDir       = "/run/orka/connection-token"
	connectionTokenSecretDir = "/var/run/orka/connection-token-secret"
	githubTokenDir           = "/var/run/orka/github"
	statusDir                = "/var/run/orka/status"
	hostAgentBinDir          = "/run/orka/bin"

	secretFileMode int32 = 0o440
)

const maxBaseLength = 40

const (
	maxNameLength    = 253
	connectionSuffix = "-connection"
)

type Names struct {
	Namespace             string
	Base                  string
	Pod                   string
	PVC                   string
	NetworkPolicy         string
	ConnectionTokenSecret string
	GitHubTokenSecret     string
	Connection            string
}

func ObjectNames(host *corev1alpha1.AgentHost, hostNamespace string) Names {
	sum := sha256.Sum256([]byte(host.UID))
	hash := hex.EncodeToString(sum[:6])
	prefix := strings.TrimRight(truncate(host.Name, maxBaseLength), "-.")
	base := prefix + "-" + hash
	connection := host.Name + connectionSuffix
	if len(connection) > maxNameLength {
		keep := maxNameLength - len(connectionSuffix) - len(hash) - 1
		connection = strings.TrimRight(truncate(host.Name, keep), "-.") + "-" + hash + connectionSuffix
	}
	return Names{
		Namespace:             hostNamespace,
		Base:                  base,
		Pod:                   base,
		PVC:                   base,
		NetworkPolicy:         base,
		ConnectionTokenSecret: base + "-connection-token",
		GitHubTokenSecret:     base + "-github-token",
		Connection:            connection,
	}
}

func OwnerLabels(host *corev1alpha1.AgentHost) map[string]string {
	return map[string]string{
		LabelComponent:     ComponentValue,
		LabelHostUID:       string(host.UID),
		LabelHostName:      strings.TrimRight(truncate(host.Name, 63), "-."),
		LabelHostNamespace: host.Namespace,
	}
}

type ControllerPeer struct {
	Namespace string
	PodLabels map[string]string
}

type PodOptions struct {
	AgentImage          string
	GitHubTokenSource   string
	ClusterCapabilities k8scaps.Capabilities
}

func meta(host *corev1alpha1.AgentHost, n Names, name, nonce string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   n.Namespace,
		Labels:      OwnerLabels(host),
		Annotations: map[string]string{AnnotationNonce: nonce, AnnotationHost: host.Namespace + "/" + host.Name},
	}
}

func ConnectionTokenSecret(host *corev1alpha1.AgentHost, n Names, nonce string, token []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: meta(host, n, n.ConnectionTokenSecret, nonce),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{TokenKey: append([]byte(nil), token...)},
	}
}

func GitHubTokenSecret(host *corev1alpha1.AgentHost, n Names, nonce string, token []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: meta(host, n, n.GitHubTokenSecret, nonce),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{TokenKey: append([]byte(nil), token...)},
	}
}

func StateVolumeClaim(
	host *corev1alpha1.AgentHost,
	n Names,
	nonce string,
	template *corev1alpha1.AgentHostTemplate,
) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: meta(host, n, n.PVC, nonce),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: template.Spec.Storage.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: template.Spec.Storage.Size.DeepCopy()},
			},
		},
	}
	claim.Annotations[AnnotationTemplateUID] = string(template.UID)
	return claim
}

func HostNetworkPolicy(
	host *corev1alpha1.AgentHost,
	n Names,
	nonce string,
	template *corev1alpha1.AgentHostTemplate,
	controller ControllerPeer,
) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: meta(host, n, n.NetworkPolicy, nonce),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelHostUID: string(host.UID)}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": controller.Namespace,
					}},
					PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(controller.PodLabels)},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &tcp, Port: new(intstr.FromInt32(corev1alpha1.AgentHostStatusPort)),
				}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
								"kubernetes.io/metadata.name": "kube-system",
							}},
							PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
						},
						{IPBlock: &networkingv1.IPBlock{CIDR: "169.254.0.0/16"}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: new(intstr.FromInt32(53))},
						{Protocol: &tcp, Port: new(intstr.FromInt32(53))},
					},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{
							"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8",
						}}},
						{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fc00::/7", "fe80::/10", "::1/128"}}},
					},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: new(intstr.FromInt32(443))}},
				},
			},
		},
	}
	for _, rule := range template.Spec.AdditionalEgress {
		egress := networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: rule.CIDR}}},
		}
		for _, port := range rule.Ports {
			egress.Ports = append(egress.Ports, networkingv1.NetworkPolicyPort{
				Protocol: &tcp, Port: new(intstr.FromInt32(port)),
			})
		}
		policy.Spec.Egress = append(policy.Spec.Egress, egress)
	}
	return policy
}

func HostPod(
	host *corev1alpha1.AgentHost,
	n Names,
	nonce string,
	template *corev1alpha1.AgentHostTemplate,
	opts PodOptions,
) *corev1.Pod {
	spec := &template.Spec
	initContainers := make([]corev1.Container, 0, 2+len(spec.InitContainers))
	initContainers = append(initContainers, stageContainer(spec, opts.AgentImage))
	for i := range spec.InitContainers {
		initContainers = append(initContainers, templateContainer(&spec.InitContainers[i], spec))
	}
	initContainers = append(initContainers, hostAgentSidecar(spec, opts.AgentImage))

	containers := make([]corev1.Container, 0, len(spec.Containers))
	for i := range spec.Containers {
		container := templateContainer(&spec.Containers[i], spec)
		if container.Name == corev1alpha1.AgentHostContainerName {
			decorateHostContainer(&container, host, opts)
		}
		containers = append(containers, container)
	}

	volumes := []corev1.Volume{
		{Name: corev1alpha1.AgentHostStateVolumeName, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: n.PVC},
		}},
		{Name: connectionTokenSecretVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: n.ConnectionTokenSecret, DefaultMode: new(secretFileMode)},
		}},
		{Name: connectionTokenVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resource.MustParse("1Mi"))},
		}},
		{Name: statusVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resource.MustParse("1Mi"))},
		}},
		{Name: hostAgentBinVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("64Mi"))},
		}},
	}
	if opts.GitHubTokenSource != "" {
		volumes = append(volumes, corev1.Volume{Name: githubTokenVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: n.GitHubTokenSecret, DefaultMode: new(secretFileMode)},
		}})
	}
	for _, volume := range spec.Volumes {
		emptyDir := &corev1.EmptyDirVolumeSource{Medium: volume.Medium}
		if volume.SizeLimit != nil {
			emptyDir.SizeLimit = new(volume.SizeLimit.DeepCopy())
		}
		volumes = append(volumes, corev1.Volume{Name: volume.Name, VolumeSource: corev1.VolumeSource{EmptyDir: emptyDir}})
	}

	podMeta := meta(host, n, n.Pod, nonce)
	podMeta.Annotations[AnnotationTemplateUID] = string(template.UID)
	podMeta.Annotations[AnnotationGitHubToken] = opts.GitHubTokenSource
	podSecurity := &corev1.PodSecurityContext{
		RunAsNonRoot:   new(true),
		RunAsUser:      new(spec.SecurityContext.RunAsUser),
		RunAsGroup:     new(spec.SecurityContext.RunAsGroup),
		FSGroup:        new(spec.SecurityContext.RunAsGroup),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if opts.ClusterCapabilities.SupplementalGroupsStrict {
		podSecurity.SupplementalGroupsPolicy = new(corev1.SupplementalGroupsPolicyStrict)
	}
	podSpec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: new(false),
		EnableServiceLinks:           new(false),
		RuntimeClassName:             spec.RuntimeClassName,
		SecurityContext:              podSecurity,
		InitContainers:               initContainers,
		Containers:                   containers,
		Volumes:                      volumes,
	}
	if opts.ClusterCapabilities.PodUserNamespaces {
		podSpec.HostUsers = new(false)
	}
	if opts.ClusterCapabilities.InPlacePodResize {
		for i := range podSpec.Containers {
			podSpec.Containers[i].ResizePolicy = []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
			}
		}
	}
	if opts.ClusterCapabilities.PodLevelResources {
		podSpec.Resources = aggregatePodLevelResources(podSpec.InitContainers, podSpec.Containers)
	}
	return &corev1.Pod{
		ObjectMeta: podMeta,
		Spec:       podSpec,
	}
}

var podLevelResourceNames = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

type podResourceAccumulator struct {
	requests       corev1.ResourceList
	limits         corev1.ResourceList
	requestBounded map[corev1.ResourceName]bool
	limitBounded   map[corev1.ResourceName]bool
}

func newPodResourceAccumulator(hasContainers bool) *podResourceAccumulator {
	return &podResourceAccumulator{
		requests: corev1.ResourceList{},
		limits:   corev1.ResourceList{},
		requestBounded: map[corev1.ResourceName]bool{
			corev1.ResourceCPU:    hasContainers,
			corev1.ResourceMemory: hasContainers,
		},
		limitBounded: map[corev1.ResourceName]bool{
			corev1.ResourceCPU:    hasContainers,
			corev1.ResourceMemory: hasContainers,
		},
	}
}

func (a *podResourceAccumulator) addRunning(res corev1.ResourceRequirements) {
	for _, name := range podLevelResourceNames {
		if q, ok := effectiveContainerRequest(res, name); !ok {
			a.requestBounded[name] = false
		} else if !q.IsZero() {
			cur := a.requests[name]
			cur.Add(q)
			a.requests[name] = cur
		}
		if q, ok := res.Limits[name]; !ok || q.IsZero() {
			a.limitBounded[name] = false
		} else {
			cur := a.limits[name]
			cur.Add(q)
			a.limits[name] = cur
		}
	}
}

func (a *podResourceAccumulator) applyInit(res corev1.ResourceRequirements) {
	for _, name := range podLevelResourceNames {
		if q, ok := effectiveContainerRequest(res, name); !ok {
			a.requestBounded[name] = false
		} else if q.Cmp(a.requests[name]) > 0 {
			a.requests[name] = q.DeepCopy()
		}
		if q, ok := res.Limits[name]; !ok || q.IsZero() {
			a.limitBounded[name] = false
		} else if q.Cmp(a.limits[name]) > 0 {
			a.limits[name] = q.DeepCopy()
		}
	}
}

func effectiveContainerRequest(res corev1.ResourceRequirements, name corev1.ResourceName) (resource.Quantity, bool) {
	if q, ok := res.Requests[name]; ok {
		return q, true
	}
	q, ok := res.Limits[name]
	return q, ok
}

func isRestartableInitContainer(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

func aggregatePodLevelResources(initContainers, containers []corev1.Container) *corev1.ResourceRequirements {
	acc := newPodResourceAccumulator(len(containers) > 0)
	for i := range containers {
		acc.addRunning(containers[i].Resources)
	}
	for i := range initContainers {
		if isRestartableInitContainer(initContainers[i]) {
			acc.addRunning(initContainers[i].Resources)
		}
	}
	for i := range initContainers {
		if !isRestartableInitContainer(initContainers[i]) {
			acc.applyInit(initContainers[i].Resources)
		}
	}
	for _, name := range podLevelResourceNames {
		if !acc.requestBounded[name] {
			delete(acc.requests, name)
		}
		if !acc.limitBounded[name] {
			delete(acc.limits, name)
			continue
		}
		lim := acc.limits[name]
		if req, hasReq := acc.requests[name]; hasReq && lim.Cmp(req) < 0 {
			delete(acc.limits, name)
		}
	}
	if len(acc.requests) == 0 && len(acc.limits) == 0 {
		return nil
	}
	out := &corev1.ResourceRequirements{}
	if len(acc.requests) > 0 {
		out.Requests = acc.requests
	}
	if len(acc.limits) > 0 {
		out.Limits = acc.limits
	}
	return out
}

func ConnectionSecret(
	host *corev1alpha1.AgentHost,
	n Names,
	nonce string,
	template *corev1alpha1.AgentHostTemplate,
	token []byte,
) *corev1.Secret {
	objectMeta := meta(host, n, n.Connection, nonce)
	objectMeta.Namespace = host.Namespace
	return &corev1.Secret{
		ObjectMeta: objectMeta,
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TokenKey:               append([]byte(nil), token...),
			ConnectionNamespaceKey: []byte(n.Namespace),
			ConnectionPodKey:       []byte(n.Pod),
			ConnectionPortKey:      fmt.Appendf(nil, "%d", template.Spec.ListenPort),
		},
	}
}

func hardenedSecurity(spec *corev1alpha1.AgentHostTemplateSpec) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		RunAsUser:                new(spec.SecurityContext.RunAsUser),
		RunAsGroup:               new(spec.SecurityContext.RunAsGroup),
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Privileged:               new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func templateContainer(source *corev1alpha1.AgentHostContainer, spec *corev1alpha1.AgentHostTemplateSpec) corev1.Container {
	container := corev1.Container{
		Name:            source.Name,
		Image:           source.Image,
		Command:         append([]string(nil), source.Command...),
		Args:            append([]string(nil), source.Args...),
		WorkingDir:      source.WorkingDir,
		Resources:       *source.Resources.DeepCopy(),
		SecurityContext: hardenedSecurity(spec),
		ImagePullPolicy: corev1.PullIfNotPresent,
	}
	for _, env := range source.Env {
		container.Env = append(container.Env, corev1.EnvVar{Name: env.Name, Value: env.Value})
	}
	for _, mount := range source.VolumeMounts {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: mount.Name, MountPath: mount.MountPath, ReadOnly: mount.ReadOnly,
		})
	}
	return container
}

func stageContainer(spec *corev1alpha1.AgentHostTemplateSpec, image string) corev1.Container {
	return corev1.Container{
		Name:    stageContainerName,
		Image:   image,
		Command: []string{hostAgentBinary},
		Args: []string{
			"stage",
			"--token-from=" + connectionTokenSecretDir + "/" + TokenKey,
			"--token-to=" + connectionTokenDir + "/" + TokenKey,
			"--agent-to=" + hostAgentBinDir + hostAgentBinary,
		},
		SecurityContext: hardenedSecurity(spec),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources:       smallResources("100m", "64Mi"),
		VolumeMounts: []corev1.VolumeMount{
			{Name: connectionTokenSecretVolume, MountPath: connectionTokenSecretDir, ReadOnly: true},
			{Name: connectionTokenVolume, MountPath: connectionTokenDir},
			{Name: hostAgentBinVolume, MountPath: hostAgentBinDir},
		},
	}
}

func PodAgentImage(pod *corev1.Pod) string {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == hostAgentContainer {
			return pod.Spec.InitContainers[i].Image
		}
	}
	return ""
}

func hostAgentSidecar(spec *corev1alpha1.AgentHostTemplateSpec, image string) corev1.Container {
	args := []string{
		fmt.Sprintf("--address=:%d", corev1alpha1.AgentHostStatusPort),
		fmt.Sprintf("--listen-port=%d", spec.ListenPort),
		"--token-file=" + connectionTokenSecretDir + "/" + TokenKey,
	}
	if spec.IdleStatusFile != "" {
		args = append(args, "--status-file="+statusDir+"/"+spec.IdleStatusFile)
	}
	return corev1.Container{
		Name:            hostAgentContainer,
		Image:           image,
		Command:         []string{hostAgentBinary},
		Args:            args,
		RestartPolicy:   new(corev1.ContainerRestartPolicyAlways),
		SecurityContext: hardenedSecurity(spec),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources:       smallResources("200m", "64Mi"),
		Ports: []corev1.ContainerPort{{
			Name: statusPortName, ContainerPort: corev1alpha1.AgentHostStatusPort, Protocol: corev1.ProtocolTCP,
		}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: connectionTokenSecretVolume, MountPath: connectionTokenSecretDir, ReadOnly: true},
			{Name: statusVolume, MountPath: statusDir, ReadOnly: true},
		},
	}
}

func decorateHostContainer(container *corev1.Container, host *corev1alpha1.AgentHost, opts PodOptions) {
	container.Env = append(container.Env,
		corev1.EnvVar{Name: HostIDEnv, Value: string(host.UID)},
		corev1.EnvVar{Name: HostNameEnv, Value: host.Name},
		corev1.EnvVar{Name: ConnectionTokenFileEnv, Value: connectionTokenDir + "/" + TokenKey},
		corev1.EnvVar{Name: StatusDirEnv, Value: statusDir},
		corev1.EnvVar{Name: HostAgentEnv, Value: hostAgentBinDir + hostAgentBinary},
	)
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: connectionTokenVolume, MountPath: connectionTokenDir},
		corev1.VolumeMount{Name: statusVolume, MountPath: statusDir},
		corev1.VolumeMount{Name: hostAgentBinVolume, MountPath: hostAgentBinDir, ReadOnly: true},
	)
	if opts.GitHubTokenSource != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: GitHubTokenFileEnv, Value: githubTokenDir + "/" + TokenKey})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: githubTokenVolume, MountPath: githubTokenDir, ReadOnly: true,
		})
	}
	statusPort := intstr.FromInt32(corev1alpha1.AgentHostStatusPort)
	container.StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/startupz", Port: statusPort}},
		PeriodSeconds:    5,
		TimeoutSeconds:   10,
		FailureThreshold: 120,
	}
	container.ReadinessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: statusPort}},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	container.LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: statusPort}},
		PeriodSeconds:    20,
		TimeoutSeconds:   5,
		FailureThreshold: 6,
	}
}

func smallResources(cpuLimit, memoryLimit string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpuLimit), corev1.ResourceMemory: resource.MustParse(memoryLimit),
		},
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
