package applicationexec

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func scalingAdmissionName(r *run, side, kind string) string {
	return "orka-app-" + kind + "-" + strings.TrimPrefix(pv.Digest([]byte(string(r.object.UID)+"\x00"+side)), "sha256:")[:24]
}

func scalingPolicies(r *run, side string, deployment *appsv1.Deployment, maximum int32) ([]*admissionv1.ValidatingAdmissionPolicy, []*admissionv1.ValidatingAdmissionPolicyBinding) {
	namespace, name, uid := strconv.Quote(deployment.Namespace), strconv.Quote(deployment.Name), strconv.Quote(string(deployment.UID))
	limit := strconv.Itoa(int(maximum))
	namespaceSelector := &metav1.LabelSelector{MatchLabels: map[string]string{
		corev1.LabelMetadataName: deployment.Namespace, runLabel: string(r.object.UID),
	}}
	definitions := []struct {
		id, group, version, resource, expression string
		operations                               []admissionv1.OperationType
	}{
		{"hpa", "autoscaling", "v2", "horizontalpodautoscalers",
			"params.metadata.uid == " + uid + " && object.metadata.namespace == " + namespace +
				" && object.spec.scaleTargetRef.apiVersion == 'apps/v1' && object.spec.scaleTargetRef.kind == 'Deployment'" +
				" && object.spec.scaleTargetRef.name == " + name +
				" && object.spec.maxReplicas >= 1 && object.spec.maxReplicas <= " + limit +
				" && (!has(object.spec.minReplicas) || (object.spec.minReplicas >= 1 && object.spec.minReplicas <= object.spec.maxReplicas))",
			[]admissionv1.OperationType{admissionv1.Create, admissionv1.Update}},
		{"scale", appsv1.GroupName, "v1", resourceDeploymentScale,
			"params.metadata.uid == " + uid + " && object.metadata.namespace == " + namespace +
				" && object.metadata.name == " + name + " && object.spec.replicas >= 0 && object.spec.replicas <= " + limit,
			[]admissionv1.OperationType{admissionv1.Update}},
	}
	policies := make([]*admissionv1.ValidatingAdmissionPolicy, 0, len(definitions))
	bindings := make([]*admissionv1.ValidatingAdmissionPolicyBinding, 0, len(definitions))
	for _, definition := range definitions {
		policyName := scalingAdmissionName(r, side, definition.id)
		policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec: admissionv1.ValidatingAdmissionPolicySpec{
				FailurePolicy: new(admissionv1.Fail),
				ParamKind:     &admissionv1.ParamKind{APIVersion: appsAPIVersion, Kind: deploymentKind},
				MatchConstraints: &admissionv1.MatchResources{NamespaceSelector: namespaceSelector.DeepCopy(),
					ObjectSelector: &metav1.LabelSelector{}, MatchPolicy: new(admissionv1.Equivalent),
					ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
						Operations: definition.operations, Rule: admissionv1.Rule{APIGroups: []string{definition.group},
							APIVersions: []string{definition.version}, Resources: []string{definition.resource}, Scope: new(admissionv1.NamespacedScope)},
					}}}},
				Validations: []admissionv1.Validation{{Expression: definition.expression, Message: "application replica fixture boundary", Reason: new(metav1.StatusReasonForbidden)}},
			}}
		binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName: policyName, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
				ParamRef: &admissionv1.ParamRef{Name: deployment.Name, Namespace: deployment.Namespace, ParameterNotFoundAction: new(admissionv1.DenyAction)},
				MatchResources: &admissionv1.MatchResources{NamespaceSelector: namespaceSelector.DeepCopy(),
					ObjectSelector: &metav1.LabelSelector{}, MatchPolicy: new(admissionv1.Equivalent)},
			}}
		policies, bindings = append(policies, policy), append(bindings, binding)
	}
	return policies, bindings
}

