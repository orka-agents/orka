package patchverification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kjson "k8s.io/apimachinery/pkg/util/json"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

var applicationGoVersion = regexp.MustCompile(`^go1\.[0-9]+\.[0-9]+$`)
var applicationPackage = regexp.MustCompile(`^(?:\.|\./[A-Za-z0-9_][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_][A-Za-z0-9_.-]*)*)$`)

func ApplicationPlanDigest(plan ApplicationPlan) (string, error) {
	if !validManifestUTF8(reflect.ValueOf(plan)) {
		return "", ErrEvidence
	}
	content, err := json.Marshal(plan)
	if err != nil {
		return "", ErrEvidence
	}
	return Digest(content), nil
}

func applicationPinnedImage(image string) bool {
	name, digest, ok := strings.Cut(image, "@")
	return ok && name != "" && sha256Pattern.MatchString(digest) &&
		!strings.ContainsAny(image, " \t\r\n\x00") && !strings.Contains(name, "://")
}

func applicationName(name string) bool {
	return name != "" && len(kvalidation.IsDNS1123Label(name)) == 0
}

func ValidateApplicationPlan(plan ApplicationPlan) error {
	return ValidateApplicationPlanWithPolicy(plan, nil)
}

func validateApplicationPlan(plan ApplicationPlan, policy *ApplicationPolicy) error {
	if err := validateApplicationBuildRuntime(plan); err != nil {
		return err
	}
	if err := validateApplicationRuntimeData(plan.Runtime); err != nil {
		return err
	}
	if err := validateApplicationPlanFixtures(plan.Fixtures, policy); err != nil {
		return err
	}
	if err := validateApplicationResourcesWithPolicy(plan.Resources, policy); err != nil {
		return err
	}
	for _, permission := range plan.Permissions {
		if err := applicationPolicyPermission(permission, policy); err != nil {
			return err
		}
		if policy != nil && !applicationPermissionSubset(permission, policy.SubjectPermissions) {
			return errors.New("application subject authority exceeds the fixed operator bounds")
		}
	}
	if err := validateApplicationSyntheticSecrets(plan, policy); err != nil {
		return err
	}
	if err := validateApplicationFixtureSelection(plan, policy); err != nil {
		return err
	}
	content, err := json.Marshal(plan)
	if err != nil || len(content) > MaxApplicationInputBytes/2 || ContainsCredentialMaterial(content) {
		return ErrLimit
	}
	return nil
}

func validateApplicationBuildRuntime(plan ApplicationPlan) error {
	if plan.Version != ApplicationProtocolVersion || !validManifestUTF8(reflect.ValueOf(plan)) ||
		plan.Build.Profile != ApplicationBuildGoPackage || !applicationGoVersion.MatchString(plan.Build.GoVersion) ||
		!applicationPackage.MatchString(plan.Build.Package) ||
		(plan.Build.Package != "." && !sourceValidPath(strings.TrimPrefix(plan.Build.Package, "./"))) ||
		!applicationPinnedImage(plan.Runtime.Image) || !validResolvedImageID(plan.Runtime.ImageID) {
		return errors.New("application requires a supported fixed Go package build and pinned runtime image")
	}
	if len(plan.Runtime.Args) > 64 || len(plan.Runtime.Ports) > 16 ||
		len(plan.Resources) > MaxApplicationResources || len(plan.Permissions) > 16 || len(plan.Fixtures) > 8 {
		return ErrLimit
	}
	return nil
}

func validateApplicationRuntimeData(runtime ApplicationRuntime) error {
	for _, arg := range runtime.Args {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\r\n\x00") || !utf8.ValidString(arg) ||
			ContainsCredentialMaterial([]byte(arg)) {
			return errors.New("application arguments must be bounded data without credentials")
		}
	}
	ports := map[int]bool{}
	for _, port := range runtime.Ports {
		if port < 1024 || port > 65535 || ports[port] {
			return errors.New("application listener ports must be unique and unprivileged")
		}
		ports[port] = true
	}
	return nil
}

func validateApplicationPlanFixtures(values []ApplicationFixture, policy *ApplicationPolicy) error {
	fixtures := map[string]bool{}
	for _, fixture := range values {
		portAllowed := fixture.Port >= 1024 && fixture.Port <= 65535 ||
			fixture.Kind == ApplicationFixtureReplica && fixture.Port == 0
		if !applicationName(fixture.ID) || fixture.ID == ApplicationSubject || fixtures[fixture.ID] ||
			!portAllowed {
			return errors.New("invalid or duplicate application marker fixture")
		}
		fixtures[fixture.ID] = true
		switch fixture.Kind {
		case "", ApplicationFixtureMarker, ApplicationFixtureReceiver:
			if fixture.Profile != "" || len(fixture.Config) != 0 {
				return ErrEvidence
			}
		case ApplicationFixtureImage, ApplicationFixtureReplica:
			if err := validateApplicationFixtureCatalogueEntry(fixture, policy); err != nil {
				return err
			}
		default:
			return ErrEvidence
		}
	}
	return nil
}

