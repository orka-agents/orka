package applicationexec

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

type jobOptions struct {
	role               string
	mode               string
	image              string
	taskUID            string
	serviceAccount     string
	token              bool
	inClusterToken     bool
	source             bool
	artifacts          bool
	artifactReadOnly   bool
	marker             bool
	markerPort         int
	target             string
	deadline           int64
	hostAliases        []corev1.HostAlias
	actors             []pv.ApplicationActorIdentity
	actorSecret        string
	receiverPort       int
	markerInput        bool
	fixtureProfile     *pv.ApplicationFixtureProfile
	fixtureConfig      string
	fixtureConfigItems []corev1.KeyToPath
	subjectTLS         []pv.ApplicationSubjectTLSIdentity
	fixtureSecrets     []pv.ApplicationFixtureSecretIdentity
	fileSizeRole       string
}

func jobFileSizeRole(options jobOptions) string {
	if options.mode == workerModePrepare || options.mode == workerModeBuild {
		return options.mode
	}
	if options.mode == workerModeBoundary && options.role == rolePrepare && options.fileSizeRole == workerModePrepare {
		return workerModePrepare
	}
	return "execute"
}

func containerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)),
		RunAsNonRoot: new(true), AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
}

func containerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")}}
}

func roleResources(mode string, replica bool) corev1.ResourceRequirements {
	resources := containerResources()
	if mode == workerModeMarker || mode == workerModeBoundary || mode == workerModeObserve || mode == workerModeFixture || mode == workerModeInstall {
		for _, values := range []corev1.ResourceList{resources.Requests, resources.Limits} {
			values[corev1.ResourceCPU] = resource.MustParse("1")
			values[corev1.ResourceMemory] = resource.MustParse("256Mi")
		}
	}
	if replica {
		resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
		resources.Limits[corev1.ResourceCPU] = resource.MustParse("250m")
	}
	return resources
}

