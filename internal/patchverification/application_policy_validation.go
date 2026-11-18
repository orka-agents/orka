package patchverification

import (
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kjson "k8s.io/apimachinery/pkg/util/json"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var applicationKind = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)

func ValidateApplicationPolicy(policy ApplicationPolicy) error {
	if policy.Version != ApplicationProtocolVersion || len(policy.Resources) > 32 || len(policy.Actors) > 8 ||
		len(policy.SubjectPermissions) > 32 || len(policy.Fixtures) > 8 ||
		policy.MaxSyntheticSecrets < 0 || policy.MaxSyntheticSecrets > 16 ||
		!validManifestUTF8(reflect.ValueOf(policy)) {
		return ErrLimit
	}
	if err := validateApplicationSubjectEnvironment(policy.SubjectEnvironment); err != nil {
		return err
	}
	if err := validateApplicationFixtureCapabilities(policy); err != nil {
		return err
	}
	if err := validateApplicationPolicyResources(policy); err != nil {
		return err
	}
	for _, permission := range policy.SubjectPermissions {
		if err := applicationPolicyPermission(permission, &policy); err != nil {
			return err
		}
	}
	if err := validateApplicationPolicyActors(policy); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, fixture := range policy.Fixtures {
		if seen[fixture.ID] {
			return ErrEvidence
		}
		seen[fixture.ID] = true
		if err := validateApplicationCatalogueFixture(fixture); err != nil {
			return err
		}
	}
	content, err := json.Marshal(policy)
	if err != nil || len(content) > 128<<10 || ContainsCredentialMaterial(content) {
		return ErrLimit
	}
	return nil
}

func validateApplicationPolicyResources(policy ApplicationPolicy) error {
	seen := map[string]bool{}
	for _, rule := range policy.Resources {
		version, err := schema.ParseGroupVersion(rule.APIVersion)
		key := rule.APIVersion + "/" + rule.Resource
		if err != nil || !applicationName(version.Version) ||
			!applicationName(rule.Resource) || !applicationKind.MatchString(rule.Kind) ||
			seen[key] || len(rule.Verbs) == 0 || len(rule.Verbs) > 7 || applicationForbiddenRuleGroup(version.Group, rule) {
			return errors.New("application policy requires exact bounded data resources without control-plane authority")
		}
		seen[key] = true
		if err := validateApplicationReplicaRule(rule, &policy); err != nil {
			return err
		}
		if err := validateApplicationRuleScope(rule, version.Group); err != nil {
			return err
		}
		if err := validateApplicationRuleCRD(rule, version.Group); err != nil {
			return err
		}
		if err := validateApplicationRuleVerbs(rule); err != nil {
			return err
		}
	}
	return nil
}

func applicationForbiddenRuleGroup(group string, rule ApplicationResourceRule) bool {
	return strings.HasPrefix(group, "authorization.") || strings.HasPrefix(group, "authentication.") ||
		strings.HasPrefix(group, "rbac.") || strings.HasPrefix(group, "admissionregistration.") ||
		group == "apiextensions.k8s.io" || group == "batch" ||
		group == applicationAppsGroup && !applicationReplicaRule(rule)
}

func validateApplicationRuleScope(rule ApplicationResourceRule, group string) error {
	if rule.Namespaced {
		return nil
	}
	if rule.CRD == nil || rule.Status || applicationBuiltinAPIGroup(group) {
		return errors.New("cluster application reads require an explicitly scoped custom CRD, never builtin APIs or status authority")
	}
	for _, verb := range rule.Verbs {
		if !slices.Contains([]string{ApplicationVerbGet, ApplicationVerbList, ApplicationVerbWatch}, verb) {
			return errors.New("cluster application resources support subject read-only access only")
		}
	}
	return nil
}

func validateApplicationRuleCRD(rule ApplicationResourceRule, group string) error {
	if rule.CRD == nil {
		expected := applicationBuiltinRule(rule.APIVersion, rule.Kind)
		if expected == nil || rule.Resource != expected.Resource {
			return errors.New("custom application resources require a frozen preinstalled CRD identity")
		}
	} else if applicationBuiltinAPIGroup(group) || len(kvalidation.IsDNS1123Subdomain(group)) != 0 ||
		rule.CRD.Name != rule.Resource+"."+group || !identifierPattern.MatchString(rule.CRD.UID) ||
		!sha256Pattern.MatchString(rule.CRD.SchemaDigest) {
		return ErrBinding
	}
	return nil
}