func validateApplicationFixtureCatalogueEntry(fixture ApplicationFixture, policy *ApplicationPolicy) error {
	found := false
	if policy != nil {
		for _, profile := range policy.Fixtures {
			portAllowed := slices.Contains(profile.Ports, fixture.Port) ||
				fixture.Kind == ApplicationFixtureReplica && fixture.Port == 0 && len(profile.Ports) == 0
			if profile.ID != fixture.Profile || !portAllowed {
				continue
			}
			found = true
			for key, value := range fixture.Config {
				if !slices.Contains(profile.ConfigKeys, key) || len(value) > 16<<10 || ContainsCredentialMaterial([]byte(value)) {
					return ErrEvidence
				}
			}
		}
	}
	if !found {
		return errors.New("application image fixture is not in the operator catalogue")
	}
	return nil
}

func validateApplicationPermission(permission ApplicationPermission) error {
	if len(permission.Resources) == 0 || len(permission.Resources) > 16 ||
		len(permission.Verbs) == 0 || len(permission.Verbs) > 6 {
		return errors.New("application permissions require explicit bounded resources and verbs")
	}
	seen := map[string]bool{}
	for _, resource := range permission.Resources {
		if seen[resource] || !applicationAPIResource(permission.APIGroup, resource) {
			return errors.New("application permission resource is outside the fixed namespaced policy")
		}
		seen[resource] = true
	}
	seen = map[string]bool{}
	for _, verb := range permission.Verbs {
		if seen[verb] || !slices.Contains([]string{ApplicationVerbGet, ApplicationVerbList, ApplicationVerbWatch, ApplicationVerbPatch, ApplicationActionUpdate}, verb) {
			return errors.New("application permissions cannot create workloads, credentials or RBAC")
		}
		seen[verb] = true
	}
	return nil
}

func applicationAPIResource(group, resource string) bool {
	switch group {
	case "":
		return slices.Contains([]string{"configmaps", "services", "endpoints", applicationPods, "pods/status", "services/status"}, resource)
	case "networking.k8s.io":
		return resource == "ingresses" || resource == "ingresses/status"
	case "discovery.k8s.io":
		return resource == "endpointslices"
	default:
		return false
	}
}

func applicationAPIVersion(version, resource string) bool {
	switch version {
	case "v1":
		return applicationAPIResource("", resource)
	case "networking.k8s.io/v1":
		return applicationAPIResource("networking.k8s.io", resource)
	case "discovery.k8s.io/v1":
		return applicationAPIResource("discovery.k8s.io", resource)
	default:
		return false
	}
}

func ValidateApplicationCheck(check Check) error {
	return ValidateApplicationCheckWithPolicy(check, nil)
}

func validateApplicationCheck(check Check, policy *ApplicationPolicy) error {
	app := check.Application
	if err := validateApplicationCheckShape(check); err != nil {
		return err
	}
	if err := validateApplicationResourcesWithPolicy(app.Resources, policy); err != nil {
		return err
	}
	if err := validateApplicationActions(app.Actions, policy); err != nil {
		return err
	}
	if err := validateApplicationReplicaCheck(check, policy); err != nil {
		return err
	}
	if err := validateApplicationNormalValue(check, policy); err != nil {
		return err
	}
	if api := app.Kubernetes; api != nil {
		if !applicationAPIVersionWithPolicy(api.APIVersion, api.Resource, policy) || strings.Contains(api.Resource, "/") ||
			!applicationName(api.Name) || !applicationJSONPointer(api.Pointer) {
			return errors.New("application API check is outside the fixed named/status observation policy")
		}
	}
	if app.TCP != nil {
		if err := validateApplicationTCPCheck(check, policy); err != nil {
			return err
		}
	}
	for _, expected := range []Expectation{check.Healthy, check.Failure} {
		if err := validateApplicationExpectation(check, expected); err != nil {
			return err
		}
	}
	if reflect.DeepEqual(check.Healthy, check.Failure) {
		return errors.New("application healthy and failure observations must differ")
	}
	return nil
}

