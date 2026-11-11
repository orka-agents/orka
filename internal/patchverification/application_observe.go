package patchverification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func ApplicationResolvedImageID(value string) string {
	if sha256Pattern.MatchString(value) {
		return value
	}
	if _, digest, ok := strings.Cut(value, "@"); ok && sha256Pattern.MatchString(digest) {
		return digest
	}
	for _, prefix := range []string{"containerd://", "docker://", "cri-o://"} {
		if digest, ok := strings.CutPrefix(value, prefix); ok && sha256Pattern.MatchString(digest) {
			return digest
		}
	}
	return ""
}

// ObserveApplication runs only in the independently authenticated observer Pod.
// The subject's image, container, running instance and target endpoint are read
// again after observation; a replaced/restarted subject cannot produce success.
func ObserveApplication(ctx context.Context, input ApplicationWorkerInput, kube kubernetes.Interface, resources dynamic.Interface, marker []byte) (ExecutionEvidence, error) {
	return ObserveApplicationWithActors(ctx, input, kube, resources, marker, nil, nil)
}

func ObserveApplicationWithActors(ctx context.Context, input ApplicationWorkerInput, kube kubernetes.Interface, resources dynamic.Interface, marker []byte,
	actors map[string]ApplicationActorClient, onAction func(ApplicationActionObservation) error) (ExecutionEvidence, error) {
	check, err := validateApplicationObservationInput(input, kube, resources)
	if err != nil {
		return ExecutionEvidence{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
	defer cancel()
	started := time.Now().UTC()
	subjectPod, err := applicationLiveContainer(ctx, kube, input.Target, *input.Subject, input.Manifest.Application.Runtime.Image, true)
	if err != nil {
		return ExecutionEvidence{}, err
	}
	if err := applicationSubjectMaterialMounts(subjectPod, input.SubjectTLS); err != nil {
		return ExecutionEvidence{}, err
	}
	if _, err := applicationLiveContainer(ctx, kube, input.Target, input.Worker,
		input.Manifest.Environment.Dependencies[ApplicationObserverImageKey], false); err != nil {
		return ExecutionEvidence{}, err
	}
	var receiver *applicationReceiver
	if check.Application.TCP != nil && check.Application.TCP.Mode == ApplicationTCPReceive {
		receiver, err = newApplicationReceiver(ctx, input, check, kube, subjectPod.Status.PodIP, marker)
		if err != nil {
			return ExecutionEvidence{}, err
		}
		defer receiver.close()
		ctx = receiver.capture.ctx
	}
	actionEvidence, err := executeApplicationActions(ctx, input, check, actors, onAction)
	if err != nil {
		return ExecutionEvidence{}, err
	}
	apiBaseline, err := applicationCheckAPIBaseline(input, check, actionEvidence)
	if err != nil {
		return ExecutionEvidence{}, err
	}
	if err := waitApplicationSettle(ctx, check.Application.SettleSeconds); err != nil {
		return ExecutionEvidence{}, err
	}
	planDigest, _ := ApplicationPlanDigest(*input.Manifest.Application)
	app := &ApplicationObservation{
		Version: ApplicationProtocolVersion, PlanDigest: planDigest,
		Target: input.Target, Artifact: *input.Artifact, Subject: *input.Subject,
		Actions: actionEvidence, SubjectTLS: input.SubjectTLS, APIBaseline: apiBaseline,
	}
	content, err := observeApplicationCheck(ctx, input, check, kube, resources, receiver, marker, app)
	if err != nil {
		return ExecutionEvidence{}, err
	}
	if app.TCP != nil {
		app.TCP.MarkerRef = input.MarkerRef
	}
	if _, err := applicationLiveContainer(ctx, kube, input.Target, *input.Subject, input.Manifest.Application.Runtime.Image, true); err != nil {
		return ExecutionEvidence{}, err
	}
	if _, err := applicationLiveContainer(ctx, kube, input.Target, input.Worker,
		input.Manifest.Environment.Dependencies[ApplicationObserverImageKey], false); err != nil || ctx.Err() != nil {
		return ExecutionEvidence{}, ErrBinding
	}
	return completedApplicationObservation(input, check, app, content, started)
}

func validateApplicationObservationInput(input ApplicationWorkerInput, kube kubernetes.Interface, resources dynamic.Interface) (Check, error) {
	if ValidateApplicationWorkerInput(input) != nil || input.Artifact == nil || input.Subject == nil ||
		kube == nil || resources == nil || !applicationContainerValid(input.Worker) ||
		!applicationContainerValid(*input.Subject) ||
		input.Worker.ImageID != input.Manifest.Environment.Dependencies[ApplicationObserverImageIDKey] ||
		input.Subject.ImageID != input.Manifest.Application.Runtime.ImageID {
		return Check{}, ErrBinding
	}
	var check Check
	for _, item := range input.Manifest.Checks {
		if item.ID == input.CheckID {
			check = item
			break
		}
	}
	if ValidateApplicationCheckWithPolicy(check, input.Manifest.ApplicationPolicy) != nil {
		return Check{}, ErrEvidence
	}
	if ValidateApplicationSubjectTLSIdentities(input.Manifest.ApplicationPolicy, input.Target, input.SubjectTLS) != nil {
		return Check{}, ErrBinding
	}
	return check, nil
}

func waitApplicationSettle(ctx context.Context, seconds int) error {
	if seconds != 0 {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return errors.New("application observation did not finish its frozen settling interval")
		case <-timer.C:
		}
	}
	return nil
}

func observeApplicationCheck(ctx context.Context, input ApplicationWorkerInput, check Check, kube kubernetes.Interface,
	resources dynamic.Interface, receiver *applicationReceiver, marker []byte, app *ApplicationObservation) ([]byte, error) {
	var content []byte
	var err error
	if check.Application.Kubernetes != nil {
		app.Kubernetes, content, err = ObserveApplicationAPIWithPolicy(ctx, resources, input.Target.Namespace,
			*check.Application.Kubernetes, input.Manifest.ApplicationPolicy)
	} else if receiver != nil {
		app.TCP, content, err = receiver.observe(ctx)
	} else {
		app.TCP, content, err = observeApplicationRequest(ctx, input, check, kube, marker)
	}
	return content, err
}

func observeApplicationRequest(ctx context.Context, input ApplicationWorkerInput, check Check, kube kubernetes.Interface, marker []byte) (*ApplicationTCPIdentity, []byte, error) {
	if !sha256Pattern.MatchString(input.MarkerDigest) || Digest(marker) != input.MarkerDigest {
		return nil, nil, errors.New("application marker does not match the backend-frozen fixture digest")
	}
	endpoint, found := applicationCheckEndpoint(input, check)
	if !found {
		return nil, nil, ErrBinding
	}
	expectedImage := input.Manifest.Environment.Dependencies[ApplicationObserverImageKey]
	if endpoint.ID == ApplicationSubject {
		expectedImage = input.Manifest.Application.Runtime.Image
		if endpoint.Pod != *input.Subject {
			return nil, nil, ErrBinding
		}
	} else if endpoint.Pod.ImageID != input.Manifest.Environment.Dependencies[ApplicationObserverImageIDKey] {
		return nil, nil, ErrBinding
	}
	pod, err := applicationLiveContainer(ctx, kube, input.Target, endpoint.Pod, expectedImage, endpoint.ID == ApplicationSubject)
	if err != nil || !applicationPodAddress(pod, endpoint.Address) {
		return nil, nil, ErrBinding
	}
	result, content, err := ObserveApplicationTCP(ctx, *check.Application.TCP, endpoint, marker)
	if err == nil {
		pod, checkErr := applicationLiveContainer(ctx, kube, input.Target, endpoint.Pod, expectedImage, endpoint.ID == ApplicationSubject)
		if checkErr != nil || !applicationPodAddress(pod, endpoint.Address) {
			err = ErrBinding
		}
	}
	return result, content, err
}

func completedApplicationObservation(input ApplicationWorkerInput, check Check, app *ApplicationObservation, content []byte, started time.Time) (ExecutionEvidence, error) {
	source, taskID, _ := applicationSource(input.Manifest, input.Binding, input.Side)
	zero := 0
	app.Completed = true
	observation := Observation{
		RunID: input.Binding.RunID, AttemptID: input.Binding.AttemptID, TaskID: taskID,
		ManifestDigest: input.Binding.ManifestDigest, Side: input.Side, CheckID: check.ID,
		SourceTree: source.Tree, ImageID: input.Worker.ImageID, ContainerID: input.Worker.ContainerID,
		JobUID: input.Worker.JobUID, PodUID: input.Worker.PodUID, Origin: ApplicationObserverOrigin,
		StartedAt: started, FinishedAt: time.Now().UTC(), Executed: true, ExitCode: &zero,
		StdoutDigest: Digest(content), StdoutBytes: len(content), StderrDigest: Digest(nil), Application: app,
	}
	if err := ValidateApplicationWorkerObservation(input.Manifest, input.Binding, observation); err != nil {
		return ExecutionEvidence{}, err
	}
	return ExecutionEvidence{Observation: observation, Blobs: map[string][]byte{Digest(content): content, Digest(nil): {}}}, nil
}

func applicationCheckEndpoint(input ApplicationWorkerInput, check Check) (ApplicationEndpoint, bool) {
	for _, endpoint := range input.Endpoints {
		if endpoint.ID == check.Application.TCP.Target && endpoint.Port == check.Application.TCP.Port {
			return endpoint, true
		}
	}
	return ApplicationEndpoint{}, false
}

func applicationPodAddress(pod *corev1.Pod, address string) bool {
	if pod == nil || net.ParseIP(address) == nil {
		return false
	}
	for _, ip := range pod.Status.PodIPs {
		if ip.IP == address {
			return true
		}
	}
	return pod.Status.PodIP == address
}

func applicationSubjectMaterialMounts(pod *corev1.Pod, identities []ApplicationSubjectTLSIdentity) error {
	if pod == nil || len(pod.Spec.Containers) != 1 {
		return ErrBinding
	}
	for _, identity := range identities {
		found := false
		for _, mount := range pod.Spec.Containers[0].VolumeMounts {
			if mount.MountPath != ApplicationSubjectConfigPath+"/"+identity.ID || !mount.ReadOnly ||
				mount.SubPath != "" || mount.SubPathExpr != "" {
				continue
			}
			for _, volume := range pod.Spec.Volumes {
				if volume.Name == mount.Name && volume.Secret != nil && volume.Secret.SecretName == identity.Name &&
					(volume.Secret.Optional == nil || !*volume.Secret.Optional) {
					found = true
				}
			}
		}
		if !found {
			return errors.New("application startup TLS is not mounted from its exact read-only synthetic Secret")
		}
	}
	return nil
}

func applicationLiveContainer(ctx context.Context, kube kubernetes.Interface, target ApplicationTarget, identity ApplicationContainerIdentity, image string, subject bool) (*corev1.Pod, error) {
	target = applicationIdentityTarget(target, identity)
	pod, err := kube.CoreV1().Pods(target.Namespace).Get(ctx, identity.PodName, metav1.GetOptions{})
	if err != nil || !applicationPodIdentityValid(pod, identity) {
		return nil, errors.New("application Pod identity or isolation changed")
	}
	if !applicationPodOwnerMatches(pod, identity.JobUID) {
		return nil, ErrBinding
	}
	account, err := kube.CoreV1().ServiceAccounts(target.Namespace).Get(ctx, identity.ServiceAccountName, metav1.GetOptions{})
	if err != nil || string(account.UID) != identity.ServiceAccountUID || account.DeletionTimestamp != nil {
		return nil, errors.New("application test identity changed")
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			return nil, errors.New("host filesystem exposure is unsupported")
		}
	}
	if err := validateApplicationContainerIsolation(pod, identity, image); err != nil {
		return nil, err
	}
	if subject {
		if err := validateApplicationSubjectLauncher(pod.Spec.Containers[0]); err != nil {
			return nil, err
		}
	}
	if err := validateApplicationLiveStatus(pod.Status.ContainerStatuses, identity); err != nil {
		return nil, err
	}
	return pod, nil
}