func (b *Backend) ensureScalingAdmission(ctx context.Context, r *run, side string, deployment *appsv1.Deployment, maximum int32) (bool, error) {
	if maximum < 1 || maximum > 4 || deployment.UID == "" || deployment.Namespace != namespaceName(r, side) {
		return false, contract.ErrInvalid
	}
	policies, bindings := scalingPolicies(r, side, deployment, maximum)
	ready := true
	var identities []string
	for index, policy := range policies {
		object, err := b.ensureObject(ctx, r, resourceKey(side, "scaling-policy-"+strconv.Itoa(index)), policy)
		if err != nil {
			return false, err
		}
		actual := object.(*admissionv1.ValidatingAdmissionPolicy)
		identities = append(identities, string(actual.UID))
		if actual.Status.ObservedGeneration != actual.Generation || actual.Status.TypeChecking == nil {
			ready = false
		} else if len(actual.Status.TypeChecking.ExpressionWarnings) != 0 {
			return false, ErrIsolation
		}
		binding, err := b.ensureObject(ctx, r, resourceKey(side, "scaling-binding-"+strconv.Itoa(index)), bindings[index])
		if err != nil {
			return false, err
		}
		identities = append(identities, string(binding.GetUID()))
	}
	if !ready {
		return false, nil
	}
	content, _ := json.Marshal(struct {
		DeploymentUID string
		Maximum       int32
		Policies      []string
	}{string(deployment.UID), maximum, identities})
	digest := pv.Digest(content)
	if previous := r.state.ScalingAdmission[side]; previous != "" {
		if previous != digest {
			return false, contract.ErrConflict
		}
		return true, nil
	}
	// Policy existence/typechecking is not proof of enforcement. The same
	// authorized management identity must pass bounded positive dry runs and
	// be denied for both wrong targets and out-of-bound desired replica values.
	if err := b.probeScalingAdmission(ctx, deployment, maximum); err != nil {
		return false, err
	}
	if r.state.ScalingAdmission == nil {
		r.state.ScalingAdmission = make(map[string]string)
	}
	r.state.ScalingAdmission[side] = digest
	return true, b.save(ctx, r)
}

func (b *Backend) probeScalingAdmission(ctx context.Context, deployment *appsv1.Deployment, maximum int32) error {
	for _, probes := range scalingHPAProbes(deployment, maximum) {
		if err := b.probeHPAAdmission(ctx, probes); err != nil {
			return err
		}
	}
	return b.probeScaleAdmission(ctx, deployment, maximum)
}

type hpaAdmissionProbes struct {
	allowed client.Object
	denied  []client.Object
}

func scalingHPAProbes(deployment *appsv1.Deployment, maximum int32) []hpaAdmissionProbes {
	v2 := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "orka-admission-probe", Namespace: deployment.Namespace},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: appsAPIVersion, Kind: deploymentKind, Name: deployment.Name},
			MinReplicas:    new(int32(1)), MaxReplicas: maximum,
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: new(int32(50))}}}},
		}}
	v1 := &autoscalingv1.HorizontalPodAutoscaler{ObjectMeta: v2.ObjectMeta,
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{APIVersion: appsAPIVersion, Kind: deploymentKind, Name: deployment.Name},
			MinReplicas:    new(int32(1)), MaxReplicas: maximum, TargetCPUUtilizationPercentage: new(int32(50)),
		}}
	wrongV2, excessiveV2, replicaSetV2 := v2.DeepCopy(), v2.DeepCopy(), v2.DeepCopy()
	wrongV2.Spec.ScaleTargetRef.Name = "orka-unowned-target"
	excessiveV2.Spec.MaxReplicas = maximum + 1
	replicaSetV2.Spec.ScaleTargetRef.Kind = replicaSetKind
	wrongV1, excessiveV1, replicaSetV1 := v1.DeepCopy(), v1.DeepCopy(), v1.DeepCopy()
	wrongV1.Spec.ScaleTargetRef.Name = "orka-unowned-target"
	excessiveV1.Spec.MaxReplicas = maximum + 1
	replicaSetV1.Spec.ScaleTargetRef.Kind = replicaSetKind
	return []hpaAdmissionProbes{
		{allowed: v1, denied: []client.Object{wrongV1, excessiveV1, replicaSetV1}},
		{allowed: v2, denied: []client.Object{wrongV2, excessiveV2, replicaSetV2}},
	}
}

func (b *Backend) probeHPAAdmission(ctx context.Context, probes hpaAdmissionProbes) error {
	dryRun := &client.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict}
	if err := b.target.Create(ctx, probes.allowed, dryRun); err != nil {
		return safeError(ErrIsolation, err)
	}
	for _, invalid := range probes.denied {
		err := b.target.Create(ctx, invalid, dryRun)
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "application replica fixture boundary") {
			return safeError(ErrIsolation, err)
		}
	}
	return nil
}

func (b *Backend) probeScaleAdmission(ctx context.Context, deployment *appsv1.Deployment, maximum int32) error {
	scale, err := b.targetKube.AppsV1().Deployments(deployment.Namespace).GetScale(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		return safeError(ErrIsolation, err)
	}
	if scale.UID != deployment.UID || scale.Namespace != deployment.Namespace || scale.Name != deployment.Name {
		return contract.ErrConflict
	}
	scale.Spec.Replicas = 1
	options := metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict}
	if _, err := b.targetKube.AppsV1().Deployments(deployment.Namespace).UpdateScale(ctx, deployment.Name, scale.DeepCopy(), options); err != nil {
		return safeError(ErrIsolation, err)
	}
	scale.Spec.Replicas = maximum + 1
	_, err = b.targetKube.AppsV1().Deployments(deployment.Namespace).UpdateScale(ctx, deployment.Name, scale, options)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "application replica fixture boundary") {
		return safeError(ErrIsolation, err)
	}
	return nil
}