func validateApplicationCheckShape(check Check) error {
	app := check.Application
	if app == nil || app.Version != ApplicationProtocolVersion || check.HTTP != nil ||
		len(check.Command) != 0 || check.Stdin != "" || len(check.Lifecycle) != 0 ||
		!identifierPattern.MatchString(check.ID) || (check.Kind != Reproduction && check.Kind != Normal) ||
		check.TimeoutSeconds < 1 || check.TimeoutSeconds > 300 ||
		app.SettleSeconds < 0 || app.SettleSeconds >= check.TimeoutSeconds ||
		(app.Kubernetes == nil) == (app.TCP == nil) {
		return errors.New("application checks require one bounded independent observer, not executable checks")
	}
	return nil
}

func validateApplicationTCPCheck(check Check, policy *ApplicationPolicy) error {
	tcp := check.Application.TCP
	if !applicationName(tcp.Target) || !applicationName(tcp.MarkerID) || tcp.MarkerID == ApplicationSubject ||
		tcp.Port < 1024 || tcp.Port > 65535 || tcp.MaxBytes < 32 || tcp.MaxBytes > MaxApplicationTCPBytes ||
		len(tcp.Request) > 4096 || ContainsCredentialMaterial(tcp.Request) {
		return errors.New("application TCP check requires a bounded target, request and protected marker")
	}
	if tcp.Mode == ApplicationTCPReceive {
		return validateApplicationReceiveCheck(check, policy)
	}
	if (tcp.Mode != "" && tcp.Mode != "request") || tcp.WindowSeconds != 0 ||
		len(tcp.Terminator) != 0 || len(tcp.Response) != 0 {
		return ErrEvidence
	}
	return nil
}

func validateApplicationReceiveCheck(check Check, policy *ApplicationPolicy) error {
	app, tcp := check.Application, check.Application.TCP
	if policy == nil || len(app.Actions) == 0 || tcp.WindowSeconds < 1 || tcp.WindowSeconds > 120 ||
		tcp.WindowSeconds+app.SettleSeconds >= check.TimeoutSeconds || len(tcp.Request) != 0 ||
		len(tcp.Terminator) > 8 || len(tcp.Response) > 4096 || ContainsCredentialMaterial(tcp.Response) {
		return errors.New("receive observation requires a complete bounded window, explicit actors and frozen framing")
	}
	return nil
}

func applicationJSONPointer(pointer string) bool {
	if len(pointer) > 256 || !utf8.ValidString(pointer) ||
		(!strings.HasPrefix(pointer, "/status/") && pointer != "/status") || strings.ContainsAny(pointer, "\r\n\x00") {
		return false
	}
	for component := range strings.SplitSeq(pointer[1:], "/") {
		if component == "" {
			return false
		}
		for i := 0; i < len(component); i++ {
			if component[i] == '~' {
				i++
				if i >= len(component) || (component[i] != '0' && component[i] != '1') {
					return false
				}
			}
		}
	}
	return true
}

func validateApplicationManifest(manifest Manifest) error {
	if manifest.Application == nil {
		return validateAbsentApplicationPlan(manifest)
	}
	if err := ValidateApplicationPlanWithPolicy(*manifest.Application, manifest.ApplicationPolicy); err != nil {
		return err
	}
	if err := validateApplicationEnvironment(manifest); err != nil {
		return err
	}
	if err := validateApplicationManifestChecks(manifest); err != nil {
		return err
	}
	if err := validateApplicationReplicaControls(manifest); err != nil {
		return err
	}
	if err := validateApplicationNormalControls(manifest.Checks); err != nil {
		return err
	}
	if manifest.ApplicationSourceMetadata != nil || manifest.Sources.Original.ArchiveDigest != Digest(nil) {
		if err := ValidateApplicationSourceMetadata(manifest); err != nil {
			return err
		}
	}
	return nil
}

func validateAbsentApplicationPlan(manifest Manifest) error {
	if manifest.ApplicationPolicy != nil || manifest.ApplicationSourceMetadata != nil {
		return ErrEvidence
	}
	for _, check := range manifest.Checks {
		if check.Application != nil {
			return errors.New("application observations require a frozen application plan")
		}
	}
	if manifest.Environment.Profile == ApplicationRuntimeProfile {
		return errors.New("application profile requires a frozen application plan")
	}
	return nil
}