func (b *Backend) job(r *run, side, name string, options jobOptions) (*batchv1.Job, error) {
	namespace := namespaceName(r, side)
	labels := map[string]string{runLabel: string(r.object.UID), roleLabel: options.role}
	if options.target != "" {
		labels[pv.ApplicationTargetLabel] = options.target
	}
	deadline, err := b.jobDeadline(r, namespace, name, options.deadline)
	if err != nil {
		return nil, err
	}
	mode := int32(0444)
	workSize, runnerSize := resource.MustParse("2Gi"), resource.MustParse("128Mi")
	spec := corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, ServiceAccountName: "orka-" + options.serviceAccount,
		AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
		SchedulerName: "default-scheduler",
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{
				Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{r.state.Target.NodeName},
			}}}},
		}}},
		NodeSelector: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": strings.TrimPrefix(b.config.Platform, "linux/"),
			b.config.NodeSelectorKey: b.config.NodeSelectorValue},
		TerminationGracePeriodSeconds: new(int64(5)),
		HostAliases:                   options.hostAliases, DNSPolicy: corev1.DNSNone,
		DNSConfig: &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)), FSGroup: new(int64(65532)),
			RunAsNonRoot: new(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			FSGroupChangePolicy: new(corev1.FSGroupChangeOnRootMismatch)},
		Volumes: []corev1.Volume{
			{Name: "input", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name + "-input"}, DefaultMode: &mode}}},
			{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workSize}}},
		}}
	if b.config.RuntimeClassName != "" {
		spec.RuntimeClassName = new(b.config.RuntimeClassName)
	}
	container := corev1.Container{Name: options.mode, Image: options.image, ImagePullPolicy: corev1.PullIfNotPresent,
		Command:    []string{pv.ApplicationWorkerPath, options.mode, "--input", pv.ApplicationInputPath},
		WorkingDir: pv.ApplicationWorkPath, SecurityContext: containerSecurity(),
		Resources: roleResources(options.mode, options.fixtureProfile != nil && options.fixtureProfile.ReplicaTarget != nil),
		Env: []corev1.EnvVar{
			{Name: "ORKA_APPLICATION_TASK_UID", Value: options.taskUID},
			{Name: "ORKA_APPLICATION_DEADLINE", Value: r.state.Deadline.Format(time.RFC3339Nano)},
			{Name: "ORKA_APPLICATION_CONTAINER_NAME", Value: options.mode},
			{Name: "ORKA_APPLICATION_FILESIZE_ROLE", Value: jobFileSizeRole(options)},
			{Name: "ORKA_APPLICATION_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
			{Name: "ORKA_APPLICATION_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
			{Name: "HOME", Value: pv.ApplicationWorkPath}, {Name: "TMPDIR", Value: pv.ApplicationWorkPath},
			{Name: "GOMAXPROCS", Value: "2"}, {Name: "GOMEMLIMIT", Value: "1536MiB"},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "input", MountPath: "/input", ReadOnly: true}, {Name: "work", MountPath: pv.ApplicationWorkPath}}}
	if b.config.RuntimeQualification != nil {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "ORKA_APPLICATION_RUNTIME_PROFILE", Value: b.config.RuntimeQualification.Profile},
			corev1.EnvVar{Name: "ORKA_APPLICATION_TARGET_CLUSTER_UID", Value: b.config.TargetClusterUID},
			corev1.EnvVar{Name: "ORKA_APPLICATION_NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1", FieldPath: "spec.nodeName"}}})
	}
	if options.mode == workerModePrepare {
		spec.HostAliases = nil
		for _, endpoint := range b.config.SourceEndpoints {
			spec.HostAliases = append(spec.HostAliases, corev1.HostAlias{IP: endpoint.Address, Hostnames: []string{endpoint.Host}})
		}
	}
	if options.image != b.config.WorkerImage {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: runnerVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &runnerSize}}})
		spec.InitContainers = []corev1.Container{{
			Name: workerModeInstall, Image: b.config.WorkerImage, ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{pv.ApplicationWorkerPath, workerModeInstall, "--destination", "/runner/worker"},
			SecurityContext: containerSecurity(), Resources: containerResources(),
			VolumeMounts: []corev1.VolumeMount{{Name: runnerVolumeName, MountPath: "/runner"}},
		}}
		for _, resources := range []corev1.ResourceList{spec.InitContainers[0].Resources.Requests, spec.InitContainers[0].Resources.Limits} {
			resources[corev1.ResourceCPU] = resource.MustParse("1")
			resources[corev1.ResourceMemory] = resource.MustParse("256Mi")
		}
		container.Command[0] = "/runner/worker"
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: runnerVolumeName, MountPath: "/runner", ReadOnly: true})
	}
	if options.source {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: sourceVolumeName, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "orka-source", ReadOnly: options.mode != workerModePrepare}}})
		mount := corev1.VolumeMount{Name: sourceVolumeName, MountPath: pv.ApplicationSourceVolumePath, ReadOnly: options.mode != workerModePrepare}
		if options.mode == workerModeBuild {
			mount.MountPath = pv.ApplicationSourcePath
			switch side {
			case pv.Original:
				mount.SubPath = pv.ApplicationOriginalSourceSubPath
			case pv.Patched:
				mount.SubPath = pv.ApplicationPatchedSourceSubPath
			default:
				return nil, contract.ErrInvalid
			}
		}
		container.VolumeMounts = append(container.VolumeMounts, mount)
	}
	if options.artifacts {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: artifactsVolumeName, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "orka-artifacts", ReadOnly: options.artifactReadOnly}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: artifactsVolumeName, MountPath: pv.ApplicationArtifactsPath, ReadOnly: options.artifactReadOnly})
	}
	if options.token {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "target", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: new(int32(0444)), Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: new(int64(600))}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "orka-target"},
					Items: []corev1.KeyToPath{{Key: caCertificateKey, Path: caCertificateKey}, {Key: kubeconfigKey, Path: kubeconfigKey}, {Key: namespaceKey, Path: namespaceKey}}}},
			},
		}}})
		mount := "/target"
		if options.inClusterToken {
			mount = "/var/run/secrets/kubernetes.io/serviceaccount"
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "target", MountPath: mount, ReadOnly: true})
		container.Env = append(container.Env, corev1.EnvVar{Name: "ORKA_APPLICATION_API_SERVER", Value: b.config.TargetAPIServer})
		apiIP, port, _ := apiAddress(b.config.TargetAPIServer)
		container.Env = append(container.Env, corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: apiIP.String()},
			corev1.EnvVar{Name: "KUBERNETES_SERVICE_PORT", Value: strconv.Itoa(port)},
			corev1.EnvVar{Name: "KUBERNETES_SERVICE_PORT_HTTPS", Value: strconv.Itoa(port)})
	}
	if options.marker {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: markerVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: markerSecretName, DefaultMode: new(int32(0444))}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: markerVolumeName, MountPath: "/marker", ReadOnly: true})
	}
	if err := mountStartupMaterials(namespace, options, &spec, &container); err != nil {
		return nil, err
	}
	if options.receiverPort != 0 {
		container.Ports = []corev1.ContainerPort{{Name: "receiver", ContainerPort: int32(options.receiverPort), Protocol: corev1.ProtocolTCP}}
		container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(options.receiverPort)}},
			PeriodSeconds: 1, TimeoutSeconds: 1, FailureThreshold: 2, SuccessThreshold: 1}
	}
	if options.mode == workerModeMarker {
		if !options.markerInput {
			container.Command = []string{pv.ApplicationWorkerPath, workerModeMarker, "--port", strconv.Itoa(options.markerPort)}
		}
	}
	if options.fixtureProfile != nil && options.fixtureConfig != "" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "fixture-config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: options.fixtureConfig},
				DefaultMode: new(int32(0444)), Items: options.fixtureConfigItems}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "fixture-config", MountPath: "/fixture-config", ReadOnly: true})
	}
	spec.Containers = []corev1.Container{container}
	if b.config.RuntimeQualification != nil {
		for index := range spec.Volumes {
			if spec.Volumes[index].EmptyDir != nil {
				spec.Volumes[index].EmptyDir.Medium = corev1.StorageMediumMemory
			}
		}
	}
	if err := b.validatePod(spec); err != nil {
		return nil, err
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), Completions: new(int32(1)), Parallelism: new(int32(1)),
			ActiveDeadlineSeconds: new(deadline), PodReplacementPolicy: new(batchv1.Failed),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}}}, nil
}