func applicationPodIdentityValid(pod *corev1.Pod, identity ApplicationContainerIdentity) bool {
	return pod != nil && string(pod.UID) == identity.PodUID && pod.DeletionTimestamp == nil &&
		pod.Spec.ServiceAccountName == identity.ServiceAccountName && !pod.Spec.HostNetwork &&
		!pod.Spec.HostPID && !pod.Spec.HostIPC && pod.Spec.NodeName != "" &&
		(pod.Spec.ShareProcessNamespace == nil || !*pod.Spec.ShareProcessNamespace) &&
		len(pod.Spec.Containers) == 1 && len(pod.Spec.EphemeralContainers) == 0
}

func applicationPodOwnerMatches(pod *corev1.Pod, jobUID string) bool {
	if jobUID == "" {
		return true
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" && string(owner.UID) == jobUID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func applicationEffectiveSecurity(pod *corev1.Pod, security corev1.SecurityContext) corev1.SecurityContext {
	if inherited := pod.Spec.SecurityContext; inherited != nil {
		if security.RunAsUser == nil {
			security.RunAsUser = inherited.RunAsUser
		}
		if security.RunAsGroup == nil {
			security.RunAsGroup = inherited.RunAsGroup
		}
		if security.RunAsNonRoot == nil {
			security.RunAsNonRoot = inherited.RunAsNonRoot
		}
		if security.SeccompProfile == nil {
			security.SeccompProfile = inherited.SeccompProfile
		}
	}
	return security
}

func validateApplicationContainerIsolation(pod *corev1.Pod, identity ApplicationContainerIdentity, image string) error {
	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil {
		return ErrEvidence
	}
	security := applicationEffectiveSecurity(pod, *container.SecurityContext)
	uid, gid, nonRoot, seccomp := security.RunAsUser, security.RunAsGroup, security.RunAsNonRoot, security.SeccompProfile
	if container.Name != identity.ContainerName || container.Image != image ||
		security.Privileged != nil && *security.Privileged ||
		security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
		nonRoot == nil || !*nonRoot || uid == nil || *uid != 65532 || gid == nil || *gid != 65532 ||
		seccomp == nil || seccomp.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		security.Capabilities == nil || len(security.Capabilities.Add) != 0 ||
		len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		return errors.New("application container no longer has the admitted non-privileged profile")
	}
	return nil
}