func validateApplicationEnvironment(manifest Manifest) error {
	env := manifest.Environment
	if manifest.Action == "" || len(manifest.Sources.Paths) != 0 || len(manifest.Files) != 0 ||
		!publicRepositoryPattern.MatchString(manifest.Sources.Repository) ||
		env.Profile != ApplicationRuntimeProfile || len(env.Services) != 0 || len(env.Variables) != 0 ||
		(env.Platform != "linux/amd64" && env.Platform != "linux/arm64") ||
		!applicationPinnedImage(env.Image) || !validResolvedImageID(env.ImageID) ||
		env.Dependencies[ApplicationPolicyKey] != ApplicationPolicyVersion ||
		!applicationPinnedImage(env.Dependencies[ApplicationObserverImageKey]) ||
		!validResolvedImageID(env.Dependencies[ApplicationObserverImageIDKey]) ||
		env.Dependencies["orka.kubernetes.policy"] != "" || env.Dependencies["orka.local-runner.policy"] != "" {
		return errors.New("application runtime requires complete public source, dedicated policy and pinned observer/toolchain")
	}
	return nil
}

func validateApplicationManifestChecks(manifest Manifest) error {
	reproductions, controls := 0, 0
	ids := map[string]bool{}
	for _, check := range manifest.Checks {
		if err := ValidateApplicationCheckWithPolicy(check, manifest.ApplicationPolicy); err != nil {
			return err
		}
		if ids[check.ID] {
			return errors.New("duplicate application check")
		}
		ids[check.ID] = true
		if check.Kind == Reproduction {
			reproductions++
		} else {
			controls++
		}
		combined := append(slices.Clone(manifest.Application.Resources), check.Application.Resources...)
		if err := validateApplicationResourcesWithPolicy(combined, manifest.ApplicationPolicy); err != nil {
			return err
		}
		if err := validateApplicationNormalTarget(manifest, check); err != nil {
			return err
		}
		if tcp := check.Application.TCP; tcp != nil {
			if err := validateApplicationTCPReferences(*manifest.Application, check.Kind, tcp); err != nil {
				return err
			}
		}
	}
	if reproductions == 0 || controls == 0 {
		return errors.New("application runtime requires reproduction and normal controls even for report validation")
	}
	return nil
}

func validateApplicationTCPReferences(plan ApplicationPlan, kind string, tcp *ApplicationTCPCheck) error {
	if kind == Reproduction && tcp.Mode != ApplicationTCPReceive && tcp.Target != ApplicationSubject {
		return errors.New("application reproduction must observe the actual subject, not a fixture directly")
	}
	markerFound, targetFound := false, tcp.Target == ApplicationSubject && slices.Contains(plan.Runtime.Ports, tcp.Port)
	for _, fixture := range plan.Fixtures {
		markerFound = markerFound || (fixture.ID == tcp.MarkerID && (fixture.Kind == "" || fixture.Kind == ApplicationFixtureMarker))
		if fixture.ID == tcp.Target && fixture.Port == tcp.Port {
			targetFound = tcp.Mode == ApplicationTCPReceive && fixture.Kind == ApplicationFixtureReceiver ||
				tcp.Mode != ApplicationTCPReceive && fixture.Kind != ApplicationFixtureReceiver
		}
	}
	for _, secret := range plan.SyntheticSecrets {
		for _, key := range secret.Keys {
			markerFound = markerFound || key.MarkerID == tcp.MarkerID
		}
	}
	if !markerFound || !targetFound {
		return errors.New("TCP observations must reference declared endpoints and protected marker fixtures")
	}
	return nil
}

func validateApplicationReplicaControls(manifest Manifest) error {
	for _, fixture := range manifest.Application.Fixtures {
		if fixture.Kind != ApplicationFixtureReplica {
			continue
		}
		normal := false
		for _, check := range manifest.Checks {
			api := check.Application.Kubernetes
			normal = normal || check.Kind == Normal && api != nil && api.APIVersion == applicationAppsVersion &&
				api.Resource == applicationDeployments && api.Name == fixture.ID
		}
		if !normal {
			return errors.New("a replica fixture requires actual non-initial normal reconciliation evidence")
		}
	}
	return nil
}

func validateApplicationNormalControls(checks []Check) error {
	for _, reproduction := range checks {
		if reproduction.Kind != Reproduction {
			continue
		}
		matched := false
		for _, control := range checks {
			if control.Kind != Normal {
				continue
			}
			matched = matched || applicationNormalControlMatches(reproduction, control)
		}
		if !matched {
			return errors.New("each application reproduction requires a matched normal subject control")
		}
	}
	return nil
}