func validateApplicationRuleVerbs(rule ApplicationResourceRule) error {
	verbs := map[string]bool{}
	for _, verb := range rule.Verbs {
		if !slices.Contains([]string{ApplicationVerbGet, ApplicationVerbList, ApplicationVerbWatch, ApplicationActionCreate, ApplicationActionUpdate, ApplicationVerbPatch, "delete"}, verb) || verbs[verb] {
			return ErrEvidence
		}
		verbs[verb] = true
		if rule.APIVersion == "v1" && (rule.Resource == applicationPods || rule.Resource == applicationSecrets) &&
			!slices.Contains([]string{ApplicationVerbGet, ApplicationVerbList, ApplicationVerbWatch}, verb) {
			return errors.New("application policy cannot mutate Pods or credential resources")
		}
	}
	return nil
}

func validateApplicationPolicyActors(policy ApplicationPolicy) error {
	seen := map[string]bool{}
	for _, actor := range policy.Actors {
		if !applicationName(actor.ID) || seen[actor.ID] || len(actor.Permissions) > 32 {
			return ErrEvidence
		}
		seen[actor.ID] = true
		for _, permission := range actor.Permissions {
			if err := applicationPolicyPermission(permission, &policy); err != nil {
				return err
			}
			if applicationPermissionUsesCluster(permission, &policy) {
				return errors.New("application actors cannot receive cluster resource authority")
			}
			if permission.APIGroup == applicationAppsGroup || permission.APIGroup == "autoscaling" {
				return errors.New("application actors cannot drive the subject-owned replica primitive")
			}
		}
	}
	return nil
}

func validateApplicationCatalogueFixture(fixture ApplicationFixtureProfile) error {
	if !applicationName(fixture.ID) || !applicationPinnedImage(fixture.Image) ||
		!validResolvedImageID(fixture.ImageID) || len(fixture.Args) == 0 || len(fixture.Args) > 64 ||
		len(fixture.Ports) == 0 && fixture.ReplicaTarget == nil || len(fixture.Ports) > 8 || len(fixture.ConfigKeys) > 16 {
		return ErrEvidence
	}
	if !applicationFixtureExecutable(fixture.Args[0]) {
		return ErrEvidence
	}
	for _, arg := range fixture.Args {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\r\n\x00") {
			return ErrLimit
		}
	}
	for _, port := range fixture.Ports {
		if port < 1024 || port > 65535 {
			return ErrEvidence
		}
	}
	for _, key := range fixture.ConfigKeys {
		if len(key) > 128 || !sourceValidPath(key) {
			return ErrEvidence
		}
	}
	return nil
}

func applicationFixtureExecutable(executable string) bool {
	return path.IsAbs(executable) && path.Clean(executable) == executable &&
		!strings.HasPrefix(executable, "/work/") && !strings.HasPrefix(executable, "/fixture-config/") &&
		!strings.HasPrefix(executable, "/proc/") && !strings.HasPrefix(executable, "/sys/") &&
		!strings.HasPrefix(executable, "/dev/") && executable != ApplicationWorkerPath && executable != "/runner/worker"
}

func applicationBuiltinAPIGroup(group string) bool {
	if group == "" || group == "apiextensions.k8s.io" || group == "apiregistration.k8s.io" || group == "metrics.k8s.io" {
		return true
	}
	for _, version := range clientgoscheme.Scheme.PrioritizedVersionsAllGroups() {
		if version.Group == group {
			return true
		}
	}
	return false
}

func applicationPermissionUsesCluster(permission ApplicationPermission, policy *ApplicationPolicy) bool {
	if policy == nil {
		return false
	}
	for _, resource := range permission.Resources {
		base, _, _ := strings.Cut(resource, "/")
		for _, rule := range policy.Resources {
			version, err := schema.ParseGroupVersion(rule.APIVersion)
			if err == nil && version.Group == permission.APIGroup && rule.Resource == base && !rule.Namespaced {
				return true
			}
		}
	}
	return false
}

func ApplicationPolicyDigest(policy ApplicationPolicy) (string, error) {
	if err := ValidateApplicationPolicy(policy); err != nil {
		return "", err
	}
	content, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	return Digest(content), nil
}

func ValidateApplicationPlanWithPolicy(plan ApplicationPlan, policy *ApplicationPolicy) error {
	if policy != nil {
		if err := ValidateApplicationPolicy(*policy); err != nil {
			return err
		}
	}
	return validateApplicationPlan(plan, policy)
}

func ValidateApplicationCheckWithPolicy(check Check, policy *ApplicationPolicy) error {
	if policy != nil {
		if err := ValidateApplicationPolicy(*policy); err != nil {
			return err
		}
	}
	return validateApplicationCheck(check, policy)
}