func (b *Backend) jobDeadline(r *run, namespace, name string, requested int64) (int64, error) {
	deadline := requested
	if deadline == 0 {
		deadline = int64(b.config.RunTimeout / time.Second)
	}
	deadline = min(deadline, int64(time.Until(r.state.Deadline)/time.Second))
	for _, entry := range r.state.Effects {
		if entry.Kind == jobKind && entry.Namespace == namespace && entry.Name == name {
			deadline = entry.DeadlineSeconds
		}
	}
	if deadline < 1 {
		return 0, safeError(contract.ErrUnavailable, context.DeadlineExceeded)
	}
	return deadline, nil
}

func mountStartupMaterials(namespace string, options jobOptions, spec *corev1.PodSpec, container *corev1.Container) error {
	for index, identity := range options.subjectTLS {
		if identity.Namespace != namespace || identity.UID == "" {
			return contract.ErrInvalid
		}
		name := "subject-tls-" + strconv.Itoa(index)
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: identity.Name, DefaultMode: new(int32(0440)),
				Items: []corev1.KeyToPath{{Key: caCertificateKey, Path: caCertificateKey}, {Key: corev1.TLSCertKey, Path: corev1.TLSCertKey}, {Key: corev1.TLSPrivateKeyKey, Path: corev1.TLSPrivateKeyKey}}}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: "/subject-config/" + identity.ID, ReadOnly: true})
	}
	if len(options.fixtureSecrets) != 0 {
		identity := options.fixtureSecrets[0]
		if identity.Namespace != namespace || identity.UID == "" {
			return contract.ErrInvalid
		}
		items := make([]corev1.KeyToPath, 0, len(options.fixtureSecrets))
		for _, file := range options.fixtureSecrets {
			if file.Name != identity.Name || file.UID != identity.UID || file.Namespace != namespace {
				return contract.ErrInvalid
			}
			items = append(items, corev1.KeyToPath{Key: file.Key, Path: file.ID})
		}
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "fixture-secrets", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: identity.Name, DefaultMode: new(int32(0440)), Items: items}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "fixture-secrets", MountPath: "/fixture-secrets", ReadOnly: true})
	}
	if len(options.actors) != 0 {
		items := make([]corev1.KeyToPath, 0, len(options.actors))
		for _, actor := range options.actors {
			items = append(items, corev1.KeyToPath{Key: actor.ID, Path: actor.ID + "/token"})
		}
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "actors", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: options.actorSecret, DefaultMode: new(int32(0444)), Items: items}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "actors", MountPath: "/actors", ReadOnly: true})
	}
	return nil
}