func applicationNormalControlMatches(reproduction, control Check) bool {
	if api := reproduction.Application.Kubernetes; api != nil && control.Application.Kubernetes != nil {
		other := control.Application.Kubernetes
		var positive ApplicationAPIResult
		return json.Unmarshal([]byte(control.Healthy.Stdout), &positive) == nil && positive.Exists && positive.Found &&
			api.APIVersion == other.APIVersion && api.Resource == other.Resource && api.Pointer == other.Pointer
	}
	if tcp := reproduction.Application.TCP; tcp != nil && control.Application.TCP != nil {
		other := control.Application.TCP
		positive, _ := ApplicationTCPExpectation(true)
		targetMatched := tcp.Mode == ApplicationTCPReceive && other.Mode == ApplicationTCPReceive && tcp.Target == other.Target ||
			tcp.Mode != ApplicationTCPReceive && other.Mode != ApplicationTCPReceive && other.Target == ApplicationSubject
		return targetMatched && tcp.Port == other.Port &&
			tcp.MarkerID == other.MarkerID && control.Healthy.Stdout == positive.Stdout
	}
	return false
}

// ValidateApplicationResource intentionally admits data objects only. Subjects,
// marker Pods, ServiceAccounts and Roles must be constructed by trusted code.
func ValidateApplicationResource(raw json.RawMessage) (*unstructured.Unstructured, error) {
	if len(raw) == 0 || len(raw) > 32<<10 || ContainsCredentialMaterial(raw) {
		return nil, ErrLimit
	}
	var object map[string]any
	if err := kjson.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, ErrEvidence
	}
	value := &unstructured.Unstructured{Object: object}
	if !applicationName(value.GetName()) || strings.HasPrefix(value.GetName(), "orka-") {
		return nil, errors.New("application resource requires a non-reserved bounded name")
	}
	if err := validateApplicationResourceMetadata(object); err != nil {
		return nil, err
	}
	switch {
	case value.GetAPIVersion() == "v1" && value.GetKind() == "ConfigMap":
		if err := validateApplicationConfigMap(object); err != nil {
			return nil, err
		}
	case value.GetAPIVersion() == "v1" && value.GetKind() == "Service":
		if !applicationOnlyKeys(object, "apiVersion", "kind", "metadata", "spec") || !applicationService(object["spec"]) {
			return nil, errors.New("application Service must be an unprivileged ClusterIP selecting a frozen endpoint")
		}
	case value.GetAPIVersion() == "networking.k8s.io/v1" && value.GetKind() == "Ingress":
		if !applicationOnlyKeys(object, "apiVersion", "kind", "metadata", "spec") || !applicationIngress(object["spec"]) {
			return nil, errors.New("application Ingress supports only local Service backends without TLS credentials")
		}
	default:
		return nil, errors.New("application resource kind is unsupported by the fixed target policy")
	}
	return value, nil
}

func validateApplicationResourceMetadata(object map[string]any) error {
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		return ErrEvidence
	}
	for key, field := range metadata {
		switch key {
		case "name":
		case "labels", "annotations":
			entries, ok := field.(map[string]any)
			if !ok || len(entries) > 16 {
				return ErrEvidence
			}
			for name, entry := range entries {
				text, ok := entry.(string)
				if !ok || len(text) > 1024 || len(kvalidation.IsQualifiedName(name)) != 0 ||
					strings.Contains(name, "orka.ai/") || strings.ContainsAny(text, "\r\n\x00") {
					return ErrEvidence
				}
			}
		default:
			return errors.New("application resource cannot set namespace, identity or ownership")
		}
	}
	return nil
}

func validateApplicationConfigMap(object map[string]any) error {
	if !applicationOnlyKeys(object, "apiVersion", "kind", "metadata", "data", "immutable") {
		return ErrEvidence
	}
	if data, exists := object["data"]; exists {
		entries, ok := data.(map[string]any)
		if !ok || len(entries) > 64 {
			return ErrEvidence
		}
		for name, entry := range entries {
			text, ok := entry.(string)
			if !ok || len(kvalidation.IsConfigMapKey(name)) != 0 || len(text) > 16<<10 {
				return ErrEvidence
			}
		}
	}
	if immutable, exists := object["immutable"]; exists && immutable != true {
		return ErrEvidence
	}
	return nil
}

func applicationOnlyKeys(object map[string]any, keys ...string) bool {
	for key := range object {
		if !slices.Contains(keys, key) {
			return false
		}
	}
	return true
}

func applicationService(raw any) bool {
	spec, ok := raw.(map[string]any)
	if !ok || !applicationOnlyKeys(spec, "type", "ports", "selector") ||
		(spec["type"] != nil && spec["type"] != "ClusterIP") {
		return false
	}
	selector, ok := spec["selector"].(map[string]any)
	target, targetOK := selector[ApplicationTargetLabel].(string)
	if !ok || len(selector) != 1 || !targetOK || !applicationName(target) {
		return false
	}
	ports, ok := spec["ports"].([]any)
	if !ok || len(ports) == 0 || len(ports) > 16 {
		return false
	}
	for _, rawPort := range ports {
		port, ok := rawPort.(map[string]any)
		if !ok || !applicationOnlyKeys(port, "name", "port", "targetPort", "protocol") ||
			(port["protocol"] != nil && port["protocol"] != "TCP") ||
			!applicationPort(port["port"]) || !applicationPort(port["targetPort"]) {
			return false
		}
		if name, exists := port["name"]; exists {
			text, ok := name.(string)
			if !ok || len(kvalidation.IsValidPortName(text)) != 0 {
				return false
			}
		}
	}
	return true
}