func validateApplicationSubjectLauncher(container corev1.Container) error {
	command := append(append([]string{}, container.Command...), container.Args...)
	if len(command) != 4 || (command[0] != ApplicationWorkerPath && command[0] != "/runner/worker") ||
		command[1] != ApplicationSubject || command[2] != "--input" || command[3] != ApplicationInputPath {
		return errors.New("application container does not execute the trusted measured-binary launcher")
	}
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == ApplicationArtifactsPath && mount.ReadOnly {
			return nil
		}
	}
	return errors.New("application artifact is not mounted read-only")
}

func validateApplicationLiveStatus(statuses []corev1.ContainerStatus, identity ApplicationContainerIdentity) error {
	for _, status := range statuses {
		if status.Name == identity.ContainerName {
			if status.ContainerID != identity.ContainerID ||
				ApplicationResolvedImageID(status.ImageID) != ApplicationResolvedImageID(identity.ImageID) ||
				status.State.Running == nil || status.RestartCount != 0 {
				return errors.New("application container was replaced, restarted or is not running")
			}
			return nil
		}
	}
	return errors.New("application container has no live runtime identity")
}

// ReadApplicationContainerIdentity is for the trusted observer's own downward-
// API-fenced Pod only. Subject and build identities must be frozen by the backend.
func ReadApplicationContainerIdentity(ctx context.Context, kube kubernetes.Interface, target ApplicationTarget, name, uid, containerName string) (ApplicationContainerIdentity, error) {
	pod, err := kube.CoreV1().Pods(target.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || pod == nil || string(pod.UID) != uid || pod.DeletionTimestamp != nil {
		return ApplicationContainerIdentity{}, ErrBinding
	}
	account, err := kube.CoreV1().ServiceAccounts(target.Namespace).Get(ctx, pod.Spec.ServiceAccountName, metav1.GetOptions{})
	if err != nil {
		return ApplicationContainerIdentity{}, ErrBinding
	}
	identity := ApplicationContainerIdentity{PodName: name, PodUID: uid, ContainerName: containerName,
		ServiceAccountName: account.Name, ServiceAccountUID: string(account.UID), Namespace: target.Namespace, NamespaceUID: target.NamespaceUID}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" && owner.Controller != nil && *owner.Controller {
			identity.JobUID = string(owner.UID)
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName && status.State.Running != nil && status.RestartCount == 0 {
			identity.ContainerID, identity.ImageID = status.ContainerID, ApplicationResolvedImageID(status.ImageID)
		}
	}
	if !applicationContainerValid(identity) {
		return ApplicationContainerIdentity{}, ErrBinding
	}
	return identity, nil
}

