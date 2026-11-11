package applicationexec

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func replicaProfile(plan *pv.ApplicationPlan, policy *pv.ApplicationPolicy) (*pv.ApplicationFixture, *pv.ApplicationFixtureProfile, error) {
	if plan == nil {
		return nil, nil, nil
	}
	var selected *pv.ApplicationFixture
	var profile *pv.ApplicationFixtureProfile
	for index := range plan.Fixtures {
		fixture := &plan.Fixtures[index]
		if fixture.Kind != fixtureKindReplica {
			continue
		}
		if selected != nil || policy == nil {
			return nil, nil, contract.ErrInvalid
		}
		selected = fixture
		for i := range policy.Fixtures {
			if policy.Fixtures[i].ID == fixture.Profile {
				profile = &policy.Fixtures[i]
			}
		}
		if profile == nil || profile.ReplicaTarget == nil || profile.ReplicaTarget.Name != fixture.ID ||
			profile.ReplicaTarget.MaxReplicas < 1 || profile.ReplicaTarget.MaxReplicas > 4 ||
			profile.ReplicaTarget.InitialReplicas < 0 || profile.ReplicaTarget.InitialReplicas > profile.ReplicaTarget.MaxReplicas ||
			len(fixture.Config) != 0 || len(profile.ConfigKeys) != 0 || len(profile.SecretFiles) != 0 {
			return nil, nil, contract.ErrInvalid
		}
	}
	return selected, profile, nil
}

func scalingPermission(group, resource string) bool {
	return group == appsv1.GroupName && resource == resourceDeploymentScale ||
		group == "autoscaling" && resource == "horizontalpodautoscalers"
}

func (b *Backend) ensureReplica(ctx context.Context, r *run, side string) (bool, error) {
	fixture, profile, err := replicaProfile(r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy)
	if err != nil || fixture == nil {
		return err == nil, err
	}
	namespace := namespaceName(r, side)
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: replicaAccountName, Namespace: namespace}, AutomountServiceAccountToken: new(false)}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "sa-replica"), account); err != nil {
		return false, err
	}
	input, err := b.workerInput(r, side, "", roleReplica)
	if err != nil {
		return false, err
	}
	input.FixtureID, input.Worker.ContainerName, input.Worker.ImageID = fixture.ID, workerModeFixture, profile.ImageID
	name := profile.ReplicaTarget.Name
	expected, err := b.replicaDeployment(r, side, fixture, profile, input.TaskUID)
	if err != nil {
		return false, err
	}
	files, err := b.runtimeInputFiles(expected.Spec.Template.Spec.Containers[0].Resources, "execute")
	if err != nil {
		return false, err
	}
	content, err := marshalInput(input)
	if err != nil {
		return false, err
	}
	files["input.json"] = content
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-input", Namespace: namespace},
		Immutable: new(true), BinaryData: files}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "replica-input"), config); err != nil {
		return false, err
	}
	object, err := b.ensureObject(ctx, r, resourceKey(side, "replica-deployment"), expected)
	if err != nil {
		return false, err
	}
	actual := object.(*appsv1.Deployment)
	if !b.replicaDeploymentMatches(expected, actual, profile.ReplicaTarget.MaxReplicas) {
		return false, ErrIsolation
	}
	if previous, found := r.state.ReplicaTemplates[side]; found {
		if !reflect.DeepEqual(previous, actual.Spec.Template) {
			return false, contract.ErrConflict
		}
	} else {
		if r.state.ReplicaTemplates == nil {
			r.state.ReplicaTemplates = make(map[string]corev1.PodTemplateSpec)
		}
		r.state.ReplicaTemplates[side] = *actual.Spec.Template.DeepCopy()
		if err := b.save(ctx, r); err != nil {
			return false, err
		}
	}
	ready, err := b.ensureScalingAdmission(ctx, r, side, actual, profile.ReplicaTarget.MaxReplicas)
	if err != nil || !ready {
		return false, err
	}
	if err := b.ensureScalingRole(ctx, r, side, profile.ReplicaTarget.Name); err != nil {
		return false, err
	}
	return true, b.captureReplicaDescendants(ctx, r, side, expected, profile, false)
}