func applicationPort(raw any) bool {
	switch value := raw.(type) {
	case int64:
		return value >= 1024 && value <= 65535
	case float64:
		return value >= 1024 && value <= 65535 && value == float64(int(value))
	default:
		return false
	}
}

func applicationIngress(raw any) bool {
	spec, ok := raw.(map[string]any)
	if !ok || !applicationOnlyKeys(spec, "ingressClassName", "defaultBackend", "rules") {
		return false
	}
	if class, exists := spec["ingressClassName"]; exists {
		text, ok := class.(string)
		if !ok || !applicationName(text) {
			return false
		}
	}
	haveBackend := false
	if backend, exists := spec["defaultBackend"]; exists {
		if !applicationIngressBackend(backend) {
			return false
		}
		haveBackend = true
	}
	if rawRules, exists := spec["rules"]; exists {
		rules, ok := rawRules.([]any)
		if !ok || len(rules) == 0 || len(rules) > 8 {
			return false
		}
		for _, rawRule := range rules {
			if !applicationIngressRule(rawRule) {
				return false
			}
		}
		haveBackend = true
	}
	return haveBackend
}

func applicationIngressRule(raw any) bool {
	rule, ok := raw.(map[string]any)
	if !ok || !applicationOnlyKeys(rule, "host", "http") {
		return false
	}
	if host, exists := rule["host"]; exists {
		text, ok := host.(string)
		if !ok || len(kvalidation.IsDNS1123Subdomain(text)) != 0 {
			return false
		}
	}
	http, ok := rule["http"].(map[string]any)
	if !ok || !applicationOnlyKeys(http, "paths") {
		return false
	}
	paths, ok := http["paths"].([]any)
	if !ok || len(paths) == 0 || len(paths) > 8 {
		return false
	}
	for _, rawPath := range paths {
		path, ok := rawPath.(map[string]any)
		if !ok || !applicationOnlyKeys(path, "path", "pathType", "backend") ||
			(path["pathType"] != "Exact" && path["pathType"] != "Prefix") ||
			!applicationIngressBackend(path["backend"]) {
			return false
		}
		name, ok := path["path"].(string)
		if !ok || validateHTTPPath(name) != nil {
			return false
		}
	}
	return true
}

func applicationIngressBackend(raw any) bool {
	backend, ok := raw.(map[string]any)
	if !ok || len(backend) != 1 {
		return false
	}
	service, ok := backend["service"].(map[string]any)
	if !ok || !applicationOnlyKeys(service, "name", "port") {
		return false
	}
	name, ok := service["name"].(string)
	if !ok || !applicationName(name) {
		return false
	}
	port, ok := service["port"].(map[string]any)
	return ok && len(port) == 1 && applicationPort(port["number"])
}

func DecodeApplicationWorkerInput(reader io.Reader) (ApplicationWorkerInput, error) {
	var input ApplicationWorkerInput
	err := decodePreparationJSON(reader, &input, MaxApplicationInputBytes)
	if err == nil {
		err = ValidateApplicationWorkerInput(input)
	}
	return input, err
}

func ValidateApplicationWorkerInput(input ApplicationWorkerInput) error {
	if input.Version != ApplicationProtocolVersion || input.Manifest.Application == nil ||
		ValidateManifest(input.Manifest) != nil || ValidateRunBinding(input.Manifest, input.Binding) != nil ||
		!identifierPattern.MatchString(input.TaskUID) || !applicationTargetValid(input.Target) {
		return ErrEvidence
	}
	if input.Side != Original && (input.Side != Patched || input.Manifest.Action == ValidateReport) {
		return ErrBinding
	}
	if input.CheckID != "" {
		if err := validateApplicationWorkerCheck(input); err != nil {
			return err
		}
	} else if input.APIBaseline != nil {
		return ErrBinding
	}
	if input.Artifact != nil {
		if err := ValidateApplicationArtifact(input.Manifest, input.Binding, input.Side, *input.Artifact); err != nil {
			return err
		}
	}
	if err := validateApplicationWorkerMarker(input); err != nil {
		return err
	}
	if err := validateApplicationWorkerFixture(input); err != nil {
		return err
	}
	if len(input.SubjectTLS) != 0 {
		if err := ValidateApplicationSubjectTLSIdentities(input.Manifest.ApplicationPolicy, input.Target, input.SubjectTLS); err != nil {
			return err
		}
	}
	return validateApplicationWorkerEndpoints(input.Endpoints)
}