func applicationBuiltinRule(version, kind string) *ApplicationResourceRule {
	var resource string
	switch version + "/" + kind {
	case "v1/ConfigMap":
		resource = "configmaps"
	case "v1/Service":
		resource = "services"
	case "v1/Endpoints":
		resource = "endpoints"
	case "v1/Pod":
		resource = applicationPods
	case "v1/Secret":
		resource = applicationSecrets
	case "networking.k8s.io/v1/Ingress":
		resource = "ingresses"
	case "discovery.k8s.io/v1/EndpointSlice":
		resource = "endpointslices"
	case "apps/v1/Deployment":
		resource = applicationDeployments
	case "autoscaling/v2/HorizontalPodAutoscaler":
		resource = "horizontalpodautoscalers"
	default:
		return nil
	}
	return &ApplicationResourceRule{APIVersion: version, Kind: kind, Resource: resource, Namespaced: true}
}

func ApplicationResourceRuleFor(policy *ApplicationPolicy, version, kind string) (ApplicationResourceRule, error) {
	if policy != nil {
		for _, rule := range policy.Resources {
			if rule.APIVersion == version && rule.Kind == kind {
				return rule, nil
			}
		}
	}
	if builtin := applicationBuiltinRule(version, kind); builtin != nil {
		return *builtin, nil
	}
	return ApplicationResourceRule{}, ErrEvidence
}

func applicationAPIVersionWithPolicy(version, resource string, policy *ApplicationPolicy) bool {
	if resource == applicationSecrets {
		return false
	}
	if applicationAPIVersion(version, resource) {
		return true
	}
	if policy != nil {
		for _, rule := range policy.Resources {
			if rule.APIVersion == version && rule.Resource == resource && rule.Namespaced && rule.Status && slices.Contains(rule.Verbs, ApplicationVerbGet) {
				return true
			}
		}
	}
	return false
}

func applicationPolicyPermission(permission ApplicationPermission, policy *ApplicationPolicy) error {
	if policy == nil {
		return validateApplicationPermission(permission)
	}
	if len(permission.Resources) == 0 || len(permission.Resources) > 16 ||
		len(permission.Verbs) == 0 || len(permission.Verbs) > 7 || len(permission.ResourceNames) > 32 {
		return ErrLimit
	}
	for _, name := range permission.ResourceNames {
		if !applicationName(name) || strings.HasPrefix(name, "orka-") {
			return ErrEvidence
		}
	}
	for _, resource := range permission.Resources {
		base, subresource, _ := strings.Cut(resource, "/")
		found := false
		for _, rule := range policy.Resources {
			version, err := schema.ParseGroupVersion(rule.APIVersion)
			if err != nil || version.Group != permission.APIGroup || rule.Resource != base {
				continue
			}
			verbs := rule.Verbs
			if subresource == "scale" {
				profile := ApplicationReplicaTargetFor(policy)
				if !applicationReplicaRule(rule) || !rule.Scale || profile == nil ||
					len(permission.ResourceNames) != 1 || permission.ResourceNames[0] != profile.ReplicaTarget.Name {
					continue
				}
				verbs = []string{ApplicationVerbGet, ApplicationVerbPatch, ApplicationActionUpdate}
			} else if subresource != "" && (subresource != "status" || !rule.Status) {
				continue
			}
			allowed := true
			for _, verb := range permission.Verbs {
				if !slices.Contains(verbs, verb) {
					allowed = false
				}
			}
			found = found || allowed
		}
		if !found {
			return errors.New("application permission is outside the operator-frozen resource policy")
		}
	}
	return nil
}