func (b *Backend) replicaDeployment(r *run, side string, fixture *pv.ApplicationFixture,
	profile *pv.ApplicationFixtureProfile, taskUID string) (*appsv1.Deployment, error) {
	name := profile.ReplicaTarget.Name
	templateJob, err := b.job(r, side, name, jobOptions{role: roleReplica, mode: workerModeFixture, image: profile.Image,
		taskUID: taskUID, serviceAccount: roleReplica, fixtureProfile: profile, target: fixture.ID})
	if err != nil {
		return nil, err
	}
	template := *templateJob.Spec.Template.DeepCopy()
	template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	// Deployment/ReplicaSet templates forbid Pod activeDeadlineSeconds.
	// The trusted supervisor enforces the immutable absolute run deadline.
	template.Spec.ActiveDeadlineSeconds = nil
	if !replicaRunDeadline(r, template.Spec) {
		return nil, contract.ErrConflict
	}
	return &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: deploymentKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespaceName(r, side)},
		Spec: appsv1.DeploymentSpec{Replicas: new(profile.ReplicaTarget.InitialReplicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{runLabel: string(r.object.UID), pv.ApplicationTargetLabel: fixture.ID}},
			Template: template, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			RevisionHistoryLimit: new(int32(0)), ProgressDeadlineSeconds: new(int32(300))}}, nil
}

func replicaRunDeadline(r *run, spec corev1.PodSpec) bool {
	deadline, err := replicaDeadline(spec)
	return err == nil && !r.state.Deadline.IsZero() && deadline.Equal(r.state.Deadline) &&
		time.Now().Before(deadline) && spec.ActiveDeadlineSeconds == nil
}

func (b *Backend) ensureScalingRole(ctx context.Context, r *run, side, name string) error {
	rules, err := scalingRules(r.state.Request.Checks.Application, name)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}
	namespace := namespaceName(r, side)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "orka-scaling", Namespace: namespace}, Rules: rules}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "scaling-role"), role); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: namespace},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Namespace: namespace, Name: subjectAccountName}}}
	_, err = b.ensureObject(ctx, r, resourceKey(side, "scaling-role-binding"), binding)
	return err
}

func scalingRules(plan *pv.ApplicationPlan, name string) ([]rbacv1.PolicyRule, error) {
	var rules []rbacv1.PolicyRule
	for _, permission := range plan.Permissions {
		for _, resource := range permission.Resources {
			if !scalingPermission(permission.APIGroup, resource) {
				continue
			}
			for _, verb := range permission.Verbs {
				rule := rbacv1.PolicyRule{APIGroups: []string{permission.APIGroup}, Resources: []string{resource}, Verbs: []string{verb},
					ResourceNames: slices.Clone(permission.ResourceNames)}
				if resource == resourceDeploymentScale {
					if !slices.Contains([]string{verbGet, verbPatch, verbUpdate}, verb) ||
						len(permission.ResourceNames) != 0 && !slices.Contains(permission.ResourceNames, name) {
						return nil, contract.ErrInvalid
					}
					rule.ResourceNames = []string{name}
				} else if !slices.Contains([]string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}, verb) {
					return nil, contract.ErrInvalid
				}
				rules = append(rules, rule)
			}
		}
	}
	return rules, nil
}

func (b *Backend) replicaDeploymentMatches(expected, actual *appsv1.Deployment, maximum int32) bool {
	if actual.Spec.Replicas == nil || *actual.Spec.Replicas < 0 || *actual.Spec.Replicas > maximum ||
		actual.Spec.Paused || actual.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType ||
		actual.Spec.Strategy.RollingUpdate != nil || !reflect.DeepEqual(expected.Spec.Selector, actual.Spec.Selector) ||
		!reflect.DeepEqual(expected.Spec.Template.Labels, actual.Spec.Template.Labels) ||
		!reflect.DeepEqual(expected.Spec.Template.Annotations, actual.Spec.Template.Annotations) ||
		actual.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways ||
		expected.Spec.Template.Spec.ActiveDeadlineSeconds != nil || actual.Spec.Template.Spec.ActiveDeadlineSeconds != nil {
		return false
	}
	return b.replicaPodMatches(expected.Spec.Template.Spec, actual.Spec.Template.Spec)
}

