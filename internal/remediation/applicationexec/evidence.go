package applicationexec

import (
	"context"
	"reflect"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) validateReport(ctx context.Context, r *run, input pv.ApplicationWorkerInput,
	observer pv.ApplicationContainerIdentity, report pv.ApplicationReport) error {
	observation := report.Evidence.Observation
	app := observation.Application
	if !reportIdentityMatches(input, observer, report) {
		return contract.ErrConflict
	}
	if err := validateNormalAPIBaseline(r, input, observation); err != nil {
		return err
	}
	if _, err := b.verifyTarget(ctx, &r.state.Target); err != nil {
		return err
	}
	if err := b.verifyClusterReadSet(ctx, r); err != nil {
		return err
	}
	target, err := b.inputTarget(r, input.Side)
	if err != nil || target != app.Target {
		return contract.ErrConflict
	}
	if _, err := b.boundObject(ctx, r, resourceKey(input.Side, "namespace")); err != nil {
		return err
	}
	if err := b.liveIdentity(ctx, r, input.Side, app.Subject, true); err != nil {
		return err
	}
	tlsIdentities, err := b.subjectTLSIdentities(ctx, r, input.Side)
	if err != nil || !reflect.DeepEqual(tlsIdentities, r.state.SubjectTLS[input.Side]) {
		return contract.ErrConflict
	}
	if err := b.liveIdentity(ctx, r, input.Side, app.Artifact.Build, false); err != nil {
		return err
	}
	if app.Kubernetes != nil {
		if err := b.validateAPIIdentity(ctx, r, input.Side, app.Kubernetes); err != nil {
			return err
		}
	}
	if err := b.validateReplicaEvidence(ctx, r, input.Side, observation); err != nil {
		return err
	}
	if err := b.verifyFixtureSecretFiles(ctx, r, input.Side); err != nil {
		return err
	}
	if app.TCP != nil {
		if err := b.validateTCPIdentity(ctx, r, input, observer, app.TCP); err != nil {
			return err
		}
	}
	return b.validateActorIdentities(ctx, input.Actors)
}

func reportIdentityMatches(input pv.ApplicationWorkerInput, observer pv.ApplicationContainerIdentity, report pv.ApplicationReport) bool {
	observation, app := report.Evidence.Observation, report.Evidence.Observation.Application
	if report.Version != pv.ApplicationProtocolVersion || report.TaskUID != input.TaskUID ||
		report.PodUID != observer.PodUID || observation.PodUID != observer.PodUID ||
		observation.ContainerID != observer.ContainerID || observation.JobUID != observer.JobUID ||
		observation.ImageID != observer.ImageID || observation.Side != input.Side || observation.CheckID != input.CheckID ||
		app == nil || input.Artifact == nil || input.Subject == nil || app.Target != input.Target ||
		app.Artifact != *input.Artifact || app.Subject != *input.Subject ||
		!reflect.DeepEqual(app.SubjectTLS, input.SubjectTLS) ||
		pv.ValidateObservation(input.Manifest, input.Binding, observation) != nil {
		return false
	}
	return true
}

func (b *Backend) validateTCPIdentity(ctx context.Context, r *run, input pv.ApplicationWorkerInput,
	observer pv.ApplicationContainerIdentity, tcp *pv.ApplicationTCPIdentity) error {
	endpoint := tcp.Endpoint
	if tcp.Mode == receiveMode {
		if endpoint.Pod != observer || input.ReceiverService == nil ||
			endpoint.Port != input.ReceiverService.Port || endpoint.Pod.Namespace != input.ReceiverService.Namespace ||
			endpoint.Pod.NamespaceUID != input.ReceiverService.NamespaceUID || tcp.ReceiverRoute == nil {
			return contract.ErrConflict
		}
		if err := b.validateReceiverReceipt(ctx, r, *input.ReceiverService, observer, *tcp.ReceiverRoute); err != nil {
			return err
		}
		if tcp.ReceiverRoute.PodUID != observer.PodUID {
			return contract.ErrConflict
		}
		current := &corev1.Pod{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: observer.Namespace, Name: observer.PodName}, current); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		if string(current.UID) != observer.PodUID || current.Status.PodIP != endpoint.Address {
			return contract.ErrConflict
		}
	} else {
		known := false
		for _, expected := range input.Endpoints {
			known = known || expected == endpoint
		}
		if !known {
			return contract.ErrConflict
		}
		if err := b.liveIdentity(ctx, r, input.Side, endpoint.Pod, true); err != nil {
			return err
		}
	}
	markerSide, markerKey := input.Side, legacyMarkerKey
	if input.MarkerRef != nil {
		markerSide, markerKey = protectedSide(input.Side), input.MarkerRef.Key
	}
	object, err := b.boundObject(ctx, r, resourceKey(markerSide, "marker-secret"))
	if err != nil {
		return err
	}
	secret := object.(*corev1.Secret)
	if tcp.MarkerDigest != pv.Digest(secret.Data[markerKey]) ||
		tcp.MarkerDigest != input.MarkerDigest || tcp.MarkerBytes != len(secret.Data[markerKey]) {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) validateActorIdentities(ctx context.Context, actors []pv.ApplicationActorIdentity) error {
	for _, actor := range actors {
		account := &corev1.ServiceAccount{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: actor.Namespace, Name: actor.ServiceAccountName}, account); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		if string(account.UID) != actor.ServiceAccountUID || !account.DeletionTimestamp.IsZero() {
			return contract.ErrConflict
		}
	}
	return nil
}

