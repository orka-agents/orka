package applicationexec

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

// Admission is intentionally narrower than protocol syntax. Subject writes are
// limited to exact, precreated data objects. Namespace RBAC alone cannot safely
// authorize workload creation, CRDs, cluster roles, credentials, or arbitrary
// admission-affecting resources; those are not supported by this backend.
func (b *Backend) admit(request contract.ExecutionRequest) error {
	plan := request.Checks.Application
	if !b.validApplicationRequestShape(request) {
		return contract.ErrInvalid
	}
	names := make(map[string]bool)
	resources := slices.Clone(plan.Resources)
	reproduction, normal := false, false
	for _, check := range request.Checks.Checks {
		if err := pv.ValidateApplicationCheckWithPolicy(check, request.ApplicationPolicy); err != nil {
			return contract.ErrInvalid
		}
		reproduction = reproduction || check.Kind == pv.Reproduction
		normal = normal || check.Kind == pv.Normal
		resources = append(resources, check.Application.Resources...)
		actionObjects := make(map[string]bool)
		for _, action := range check.Application.Actions {
			object, err := pv.ValidateApplicationResourceWithPolicy(action.Resource, request.ApplicationPolicy)
			if err != nil {
				return contract.ErrInvalid
			}
			key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetName()
			if actionObjects[key] {
				// An update cannot freeze the UID of an object that another
				// action in this same observer has not created yet.
				return contract.ErrInvalid
			}
			actionObjects[key] = true
		}
	}
	if !reproduction || !normal || len(resources) > pv.MaxApplicationResources {
		return contract.ErrInvalid
	}
	fixture, _, err := replicaProfile(plan, request.ApplicationPolicy)
	if err != nil {
		return err
	}
	for _, permission := range plan.Permissions {
		for _, resource := range permission.Resources {
			if scalingPermission(permission.APIGroup, resource) && fixture == nil {
				return contract.ErrInvalid
			}
		}
	}
	for _, raw := range resources {
		object, err := pv.ValidateApplicationResourceWithPolicy(raw, request.ApplicationPolicy)
		if err != nil || strings.HasPrefix(object.GetName(), "orka-") {
			return contract.ErrInvalid
		}
		key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetName()
		if names[key] {
			// A check may add a new named fixture, but cannot overwrite another
			// check's inputs or an already-observed resource identity.
			return contract.ErrInvalid
		}
		names[key] = true
	}
	_, err = subjectRules(plan, request.Checks.Checks, request.ApplicationPolicy)
	return err
}

func (b *Backend) validApplicationRequestShape(request contract.ExecutionRequest) bool {
	plan := request.Checks.Application
	return plan != nil && reflect.DeepEqual(request.ApplicationPolicy, b.config.ResourcePolicy) &&
		pv.ValidateApplicationPlanWithPolicy(*plan, request.ApplicationPolicy) == nil &&
		plan.Build.GoVersion == b.config.GoVersion && b.allowedRuntime(plan.Runtime.Image, plan.Runtime.ImageID) &&
		len(request.Source.Paths) == 0 && len(request.Checks.Files) == 0 && len(request.Checks.Services) == 0 &&
		len(request.Checks.Requirements) == 0 && len(request.Checks.Checks) >= 2 && len(request.Checks.Checks) <= 8
}