func (b *Backend) replicaPodMatches(expected, actual corev1.PodSpec) bool {
	if expected.RestartPolicy != corev1.RestartPolicyAlways || actual.RestartPolicy != corev1.RestartPolicyAlways ||
		expected.ActiveDeadlineSeconds != nil || actual.ActiveDeadlineSeconds != nil {
		return false
	}
	deadline, err := replicaDeadline(expected)
	if err != nil || !time.Now().Before(deadline) {
		return false
	}
	actualDeadline, err := replicaDeadline(actual)
	if err != nil || !actualDeadline.Equal(deadline) {
		return false
	}
	want, observed := *expected.DeepCopy(), *actual.DeepCopy()
	want.RestartPolicy, observed.RestartPolicy = corev1.RestartPolicyNever, corev1.RestartPolicyNever
	return b.podExecutionMatches(want, observed) && podPolicyMatches(want, observed) && b.validatePod(observed) == nil
}

func replicaDeadline(spec corev1.PodSpec) (time.Time, error) {
	if len(spec.Containers) != 1 {
		return time.Time{}, contract.ErrConflict
	}
	var value string
	found := false
	for _, variable := range spec.Containers[0].Env {
		if variable.Name != "ORKA_APPLICATION_DEADLINE" {
			continue
		}
		if found || variable.ValueFrom != nil {
			return time.Time{}, contract.ErrConflict
		}
		value, found = variable.Value, true
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !found || deadline.IsZero() {
		return time.Time{}, contract.ErrConflict
	}
	return deadline, nil
}

func exactAppsOwner(object client.Object, kind, name string, uid types.UID) bool {
	owners := object.GetOwnerReferences()
	return len(owners) == 1 && owners[0].APIVersion == appsv1.SchemeGroupVersion.String() && owners[0].Kind == kind &&
		owners[0].Name == name && owners[0].UID == uid && ptr.Deref(owners[0].Controller, false)
}

func (b *Backend) recordGenerated(ctx context.Context, r *run, side, prefix string, object client.Object, ownerUID types.UID) error {
	gvk, err := b.kind(object)
	if err != nil || object.GetUID() == "" || object.GetNamespace() != namespaceName(r, side) || ownerUID == "" {
		return contract.ErrConflict
	}
	for _, previous := range r.state.Effects {
		if previous.APIVersion == gvk.GroupVersion().String() && previous.Kind == gvk.Kind &&
			previous.Namespace == object.GetNamespace() && previous.Name == object.GetName() {
			if previous.UID != object.GetUID() || previous.GeneratedBy != ownerUID || previous.Deleted {
				return contract.ErrConflict
			}
			return nil
		}
	}
	if len(r.state.Effects) >= maxEffects {
		return contract.ErrInvalid
	}
	content, _ := json.Marshal([]string{gvk.String(), object.GetNamespace(), object.GetName(), string(object.GetUID()), string(ownerUID)})
	r.state.Effects = append(r.state.Effects, effect{Key: resourceKey(side, prefix+"-"+string(object.GetUID())),
		APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Namespace: object.GetNamespace(), Name: object.GetName(),
		UID: object.GetUID(), Digest: pv.Digest(content), Attempted: true, GeneratedBy: ownerUID})
	return b.save(ctx, r)
}

func (b *Backend) captureReplicaDescendants(ctx context.Context, r *run, side string, expected *appsv1.Deployment,
	profile *pv.ApplicationFixtureProfile, cleanup bool) error {
	entry := r.effect(resourceKey(side, "replica-deployment"))
	if entry == nil {
		return nil
	}
	if entry.UID == "" {
		return ErrUncertain
	}
	owned, err := b.captureReplicaSets(ctx, r, side, *entry, expected, profile, cleanup)
	if err != nil {
		return err
	}
	if err := b.captureReplicaPods(ctx, r, side, entry.Namespace, owned, expected, cleanup); err != nil {
		return err
	}
	return b.captureReplicaHPAs(ctx, r, side, *entry, profile, cleanup)
}

func (b *Backend) captureReplicaSets(ctx context.Context, r *run, side string, entry effect, expected *appsv1.Deployment,
	profile *pv.ApplicationFixtureProfile, cleanup bool) (map[types.UID]*appsv1.ReplicaSet, error) {
	sets := &appsv1.ReplicaSetList{}
	if err := b.targetReader.List(ctx, sets, client.InNamespace(entry.Namespace), client.Limit(3)); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if sets.Continue != "" || len(sets.Items) > 2 {
		return nil, ErrIsolation
	}
	owned := make(map[types.UID]*appsv1.ReplicaSet)
	for index := range sets.Items {
		set := &sets.Items[index]
		if !exactAppsOwner(set, deploymentKind, entry.Name, entry.UID) {
			return nil, contract.ErrConflict
		}
		if !cleanup && (expected == nil || profile == nil ||
			!b.replicaPodMatches(expected.Spec.Template.Spec, set.Spec.Template.Spec) ||
			ptr.Deref(set.Spec.Replicas, int32(-1)) < 0 || *set.Spec.Replicas > profile.ReplicaTarget.MaxReplicas) {
			return nil, ErrIsolation
		}
		if err := b.recordGenerated(ctx, r, side, "replica-set", set, entry.UID); err != nil {
			return nil, err
		}
		owned[set.UID] = set
	}
	for _, previous := range r.state.Effects {
		if previous.Kind == replicaSetKind && previous.GeneratedBy == entry.UID && previous.UID != "" && !previous.Deleted {
			if _, found := owned[previous.UID]; !found {
				owned[previous.UID] = &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: previous.Name, UID: previous.UID}}
			}
		}
	}
	return owned, nil
}

