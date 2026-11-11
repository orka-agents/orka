package applicationexec

import (
	"context"
	"reflect"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) enrichReplicaEvidence(ctx context.Context, r *run, side string, observation *pv.Observation) error {
	app := observation.Application
	if app == nil || app.Kubernetes == nil || app.Kubernetes.APIVersion != appsAPIVersion || app.Kubernetes.Resource != resourceDeployments {
		return nil
	}
	fixture, profile, err := replicaProfile(r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy)
	if err != nil || fixture == nil || app.Kubernetes.Name != profile.ReplicaTarget.Name {
		return contract.ErrConflict
	}
	object, err := b.boundObject(ctx, r, resourceKey(side, "replica-deployment"))
	if err != nil {
		return err
	}
	deployment := object.(*appsv1.Deployment)
	template, ok := r.state.ReplicaTemplates[side]
	if !ok || !reflect.DeepEqual(template, deployment.Spec.Template) || deployment.Spec.Replicas == nil ||
		string(deployment.UID) != app.Kubernetes.UID {
		return contract.ErrConflict
	}
	if err := b.captureReplicaDescendants(ctx, r, side, deployment, profile, false); err != nil {
		return err
	}
	evidence := &pv.ApplicationReplicaObservation{DeploymentUID: string(deployment.UID), DesiredReplicas: *deployment.Spec.Replicas,
		ReadyReplicas: deployment.Status.ReadyReplicas, InitialReplicas: profile.ReplicaTarget.InitialReplicas,
		MaxReplicas: profile.ReplicaTarget.MaxReplicas}
	sets := &appsv1.ReplicaSetList{}
	if err := b.targetReader.List(ctx, sets, client.InNamespace(deployment.Namespace), client.Limit(3)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	owners := make(map[string]*appsv1.ReplicaSet)
	for index := range sets.Items {
		set := &sets.Items[index]
		if !exactAppsOwner(set, deploymentKind, deployment.Name, deployment.UID) || !set.DeletionTimestamp.IsZero() {
			return contract.ErrConflict
		}
		evidence.ReplicaSetUIDs = append(evidence.ReplicaSetUIDs, string(set.UID))
		owners[string(set.UID)] = set
	}
	pods := &corev1.PodList{}
	if err := b.targetReader.List(ctx, pods, client.InNamespace(deployment.Namespace), client.Limit(64)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if sets.Continue != "" || len(sets.Items) > 2 || pods.Continue != "" {
		return ErrIsolation
	}
	evidence.Pods, err = b.readyReplicaPods(r, side, template.Spec, profile.ImageID, pods.Items, owners)
	if err != nil {
		return err
	}
	slices.Sort(evidence.ReplicaSetUIDs)
	slices.SortFunc(evidence.Pods, func(a, b pv.ApplicationContainerIdentity) int { return strings.Compare(a.PodUID, b.PodUID) })
	if int32(len(evidence.Pods)) != evidence.ReadyReplicas || len(evidence.Pods) > int(profile.ReplicaTarget.MaxReplicas) {
		return contract.ErrConflict
	}
	if app.Replica != nil && !reflect.DeepEqual(app.Replica, evidence) {
		return contract.ErrConflict
	}
	app.Replica = evidence
	return nil
}

func (b *Backend) readyReplicaPods(r *run, side string, template corev1.PodSpec, imageID string,
	pods []corev1.Pod, owners map[string]*appsv1.ReplicaSet) ([]pv.ApplicationContainerIdentity, error) {
	account := r.effect(resourceKey(side, "sa-replica"))
	namespace := r.effect(resourceKey(side, "namespace"))
	if account == nil || namespace == nil {
		return nil, contract.ErrConflict
	}
	var identities []pv.ApplicationContainerIdentity
	for index := range pods {
		pod := &pods[index]
		controller := metav1.GetControllerOf(pod)
		if controller == nil || owners[string(controller.UID)] == nil {
			continue
		}
		if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning ||
			len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready {
			continue
		}
		status := pod.Status.ContainerStatuses[0]
		if status.RestartCount != 0 || status.State.Running == nil || imageDigest(status.ImageID) != imageDigest(imageID) ||
			pod.Spec.ServiceAccountName != account.Name || !b.replicaPodMatches(template, pod.Spec) {
			return nil, contract.ErrConflict
		}
		identities = append(identities, pv.ApplicationContainerIdentity{PodName: pod.Name, PodUID: string(pod.UID),
			ContainerName: status.Name, ContainerID: status.ContainerID, ImageID: imageID,
			ServiceAccountName: account.Name, ServiceAccountUID: string(account.UID),
			Namespace: namespace.Name, NamespaceUID: string(namespace.UID)})
	}
	return identities, nil
}

func (b *Backend) validateReplicaEvidence(ctx context.Context, r *run, side string, observation pv.Observation) error {
	app := observation.Application
	if app == nil || app.Kubernetes == nil || app.Kubernetes.Resource != resourceDeployments || app.Kubernetes.APIVersion != appsAPIVersion {
		if app != nil && app.Replica != nil {
			return contract.ErrConflict
		}
		return nil
	}
	fixture, profile, err := replicaProfile(r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy)
	if err != nil || fixture == nil || app.Replica == nil || app.Kubernetes.Name != profile.ReplicaTarget.Name {
		return contract.ErrConflict
	}
	object, err := b.boundObject(ctx, r, resourceKey(side, "replica-deployment"))
	if err != nil {
		return err
	}
	deployment := object.(*appsv1.Deployment)
	template, found := r.state.ReplicaTemplates[side]
	if !found || !reflect.DeepEqual(template, deployment.Spec.Template) {
		return contract.ErrConflict
	}
	evidence := app.Replica
	if !replicaSummaryMatches(deployment, profile.ReplicaTarget, app) {
		return contract.ErrConflict
	}
	if err := b.captureReplicaDescendants(ctx, r, side, deployment, profile, false); err != nil {
		return err
	}
	sets := make(map[string]effect)
	for _, entry := range r.state.Effects {
		if entry.Kind == replicaSetKind && entry.GeneratedBy == deployment.UID && entry.UID != "" && !entry.Deleted {
			sets[string(entry.UID)] = entry
		}
	}
	for _, uid := range evidence.ReplicaSetUIDs {
		if _, known := sets[uid]; !known {
			return contract.ErrConflict
		}
	}
	seen := make(map[string]bool)
	for _, identity := range evidence.Pods {
		if seen[identity.PodUID] {
			return contract.ErrConflict
		}
		seen[identity.PodUID] = true
		if err := b.validateReplicaPod(ctx, r, side, identity, template.Spec, profile.ImageID, sets, evidence.ReplicaSetUIDs); err != nil {
			return err
		}
	}
	return nil
}

func replicaSummaryMatches(deployment *appsv1.Deployment, profile *pv.ApplicationReplicaTargetProfile, app *pv.ApplicationObservation) bool {
	evidence := app.Replica
	return evidence.DeploymentUID == string(deployment.UID) && evidence.DeploymentUID == app.Kubernetes.UID &&
		deployment.Spec.Replicas != nil && evidence.DesiredReplicas == *deployment.Spec.Replicas &&
		evidence.ReadyReplicas == deployment.Status.ReadyReplicas &&
		evidence.InitialReplicas == profile.InitialReplicas && evidence.MaxReplicas == profile.MaxReplicas &&
		len(evidence.Pods) <= int(profile.MaxReplicas) && int32(len(evidence.Pods)) == evidence.ReadyReplicas
}

func (b *Backend) validateReplicaPod(ctx context.Context, r *run, side string, identity pv.ApplicationContainerIdentity,
	template corev1.PodSpec, imageID string, sets map[string]effect, expectedSetUIDs []string) error {
	namespace := namespaceName(r, side)
	namespaceEffect := r.effect(resourceKey(side, "namespace"))
	if namespaceEffect == nil || namespaceEffect.UID == "" || namespaceEffect.Deleted ||
		identity.ImageID != imageID || identity.Namespace != namespace ||
		identity.NamespaceUID != string(namespaceEffect.UID) ||
		identity.ServiceAccountName != replicaAccountName {
		return contract.ErrConflict
	}
	account := r.effect(resourceKey(side, "sa-replica"))
	if account == nil || string(account.UID) != identity.ServiceAccountUID {
		return contract.ErrConflict
	}
	pod := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identity.PodName}, pod); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if string(pod.UID) != identity.PodUID || !pod.DeletionTimestamp.IsZero() || pod.Spec.NodeName != r.state.Target.NodeName ||
		pod.Spec.ServiceAccountName != identity.ServiceAccountName || !b.replicaPodMatches(template, pod.Spec) ||
		len(pod.Status.ContainerStatuses) != 1 {
		return contract.ErrConflict
	}
	status := pod.Status.ContainerStatuses[0]
	if !status.Ready || !containerIdentityMatches(status, identity, true) {
		return contract.ErrConflict
	}
	owned := false
	for uid, set := range sets {
		owned = owned || slices.Contains(expectedSetUIDs, uid) && exactAppsOwner(pod, replicaSetKind, set.Name, set.UID)
	}
	if !owned {
		return contract.ErrConflict
	}
	if err := b.rememberPod(ctx, r, resourceKey(side, "replica-pod-"+identity.PodUID),
		podReceipt{Namespace: pod.Namespace, Identity: identity}); err != nil {
		return err
	}
	return nil
}