func ObserveApplicationAPI(ctx context.Context, resources dynamic.Interface, namespace string, check ApplicationKubernetesCheck) (*ApplicationAPIIdentity, []byte, error) {
	return ObserveApplicationAPIWithPolicy(ctx, resources, namespace, check, nil)
}

func ObserveApplicationAPIWithPolicy(ctx context.Context, resources dynamic.Interface, namespace string, check ApplicationKubernetesCheck, policy *ApplicationPolicy) (*ApplicationAPIIdentity, []byte, error) {
	if resources == nil || !applicationName(namespace) || !applicationAPIVersionWithPolicy(check.APIVersion, check.Resource, policy) ||
		strings.Contains(check.Resource, "/") || !applicationName(check.Name) || !applicationJSONPointer(check.Pointer) {
		return nil, nil, ErrEvidence
	}
	version, err := schema.ParseGroupVersion(check.APIVersion)
	if err != nil {
		return nil, nil, ErrEvidence
	}
	identity := &ApplicationAPIIdentity{APIVersion: check.APIVersion, Resource: check.Resource, Name: check.Name, Pointer: check.Pointer}
	object, err := resources.Resource(version.WithResource(check.Resource)).Namespace(namespace).Get(ctx, check.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && ctx.Err() == nil {
		content, _ := json.Marshal(identity.Result)
		return identity, content, nil
	}
	if err != nil || ctx.Err() != nil {
		return nil, nil, errors.New("application API observation did not return an exact complete object")
	}
	identity, err = ApplicationAPIIdentityFromObject(object, namespace, check, policy)
	if err != nil {
		return nil, nil, err
	}
	content, _ := json.Marshal(identity.Result)
	return identity, content, nil
}

// ApplicationAPIIdentityFromObject reduces an authoritative API response to
// bounded metadata. For initial evidence callers must supply the Create/Update
// response itself, not a later GET which may already include reconciliation.
func ApplicationAPIIdentityFromObject(object *unstructured.Unstructured, namespace string,
	check ApplicationKubernetesCheck, policy *ApplicationPolicy) (*ApplicationAPIIdentity, error) {
	if !applicationName(namespace) || !applicationName(check.Name) || !applicationJSONPointer(check.Pointer) ||
		!applicationAPIVersionWithPolicy(check.APIVersion, check.Resource, policy) || strings.Contains(check.Resource, "/") ||
		object == nil || object.GetName() != check.Name || object.GetNamespace() != namespace ||
		object.GetAPIVersion() != check.APIVersion || object.GetUID() == "" || object.GetResourceVersion() == "" ||
		object.GetDeletionTimestamp() != nil {
		return nil, errors.New("application API response did not bind an exact complete object")
	}
	identity := &ApplicationAPIIdentity{APIVersion: check.APIVersion, Resource: check.Resource, Name: check.Name, Pointer: check.Pointer,
		UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion()}
	value, found := applicationPointerValue(object.Object, check.Pointer)
	raw := []byte(nil)
	var err error
	if found {
		raw, err = json.Marshal(value)
		if err != nil {
			return nil, ErrEvidence
		}
	}
	identity.Result, err = applicationAPIResult(ApplicationAPIValue{Exists: true, Found: found, Value: raw})
	if err != nil {
		return nil, err
	}
	return identity, nil
}

func applicationPointerValue(value any, pointer string) (any, bool) {
	for part := range strings.SplitSeq(pointer[1:], "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch typed := value.(type) {
		case map[string]any:
			var exists bool
			value, exists = typed[key]
			if !exists {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(typed) || strconv.Itoa(index) != key {
				return nil, false
			}
			value = typed[index]
		default:
			return nil, false
		}
	}
	return value, true
}

// ObserveApplicationTCP captures an entire bounded EOF-terminated response.
// A timeout, refused connection, read error or oversized response never proves
// marker absence. Only marker-match and digest/size metadata leave this function.
func ObserveApplicationTCP(ctx context.Context, check ApplicationTCPCheck, endpoint ApplicationEndpoint, marker []byte) (*ApplicationTCPIdentity, []byte, error) {
	if endpoint.ID != check.Target || endpoint.Port != check.Port || net.ParseIP(endpoint.Address) == nil ||
		len(marker) < 32 || len(marker) > 128 || len(marker) > check.MaxBytes ||
		check.MaxBytes > MaxApplicationTCPBytes || len(check.Request) > 4096 {
		return nil, nil, ErrEvidence
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Address, strconv.Itoa(endpoint.Port)))
	if err != nil {
		return nil, nil, errors.New("application TCP observer could not connect")
	}
	defer func() { _ = connection.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, nil, ErrEvidence
	}
	cancelRead := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer cancelRead()
	if len(check.Request) != 0 {
		n, err := connection.Write(check.Request)
		if err != nil || n != len(check.Request) {
			return nil, nil, errors.New("application TCP request was incomplete")
		}
	}
	response, err := io.ReadAll(io.LimitReader(connection, int64(check.MaxBytes)+1))
	if err != nil || len(response) > check.MaxBytes || ctx.Err() != nil {
		return nil, nil, errors.New("application TCP response was incomplete, late or oversized")
	}
	result := ApplicationTCPValue{Connected: true, MarkerMatched: bytes.Contains(response, marker)}
	observation := &ApplicationTCPIdentity{Endpoint: endpoint, MarkerID: check.MarkerID, MarkerDigest: Digest(marker),
		MarkerBytes: len(marker), Bytes: len(response), Result: result}
	content, _ := json.Marshal(result)
	return observation, content, nil
}