func jobName(role string) string {
	return "orka-" + strings.TrimPrefix(pv.Digest([]byte(role)), "sha256:")[:24]
}

func (b *Backend) ensureJob(ctx context.Context, r *run, side, role string, input any, options jobOptions) (*batchv1.Job, error) {
	name := jobName(role)
	expected, err := b.job(r, side, name, options)
	if err != nil {
		return nil, err
	}
	content, err := marshalInput(input)
	if err != nil {
		return nil, err
	}
	files, err := b.runtimeInputFiles(expected.Spec.Template.Spec.Containers[0].Resources, jobFileSizeRole(options))
	if err != nil {
		return nil, err
	}
	files["input.json"] = content
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-input", Namespace: namespaceName(r, side)},
		Immutable: new(true), BinaryData: files}
	object, err := b.ensureObject(ctx, r, resourceKey(side, role+"-input"), cm)
	if err != nil {
		return nil, err
	}
	actualCM := object.(*corev1.ConfigMap)
	if !ptr.Deref(actualCM.Immutable, false) || !reflect.DeepEqual(actualCM.BinaryData, cm.BinaryData) || len(actualCM.Data) != 0 {
		return nil, contract.ErrConflict
	}
	object, err = b.ensureObject(ctx, r, resourceKey(side, role), expected)
	if err != nil {
		return nil, err
	}
	actual := object.(*batchv1.Job)
	if !b.jobExecutionMatches(expected, actual) || !podPolicyMatches(expected.Spec.Template.Spec, actual.Spec.Template.Spec) ||
		b.validatePod(actual.Spec.Template.Spec) != nil || actual.Spec.Template.Spec.NodeName != "" {
		return nil, ErrIsolation
	}
	return actual, nil
}

func (b *Backend) jobExecutionMatches(expected, observed *batchv1.Job) bool {
	if ptr.Deref(expected.Spec.Template.Spec.RuntimeClassName, "") != b.config.RuntimeClassName ||
		!reflect.DeepEqual(expected.Spec.Template.Spec.RuntimeClassName, observed.Spec.Template.Spec.RuntimeClassName) {
		return false
	}
	want, actual := expected.DeepCopy(), observed.DeepCopy()
	want.Spec.Template.Spec.RuntimeClassName, actual.Spec.Template.Spec.RuntimeClassName = nil, nil
	return pvkube.JobExecutionMatches(want, actual)
}

func (b *Backend) podExecutionMatches(expected, observed corev1.PodSpec) bool {
	if ptr.Deref(expected.RuntimeClassName, "") != b.config.RuntimeClassName ||
		!reflect.DeepEqual(expected.RuntimeClassName, observed.RuntimeClassName) {
		return false
	}
	want, actual := *expected.DeepCopy(), *observed.DeepCopy()
	want.RuntimeClassName, actual.RuntimeClassName = nil, nil
	return pvkube.PodExecutionMatches(want, actual)
}