func validateApplicationWorkerCheck(input ApplicationWorkerInput) error {
	found := false
	for _, check := range input.Manifest.Checks {
		if check.ID == input.CheckID {
			found = true
			if err := validateApplicationWorkerActions(input, check); err != nil {
				return err
			}
			if err := validateApplicationWorkerAPIBaseline(input, check); err != nil {
				return err
			}
			if input.Manifest.ApplicationPolicy != nil && check.Application.TCP != nil && input.MarkerRef == nil {
				return errors.New("operator-profile TCP observations require a protected marker reference")
			}
		}
	}
	if !found {
		return ErrBinding
	}
	return nil
}

func validateApplicationWorkerMarker(input ApplicationWorkerInput) error {
	if input.MarkerDigest != "" && !sha256Pattern.MatchString(input.MarkerDigest) {
		return ErrBinding
	}
	if ref := input.MarkerRef; ref != nil {
		if !applicationName(ref.Namespace) || !identifierPattern.MatchString(ref.NamespaceUID) ||
			ref.Namespace == input.Target.Namespace || ref.NamespaceUID == input.Target.NamespaceUID ||
			!applicationName(ref.Name) || !identifierPattern.MatchString(ref.UID) ||
			len(kvalidation.IsConfigMapKey(ref.Key)) != 0 || !sha256Pattern.MatchString(ref.Digest) ||
			ref.Digest != input.MarkerDigest {
			return ErrBinding
		}
	}
	return nil
}

func validateApplicationWorkerFixture(input ApplicationWorkerInput) error {
	if input.FixtureID != "" {
		found := false
		for _, fixture := range input.Manifest.Application.Fixtures {
			found = found || fixture.ID == input.FixtureID
		}
		if !found {
			return ErrBinding
		}
		return ValidateApplicationFixtureSecretIdentities(input)
	}
	if len(input.FixtureSecrets) != 0 {
		return ErrBinding
	}
	return nil
}

func validateApplicationWorkerEndpoints(endpoints []ApplicationEndpoint) error {
	if len(endpoints) > 17 {
		return ErrLimit
	}
	seen := map[string]bool{}
	for _, endpoint := range endpoints {
		if !applicationName(endpoint.ID) || seen[endpoint.ID] || net.ParseIP(endpoint.Address) == nil ||
			endpoint.Port < 1024 || endpoint.Port > 65535 || !applicationContainerValid(endpoint.Pod) {
			return ErrBinding
		}
		seen[endpoint.ID] = true
	}
	return nil
}

func applicationTargetValid(target ApplicationTarget) bool {
	return identifierPattern.MatchString(target.ClusterUID) && applicationName(target.Namespace) &&
		identifierPattern.MatchString(target.NamespaceUID) && target.NamespaceUID != target.ClusterUID &&
		target.Namespace != "default" && !strings.HasPrefix(target.Namespace, "kube-")
}

func applicationContainerValid(identity ApplicationContainerIdentity) bool {
	if identity.Namespace != "" || identity.NamespaceUID != "" {
		if !applicationName(identity.Namespace) || !identifierPattern.MatchString(identity.NamespaceUID) {
			return false
		}
	}
	return applicationName(identity.PodName) && identifierPattern.MatchString(identity.PodUID) &&
		applicationName(identity.ContainerName) && len(identity.ContainerID) >= 8 &&
		len(identity.ContainerID) <= 256 && !strings.ContainsAny(identity.ContainerID, " \t\r\n\x00") &&
		validResolvedImageID(identity.ImageID) && applicationName(identity.ServiceAccountName) &&
		identifierPattern.MatchString(identity.ServiceAccountUID) &&
		(identity.JobUID == "" || identifierPattern.MatchString(identity.JobUID))
}

func applicationSource(manifest Manifest, binding Binding, side string) (SourceIdentity, string, error) {
	switch side {
	case Original:
		return manifest.Sources.Original, binding.OriginalTaskID, nil
	case Patched:
		if manifest.Action != ValidateReport {
			return manifest.Sources.Patched, binding.PatchedTaskID, nil
		}
	}
	return SourceIdentity{}, "", ErrBinding
}