func (b *Backend) liveIdentity(ctx context.Context, r *run, side string, identity pv.ApplicationContainerIdentity, running bool) error {
	namespace := namespaceName(r, side)
	if identity.Namespace != "" {
		namespace = identity.Namespace
	}
	if !knownPodIdentity(r, namespace, identity) {
		return contract.ErrConflict
	}
	pod := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identity.PodName}, pod); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if string(pod.UID) != identity.PodUID || !pod.DeletionTimestamp.IsZero() || pod.Spec.NodeName != r.state.Target.NodeName ||
		pod.Spec.ServiceAccountName != identity.ServiceAccountName || len(pod.Status.ContainerStatuses) != 1 ||
		b.validatePod(pod.Spec) != nil {
		return contract.ErrConflict
	}
	if !containerIdentityMatches(pod.Status.ContainerStatuses[0], identity, running) {
		return contract.ErrConflict
	}
	if err := b.livePodOwner(ctx, r, identity, pod); err != nil {
		return err
	}
	sa := &corev1.ServiceAccount{}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: identity.ServiceAccountName}, sa); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if string(sa.UID) != identity.ServiceAccountUID || !sa.DeletionTimestamp.IsZero() {
		return contract.ErrConflict
	}
	if running {
		return b.recordVolumes(ctx, r, pod)
	}
	return nil
}

func knownPodIdentity(r *run, namespace string, identity pv.ApplicationContainerIdentity) bool {
	namespaceOwned := false
	for _, entry := range r.state.Effects {
		namespaceOwned = namespaceOwned || entry.Kind == namespaceKind && entry.Name == namespace &&
			entry.UID != "" && !entry.Deleted && (identity.NamespaceUID == "" || string(entry.UID) == identity.NamespaceUID)
	}
	if !namespaceOwned {
		return false
	}
	known := false
	for _, receipt := range r.state.Pods {
		if receipt.Namespace == namespace && receipt.Identity == identity {
			known = true
		}
	}
	return known
}

func containerIdentityMatches(status corev1.ContainerStatus, identity pv.ApplicationContainerIdentity, running bool) bool {
	if status.Name != identity.ContainerName || status.ContainerID != identity.ContainerID ||
		imageDigest(status.ImageID) != imageDigest(identity.ImageID) || status.RestartCount != 0 ||
		(running && (status.State.Running == nil || status.State.Terminated != nil)) ||
		(!running && (status.State.Terminated == nil || status.State.Terminated.ExitCode != 0)) {
		return false
	}
	return true
}

func (b *Backend) livePodOwner(ctx context.Context, r *run, identity pv.ApplicationContainerIdentity, pod *corev1.Pod) error {
	var job *batchv1.Job
	for _, entry := range r.state.Effects {
		if entry.Kind == jobKind && string(entry.UID) == identity.JobUID && !entry.Deleted {
			actual, err := b.boundObject(ctx, r, entry.Key)
			if err != nil {
				return err
			}
			job = actual.(*batchv1.Job)
		}
	}
	if job == nil || !ownedByJob(pod, job) ||
		!b.podExecutionMatches(job.Spec.Template.Spec, pod.Spec) || !podPolicyMatches(job.Spec.Template.Spec, pod.Spec) {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) validateAPIIdentity(ctx context.Context, r *run, side string, identity *pv.ApplicationAPIIdentity) error {
	kind := map[string]string{resourceConfigMaps: "ConfigMap", resourceServices: serviceKind, resourceIngresses: "Ingress", resourcePods: podKind}[identity.Resource]
	if policy := r.state.Request.ApplicationPolicy; policy != nil {
		for _, rule := range policy.Resources {
			if rule.APIVersion == identity.APIVersion && rule.Resource == identity.Resource {
				kind = rule.Kind
			}
		}
	}
	if kind == "" {
		return contract.ErrInvalid
	}
	var expected *effect
	for i := range r.state.Effects {
		entry := &r.state.Effects[i]
		if entry.Namespace == namespaceName(r, side) && entry.APIVersion == identity.APIVersion &&
			entry.Kind == kind && entry.Name == identity.Name && !entry.Deleted {
			expected = entry
		}
	}
	if expected == nil {
		if identity.Result.Exists {
			return contract.ErrConflict
		}
		return nil
	}
	if !identity.Result.Exists && b.rejectedCreateWasAbsent(r, side, expected) {
		object, err := b.objectFor(*expected)
		if err != nil {
			return err
		}
		err = b.targetReader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace, Name: expected.Name}, object)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		return contract.ErrConflict
	}
	if !identity.Result.Exists || string(expected.UID) != identity.UID {
		return contract.ErrConflict
	}

	object, err := b.objectFor(*expected)
	if err != nil {
		return err
	}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace, Name: expected.Name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return contract.ErrConflict
		}
		return safeError(contract.ErrUnavailable, err)
	}
	if object.GetUID() != expected.UID || !object.GetDeletionTimestamp().IsZero() {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) rejectedCreateWasAbsent(r *run, side string, entry *effect) bool {
	if !entry.Actor || !entry.Attempted || !entry.Rejected || entry.UID != "" || entry.Failure != effectCreateRejected {
		return false
	}
	for _, action := range r.state.Actions {
		if action.Side != side || action.EffectKey != entry.Key || action.NotStarted || !action.Target.MustBeAbsent ||
			action.Target.UID != "" || action.Target.ResourceVersion != "" || action.Outcome == nil {
			continue
		}
		outcome := action.Outcome
		if outcome.Target != action.Target || outcome.UID != "" || outcome.ResourceVersion != "" {
			return false
		}
		switch outcome.StatusCode {
		case 400, 401, 403, 404, 405, 409, 422, 429:
			return true
		default:
			return false
		}
	}
	return false
}