func podPolicyMatches(expected, observed corev1.PodSpec) bool {
	return expected.ServiceAccountName == observed.ServiceAccountName &&
		(expected.NodeName == "" || expected.NodeName == observed.NodeName) && expected.DNSPolicy == observed.DNSPolicy &&
		reflect.DeepEqual(expected.DNSConfig, observed.DNSConfig) &&
		reflect.DeepEqual(expected.HostAliases, observed.HostAliases) &&
		reflect.DeepEqual(expected.ImagePullSecrets, observed.ImagePullSecrets) &&
		expected.PriorityClassName == observed.PriorityClassName &&
		reflect.DeepEqual(expected.Affinity, observed.Affinity) &&
		observed.SchedulerName == expected.SchedulerName
}

func (b *Backend) inputTarget(r *run, side string) (pv.ApplicationTarget, error) {
	entry := r.effect(resourceKey(side, "namespace"))
	if entry == nil || entry.UID == "" || entry.Deleted {
		return pv.ApplicationTarget{}, contract.ErrConflict
	}
	return pv.ApplicationTarget{ClusterUID: r.state.TargetUID, Namespace: entry.Name, NamespaceUID: string(entry.UID)}, nil
}

func (b *Backend) workerInput(r *run, side, checkID, role string) (pv.ApplicationWorkerInput, error) {
	target, err := b.inputTarget(r, side)
	if err != nil || r.state.Manifest == nil || r.state.Binding == nil {
		return pv.ApplicationWorkerInput{}, contract.ErrConflict
	}
	task := r.state.ArmTasks[side]
	if task.UID == "" {
		return pv.ApplicationWorkerInput{}, contract.ErrConflict
	}
	input := pv.ApplicationWorkerInput{Version: pv.ApplicationProtocolVersion, Manifest: *r.state.Manifest,
		Binding: *r.state.Binding, Side: side, CheckID: checkID, TaskUID: string(task.UID), Target: target}
	sa, imageID := roleWorker, b.config.ToolchainImageID
	physicalSide := side
	switch role {
	case workerModeObserve:
		sa, imageID = roleObserver, b.config.WorkerImageID
		physicalSide = observerSide(r, side)
	case workerModeSubject:
		sa, imageID = roleSubject, r.state.Request.Checks.Application.Runtime.ImageID
	case workerModeMarker:
		sa, imageID = roleMarker, b.config.WorkerImageID
		physicalSide = observerSide(r, side)
	case workerModeFixture:
		sa, imageID = roleWorker, b.config.WorkerImageID
		physicalSide = observerSide(r, side)
	case roleReplica:
		sa, imageID = roleReplica, b.config.WorkerImageID
	}
	entry := r.effect(resourceKey(physicalSide, "sa-"+sa))
	if sa == roleMarker {
		entry = r.effect(resourceKey(physicalSide, "account-orka-marker"))
	}
	if entry == nil || entry.UID == "" {
		return pv.ApplicationWorkerInput{}, contract.ErrConflict
	}
	input.Worker = pv.ApplicationContainerIdentity{ContainerName: role, ImageID: imageID,
		ServiceAccountName: "orka-" + sa, ServiceAccountUID: string(entry.UID)}
	namespace := r.effect(resourceKey(physicalSide, "namespace"))
	if namespace == nil || namespace.UID == "" {
		return pv.ApplicationWorkerInput{}, contract.ErrConflict
	}
	input.Worker.Namespace, input.Worker.NamespaceUID = namespace.Name, string(namespace.UID)
	input.APIBaseline, err = b.inputAPIBaseline(r, side, checkID)
	if err != nil {
		return pv.ApplicationWorkerInput{}, err
	}
	return input, nil
}

func (b *Backend) pause(ctx context.Context) error {
	timer := time.NewTimer(b.config.ReconcileInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return safeError(contract.ErrUnavailable, ctx.Err())
	case <-timer.C:
		return nil
	}
}