func ApplicationAPIExpectation(value ApplicationAPIValue) (Expectation, error) {
	result, err := applicationAPIResult(value)
	if err != nil {
		return Expectation{}, err
	}
	content, _ := json.Marshal(result)
	return Expectation{Stdout: string(content)}, nil
}

func applicationAPIResult(value ApplicationAPIValue) (ApplicationAPIResult, error) {
	result := ApplicationAPIResult{Exists: value.Exists, Found: value.Found}
	if (!value.Exists && value.Found) || (!value.Found && len(value.Value) != 0) {
		return result, ErrEvidence
	}
	if value.Found {
		canonical, err := applicationCanonicalValue(value.Value)
		if err != nil {
			return result, err
		}
		result.ValueDigest, result.ValueBytes = Digest(canonical), len(canonical)
	}
	return result, nil
}

func applicationCanonicalValue(value []byte) ([]byte, error) {
	if len(value) == 0 || len(value) > 16<<10 {
		return nil, ErrLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var parsed any
	if decoder.Decode(&parsed) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrEvidence
	}
	content, err := json.Marshal(parsed)
	if err != nil || len(content) > 16<<10 {
		return nil, ErrLimit
	}
	return content, nil
}

func ApplicationTCPExpectation(markerMatched bool) (Expectation, error) {
	content, err := json.Marshal(ApplicationTCPValue{Connected: true, MarkerMatched: markerMatched})
	return Expectation{Stdout: string(content)}, err
}

func validateApplicationExpectation(check Check, expected Expectation) error {
	if expected.ExitCode != 0 || len(expected.Services) != 0 || len(expected.Stdout) > 8192 {
		return errors.New("application expectations require observer exit zero and canonical metadata only")
	}
	var value any
	if check.Application.Kubernetes != nil {
		result := new(ApplicationAPIResult)
		if decodePreparationJSON(strings.NewReader(expected.Stdout), result, 8192) != nil ||
			!applicationAPIResultValid(*result) {
			return ErrEvidence
		}
		value = result
	} else {
		result := new(ApplicationTCPValue)
		if decodePreparationJSON(strings.NewReader(expected.Stdout), result, 8192) != nil ||
			(!result.Connected && (check.Application.TCP.Mode != ApplicationTCPReceive || result.MarkerMatched)) {
			return ErrEvidence
		}
		value = result
	}
	content, _ := json.Marshal(value)
	if string(content) != expected.Stdout {
		return errors.New("application expectation is not canonical")
	}
	return nil
}

func applicationAPIResultValid(result ApplicationAPIResult) bool {
	if !result.Exists && result.Found {
		return false
	}
	if !result.Found {
		return result.ValueDigest == "" && result.ValueBytes == 0
	}
	return sha256Pattern.MatchString(result.ValueDigest) && result.ValueBytes > 0 && result.ValueBytes <= 16<<10
}

func ApplicationBoundaryInputDigest(input ApplicationBoundaryInput) (string, error) {
	if _, err := ApplicationFileSizeLimit(input.FileSizeRole); err != nil {
		return "", err
	}
	if input.Version != ApplicationProtocolVersion || !identifierPattern.MatchString(input.ID) ||
		!identifierPattern.MatchString(input.TaskUID) || !applicationTargetValid(input.Target) ||
		net.ParseIP(input.CanaryHost) == nil || input.CanaryPort < 1 || input.CanaryPort > 65535 {
		return "", ErrEvidence
	}
	if input.SourceHost != "" && ((input.SourceHost != "github.com" && input.SourceHost != "api.github.com") || input.CanaryPort != 443) {
		return "", errors.New("source boundary probes require an admitted public TLS host on the exact HTTPS relay port")
	}
	if input.RuntimeQualification != nil {
		if ValidateApplicationRuntimeQualification(*input.RuntimeQualification) != nil ||
			input.RuntimeQualification.TargetClusterUID != input.Target.ClusterUID || input.RuntimeLimits == nil ||
			ValidateApplicationRuntimeLimits(*input.RuntimeLimits) != nil {
			return "", ErrBinding
		}
	} else if input.RuntimeLimits != nil {
		return "", ErrBinding
	}
	if input.APIAddress != "" {
		endpoint, err := url.Parse(input.APIAddress)
		if err != nil || endpoint.Scheme != "https" || net.ParseIP(endpoint.Hostname()) == nil ||
			endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil ||
			!applicationName(input.ServiceAccountName) {
			return "", fmt.Errorf("boundary API endpoint requires the exact isolated target address")
		}
	} else if input.ServiceAccountName != "" {
		return "", ErrEvidence
	}
	content, err := json.Marshal(input)
	if err != nil {
		return "", ErrEvidence
	}
	return Digest(content), nil
}