func (b *Backend) captureReplicaPods(ctx context.Context, r *run, side, namespace string,
	owned map[types.UID]*appsv1.ReplicaSet, expected *appsv1.Deployment, cleanup bool) error {
	pods := &corev1.PodList{}
	if err := b.targetReader.List(ctx, pods, client.InNamespace(namespace), client.Limit(64)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if pods.Continue != "" {
		return ErrIsolation
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		for _, owner := range pod.OwnerReferences {
			set := owned[owner.UID]
			if set == nil {
				continue
			}
			if !exactAppsOwner(pod, replicaSetKind, set.Name, set.UID) {
				return contract.ErrConflict
			}
			if !cleanup && (!b.replicaPodMatches(expected.Spec.Template.Spec, pod.Spec) ||
				pod.Spec.NodeName != "" && pod.Spec.NodeName != r.state.Target.NodeName) {
				return ErrIsolation
			}
			if err := b.recordGenerated(ctx, r, side, "replica-pod", pod, set.UID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) captureReplicaHPAs(ctx context.Context, r *run, side string, entry effect,
	profile *pv.ApplicationFixtureProfile, cleanup bool) error {
	hpas := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := b.targetReader.List(ctx, hpas, client.InNamespace(entry.Namespace), client.Limit(3)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if hpas.Continue != "" || len(hpas.Items) > 2 {
		return ErrIsolation
	}
	for i := range hpas.Items {
		hpa := &hpas.Items[i]
		if !cleanup && (profile == nil || hpa.Spec.ScaleTargetRef.APIVersion != appsAPIVersion ||
			hpa.Spec.ScaleTargetRef.Kind != deploymentKind || hpa.Spec.ScaleTargetRef.Name != entry.Name ||
			hpa.Spec.MaxReplicas < 1 || hpa.Spec.MaxReplicas > profile.ReplicaTarget.MaxReplicas) {
			return ErrIsolation
		}
		if err := b.recordGenerated(ctx, r, side, "replica-hpa", hpa, entry.UID); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) captureReplicaCleanup(ctx context.Context, r *run) error {
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		entry := r.effect(resourceKey(side, "replica-deployment"))
		if entry == nil || entry.Rejected {
			continue
		}
		present, err := b.cleanupNamespace(ctx, r, entry.Namespace)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if err := b.captureReplicaDescendants(ctx, r, side, nil, nil, true); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) stopReplicaControllers(ctx context.Context, r *run) (bool, error) {
	gone := true
	for index := range r.state.Effects {
		if r.state.Effects[index].Kind != deploymentKind && r.state.Effects[index].Kind != replicaSetKind {
			continue
		}
		done, err := b.deleteEffect(ctx, r, index)
		if err != nil {
			return false, err
		}
		gone = gone && done
	}
	return gone, nil
}