func applicationPermissionSubset(request ApplicationPermission, bounds []ApplicationPermission) bool {
	for _, resource := range request.Resources {
		for _, verb := range request.Verbs {
			found := false
			for _, bound := range bounds {
				if bound.APIGroup != request.APIGroup || !slices.Contains(bound.Resources, resource) || !slices.Contains(bound.Verbs, verb) {
					continue
				}
				names := len(bound.ResourceNames) == 0
				if len(request.ResourceNames) != 0 {
					names = true
					for _, name := range request.ResourceNames {
						names = names && (len(bound.ResourceNames) == 0 || slices.Contains(bound.ResourceNames, name))
					}
				}
				found = found || names
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func ValidateApplicationResourceWithPolicy(raw json.RawMessage, policy *ApplicationPolicy) (*unstructured.Unstructured, error) {
	if policy == nil {
		return ValidateApplicationResource(raw)
	}
	if err := ValidateApplicationPolicy(*policy); err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 32<<10 || ContainsCredentialMaterial(raw) {
		return nil, ErrLimit
	}
	var object map[string]any
	// Thaw frozen numeric input using Kubernetes' int64-preserving decoder.
	if kjson.Unmarshal(raw, &object) != nil || object == nil {
		return nil, ErrEvidence
	}
	value := &unstructured.Unstructured{Object: object}
	rule, err := ApplicationResourceRuleFor(policy, value.GetAPIVersion(), value.GetKind())
	if err != nil {
		return nil, err
	}
	if !rule.Namespaced {
		return nil, errors.New("application resources and actions cannot create or mutate cluster-scoped objects")
	}
	if applicationReplicaRule(rule) || applicationHPARule(rule) {
		return nil, errors.New("deployment and HPA objects belong to the bounded subject-owned replica primitive, not model resources or actors")
	}
	if rule.CRD == nil {
		return ValidateApplicationResource(raw)
	}
	if !applicationOnlyKeys(object, "apiVersion", "kind", "metadata", "spec") || !applicationName(value.GetName()) ||
		strings.HasPrefix(value.GetName(), "orka-") {
		return nil, ErrEvidence
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok || !applicationOnlyKeys(metadata, "name", "labels", "annotations") {
		return nil, ErrEvidence
	}
	for _, key := range []string{"labels", "annotations"} {
		if raw, exists := metadata[key]; exists {
			entries, ok := raw.(map[string]any)
			if !ok || len(entries) > 16 {
				return nil, ErrLimit
			}
			for name, raw := range entries {
				text, ok := raw.(string)
				if !ok || len(text) > 1024 || len(kvalidation.IsQualifiedName(name)) != 0 ||
					strings.Contains(name, "orka.ai/") || strings.ContainsAny(text, "\r\n\x00") {
					return nil, ErrEvidence
				}
			}
		}
	}
	if _, ok := object["spec"].(map[string]any); !ok {
		return nil, errors.New("application custom resource requires bounded data-only spec")
	}
	return value, nil
}

func validateApplicationResourcesWithPolicy(resources []json.RawMessage, policy *ApplicationPolicy) error {
	if len(resources) > MaxApplicationResources {
		return ErrLimit
	}
	seen := map[string]bool{}
	for _, raw := range resources {
		object, err := ValidateApplicationResourceWithPolicy(raw, policy)
		if err != nil {
			return err
		}
		key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetName()
		if seen[key] {
			return ErrEvidence
		}
		seen[key] = true
	}
	return nil
}

func validateApplicationActions(actions []ApplicationAction, policy *ApplicationPolicy) error {
	if len(actions) > 4 || len(actions) != 0 && policy == nil {
		return ErrLimit
	}
	seen := map[string]bool{}
	for _, action := range actions {
		actorFound := false
		for _, actor := range policy.Actors {
			actorFound = actorFound || actor.ID == action.ActorID
		}
		if !actorFound || (action.Verb != ApplicationActionCreate && action.Verb != ApplicationActionUpdate) ||
			(action.ExpectedStatus != 403 && (action.Verb == ApplicationActionCreate && action.ExpectedStatus != 201 ||
				action.Verb == ApplicationActionUpdate && action.ExpectedStatus != 200)) {
			return ErrEvidence
		}
		object, err := ValidateApplicationResourceWithPolicy(action.Resource, policy)
		if err != nil {
			return err
		}
		rule, err := ApplicationResourceRuleFor(policy, object.GetAPIVersion(), object.GetKind())
		if err != nil || !slices.Contains(rule.Verbs, action.Verb) {
			return errors.New("application action is not admitted by the fixed target policy")
		}
		key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetName()
		if seen[key] {
			return errors.New("application action targets must be unique within one check")
		}
		seen[key] = true
	}
	return nil
}

func validateApplicationSyntheticSecrets(plan ApplicationPlan, policy *ApplicationPolicy) error {
	if len(plan.SyntheticSecrets) == 0 {
		return nil
	}
	if policy == nil || len(plan.SyntheticSecrets) > policy.MaxSyntheticSecrets {
		return errors.New("synthetic application Secrets require explicit bounded operator approval")
	}
	names, markers := map[string]bool{}, map[string]bool{}
	for _, fixture := range plan.Fixtures {
		markers[fixture.ID] = true
	}
	for _, secret := range plan.SyntheticSecrets {
		if !applicationName(secret.Name) || strings.HasPrefix(secret.Name, "orka-") || names[secret.Name] ||
			len(secret.Keys) == 0 || len(secret.Keys) > 8 {
			return ErrEvidence
		}
		names[secret.Name] = true
		keys := map[string]bool{}
		for _, key := range secret.Keys {
			if len(kvalidation.IsConfigMapKey(key.Name)) != 0 || keys[key.Name] ||
				!applicationName(key.MarkerID) || markers[key.MarkerID] || key.MarkerID == ApplicationSubject {
				return ErrEvidence
			}
			keys[key.Name], markers[key.MarkerID] = true, true
		}
	}
	return nil
}

func ApplicationReceiveExpectation(connected, markerMatched bool) (Expectation, error) {
	if markerMatched && !connected {
		return Expectation{}, ErrEvidence
	}
	content, err := json.Marshal(ApplicationTCPValue{Connected: connected, MarkerMatched: markerMatched})
	return Expectation{Stdout: string(content)}, err
}