func admittedResources(plan *pv.ApplicationPlan, checks []pv.Check, policy *pv.ApplicationPolicy) ([]*unstructured.Unstructured, error) {
	raw := slices.Clone(plan.Resources)
	for _, check := range checks {
		raw = append(raw, check.Application.Resources...)
		for _, action := range check.Application.Actions {
			raw = append(raw, action.Resource)
		}
	}
	objects := make([]*unstructured.Unstructured, 0, len(raw))
	for _, content := range raw {
		object, err := pv.ValidateApplicationResourceWithPolicy(content, policy)
		if err != nil {
			return nil, contract.ErrInvalid
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func subjectRules(plan *pv.ApplicationPlan, checks []pv.Check, policy *pv.ApplicationPolicy) ([]rbacv1.PolicyRule, error) {
	objects, err := admittedResources(plan, checks, policy)
	if err != nil {
		return nil, err
	}
	names := make(map[string][]string)
	for _, object := range objects {
		resource := resourceName(object.GetAPIVersion(), object.GetKind(), policy)
		if resource == "" {
			return nil, contract.ErrInvalid
		}
		key := object.GroupVersionKind().Group + "/" + resource
		names[key] = append(names[key], object.GetName())
	}
	for _, secret := range plan.SyntheticSecrets {
		names["/secrets"] = append(names["/secrets"], secret.Name)
	}
	var rules []rbacv1.PolicyRule
	for _, permission := range plan.Permissions {
		for _, resource := range permission.Resources {
			if scalingPermission(permission.APIGroup, resource) {
				// Scale/HPA authority is granted separately only after live
				// fail-closed admission has passed positive/negative dry runs.
				continue
			}
			if clusterReadResource(policy, permission.APIGroup, resource) != nil {
				// Cluster-scoped reads are never made effective by pretending
				// that a namespace RoleBinding can authorize them.
				continue
			}
			for _, verb := range permission.Verbs {
				rule := rbacv1.PolicyRule{APIGroups: []string{permission.APIGroup}, Resources: []string{resource}, Verbs: []string{verb},
					ResourceNames: slices.Clone(permission.ResourceNames)}
				switch verb {
				case verbGet, verbList, verbWatch:
					// These are data/Pod observations within the exact owned
					// namespace. Secrets, RBAC and exec are never in this list.
					allowedSecret := permission.APIGroup == "" && resource == "secrets" && policy != nil && len(plan.SyntheticSecrets) != 0
					if !allowedReadResource(permission.APIGroup, resource) && !policyResource(permission.APIGroup, resource, policy) && !allowedSecret {
						return nil, contract.ErrInvalid
					}
				case verbPatch, verbUpdate:
					base := strings.TrimSuffix(resource, "/status")
					allowed := permission.APIGroup == "" && (base == resourceServices || base == resourceConfigMaps) ||
						permission.APIGroup == networkingv1.GroupName && base == resourceIngresses ||
						policyResource(permission.APIGroup, resource, policy)
					if !allowed {
						return nil, contract.ErrInvalid
					}
					if len(rule.ResourceNames) == 0 {
						rule.ResourceNames = slices.Clone(names[permission.APIGroup+"/"+base])
					}
					if len(rule.ResourceNames) == 0 {
						return nil, contract.ErrInvalid
					}
					slices.Sort(rule.ResourceNames)
					rule.ResourceNames = slices.Compact(rule.ResourceNames)
				default:
					return nil, contract.ErrInvalid
				}
				rules = append(rules, rule)
			}
		}
	}
	if _, err := subjectClusterRules(plan, policy); err != nil {
		return nil, err
	}
	return rules, nil
}

func resourceName(apiVersion, kind string, policy *pv.ApplicationPolicy) string {
	if policy != nil {
		for _, rule := range policy.Resources {
			if rule.APIVersion == apiVersion && rule.Kind == kind {
				return rule.Resource
			}
		}
	}
	switch apiVersion + "/" + kind {
	case "v1/ConfigMap":
		return resourceConfigMaps
	case "v1/Service":
		return resourceServices
	case "networking.k8s.io/v1/Ingress":
		return resourceIngresses
	}
	return ""
}

func policyResource(group, resource string, policy *pv.ApplicationPolicy) bool {
	if policy == nil {
		return false
	}
	for _, rule := range policy.Resources {
		apiGroup, _, hasGroup := strings.Cut(rule.APIVersion, "/")
		if !hasGroup {
			apiGroup = ""
		}
		if apiGroup == group && (rule.Resource == resource || rule.Status && rule.Resource+"/status" == resource) {
			return true
		}
	}
	return false
}

func allowedReadResource(group, resource string) bool {
	switch group {
	case "":
		return slices.Contains([]string{resourceConfigMaps, resourceServices, resourceEndpoints, resourcePods, "pods/status", "services/status"}, resource)
	case networkingv1.GroupName:
		return resource == resourceIngresses || resource == "ingresses/status"
	case "discovery.k8s.io":
		return resource == resourceEndpointSlices
	default:
		return false
	}
}

func (b *Backend) validatePod(spec corev1.PodSpec) error {
	if !b.validPodShape(spec) || !validPodSecurity(spec.SecurityContext) {
		return ErrIsolation
	}
	for _, volume := range spec.Volumes {
		if !validVolume(volume) {
			return ErrIsolation
		}
	}
	containers := append(slices.Clone(spec.InitContainers), spec.Containers...)
	for _, container := range containers {
		if err := b.validateContainer(container); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) validPodShape(spec corev1.PodSpec) bool {
	if spec.HostNetwork || spec.HostPID || spec.HostIPC || ptr.Deref(spec.ShareProcessNamespace, false) ||
		ptr.Deref(spec.AutomountServiceAccountToken, true) || ptr.Deref(spec.EnableServiceLinks, true) ||
		len(spec.EphemeralContainers) != 0 || ptr.Deref(spec.RuntimeClassName, "") != b.config.RuntimeClassName ||
		spec.ServiceAccountName == "" || spec.ServiceAccountName == "default" ||
		spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		spec.RestartPolicy != corev1.RestartPolicyNever ||
		len(spec.Containers) != 1 || len(spec.InitContainers) > 1 || len(spec.ImagePullSecrets) != 0 ||
		len(spec.HostAliases) > pv.MaxApplicationResources || len(spec.SchedulingGates) != 0 || spec.PriorityClassName != "" {
		return false
	}
	return true
}

func validPodSecurity(security *corev1.PodSecurityContext) bool {
	return security != nil && ptr.Deref(security.RunAsNonRoot, false) &&
		ptr.Deref(security.RunAsUser, int64(0)) == 65532 && ptr.Deref(security.RunAsGroup, int64(0)) == 65532 &&
		security.SeccompProfile != nil && security.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault &&
		len(security.Sysctls) == 0 && security.SELinuxOptions == nil && security.WindowsOptions == nil
}

func validVolume(volume corev1.Volume) bool {
	count := 0
	for _, field := range reflect.ValueOf(volume.VolumeSource).Fields() {
		if !field.IsNil() {
			count++
		}
	}
	if count != 1 || (volume.EmptyDir == nil && volume.PersistentVolumeClaim == nil &&
		volume.ConfigMap == nil && volume.Secret == nil && volume.Projected == nil) {
		return false
	}
	return volume.EmptyDir == nil || volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.Sign() > 0
}

func validContainerSecurity(security *corev1.SecurityContext) bool {
	return security != nil && !ptr.Deref(security.Privileged, false) && !ptr.Deref(security.AllowPrivilegeEscalation, true) &&
		ptr.Deref(security.ReadOnlyRootFilesystem, false) && ptr.Deref(security.RunAsNonRoot, false) &&
		ptr.Deref(security.RunAsUser, int64(0)) == 65532 && ptr.Deref(security.RunAsGroup, int64(0)) == 65532 &&
		security.Capabilities != nil && len(security.Capabilities.Add) == 0 &&
		reflect.DeepEqual(security.Capabilities.Drop, []corev1.Capability{"ALL"}) &&
		security.ProcMount == nil && security.SELinuxOptions == nil && security.WindowsOptions == nil
}

func (b *Backend) validateContainer(container corev1.Container) error {
	if !validContainerSecurity(container.SecurityContext) || !b.allowedPodImage(container.Image) ||
		len(container.EnvFrom) != 0 || container.Lifecycle != nil || len(container.VolumeDevices) != 0 {
		return ErrIsolation
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		request, limit := container.Resources.Requests[name], container.Resources.Limits[name]
		if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			return ErrIsolation
		}
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 || port.HostIP != "" || port.ContainerPort < 1024 {
			return ErrIsolation
		}
	}
	for _, mount := range container.VolumeMounts {
		if mount.MountPropagation != nil || mount.SubPathExpr != "" {
			return ErrIsolation
		}
	}
	for _, env := range container.Env {
		if env.ValueFrom != nil && (env.ValueFrom.SecretKeyRef != nil || env.ValueFrom.ConfigMapKeyRef != nil ||
			env.ValueFrom.ResourceFieldRef != nil) {
			return ErrIsolation
		}
	}
	return nil
}

func (b *Backend) allowedPodImage(image string) bool {
	if image == b.config.WorkerImage || image == b.config.ToolchainImage ||
		image == b.config.RuntimeImage || b.config.RuntimeImages[image] != "" {
		return true
	}
	if b.config.ResourcePolicy != nil {
		for _, fixture := range b.config.ResourcePolicy.Fixtures {
			if fixture.Image == image {
				return true
			}
		}
	}
	return false
}

func marshalInput(value any) ([]byte, error) {
	content, err := json.Marshal(value)
	if err != nil || len(content) > pv.MaxPreparationInputBytes || pv.ContainsCredentialMaterial(content) {
		return nil, contract.ErrInvalid
	}
	return content, nil
}
