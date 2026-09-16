package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const testCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.test
spec:
  group: example.test
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
`

func TestDecodeCRD(t *testing.T) {
	crd, err := decodeCRD(testCRD)
	if err != nil {
		t.Fatalf("decodeCRD() error = %v", err)
	}
	if crd.Spec.Group != "example.test" || crd.Spec.Names.Kind != "Widget" {
		t.Fatalf("decodeCRD() = %q/%q, want example.test/Widget", crd.Spec.Group, crd.Spec.Names.Kind)
	}
}

func TestObjectSetWritesOnlyCRDs(t *testing.T) {
	destination := t.TempDir()
	oldOutput := *outputDir
	*outputDir = destination
	t.Cleanup(func() { *outputDir = oldOutput })

	set := objectSet{}
	if err := set.add(testCRD); err != nil {
		t.Fatalf("add(CRD) error = %v", err)
	}
	if err := set.add(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: ignored
`); err != nil {
		t.Fatalf("add(Deployment) error = %v", err)
	}
	if err := set.write(); err != nil {
		t.Fatalf("write() error = %v", err)
	}

	crdPath := filepath.Join(destination, "crds", "widget-customresourcedefinition.yaml")
	contents, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}
	if !strings.Contains(string(contents), "kind: CustomResourceDefinition") {
		t.Fatalf("generated CRD does not contain the CRD kind")
	}
	if _, err := os.Stat(filepath.Join(destination, "templates")); !os.IsNotExist(err) {
		t.Fatalf("non-CRD objects unexpectedly produced a templates directory: %v", err)
	}
}

func TestObjectSetRejectsDuplicateCRDFilenames(t *testing.T) {
	destination := t.TempDir()
	oldOutput := *outputDir
	*outputDir = destination
	t.Cleanup(func() { *outputDir = oldOutput })

	set := objectSet{crds: []string{testCRD, testCRD}}
	if err := set.write(); err == nil || !strings.Contains(err.Error(), "duplicate generated output filename") {
		t.Fatalf("write() error = %v, want duplicate filename error", err)
	}
}

func helmTemplateStaticChart(t *testing.T, args ...string) (string, error) {
	return helmTemplateStaticChartForRelease(t, "test", "orka-test", args...)
}

func helmTemplateStaticChartForRelease(
	t *testing.T,
	releaseName string,
	namespace string,
	args ...string,
) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for static chart render tests")
	}

	defaults := staticChartDefaultArgs()
	commandArgs := make([]string, 0, 5+len(defaults)+2+len(args))
	commandArgs = append(commandArgs, "template", releaseName, "static", "--namespace", namespace)
	commandArgs = append(commandArgs, defaults...)
	commandArgs = append(commandArgs, "--set-string", "controller.watchNamespace="+namespace)
	commandArgs = append(commandArgs, args...)
	output, err := exec.Command(helm, commandArgs...).CombinedOutput()
	return string(output), err
}

func staticChartDefaultArgs() []string {
	digest := "sha256:" + strings.Repeat("0", 64)
	return []string{
		"--values", "testdata/provider-proxy-values.yaml",
		"--set-string", "controller.watchNamespace=orka-test",
		"--set-string", "controller.image.digest=" + digest,
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--set-string", "webhooks.tls.existingSecret=controller-webhook-tls",
		"--set-string", "webhooks.caBundle=Y2E=",
		"--set-string", "publisher.image.digest=" + digest,
		"--set", "providerProxy.enabled=true",
	}
}

func TestStaticChartGrantsSessionAuthorizationRBAC(t *testing.T) {
	output, err := helmTemplateStaticChart(t, "--show-only", "templates/rbac.yaml")
	if err != nil {
		t.Fatalf("helm template rejected controller RBAC: %v\n%s", err, output)
	}
	start := strings.Index(output, "# Controller tenant Role.")
	if start < 0 {
		t.Fatalf("rendered RBAC is missing the controller tenant Role:\n%s", output)
	}
	controllerRole := output[start:]
	if end := strings.Index(controllerRole, "\n---"); end >= 0 {
		controllerRole = controllerRole[:end]
	}
	const want = "apiGroups: [\"core.orka.ai\"]\n    resources: [\"sessions\"]\n    verbs: [\"get\", \"list\", \"delete\"]"
	if !strings.Contains(controllerRole, want) {
		t.Fatalf("controller tenant Role is missing the least-privilege Session rule %q:\n%s", want, controllerRole)
	}
	clientStart := strings.Index(output, "# Client Role")
	if clientStart < 0 {
		t.Fatalf("rendered RBAC is missing the client Role:\n%s", output)
	}
	clientRole := output[clientStart:]
	if end := strings.Index(clientRole, "\n---"); end >= 0 {
		clientRole = clientRole[:end]
	}
	const wantClient = "apiGroups: [\"core.orka.ai\"]\n    resources: [\"sessions\"]\n" +
		"    verbs: [\"get\", \"list\", \"update\", \"delete\"]"
	if !strings.Contains(clientRole, wantClient) {
		t.Fatalf("client Role is missing the Session API rule %q:\n%s", wantClient, clientRole)
	}
}

func TestStaticChartClientVirtualAPIPermissions(t *testing.T) {
	for _, mode := range []string{"harness-v1", "harness-v2"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"--show-only", "templates/rbac.yaml", "--set-string", "controller.mode=" + mode}
			if mode == "harness-v1" {
				args = append(args,
					"--set-string", "harnessV1.image.digest=sha256:"+strings.Repeat("1", 64),
					"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
					"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				)
			}
			output := requireHelmRender(t, args...)
			_, clientRole, found := strings.Cut(output, "# Client Role")
			if !found {
				t.Fatal("rendered RBAC is missing the client Role")
			}
			_, clientRole, _ = strings.Cut(clientRole, "\n")
			clientRole, _, _ = strings.Cut(clientRole, "\n---")
			var role rbacv1.Role
			if err := yaml.Unmarshal([]byte(clientRole), &role); err != nil {
				t.Fatalf("decode client Role: %v", err)
			}
			if role.Namespace != "orka-test" {
				t.Fatalf("client Role namespace = %q, want orka-test", role.Namespace)
			}
			allows := func(group, resource, verb string) bool {
				for _, rule := range role.Rules {
					if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) &&
						slices.Contains(rule.Verbs, verb) && len(rule.ResourceNames) == 0 {
						return true
					}
				}
				return false
			}
			for resource, verbs := range map[string][]string{
				"tasks":                           {"get", "list", "create", "delete", "patch"},
				"tasks/approvals":                 {"update"},
				"sessions":                        {"get", "list", "update", "delete"},
				"chats":                           {"create"},
				"chats/config":                    {"get"},
				"memories":                        {"get", "list"},
				"memoryproposals":                 {"get", "list"},
				"repositoryscans":                 {"get", "list"},
				"repositoryscans/threatmodel":     {"get"},
				"repositoryscans/scans":           {"list"},
				"repositoryscans/slices":          {"get", "list"},
				"repositoryscans/droppedfindings": {"list"},
				"repositoryscans/findings":        {"list"},
				"securityfindings":                {"get"},
				"securityfindings/patches":        {"list"},
				"securityfindings/pullrequest":    {"get"},
				"repositorymonitors":              {"patch"},
				"repositorymonitors/runs":         {"list", "create"},
				"repositorymonitors/items":        {"list"},
				"repositorymonitors/commands":     {"create"},
				"monitorcommands":                 {"get", "list"},
				"monitoractions":                  {"get", "list"},
				"monitorworkactions":              {"get", "list"},
				"monitorimplementationjobs":       {"get", "list"},
				"monitormutations":                {"get", "list"},
				"monitorevents":                   {"list"},
			} {
				for _, verb := range verbs {
					if !allows("core.orka.ai", resource, verb) {
						t.Errorf("client cannot %s core.orka.ai/%s", verb, resource)
					}
				}
			}
			for _, resource := range []string{"gatewayevents", "gatewaydeliveries"} {
				for _, verb := range []string{"get", "list"} {
					if got := allows("gateway.orka.ai", resource, verb); got != (mode == "harness-v2") {
						t.Errorf("client %s gateway.orka.ai/%s = %v", verb, resource, got)
					}
				}
			}
			for _, permission := range [][3]string{
				{"", "secrets", "get"}, {"", "secrets", "list"},
				{"core.orka.ai", "agents", "create"},
				{"core.orka.ai", "repositoryscans", "create"},
				{"core.orka.ai", "memoryproposals", "review"},
				{"core.orka.ai", "memoryproposals", "apply"},
				{"core.orka.ai", "securityfindings", "update"},
				{"gateway.orka.ai", "gateways", "update"},
				{"gateway.orka.ai", "gatewaydeliveries", "update"},
			} {
				if allows(permission[0], permission[1], permission[2]) {
					t.Errorf("client unexpectedly grants %s %s/%s", permission[2], permission[0], permission[1])
				}
			}
			for _, rule := range role.Rules {
				if slices.Contains(rule.APIGroups, "*") || slices.Contains(rule.Resources, "*") ||
					slices.Contains(rule.Verbs, "*") {
					t.Error("client Role contains a wildcard permission")
				}
			}
		})
	}
}

func TestStaticChartOmitsUnusedControllerPermissions(t *testing.T) {
	output := requireHelmRender(t, "--show-only", "templates/rbac.yaml")
	start := strings.Index(output, "# Controller tenant Role.")
	if start < 0 {
		t.Fatal("rendered RBAC is missing the controller tenant Role")
	}
	controllerRole := output[start:]
	if end := strings.Index(controllerRole, "\n---"); end >= 0 {
		controllerRole = controllerRole[:end]
	}
	for _, resource := range []string{
		"cronjobs", "endpoints", "nodes", "replicationcontrollers", "pods/portforward",
		"statefulsets", "daemonsets", "ingresses", "horizontalpodautoscalers", "metrics.k8s.io",
	} {
		if strings.Contains(controllerRole, strconv.Quote(resource)) {
			t.Errorf("controller tenant Role still grants unused %q access", resource)
		}
	}
	for _, resource := range []string{
		"jobs", "pods/status", "persistentvolumeclaims", "serviceaccounts/token", "endpointslices",
	} {
		if !strings.Contains(controllerRole, strconv.Quote(resource)) {
			t.Errorf("controller tenant Role is missing required %q access", resource)
		}
	}
}

func forceStaticChartNamespaceMode(t *testing.T, chartDir, mode string) {
	t.Helper()
	helpersPath := filepath.Join(chartDir, "templates", "_helpers.tpl")
	helpers, err := os.ReadFile(helpersPath)
	if err != nil {
		t.Fatalf("read static chart helpers: %v", err)
	}
	namespaceLookup := `{{- $existingNamespace := lookup "v1" "Namespace" "" .Release.Namespace -}}`
	forcedNamespaceLookup := `{{- $existingNamespace := dict "metadata" (dict "labels" ` +
		`(dict "orka.ai/controller-mode" ` + strconv.Quote(mode) + `)) -}}`
	forced := strings.Replace(string(helpers), namespaceLookup, forcedNamespaceLookup, 1)
	if forced == string(helpers) {
		t.Fatalf("controller mode validation is not gated by the exact existing Namespace lookup")
	}
	if err := os.WriteFile(helpersPath, []byte(forced), 0o600); err != nil {
		t.Fatalf("force existing namespace lookup in copied chart: %v", err)
	}
}

func helmTemplateStaticChartWithExistingController(
	t *testing.T,
	existingControllerArgs []string,
	args ...string,
) (string, error) {
	t.Helper()
	return helmTemplateStaticChartWithExistingControllerSnapshot(
		t,
		existingControllerArgs,
		"snapshot-key",
		"encryption-key",
		args...,
	)
}

func helmTemplateStaticChartWithExistingControllerSnapshot(
	t *testing.T,
	existingControllerArgs []string,
	existingSnapshotSecret string,
	existingSnapshotKey string,
	args ...string,
) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for static chart render tests")
	}

	chartDir := filepath.Join(t.TempDir(), "static")
	if err := os.CopyFS(chartDir, os.DirFS("static")); err != nil {
		t.Fatalf("copy static chart: %v", err)
	}
	forceStaticChartNamespaceMode(t, chartDir, "harness-v2")
	helpersPath := filepath.Join(chartDir, "templates", "_helpers.tpl")
	helpers, err := os.ReadFile(helpersPath)
	if err != nil {
		t.Fatalf("read static chart helpers: %v", err)
	}
	lookup := `{{- $existingControllerList := lookup "apps/v1" "Deployment" .Release.Namespace "" -}}`
	forcedLookup := `{{- $existingControllerList := dict "items" (list) -}}`
	if existingControllerArgs != nil {
		quotedControllerArgs := make([]string, 0, len(existingControllerArgs))
		for _, arg := range existingControllerArgs {
			quotedControllerArgs = append(quotedControllerArgs, strconv.Quote(arg))
		}
		forcedLookup = `{{- $existingControllerList := dict "items" (list (dict ` +
			`"metadata" (dict "name" "test-orka-controller" "labels" (dict ` +
			`"app.kubernetes.io/instance" "test" "app.kubernetes.io/component" "controller" ` +
			`"app.kubernetes.io/managed-by" "Helm")) ` +
			`"spec" (dict "template" (dict "spec" (dict ` +
			`"containers" (list (dict "name" "controller" "args" (list ` +
			strings.Join(quotedControllerArgs, " ") + `))) ` +
			`"volumes" (list (dict "name" "agent-execution-snapshot-key" "secret" (dict ` +
			`"secretName" ` + strconv.Quote(existingSnapshotSecret) + ` "items" (list (dict ` +
			`"key" ` + strconv.Quote(existingSnapshotKey) + ` "path" "key")))))))))) -}}`
	}
	withController := strings.Replace(string(helpers), lookup, forcedLookup, 1)
	if withController == string(helpers) {
		t.Fatalf("controller mode validation is not gated by the release-owned Deployment list lookup")
	}
	if err := os.WriteFile(helpersPath, []byte(withController), 0o600); err != nil {
		t.Fatalf("force existing controller lookup in copied chart: %v", err)
	}

	defaults := staticChartDefaultArgs()
	commandArgs := make([]string, 0, 6+len(defaults)+len(args))
	commandArgs = append(commandArgs, "template", "test", chartDir, "--namespace", "orka-test", "--is-upgrade")
	commandArgs = append(commandArgs, defaults...)
	commandArgs = append(commandArgs, args...)
	output, err := exec.Command(helm, commandArgs...).CombinedOutput()
	return string(output), err
}

func requireHelmRender(t *testing.T, args ...string) string {
	t.Helper()
	output, err := helmTemplateStaticChart(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, output)
	}
	return output
}

func requireHarnessV1UpgradeDrainHookRender(t *testing.T, matchesDesiredGeneration bool, args ...string) string {
	t.Helper()
	output, err := helmTemplateHarnessV1UpgradeDrainHook(t, harnessV1UpgradeState{
		matchesDesiredGeneration: matchesDesiredGeneration,
		authSecret:               "harness-wrapper-auth",
		authKey:                  "token",
		tlsSecret:                "harness-wrapper-tls",
	}, args...)
	if err != nil {
		t.Fatalf("helm template forced wrapper upgrade hook failed: %v\n%s", err, output)
	}
	return output
}

type harnessV1UpgradeState struct {
	matchesDesiredGeneration bool
	wrapperMissing           bool
	controllerState          string
	authSecret               string
	authKey                  string
	tlsSecret                string
}

func helmTemplateHarnessV1UpgradeDrainHook(
	t *testing.T,
	state harnessV1UpgradeState,
	args ...string,
) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for static chart render tests")
	}

	chartDir := filepath.Join(t.TempDir(), "static")
	if err := os.CopyFS(chartDir, os.DirFS("static")); err != nil {
		t.Fatalf("copy static chart: %v", err)
	}
	forceStaticChartNamespaceMode(t, chartDir, "harness-v1")
	hookPath := filepath.Join(chartDir, "templates", "harness-wrapper-drain-hook.yaml")
	hook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read wrapper drain hook: %v", err)
	}
	lookup := `{{- $existingWrapper := lookup "apps/v1" "Deployment" .Release.Namespace $wrapperName }}`
	existingGeneration := `"current-generation"`
	if state.matchesDesiredGeneration {
		existingGeneration = `$desiredGeneration`
	}
	currentImage := "registry.example/current-wrapper@sha256:" + strings.Repeat("2", 64)
	tlsSecret := state.tlsSecret
	if tlsSecret == "" {
		tlsSecret = "harness-wrapper-tls"
	}
	forcedLookup := `{{- $existingWrapper := dict }}`
	if !state.wrapperMissing {
		forcedLookup = strings.Join([]string{
			`{{- $existingWrapper := dict`,
			`"metadata" (dict "name" $wrapperName)`,
			`"spec" (dict "template" (dict "spec" (dict`,
			`"containers" (list (dict "name" "wrapper"`,
			`"image" "` + currentImage + `" "imagePullPolicy" "Always"`,
			`"env" (list (dict "name" "ORKA_HARNESS_WRAPPER_LEDGER_GENERATION"`,
			`"value" ` + existingGeneration + `))))`,
			`"volumes" (list (dict "name" "auth" "secret"`,
			`(dict "secretName" "` + state.authSecret + `" "items"`,
			`(list (dict "key" "` + state.authKey + `" "path" "token"))))`,
			`(dict "name" "tls" "secret" (dict "secretName" "` + tlsSecret + `")))))) }}`,
		}, " ")
	}
	forced := strings.Replace(string(hook), lookup, forcedLookup, 1)
	if forced == string(hook) {
		t.Fatalf("wrapper drain hook is not gated by the exact existing Deployment lookup")
	}
	if state.controllerState != "" {
		var controllerArgs []string
		switch state.controllerState {
		case "enabled":
			controllerArgs = []string{
				`"--controller-mode=harness-v1"`,
				`"--harness-v1-auth-secret-name=` + state.authSecret + `"`,
				`"--harness-v1-auth-secret-key=` + state.authKey + `"`,
			}
		default:
			t.Fatalf("unsupported forced controller state %q", state.controllerState)
		}
		controllerLookup := `{{- $existingController := lookup "apps/v1" "Deployment" .Release.Namespace $controllerName }}`
		forcedControllerLookup := `{{- $existingController := dict "spec" (dict "template" (dict "spec" ` +
			`(dict "containers" (list (dict "name" "controller" "args" (list ` +
			strings.Join(controllerArgs, " ") + `)))))) }}`
		withController := strings.Replace(forced, controllerLookup, forcedControllerLookup, 1)
		if withController == forced {
			t.Fatalf("wrapper drain hook is not gated by the exact existing controller Deployment lookup")
		}
		forced = withController
	}
	if err := os.WriteFile(hookPath, []byte(forced), 0o600); err != nil {
		t.Fatalf("force existing wrapper lookup in copied chart: %v", err)
	}

	defaults := staticChartDefaultArgs()
	commandArgs := make([]string, 0, 6+len(defaults)+len(args))
	commandArgs = append(commandArgs, "template", "test", chartDir, "--namespace", "orka-test", "--is-upgrade")
	commandArgs = append(commandArgs, defaults...)
	commandArgs = append(commandArgs, args...)
	output, err := exec.Command(helm, commandArgs...).CombinedOutput()
	return string(output), err
}

var harnessV1GenerationPattern = regexp.MustCompile(
	`(?m)name: ORKA_HARNESS_WRAPPER_LEDGER_GENERATION\n\s+value: "([a-f0-9]{64})"`,
)

func harnessV1RenderedGeneration(t *testing.T, rendered string) string {
	t.Helper()
	match := harnessV1GenerationPattern.FindStringSubmatch(rendered)
	if len(match) != 2 {
		t.Fatalf("rendered harness v1 Deployment is missing a canonical generation:\n%s", rendered)
	}
	return match[1]
}

func requireRenderedDocument(t *testing.T, rendered string, markers ...string) string {
	t.Helper()
	for document := range strings.SplitSeq(rendered, "\n---\n") {
		matched := true
		for _, marker := range markers {
			if !strings.Contains(document, marker) {
				matched = false
				break
			}
		}
		if matched {
			return document
		}
	}
	t.Fatalf("rendered chart has no document containing %q:\n%s", markers, rendered)
	return ""
}

func renderedResourceName(t *testing.T, rendered string, markers ...string) string {
	t.Helper()
	document := requireRenderedDocument(t, rendered, markers...)
	match := regexp.MustCompile(`(?m)^metadata:\n  name: ([^\n]+)$`).FindStringSubmatch(document)
	if len(match) != 2 {
		t.Fatalf("rendered resource has no metadata.name:\n%s", document)
	}
	return strings.Trim(match[1], `"`)
}

func TestStaticChartLongReleaseNamesKeepClusterScopedResourcesDistinct(t *testing.T) {
	prefix := strings.Repeat("a", 52)
	fullNamePrefix := strings.Repeat("b", 62)
	renderedA, err := helmTemplateStaticChartForRelease(
		t, prefix+"x", "orka-test", "--set-string", "fullnameOverride="+fullNamePrefix+"x",
	)
	if err != nil {
		t.Fatalf("helm template first long release failed: %v\n%s", err, renderedA)
	}
	renderedB, err := helmTemplateStaticChartForRelease(
		t, prefix+"y", "orka-test", "--set-string", "fullnameOverride="+fullNamePrefix+"y",
	)
	if err != nil {
		t.Fatalf("helm template second long release failed: %v\n%s", err, renderedB)
	}

	resources := []struct {
		name      string
		maxLength int
		markers   []string
	}{
		{
			name:      "validating webhook",
			maxLength: 63,
			markers:   []string{"kind: ValidatingWebhookConfiguration"},
		},
		{
			name:      "controller cluster role",
			maxLength: 253,
			markers:   []string{"kind: ClusterRole", `resources: ["tokenreviews"]`},
		},
	}
	for _, resource := range resources {
		t.Run(resource.name, func(t *testing.T) {
			nameA := renderedResourceName(t, renderedA, resource.markers...)
			nameB := renderedResourceName(t, renderedB, resource.markers...)
			if nameA == nameB {
				t.Fatalf("long release names collapsed to cluster-scoped name %q", nameA)
			}
			if len(nameA) > resource.maxLength || len(nameB) > resource.maxLength {
				t.Fatalf("cluster-scoped names exceed %d characters: %q, %q", resource.maxLength, nameA, nameB)
			}
		})
	}

	clusterRoleA := renderedResourceName(t, renderedA, "kind: ClusterRole", `resources: ["tokenreviews"]`)
	bindingA := requireRenderedDocument(
		t,
		renderedA,
		"kind: ClusterRoleBinding",
		"kind: ClusterRole\n  name: "+clusterRoleA,
	)
	if bindingName := renderedResourceName(t, bindingA, "kind: ClusterRoleBinding"); bindingName != clusterRoleA {
		t.Fatalf("controller ClusterRoleBinding name %q does not match ClusterRole %q", bindingName, clusterRoleA)
	}

	shortRender := requireHelmRender(t)
	webhookName := renderedResourceName(t, shortRender, "kind: ValidatingWebhookConfiguration")
	if webhookName != "test-orka-controller" {
		t.Fatalf("short validating webhook name changed to %q", webhookName)
	}
	clusterRoleName := renderedResourceName(t, shortRender, "kind: ClusterRole", `resources: ["tokenreviews"]`)
	if clusterRoleName != "test-orka-controller-cluster" {
		t.Fatalf("short controller ClusterRole name changed to %q", clusterRoleName)
	}

	otherNamespaceRender, err := helmTemplateStaticChartForRelease(
		t, prefix+"x", "orka-other", "--set-string", "fullnameOverride="+fullNamePrefix+"x",
	)
	if err != nil {
		t.Fatalf("helm template other namespace failed: %v\n%s", err, otherNamespaceRender)
	}
	otherNamespaceWebhook := renderedResourceName(t, otherNamespaceRender, "kind: ValidatingWebhookConfiguration")
	webhookA := renderedResourceName(t, renderedA, "kind: ValidatingWebhookConfiguration")
	if webhookA == otherNamespaceWebhook {
		t.Fatalf("namespaces collapsed to cluster-scoped validating webhook name %q", webhookA)
	}
}

func TestStaticChartUsesServicePortForInClusterControllerURLs(t *testing.T) {
	rendered := requireHelmRender(t,
		"--set", "service.port=18080",
		"--set", "controller.apiPort=8080",
	)

	if got := strings.Count(rendered, "--controller-url="); got != 1 {
		t.Fatalf("controller URL argument count = %d, want 1", got)
	}
	if !strings.Contains(rendered, "--controller-url=http://test-orka.orka-test.svc:18080") {
		t.Fatalf("controller URL does not use service.port:\n%s", rendered)
	}
	for _, variable := range []string{
		"ORKA_PUBLISHER_ARTIFACT_AUTHORIZATION_BROKER_URL",
		"ORKA_PUBLISHER_ARTIFACT_API_URL",
		"ORKA_PUBLISHER_CREDENTIAL_BROKER_URL",
	} {
		marker := "name: " + variable + "\n              value: http://test-orka:18080"
		if !strings.Contains(rendered, marker) {
			t.Fatalf("%s does not use service.port", variable)
		}
	}

	service := requireHelmRender(t,
		"--set", "service.port=18080",
		"--set", "controller.apiPort=8080",
		"--show-only", "templates/service.yaml",
	)
	if !strings.Contains(service, "port: 18080") || !strings.Contains(service, "targetPort: api") {
		t.Fatalf("controller Service does not preserve service port to named API target:\n%s", service)
	}
}

func TestStaticChartProviderProxyUsesOperatorGateway(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0", 64)
	args := []string{
		"--set", "providerProxy.enabled=true",
		"--set", "controller.acpRuntime.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "controller.image.digest=" + digest,
		"--set-string", "publisher.image.digest=" + digest,
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--set-string", "controller.acpRuntime.providerProxyNamespace=orka-test",
		"--set-string", "providerProxy.upstreamBaseURL=http://model-gateway.models.svc:8080/",
	}
	rendered := requireHelmRender(t, args...)

	for _, marker := range []string{
		"--acp-provider-proxy-base-url=http://test-orka-provider-auth-proxy.orka-test.svc:8080",
		"--acp-provider-proxy-namespace=orka-test",
		"--upstream-base-url=http://model-gateway.models.svc:8080",
	} {
		if !strings.Contains(rendered, marker) {
			t.Fatalf("rendered provider proxy configuration is missing %q", marker)
		}
	}
	if strings.Contains(rendered, "--upstream-base-url=http://model-gateway.models.svc:8080/") {
		t.Fatalf("provider upstream trailing slash was not normalized")
	}

	providerPolicy := requireHelmRender(t,
		"--set", "providerProxy.enabled=true",
		"--show-only", "templates/provider-proxy-networkpolicy.yaml",
	)
	var policy networkingv1.NetworkPolicy
	if err := yaml.Unmarshal([]byte(providerPolicy), &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Egress) != 2 || len(policy.Spec.Egress[1].To) != 1 || len(policy.Spec.Egress[1].Ports) != 1 {
		t.Fatal("provider egress must contain only DNS and the configured gateway rule")
	}
	gateway := policy.Spec.Egress[1]
	peer := gateway.To[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "models" ||
		peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "model-gateway" ||
		gateway.Ports[0].Port == nil || gateway.Ports[0].Port.IntVal != 8080 {
		t.Fatalf("provider egress does not select the configured gateway: %#v", gateway)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
		t.Fatal("provider ingress must stay restricted to runtime Pods")
	}
	runtimePeer := policy.Spec.Ingress[0].From[0]
	if runtimePeer.NamespaceSelector == nil ||
		runtimePeer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "orka-runtimes" ||
		runtimePeer.PodSelector == nil || runtimePeer.PodSelector.MatchLabels["orka.ai/network-role"] != "provider-client" {
		t.Fatalf("provider ingress lost runtime isolation: %#v", runtimePeer)
	}
	if strings.Contains(rendered, "namespace: models") || strings.Contains(rendered, "vekil") {
		t.Fatal("chart must not manage the operator's gateway namespace or select Vekil")
	}
}

func TestStaticChartProviderProxyCanRemainDisabled(t *testing.T) {
	rendered := requireHelmRender(t,
		"--set", "providerProxy.enabled=false",
		"--set-string", "providerProxy.upstreamBaseURL=",
		"--set-json", "providerProxy.egress=[]",
	)
	for _, forbidden := range []string{"provider-auth-proxy", "--acp-provider-proxy-", "name: provider-auth", "vekil"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("disabled provider proxy still rendered %q", forbidden)
		}
	}
	if !strings.Contains(rendered, "--controller-mode=harness-v2") {
		t.Fatal("installing without a gateway disabled the default controller mode")
	}
}

func TestStaticChartEnforcesSQLiteControllerSafety(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{
			name:      "zero replicas",
			args:      []string{"--set", "controller.replicas=0"},
			wantError: "controller.replicas must be exactly 1 when using the SQLite store backend",
		},
		{
			name:      "multiple replicas",
			args:      []string{"--set", "controller.replicas=2"},
			wantError: "controller.replicas must be exactly 1 when using the SQLite store backend",
		},
		{
			name:      "leader election disabled",
			args:      []string{"--set", "controller.leaderElect=false"},
			wantError: "controller.leaderElect must be true for an isolated controller installation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, tt.args...)
			if err == nil {
				t.Fatalf("helm template unexpectedly accepted unsafe SQLite controller configuration:\n%s", output)
			}
			if !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm template error does not contain %q:\n%s", tt.wantError, output)
			}
		})
	}

	rendered := requireHelmRender(t, "--show-only", "templates/deployment.yaml")
	for _, marker := range []string{
		"replicas: 1",
		"strategy:\n    type: Recreate",
		"--leader-elect=true",
	} {
		if !strings.Contains(rendered, marker) {
			t.Fatalf("controller deployment is missing SQLite safety marker %q:\n%s", marker, rendered)
		}
	}
}

func TestStaticChartGeneratesAgentExecutionSnapshotSecret(t *testing.T) {
	args := []string{
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=",
		"--set-string", "controller.agentExecutionSnapshot.key=",
	}
	rendered := requireHelmRender(t, args...)

	secret := requireRenderedDocument(t, rendered, "kind: Secret\n", "\n  name: test-orka-agent-execution-snapshot\n")
	for _, marker := range []string{
		"helm.sh/resource-policy: keep",
		"app.kubernetes.io/component: controller",
		"type: Opaque",
	} {
		if !strings.Contains(secret, marker) {
			t.Fatalf("generated snapshot Secret is missing %q:\n%s", marker, secret)
		}
	}
	// The chart must never render key material: Helm keeps rendered
	// manifests in release history. The controller mints the key at runtime.
	if strings.Contains(secret, "\ndata:") || strings.Contains(secret, "\nstringData:") {
		t.Fatalf("generated snapshot Secret must be rendered empty:\n%s", secret)
	}

	deployment := requireRenderedDocument(t, rendered, "kind: Deployment\n", "\n  name: test-orka-controller\n")
	for _, marker := range []string{
		`--agent-execution-snapshot-secret=test-orka-agent-execution-snapshot`,
		`--agent-execution-snapshot-secret-key=key`,
		"--agent-execution-snapshot-key-file=/var/run/orka/agent-execution-snapshot/key",
		"secretName: \"test-orka-agent-execution-snapshot\"",
		"key: \"key\"",
		"path: key",
	} {
		if !strings.Contains(deployment, marker) {
			t.Fatalf("controller deployment does not bootstrap the generated snapshot Secret %q:\n%s", marker, deployment)
		}
	}
	volume := renderedVolume(t, deployment, "agent-execution-snapshot-key")
	if !strings.Contains(volume, "optional: true") {
		t.Fatalf("generated snapshot key mount must be optional so the Pod can start before the key exists:\n%s", volume)
	}

	explicit := requireHelmRender(t)
	if strings.Contains(explicit, "name: test-orka-agent-execution-snapshot") {
		t.Fatalf("chart generated a snapshot Secret although existingSecret was supplied:\n%s", explicit)
	}
	explicitDeployment := requireRenderedDocument(t, explicit, "kind: Deployment\n", "\n  name: test-orka-controller\n")
	if strings.Contains(explicitDeployment, "--agent-execution-snapshot-secret=") {
		t.Fatalf("operator-supplied snapshot key must not enable controller bootstrap:\n%s", explicitDeployment)
	}
	if strings.Contains(renderedVolume(t, explicitDeployment, "agent-execution-snapshot-key"), "optional: true") {
		t.Fatalf("operator-supplied snapshot key mount must stay required:\n%s", explicitDeployment)
	}
}

// renderedVolume returns the named volume block of a rendered Deployment.
func renderedVolume(t *testing.T, deployment, name string) string {
	t.Helper()
	start := strings.Index(deployment, "\n        - name: "+name+"\n")
	if start < 0 {
		t.Fatalf("controller deployment has no %s volume:\n%s", name, deployment)
	}
	volume := deployment[start+1:]
	if next := strings.Index(volume[1:], "\n        - name: "); next >= 0 {
		volume = volume[:next+1]
	}
	return volume
}

func TestStaticChartEnforcesNamespaceModeWithAdmissionPolicy(t *testing.T) {
	rendered := requireHelmRender(t)

	policy := requireRenderedDocument(t, rendered,
		"kind: ValidatingAdmissionPolicy\n", "\n  name: test-orka-namespace-mode\n")
	for _, marker := range []string{
		`expression: "object.metadata.name == 'orka-test'"`,
		`expression: "'harness-v2'"`,
		`expression: "'system:serviceaccount:orka-test:test-orka'"`,
		"claim is immutable",
		"only the release's controller may claim an existing namespace",
	} {
		if !strings.Contains(policy, marker) {
			t.Fatalf("namespace-mode policy is missing %q:\n%s", marker, policy)
		}
	}
	requireRenderedDocument(t, rendered,
		"kind: ValidatingAdmissionPolicyBinding\n", "\n  name: test-orka-namespace-mode\n")

	// The claim is no longer a controller-served webhook: the controller must
	// be able to claim its namespace before its own webhook server is up.
	webhook := requireRenderedDocument(t, rendered,
		"kind: ValidatingWebhookConfiguration\n", "\n  name: test-orka-controller\n")
	for _, marker := range []string{"namespace-mode.", "/validate-v1-namespace-execution-mode"} {
		if strings.Contains(webhook, marker) {
			t.Fatalf("namespace-mode webhook must not be rendered any more:\n%s", webhook)
		}
	}

	clusterRole := requireRenderedDocument(t, rendered, "kind: ClusterRole\n", "\n  name: test-orka-controller-cluster\n")
	if !strings.Contains(clusterRole, `resourceNames: ["orka-test"]
    verbs: ["get", "update"]`) {
		t.Fatalf("controller ClusterRole must allow updating only its own namespace:\n%s", clusterRole)
	}
	if strings.Contains(clusterRole, `resourceNames: ["orka-runtimes"]
    verbs: ["get", "update"]`) {
		t.Fatalf("controller ClusterRole must not allow updating the runtime namespace:\n%s", clusterRole)
	}
}

// requireHelmRenderWithUpstreamService renders the static chart with the
// provider proxy's upstream Service lookup forced to return service.
func requireHelmRenderWithUpstreamService(t *testing.T, service string, args ...string) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for static chart render tests")
	}
	chartDir := filepath.Join(t.TempDir(), "static")
	if err := os.CopyFS(chartDir, os.DirFS("static")); err != nil {
		t.Fatalf("copy static chart: %v", err)
	}
	templatePath := filepath.Join(chartDir, "templates", "provider-proxy-networkpolicy.yaml")
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read provider proxy NetworkPolicy template: %v", err)
	}
	lookup := `{{- $service := lookup "v1" "Service" $upstream.namespace $upstream.name -}}`
	forced := `{{- $service := ` + service + ` -}}`
	withService := strings.Replace(string(template), lookup, forced, 1)
	if withService == string(template) {
		t.Fatalf("provider proxy egress derivation is not gated by the upstream Service lookup")
	}
	if err := os.WriteFile(templatePath, []byte(withService), 0o600); err != nil {
		t.Fatalf("force upstream Service lookup in copied chart: %v", err)
	}
	defaults := staticChartDefaultArgs()
	commandArgs := make([]string, 0, 5+len(defaults)+len(args))
	commandArgs = append(commandArgs, "template", "test", chartDir, "--namespace", "orka-test")
	commandArgs = append(commandArgs, defaults...)
	commandArgs = append(commandArgs, args...)
	output, err := exec.Command(helm, commandArgs...).CombinedOutput()
	return string(output), err
}

func TestStaticChartDerivesProviderProxyEgressFromUpstreamService(t *testing.T) {
	service := `dict "spec" (dict ` +
		`"selector" (dict "app.kubernetes.io/name" "vekil" "app.kubernetes.io/instance" "vekil") ` +
		`"ports" (list (dict "port" 1337 "targetPort" 8080)))`
	output, err := requireHelmRenderWithUpstreamService(t, service,
		"--set-json", "providerProxy.egress=[]",
		"--set-string", "providerProxy.upstreamBaseURL=http://vekil.vekil-system.svc:1337",
		"--show-only", "templates/provider-proxy-networkpolicy.yaml",
	)
	if err != nil {
		t.Fatalf("helm template with a derivable upstream Service failed: %v\n%s", err, output)
	}
	var policy networkingv1.NetworkPolicy
	if err := yaml.Unmarshal([]byte(output), &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Egress) != 2 || len(policy.Spec.Egress[1].To) != 1 || len(policy.Spec.Egress[1].Ports) != 1 {
		t.Fatalf("derived egress must be exactly DNS plus the gateway rule:\n%s", output)
	}
	gateway := policy.Spec.Egress[1]
	peer := gateway.To[0]
	namespaceLabels := map[string]string{}
	if peer.NamespaceSelector != nil {
		namespaceLabels = peer.NamespaceSelector.MatchLabels
	}
	if namespaceLabels["kubernetes.io/metadata.name"] != "vekil-system" ||
		peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "vekil" ||
		peer.PodSelector.MatchLabels["app.kubernetes.io/instance"] != "vekil" ||
		gateway.Ports[0].Port == nil || gateway.Ports[0].Port.IntVal != 8080 {
		t.Fatalf("derived egress does not select the Service's Pods on its target port: %#v", gateway)
	}

	// A UDP entry sharing the port number must not win over the TCP one.
	output, err = requireHelmRenderWithUpstreamService(t,
		`dict "spec" (dict "selector" (dict "app" "gw") "ports" (list `+
			`(dict "port" 1337 "protocol" "TCP" "targetPort" 8443) `+
			`(dict "port" 1337 "protocol" "UDP" "targetPort" 9443)))`,
		"--set-json", "providerProxy.egress=[]",
		"--set-string", "providerProxy.upstreamBaseURL=http://vekil.vekil-system.svc:1337",
		"--show-only", "templates/provider-proxy-networkpolicy.yaml",
	)
	if err != nil || !strings.Contains(output, "port: 8443\n") || strings.Contains(output, "9443") {
		t.Fatalf("derivation must take the TCP target port, not the UDP one: %v\n%s", err, output)
	}

	// Without a target port the Service port is the Pod port; a cluster.local
	// suffix and the scheme's default port are accepted too.
	output, err = requireHelmRenderWithUpstreamService(t,
		`dict "spec" (dict "selector" (dict "app" "gw") "ports" (list (dict "port" 80)))`,
		"--set-json", "providerProxy.egress=[]",
		"--set-string", "providerProxy.upstreamBaseURL=http://gw.models.svc.cluster.local",
		"--show-only", "templates/provider-proxy-networkpolicy.yaml",
	)
	if err != nil || !strings.Contains(output, "port: 80\n") ||
		!strings.Contains(output, "kubernetes.io/metadata.name: \"models\"") {
		t.Fatalf("default-port derivation failed: %v\n%s", err, output)
	}

	for name, tt := range map[string]struct {
		service   string
		wantError string
	}{
		"named target port": {
			service:   `dict "spec" (dict "selector" (dict "app" "gw") "ports" (list (dict "port" 1337 "targetPort" "http")))`,
			wantError: "maps to the named target port",
		},
		"no matching port": {
			service:   `dict "spec" (dict "selector" (dict "app" "gw") "ports" (list (dict "port" 9999)))`,
			wantError: "has no port 1337",
		},
		"only a UDP entry for the port": {
			service:   `dict "spec" (dict "selector" (dict "app" "gw") "ports" (list (dict "port" 1337 "protocol" "UDP")))`,
			wantError: "has no port 1337",
		},
		"no selector": {
			service:   `dict "spec" (dict "ports" (list (dict "port" 1337)))`,
			wantError: "has no selector",
		},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := requireHelmRenderWithUpstreamService(t, tt.service,
				"--set-json", "providerProxy.egress=[]",
				"--set-string", "providerProxy.upstreamBaseURL=http://vekil.vekil-system.svc:1337",
			)
			if err == nil || !strings.Contains(output, tt.wantError) {
				t.Fatalf("render error = %v, want %q:\n%s", err, tt.wantError, output)
			}
		})
	}
}

func TestStaticChartGeneratesWebhookTLSSecret(t *testing.T) {
	args := []string{
		"--set-string", "webhooks.tls.existingSecret=",
		"--set-string", "webhooks.caBundle=",
	}
	rendered := requireHelmRender(t, args...)

	secret := requireRenderedDocument(t, rendered, "kind: Secret\n", "\n  name: test-orka-webhook-tls\n")
	for _, marker := range []string{"helm.sh/resource-policy: keep", "type: Opaque"} {
		if !strings.Contains(secret, marker) {
			t.Fatalf("generated webhook TLS Secret is missing %q:\n%s", marker, secret)
		}
	}
	if strings.Contains(secret, "\ndata:") || strings.Contains(secret, "\nstringData:") {
		t.Fatalf("generated webhook TLS Secret must be rendered empty so the controller populates it:\n%s", secret)
	}

	deployment := requireRenderedDocument(t, rendered, "kind: Deployment\n", "\n  name: test-orka-controller\n")
	for _, marker := range []string{
		`--webhook-cert-rotation-secret=test-orka-webhook-tls`,
		`--webhook-cert-rotation-webhook=test-orka-controller`,
		`--webhook-cert-rotation-dns-name=test-orka-webhook.orka-test.svc`,
		"--webhook-cert-path=/var/run/orka/webhook/tls",
	} {
		if !strings.Contains(deployment, marker) {
			t.Fatalf("controller deployment is missing rotation marker %q:\n%s", marker, deployment)
		}
	}
	// cert-controller writes only the Secret; the kubelet must project it into
	// the cert directory, so generated mode mounts the whole generated Secret.
	volume := renderedVolume(t, deployment, "webhook-tls")
	if !strings.Contains(volume, `secretName: "test-orka-webhook-tls"`) || strings.Contains(volume, "items:") {
		t.Fatalf("generated webhook TLS must mount the whole generated Secret:\n%s", volume)
	}

	webhook := requireRenderedDocument(t, rendered,
		"kind: ValidatingWebhookConfiguration\n", "\n  name: test-orka-controller\n")
	if strings.Contains(webhook, "caBundle:") {
		t.Fatalf("generated mode must leave caBundle to the controller:\n%s", webhook)
	}

	clusterRole := requireRenderedDocument(t, rendered, "kind: ClusterRole\n", "\n  name: test-orka-controller-cluster\n")
	for _, marker := range []string{
		`resources: ["validatingwebhookconfigurations"]`,
		`resourceNames: ["test-orka-controller"]`,
	} {
		if !strings.Contains(clusterRole, marker) {
			t.Fatalf("controller ClusterRole is missing rotation permission %q:\n%s", marker, clusterRole)
		}
	}

	// Update on a webhook configuration is whole-object, so a policy bounds
	// the controller to caBundle changes on its own configuration.
	guard := requireRenderedDocument(t, rendered,
		"kind: ValidatingAdmissionPolicy\n", "\n  name: test-orka-webhook-configuration\n")
	for _, marker := range []string{
		`expression: "object.metadata.name == 'test-orka-controller'"`,
		`expression: "request.userInfo.username == 'system:serviceaccount:orka-test:test-orka'"`,
		"only clientConfig.caBundle may change",
		"w.clientConfig.?url.orValue('') == o.clientConfig.?url.orValue('')",
	} {
		if !strings.Contains(guard, marker) {
			t.Fatalf("webhook configuration guard policy is missing %q:\n%s", marker, guard)
		}
	}
	requireRenderedDocument(t, rendered,
		"kind: ValidatingAdmissionPolicyBinding\n", "\n  name: test-orka-webhook-configuration\n")
}

func TestStaticChartKeepsOperatorWebhookTLS(t *testing.T) {
	rendered := requireHelmRender(t)
	if strings.Contains(rendered, "name: test-orka-webhook-tls\n") {
		t.Fatalf("chart generated a webhook TLS Secret although existingSecret was supplied:\n%s", rendered)
	}
	deployment := requireRenderedDocument(t, rendered, "kind: Deployment\n", "\n  name: test-orka-controller\n")
	if strings.Contains(deployment, "--webhook-cert-rotation-secret") {
		t.Fatalf("operator-supplied webhook TLS must not enable rotation:\n%s", deployment)
	}
	if !strings.Contains(deployment, `secretName: "controller-webhook-tls"`) {
		t.Fatalf("operator-supplied webhook TLS Secret is not mounted:\n%s", deployment)
	}
	clusterRole := requireRenderedDocument(t, rendered, "kind: ClusterRole\n", "\n  name: test-orka-controller-cluster\n")
	if strings.Contains(clusterRole, "validatingwebhookconfigurations") {
		t.Fatalf("operator-supplied webhook TLS must not grant webhook configuration writes:\n%s", clusterRole)
	}
	if strings.Contains(rendered, "name: test-orka-webhook-configuration\n") {
		t.Fatalf("operator-supplied webhook TLS needs no webhook configuration guard policy:\n%s", rendered)
	}

	output, err := helmTemplateStaticChart(t, "--set-string", "webhooks.caBundle=")
	wantError := "caBundle or caInjectionAnnotations when webhooks.tls.existingSecret is set"
	if err == nil || !strings.Contains(output, wantError) {
		t.Fatalf("operator-supplied webhook TLS without CA trust was accepted: %v\n%s", err, output)
	}
}

func TestStaticChartMountsAgentExecutionSnapshotKey(t *testing.T) {
	args := []string{
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--show-only", "templates/deployment.yaml",
	}
	rendered := requireHelmRender(t, args...)

	for _, marker := range []string{
		"--agent-execution-snapshot-key-file=/var/run/orka/agent-execution-snapshot/key",
		"mountPath: /var/run/orka/agent-execution-snapshot",
		"readOnly: true",
		"secretName: \"snapshot-key\"",
		"key: \"encryption-key\"",
		"path: key",
	} {
		if !strings.Contains(rendered, marker) {
			t.Fatalf("controller deployment is missing snapshot key marker %q:\n%s", marker, rendered)
		}
	}
}

func TestStaticChartDefaultsToHarnessV2AndAllowsV1Override(t *testing.T) {
	v2 := requireHelmRender(t)
	for _, marker := range []string{
		"--controller-mode=harness-v2",
		"app.kubernetes.io/component: acp-runtime",
		"app.kubernetes.io/component: provider-auth-proxy",
	} {
		if !strings.Contains(v2, marker) {
			t.Fatalf("harness-v2 render is missing %q:\n%s", marker, v2)
		}
	}
	if strings.Contains(v2, "app.kubernetes.io/component: agent-harness-wrapper") {
		t.Fatalf("harness-v2 render contains the harness-v1 data plane:\n%s", v2)
	}

	digest := "sha256:" + strings.Repeat("1", 64)
	v1 := requireHelmRender(t,
		"--set-string", "controller.mode=harness-v1",
		"--set-string", "harnessV1.image.digest="+digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
	)
	for _, marker := range []string{
		"--controller-mode=harness-v1",
		"app.kubernetes.io/component: agent-harness-wrapper",
		"--harness-v1-endpoint=https://test-orka-agent-harness-wrapper.orka-test.svc:8080",
	} {
		if !strings.Contains(v1, marker) {
			t.Fatalf("harness-v1 render is missing %q:\n%s", marker, v1)
		}
	}
	for _, forbidden := range []string{
		"app.kubernetes.io/component: acp-runtime",
		"app.kubernetes.io/component: provider-auth-proxy",
		"app.kubernetes.io/component: workspace-publisher",
	} {
		if strings.Contains(v1, forbidden) {
			t.Fatalf("harness-v1 render contains harness-v2 component %q:\n%s", forbidden, v1)
		}
	}
}

func TestStaticChartRejectsNonStaticControllerModes(t *testing.T) {
	for _, mode := range []string{"", "dual", "auto", "harness-v1-drain", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, "--set-string", "controller.mode="+mode)
			if err == nil || !strings.Contains(output, "controller.mode must be harness-v1 or harness-v2") {
				t.Fatalf("helm render error = %v, want static-mode rejection:\n%s", err, output)
			}
		})
	}
}

func TestStaticChartRejectsControllerWatchScopeChangesOnUpgrade(t *testing.T) {
	const watchScopeError = `controller.watchNamespace is immutable; ` +
		`the existing controller must already watch namespace "orka-test"`
	tests := []struct {
		name                   string
		existingControllerArgs []string
		wantError              string
	}{
		{
			name:                   "legacy cluster-wide controller",
			existingControllerArgs: []string{"--acp-runtime-enabled=true"},
			wantError:              watchScopeError,
		},
		{
			name: "legacy controller in another namespace",
			existingControllerArgs: []string{
				"--watch-namespace=other-namespace",
				"--acp-runtime-enabled=true",
			},
			wantError: watchScopeError,
		},
		{
			name: "legacy controller in the release namespace",
			existingControllerArgs: []string{
				"--watch-namespace=orka-test",
				"--acp-runtime-enabled=true",
			},
			wantError: "implicit or legacy harness-v2 installations cannot upgrade in place",
		},
		{
			name: "static harness v2 controller in the release namespace",
			existingControllerArgs: []string{
				"--controller-mode=harness-v2",
				"--watch-namespace=orka-test",
				"--controller-url=http://test-orka.orka-test.svc:8080",
				"--acp-runtime-namespace=orka-runtimes",
			},
		},
		{
			name:                   "static harness v2 namespace with a deleted controller",
			existingControllerArgs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChartWithExistingController(t, tt.existingControllerArgs)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("same-mode static controller upgrade failed: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm render error = %v, want watch-scope rejection:\n%s", err, output)
			}
		})
	}
}

func TestStaticChartRejectsHarnessV2IdentityChangesOnUpgrade(t *testing.T) {
	staticControllerArgs := []string{
		"--controller-mode=harness-v2",
		"--watch-namespace=orka-test",
		"--controller-url=http://test-orka.orka-test.svc:8080",
		"--acp-runtime-namespace=orka-runtimes",
	}
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{
			name:      "fullname override",
			args:      []string{"--set-string", "fullnameOverride=renamed"},
			wantError: "the effective chart fullname is immutable for harness-v2 upgrades",
		},
		{
			name:      "effective name override",
			args:      []string{"--set-string", "nameOverride=renamed"},
			wantError: "the effective chart fullname is immutable for harness-v2 upgrades",
		},
		{
			name:      "ACP runtime namespace",
			args:      []string{"--set-string", "controller.acpRuntime.namespace=other-runtimes"},
			wantError: "controller.acpRuntime.namespace is immutable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChartWithExistingController(t, staticControllerArgs, tt.args...)
			if err == nil || !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm render error = %v, want immutable identity rejection %q:\n%s", err, tt.wantError, output)
			}
		})
	}
}

func TestStaticChartRejectsAgentExecutionSnapshotIdentityChangesOnUpgrade(t *testing.T) {
	staticControllerArgs := []string{
		"--controller-mode=harness-v2",
		"--watch-namespace=orka-test",
		"--controller-url=http://test-orka.orka-test.svc:8080",
		"--acp-runtime-namespace=orka-runtimes",
	}
	tests := []struct {
		name           string
		existingSecret string
		existingKey    string
		args           []string
		wantError      string
	}{
		{
			name:           "Secret name changed",
			existingSecret: "old-snapshot-key",
			existingKey:    "encryption-key",
			args:           []string{"--set-string", "controller.agentExecutionSnapshot.existingSecret=new-snapshot-key"},
			wantError:      "controller.agentExecutionSnapshot.existingSecret is immutable for in-place upgrades",
		},
		{
			name:           "Secret item key changed",
			existingSecret: "snapshot-key",
			existingKey:    "old-encryption-key",
			args:           []string{"--set-string", "controller.agentExecutionSnapshot.key=new-encryption-key"},
			wantError:      "controller.agentExecutionSnapshot.key is immutable for in-place upgrades",
		},
		{
			name:           "explicit Secret replaced by the generated one",
			existingSecret: "snapshot-key",
			existingKey:    "encryption-key",
			args: []string{
				"--set-string", "controller.agentExecutionSnapshot.existingSecret=",
				"--set-string", "controller.agentExecutionSnapshot.key=",
			},
			wantError: "controller.agentExecutionSnapshot.existingSecret is immutable for in-place upgrades; " +
				"preserve \"snapshot-key\"",
		},
		{
			name:           "live Secret name missing",
			existingSecret: "",
			existingKey:    "encryption-key",
			wantError:      "cannot determine the existing agent execution snapshot Secret name",
		},
		{
			name:           "live Secret item key missing",
			existingSecret: "snapshot-key",
			existingKey:    "",
			wantError:      "cannot determine the existing agent execution snapshot Secret key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChartWithExistingControllerSnapshot(
				t,
				staticControllerArgs,
				tt.existingSecret,
				tt.existingKey,
				tt.args...,
			)
			if err == nil || !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm render error = %v, want snapshot identity rejection %q:\n%s", err, tt.wantError, output)
			}
		})
	}
}

func TestStaticChartAcceptsGeneratedAgentExecutionSnapshotIdentityOnUpgrade(t *testing.T) {
	output, err := helmTemplateStaticChartWithExistingControllerSnapshot(
		t,
		generatedSnapshotUpgradeControllerArgs(),
		"test-orka-agent-execution-snapshot",
		"key",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=",
		"--set-string", "controller.agentExecutionSnapshot.key=",
	)
	if err != nil {
		t.Fatalf("upgrade with the generated snapshot identity was rejected: %v\n%s", err, output)
	}
	secret := requireRenderedDocument(t, output, "kind: Secret\n", "\n  name: test-orka-agent-execution-snapshot\n")
	if strings.Contains(secret, "\ndata:") {
		t.Fatalf("upgrade must not render snapshot key material:\n%s", secret)
	}
}

func generatedSnapshotUpgradeControllerArgs() []string {
	return []string{
		"--controller-mode=harness-v2",
		"--watch-namespace=orka-test",
		"--controller-url=http://test-orka.orka-test.svc:8080",
		"--acp-runtime-namespace=orka-runtimes",
	}
}

func TestStaticChartRejectsUpgradeWithoutStaticNamespaceIdentity(t *testing.T) {
	output, err := helmTemplateStaticChart(t, "--is-upgrade")
	if err == nil || !strings.Contains(output, "controller mode identity is missing or incompatible") {
		t.Fatalf("helm render error = %v, want missing static namespace identity rejection:\n%s", err, output)
	}
}

//nolint:gocyclo // One render matrix verifies the coupled rollout, rollback, and uninstall invariants.
func TestStaticChartHarnessV1UpgradeDrainHookIsExistingDeploymentGated(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	args := []string{
		"--set-string", "controller.mode=harness-v1",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		"--set-string", "harnessV1.upgradeDrain.timeout=9m",
		"--set-string", "harnessV1.upgradeDrain.pollInterval=3s",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}
	// A fresh installation has no live Deployment and must not emit a drain
	// hook. The enabled revision must still persist its post-rollback abort hook because
	// Helm executes hooks recorded in the historical rollback target.
	fresh := requireHelmRender(t, args...)
	if strings.Contains(fresh, "app.kubernetes.io/component: agent-harness-wrapper-drain") ||
		strings.Contains(fresh, "helm.sh/hook: pre-upgrade") {
		t.Fatalf("fresh harness v1 render unexpectedly contains an upgrade drain hook:\n%s", fresh)
	}
	for _, marker := range []string{
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-abort",
		"helm.sh/hook: post-rollback",
		"- abort-rollover",
		`image: "ghcr.io/orka-agents/orka/agent-harness-wrapper@sha256:` + strings.Repeat("1", 64) + `"`,
		`secretName: "harness-wrapper-auth"`,
		`key: "token"`,
	} {
		if !strings.Contains(fresh, marker) {
			t.Fatalf("fresh enabled revision rollback hook is missing %q:\n%s", marker, fresh)
		}
	}
	if got := strings.Count(fresh, "helm.sh/hook: post-rollback"); got != 3 {
		t.Fatalf("fresh enabled rollback hook annotation count = %d, want 3:\n%s", got, fresh)
	}

	unknown, err := helmTemplateStaticChart(t, append(append([]string{}, args...), "--is-upgrade")...)
	if err == nil {
		t.Fatalf("upgrade without live controller or wrapper state rendered successfully:\n%s", unknown)
	}
	if !strings.Contains(unknown, "cannot determine the previously deployed harness v1 state during upgrade") {
		t.Fatalf("unknown-state upgrade did not fail closed:\n%s", unknown)
	}

	// Render the unchanged hook body from a copied chart with only lookup's
	// result replaced, so the existing-Deployment branch remains Helm-validated.
	hook := requireHarnessV1UpgradeDrainHookRender(t, false, args...)
	for _, marker := range []string{
		"kind: NetworkPolicy",
		"kind: Job",
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-drain",
		"app.kubernetes.io/component: agent-harness-wrapper-delete-drain",
		"helm.sh/hook: pre-upgrade,pre-rollback",
		"helm.sh/hook: pre-delete",
		`helm.sh/hook-weight: "-20"`,
		`helm.sh/hook-weight: "-10"`,
		"helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded",
		"backoffLimit: 0",
		"serviceAccountName: test-orka-agent-harness-wrapper",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"drop: [ALL]",
		`image: "registry.example/current-wrapper@sha256:` + strings.Repeat("2", 64) + `"`,
		"imagePullPolicy: Always",
		`command: ["/orka-agent-harness-wrapper"]`,
		"- drain",
		`- "--endpoint=https://test-orka-agent-harness-wrapper.orka-test.svc:8080"`,
		"- --bearer-token-file=/var/run/orka/harness-wrapper-auth/token",
		"- --ca-file=/var/run/orka/harness-wrapper-tls/ca.crt",
		`- "--timeout=9m"`,
		`- "--poll-interval=3s"`,
		`secretName: "harness-wrapper-auth"`,
		`key: "token"`,
		"defaultMode: 0440",
	} {
		if !strings.Contains(hook, marker) {
			t.Fatalf("harness v1 drain hook is missing %q:\n%s", marker, hook)
		}
	}
	if got := strings.Count(hook, "helm.sh/hook: pre-upgrade,pre-rollback"); got != 3 {
		t.Fatalf("rollover hook annotation count = %d, want 3:\n%s", got, hook)
	}
	if got := strings.Count(hook, "helm.sh/hook: pre-delete"); got != 3 {
		t.Fatalf("pre-delete hook annotation count = %d, want 3:\n%s", got, hook)
	}
	if !regexp.MustCompile(`--next-generation=[a-f0-9]{64}`).MatchString(hook) {
		t.Fatalf("rollover hook is missing its canonical replacement generation:\n%s", hook)
	}
	rolloverJob := requireRenderedDocument(t, hook,
		"kind: Job",
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-drain",
	)
	if strings.Contains(rolloverJob, "--controller-endpoint=") ||
		strings.Contains(rolloverJob, "--controller-token-file=") ||
		strings.Contains(rolloverJob, "serviceAccountToken:") {
		t.Fatalf("ordinary wrapper rollover unexpectedly retired controller-side v1 admission:\n%s", rolloverJob)
	}
	deleteJob := requireRenderedDocument(t, hook,
		"kind: Job",
		"app.kubernetes.io/component: agent-harness-wrapper-delete-drain",
	)
	if strings.Contains(deleteJob, "--controller-endpoint=") ||
		strings.Contains(deleteJob, "--controller-token-file=") ||
		strings.Contains(deleteJob, "serviceAccountToken:") {
		t.Fatalf("uninstall drain unexpectedly coordinates a cross-mode controller retirement:\n%s", deleteJob)
	}
	for _, marker := range []string{
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-abort",
		"helm.sh/hook: post-rollback",
		"- abort-rollover",
		`image: "ghcr.io/orka-agents/orka/agent-harness-wrapper@sha256:` + strings.Repeat("1", 64) + `"`,
		`secretName: "harness-wrapper-auth"`,
		`key: "token"`,
	} {
		if !strings.Contains(hook, marker) {
			t.Fatalf("changed-generation rollback hook is missing %q:\n%s", marker, hook)
		}
	}
	if strings.Contains(hook, "/usr/local/bin/node") {
		t.Fatalf("delete drain hook assumes an unavailable Node runtime:\n%s", hook)
	}
	if strings.Contains(hook, strings.Repeat("x", 32)) {
		t.Fatalf("harness v1 drain hook rendered a raw bearer token:\n%s", hook)
	}

	unchanged := requireHarnessV1UpgradeDrainHookRender(t, true, args...)
	if strings.Contains(unchanged, "helm.sh/hook: pre-upgrade,pre-rollback") ||
		strings.Contains(unchanged, "agent-harness-wrapper-rollover-drain") {
		t.Fatalf("unchanged wrapper Pod template unexpectedly triggered a rollover drain:\n%s", unchanged)
	}
	for _, marker := range []string{
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-abort",
		"helm.sh/hook: post-rollback",
		"- abort-rollover",
		`- "--endpoint=https://test-orka-agent-harness-wrapper.orka-test.svc:8080"`,
		"- --bearer-token-file=/var/run/orka/harness-wrapper-auth/token",
		"- --ca-file=/var/run/orka/harness-wrapper-tls/ca.crt",
		`secretName: "harness-wrapper-auth"`,
		`secretName: "harness-wrapper-tls"`,
		`key: "token"`,
	} {
		if !strings.Contains(unchanged, marker) {
			t.Fatalf("same-generation rollback hook is missing %q:\n%s", marker, unchanged)
		}
	}
	if got := strings.Count(unchanged, "helm.sh/hook: post-rollback"); got != 3 {
		t.Fatalf("rollback abort hook annotation count = %d, want 3:\n%s", got, unchanged)
	}
	if !regexp.MustCompile(`--expected-generation=[a-f0-9]{64}`).MatchString(unchanged) {
		t.Fatalf("rollback abort hook is missing its exact live generation:\n%s", unchanged)
	}
	if !strings.Contains(unchanged, "helm.sh/hook: pre-delete") {
		t.Fatalf("enabled release lost its uninstall drain hook:\n%s", unchanged)
	}
}

func TestStaticChartHarnessV1RejectsLiveAuthRotation(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	baseArgs := []string{
		"--set-string", "controller.mode=harness-v1",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}
	tests := []struct {
		name      string
		state     harnessV1UpgradeState
		args      []string
		wantError string
	}{
		{
			name: "Secret source",
			state: harnessV1UpgradeState{
				authSecret: "current-wrapper-auth",
				authKey:    "token",
			},
			args: []string{
				"--set-string", "harnessV1.auth.existingSecret=next-wrapper-auth",
			},
			wantError: "harnessV1.auth.existingSecret cannot change while the previously deployed " +
				"harness v1 route remains enabled",
		},
		{
			name: "Secret key",
			state: harnessV1UpgradeState{
				authSecret: "harness-wrapper-auth",
				authKey:    "current-token",
			},
			args: []string{
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.auth.tokenKey=next-token",
			},
			wantError: "harnessV1.auth.tokenKey cannot change while the previously deployed harness v1 route remains enabled",
		},
		{
			name: "missing wrapper Secret source",
			state: harnessV1UpgradeState{
				wrapperMissing:  true,
				controllerState: "enabled",
				authSecret:      "current-wrapper-auth",
				authKey:         "token",
			},
			args: []string{
				"--set-string", "harnessV1.auth.existingSecret=next-wrapper-auth",
			},
			wantError: "harnessV1.auth.existingSecret cannot change while the previously deployed " +
				"harness v1 route remains enabled",
		},
		{
			name: "missing wrapper Secret key",
			state: harnessV1UpgradeState{
				wrapperMissing:  true,
				controllerState: "enabled",
				authSecret:      "harness-wrapper-auth",
				authKey:         "current-token",
			},
			args: []string{
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.auth.tokenKey=next-token",
			},
			wantError: "harnessV1.auth.tokenKey cannot change while the previously deployed harness v1 route remains enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(append([]string{}, baseArgs...), tt.args...)
			output, err := helmTemplateHarnessV1UpgradeDrainHook(t, tt.state, args...)
			if err == nil {
				t.Fatalf("unsafe live auth rotation rendered successfully:\n%s", output)
			}
			if !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm template error is missing %q:\n%s", tt.wantError, output)
			}
		})
	}
}

func TestStaticChartHarnessV1TLSRotationUsesDrainedRollover(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	args := []string{
		"--set-string", "controller.mode=harness-v1",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=next-wrapper-tls",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}
	rendered, err := helmTemplateHarnessV1UpgradeDrainHook(t, harnessV1UpgradeState{
		authSecret: "harness-wrapper-auth",
		authKey:    "token",
		tlsSecret:  "current-wrapper-tls",
	}, args...)
	if err != nil {
		t.Fatalf("TLS Secret rotation failed to render drained rollover: %v\n%s", err, rendered)
	}
	rollover := requireRenderedDocument(t, rendered,
		"kind: Job",
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-drain",
	)
	if !strings.Contains(rollover, `secretName: "current-wrapper-tls"`) ||
		strings.Contains(rollover, `secretName: "next-wrapper-tls"`) {
		t.Fatalf("rollover drain did not retain the live wrapper TLS authority:\n%s", rollover)
	}
	abort := requireRenderedDocument(t, rendered,
		"kind: Job",
		"app.kubernetes.io/component: agent-harness-wrapper-rollover-abort",
	)
	if !strings.Contains(abort, `secretName: "next-wrapper-tls"`) {
		t.Fatalf("rollback abort did not use the target revision TLS authority:\n%s", abort)
	}
}

func TestStaticChartHarnessV1GenerationTracksOnlyPodTemplate(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	args := []string{
		"--set-string", "controller.mode=harness-v1",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--show-only", "templates/harness-wrapper-deployment.yaml",
	}
	first := requireHelmRender(t, append(append([]string{}, args...), "--set", "controller.apiPort=8080")...)
	second := requireHelmRender(t, append(append([]string{}, args...), "--set", "controller.apiPort=9090")...)
	firstGeneration := harnessV1RenderedGeneration(t, first)
	if secondGeneration := harnessV1RenderedGeneration(t, second); secondGeneration != firstGeneration {
		t.Fatalf("unrelated controller value changed wrapper generation: %s != %s", secondGeneration, firstGeneration)
	}
	changedArgs := append(append([]string{}, args...), "--set", "harnessV1.codexSandboxMode=read-only")
	changed := requireHelmRender(t, changedArgs...)
	if changedGeneration := harnessV1RenderedGeneration(t, changed); changedGeneration == firstGeneration {
		t.Fatalf("wrapper Pod-template change preserved generation %s", changedGeneration)
	}
	rotated := requireHelmRender(t, append(append([]string{}, args...),
		"--set-string", "harnessV1.tls.rolloutNonce=certificate-2")...)
	if rotatedGeneration := harnessV1RenderedGeneration(t, rotated); rotatedGeneration == firstGeneration {
		t.Fatalf("TLS rollout nonce preserved wrapper generation %s", rotatedGeneration)
	}
}

func TestStaticChartHarnessV1UsesOnlyExistingSecretReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	args := []string{
		"--set-string", "controller.mode=harness-v1",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}
	rendered := requireHelmRender(t, args...)
	if !strings.Contains(rendered, "secretName: \"harness-wrapper-auth\"") {
		t.Fatalf("wrapper did not mount the configured existing Secret:\n%s", rendered)
	}
	if strings.Contains(rendered, "# Source: orka/templates/harness-wrapper-secret.yaml") {
		t.Fatalf("chart rendered a managed harness wrapper Secret:\n%s", rendered)
	}
}

func TestStaticChartRejectsUnsafeHarnessV1Values(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{
			name: "missing digest",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
			},
			wantError: "harnessV1.image.digest must be a sha256 digest when controller.mode=harness-v1",
		},
		{
			name: "mutable tag-shaped digest",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=latest",
			},
			wantError: "harnessV1.image.digest must be a sha256 digest when controller.mode=harness-v1",
		},
		{
			name: "Substrate workspace provider",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set", "controller.substrate.enabled=true",
			},
			wantError: "controller.substrate.enabled is unsupported when controller.mode=harness-v1",
		},
		{
			name: "inline bearer token",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.auth.token=" + strings.Repeat("x", 32),
			},
			wantError: "harnessV1.auth.token is unsupported",
		},
		{
			name: "short bearer token",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.token=too-short",
			},
			wantError: "harnessV1.auth.token is unsupported",
		},
		{
			name: "missing existing auth Secret",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
			},
			wantError: "harnessV1.auth.existingSecret is required when controller.mode=harness-v1",
		},
		{
			name: "missing existing TLS Secret",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
			},
			wantError: "harnessV1.tls.existingSecret is required when controller.mode=harness-v1",
		},
		{
			name: "shared auth and TLS Secret",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-auth",
			},
			wantError: "harnessV1.tls.existingSecret must differ from harnessV1.auth.existingSecret",
		},
		{
			name: "missing ledger capacity",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.ledger.size=",
			},
			wantError: "harnessV1.ledger.size is required when controller.mode=harness-v1",
		},
		{
			name: "missing ledger retention",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.ledger.retention=",
			},
			wantError: "harnessV1.ledger.retention is required when controller.mode=harness-v1",
		},
		{
			name: "zero ledger retention",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.ledger.retention=0s",
			},
			wantError: "harnessV1.ledger.retention must be a positive Go duration",
		},
		{
			name: "malformed ledger retention",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.ledger.retention=immediate",
			},
			wantError: "harnessV1.ledger.retention must be a positive Go duration",
		},
		{
			name: "negative ledger retention",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.ledger.retention=-1h",
			},
			wantError: "harnessV1.ledger.retention must be a positive Go duration",
		},
		{
			name: "parallel dispatch workers",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set", "harnessV1.dispatch.workers=2",
			},
			wantError: "harnessV1.dispatch.workers must be exactly 1 when controller.mode=harness-v1",
		},
		{
			name: "unsupported Codex sandbox",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.codexSandboxMode=unrestricted",
			},
			wantError: "harnessV1.codexSandboxMode must be read-only, workspace-write, or danger-full-access",
		},
		{
			name: "invalid upgrade drain timeout",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.upgradeDrain.timeout=0s",
			},
			wantError: "harnessV1.upgradeDrain.timeout must be a positive Go duration",
		},
		{
			name: "invalid upgrade drain poll interval",
			args: []string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
				"--set-string", "harnessV1.upgradeDrain.pollInterval=immediate",
			},
			wantError: "harnessV1.upgradeDrain.pollInterval must be a positive Go duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, tt.args...)
			if err == nil {
				t.Fatalf("helm template unexpectedly accepted unsafe harness v1 values")
			}
			if !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm template error does not contain %q:\n%s", tt.wantError, output)
			}
		})
	}
}

func TestStaticChartHarnessV1EnabledRenderIsIsolatedAndDurable(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	args := []string{
		"--set-string", "controller.mode=harness-v1",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		"--set-string", "harnessV1.tls.rolloutNonce=certificate-1",
		"--set-string", "harnessV1.ledger.retention=168h",
		"--set-string", "service.port=18080",
		"--set-string", "controller.apiPort=18081",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}
	controllerDeployment := requireHelmRender(t, append(args, "--show-only", "templates/deployment.yaml")...)
	for _, marker := range []string{
		"--harness-v1-dispatch-workers=1",
		"--harness-v1-endpoint=https://test-orka-agent-harness-wrapper.orka-test.svc:8080",
		"--harness-v1-ca-file=/var/run/orka/harness-v1-tls/ca.crt",
		"mountPath: /var/run/orka/harness-v1-tls",
		`secretName: "harness-wrapper-tls"`,
		"key: ca.crt",
		`orka.ai/harness-v1-tls-rollout-nonce: "certificate-1"`,
	} {
		if !strings.Contains(controllerDeployment, marker) {
			t.Fatalf("controller Deployment is missing harness v1 TLS marker %q:\n%s", marker, controllerDeployment)
		}
	}

	deployment := requireHelmRender(t, append(args, "--show-only", "templates/harness-wrapper-deployment.yaml")...)
	for _, marker := range []string{
		"replicas: 1",
		"strategy:\n    type: Recreate",
		`image: "ghcr.io/orka-agents/orka/agent-harness-wrapper@` + digest + `"`,
		"serviceAccountName: test-orka-agent-harness-wrapper",
		"automountServiceAccountToken: false",
		"name: https",
		"name: ORKA_CONTROLLER_URL",
		"value: http://test-orka.orka-test.svc:18080",
		"name: ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE",
		"value: /var/run/orka/harness-wrapper-auth/token",
		"name: ORKA_HARNESS_WRAPPER_TLS_CERT_FILE",
		"value: /var/run/orka/harness-wrapper-tls/tls.crt",
		"name: ORKA_HARNESS_WRAPPER_TLS_KEY_FILE",
		"value: /var/run/orka/harness-wrapper-tls/tls.key",
		"scheme: HTTPS",
		"name: ORKA_HARNESS_WRAPPER_ADMISSION_LEDGER_PATH",
		"value: /var/lib/orka/harness-v1/admission-ledger.db",
		"name: ORKA_HARNESS_WRAPPER_LEDGER_GENERATION",
		"name: ORKA_HARNESS_WRAPPER_LEDGER_RETENTION",
		`value: "168h"`,
		"mountPath: /var/lib/orka/harness-v1",
		"claimName: test-orka-harness-v1-ledger",
		`secretName: "harness-wrapper-auth"`,
		`secretName: "harness-wrapper-tls"`,
		"name: controller-api-token",
		"mountPath: /var/run/secrets/kubernetes.io/serviceaccount",
		"projected:",
		"defaultMode: 0400",
		"serviceAccountToken:",
		"path: token",
		"expirationSeconds: 3600",
		`orka.ai/harness-v1-tls-rollout-nonce: "certificate-1"`,
		"key: tls.crt",
		"key: tls.key",
		"key: ca.crt",
	} {
		if !strings.Contains(deployment, marker) {
			t.Fatalf("harness v1 Deployment is missing %q:\n%s", marker, deployment)
		}
	}
	for _, forbidden := range []string{
		"ORKA_SA_TOKEN_PATH",
		"upload-token",
		"GIT_TOKEN",
		"GITHUB_TOKEN",
		"ORKA_WORKSPACE_PUBLISHER",
		"provider-auth",
	} {
		if strings.Contains(deployment, forbidden) {
			t.Fatalf("harness v1 Deployment contains forbidden ambient credential surface %q:\n%s", forbidden, deployment)
		}
	}

	for template, markers := range map[string][]string{
		"templates/harness-wrapper-service.yaml": {
			"kind: Service",
			"name: test-orka-agent-harness-wrapper",
			"name: https",
			"port: 8080",
			"targetPort: https",
		},
		"templates/harness-wrapper-serviceaccount.yaml": {
			"kind: ServiceAccount",
			"automountServiceAccountToken: false",
		},
		"templates/harness-wrapper-pvc.yaml": {
			"kind: PersistentVolumeClaim",
			"name: test-orka-harness-v1-ledger",
			"helm.sh/resource-policy: keep",
			"storage: 1Gi",
		},
		"templates/harness-wrapper-networkpolicy.yaml": {
			"kind: NetworkPolicy",
			"policyTypes: [Ingress, Egress]",
			"egress:\n" +
				"    - to:\n" +
				"        - podSelector:\n" +
				"            matchLabels:\n" +
				"              app.kubernetes.io/name: orka\n" +
				"              app.kubernetes.io/instance: test\n" +
				"              app.kubernetes.io/component: controller\n" +
				"      ports:\n" +
				"        - protocol: TCP\n" +
				"          port: 18081",
			"kubernetes.io/metadata.name: kube-system",
			"cidr: 0.0.0.0/0",
			"cidr: ::/0",
			"port: 443",
		},
	} {
		rendered := requireHelmRender(t, append(args, "--show-only", template)...)
		for _, marker := range markers {
			if !strings.Contains(rendered, marker) {
				t.Fatalf("%s is missing %q:\n%s", template, marker, rendered)
			}
		}
	}
	harnessV1RenderedGeneration(t, deployment)
}

func TestStaticChartRejectsIncompleteProviderProxyConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{
			name: "different namespace",
			args: []string{
				"--set", "providerProxy.enabled=true",
				"--set-string", "controller.acpRuntime.providerProxyNamespace=other-system",
			},
			wantError: "controller.acpRuntime.providerProxyNamespace must be empty or match the Helm release namespace",
		},
		{
			name: "missing upstream",
			args: []string{
				"--set-string", "providerProxy.upstreamBaseURL=",
			},
			wantError: "providerProxy.upstreamBaseURL must be an HTTP(S) URL",
		},
		{
			name: "no egress and no in-cluster Service to derive it from",
			args: []string{
				"--set-json", "providerProxy.egress=[]",
				"--set-string", "providerProxy.upstreamBaseURL=https://gateway.example.test:8443",
			},
			wantError: "providerProxy.egress is required unless providerProxy.upstreamBaseURL names an in-cluster Service",
		},
		{
			name: "no egress and the named Service is missing",
			args: []string{
				"--set-json", "providerProxy.egress=[]",
			},
			wantError: "names Service models/model-gateway, which was not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, tt.args...)
			if err == nil {
				t.Fatalf("helm template unexpectedly accepted unsupported provider proxy override:\n%s", output)
			}
			if !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm template error does not contain %q:\n%s", tt.wantError, output)
			}
		})
	}
}

func TestStaticChartProviderProxyURLValidation(t *testing.T) {
	for _, test := range []struct {
		url   string
		valid bool
	}{
		{url: "http://agentgateway.gateway-system.svc:3000", valid: true},
		{url: "https://gateway.example.test:8443/models/", valid: true},
		{url: "http://gateway.example.test:1", valid: true},
		{url: "https://gateway.example.test:65535", valid: true},
		{url: "http://gateway.example.test:080", valid: true},
		{url: "http://[2001:db8::1]", valid: true},
		{url: "http://[2001:db8::1]:8080", valid: true},
		{url: "http://gateway.example.test/v1/.models/model..name", valid: true},
		{url: "http://gateway.example.test/models%20test/%3F%23route", valid: true},
		{url: "http://gateway.example.test:0"},
		{url: "http://gateway.example.test:0000"},
		{url: "http://gateway.example.test:65536"},
		{url: "http://gateway.example.test:99999"},
		{url: "http://gateway.example.test:99999999999999999999999999999999"},
		{url: "http://[2001:db8::1]:0"},
		{url: "http://[2001:db8::1]:65536"},
		{url: "ftp://gateway.example.test"},
		{url: "http://user@gateway.example.test"},
		{url: "http://gateway.example.test?route=model"},
		{url: "http://gateway.example.test#model"},
		{url: "http://gateway.example.test/v1/../admin"},
		{url: "http://gateway.example.test/v1/./models"},
		{url: "http://gateway.example.test/v1/%2e%2E/admin"},
		{url: "http://gateway.example.test/v1%2f..%2fadmin"},
		{url: "http://gateway.example.test/v1%252f%252e%252e%252fadmin"},
		{url: `http://gateway.example.test/v1\admin`},
		{url: "http://gateway.example.test/v1%5cadmin"},
		{url: "http://gateway.example.test/v1%255Cadmin"},
		{url: "http://gateway.example.test/v1%00admin"},
		{url: "http://gateway.example.test/v1%2500admin"},
		{url: "http://gateway.example.test/%3F%23route/%252e%252e/admin"},
		{url: "http://gateway.example.test/%invalid"},
		{url: "http://:8080"},
		{url: "http://:8080/v1"},
		{url: "http://gateway.example.test/%25invalid"},
	} {
		t.Run(test.url, func(t *testing.T) {
			// A values file preserves backslashes that Helm's --set parser would consume.
			values, err := yaml.Marshal(map[string]any{
				"providerProxy": map[string]string{"upstreamBaseURL": test.url},
			})
			if err != nil {
				t.Fatal(err)
			}
			valuesPath := filepath.Join(t.TempDir(), "gateway.yaml")
			if err := os.WriteFile(valuesPath, values, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = helmTemplateStaticChart(t, "--values", valuesPath)
			if (err == nil) != test.valid {
				t.Fatalf("gateway URL accepted = %v, want %v", err == nil, test.valid)
			}
		})
	}
}

func TestStaticChartUsesRegisteredContextTokenTTSEndpointFlag(t *testing.T) {
	rendered := requireHelmRender(t,
		"--set-string", "controller.contextToken.tts.endpoint=https://tts.example.test/oauth/token",
		"--show-only", "templates/deployment.yaml",
	)

	if !strings.Contains(rendered, "--context-token-tts-endpoint=https://tts.example.test/oauth/token") {
		t.Fatalf("controller deployment is missing the registered TTS endpoint flag:\n%s", rendered)
	}
	if strings.Contains(rendered, "--context-token-tts-url=") {
		t.Fatalf("controller deployment rendered the unregistered TTS URL flag:\n%s", rendered)
	}
}

func TestStaticChartRendersByteLimitsAsDecimalIntegers(t *testing.T) {
	publisher := requireHelmRender(t, "--show-only", "templates/publisher-deployment.yaml")
	scmProxy := requireHelmRender(t, "--show-only", "templates/scm-egress-proxy-deployment.yaml")

	if !strings.Contains(publisher, `value: "4194304"`) {
		t.Fatalf("publisher deployment did not render max response bytes as a decimal integer:\n%s", publisher)
	}
	for _, want := range []string{
		`--max-request-header-bytes=32768`,
		`--max-response-header-bytes=65536`,
		`--max-request-bytes=4194304`,
		`--max-response-bytes=8388608`,
		`--max-tunnel-bytes=1073741824`,
	} {
		if !strings.Contains(scmProxy, want) {
			t.Fatalf("SCM proxy deployment is missing decimal byte limit %q:\n%s", want, scmProxy)
		}
	}
	if strings.Contains(publisher, "e+") || strings.Contains(scmProxy, "e+") {
		t.Fatalf("byte limit render used scientific notation:\npublisher:\n%s\nSCM proxy:\n%s", publisher, scmProxy)
	}
}

func TestStaticChartAuthRolloutNoncesTargetOnlyCredentialConsumers(t *testing.T) {
	args := []string{
		"--set-string", "publisher.auth.rolloutNonce=publisher-v2",
		"--set-string", "scmEgressProxy.auth.rolloutNonce=scm-v3",
		"--set-string", "publisher.auth.controllerToken=publisher-secret-material",
		"--set-string", "scmEgressProxy.auth.token=scm-secret-material-0123456789abcd",
	}

	controller := requireHelmRender(t, append(args, "--show-only", "templates/deployment.yaml")...)
	publisher := requireHelmRender(t, append(args, "--show-only", "templates/publisher-deployment.yaml")...)
	scmProxy := requireHelmRender(t, append(args, "--show-only", "templates/scm-egress-proxy-deployment.yaml")...)
	providerProxy := requireHelmRender(t,
		"--set", "providerProxy.enabled=true",
		"--set-string", "publisher.auth.rolloutNonce=publisher-v2",
		"--set-string", "scmEgressProxy.auth.rolloutNonce=scm-v3",
		"--show-only", "templates/provider-proxy-deployment.yaml",
	)

	publisherNonce := `orka.ai/publisher-auth-rollout-nonce: "publisher-v2"`
	scmNonce := `orka.ai/scm-egress-proxy-auth-rollout-nonce: "scm-v3"`
	if !strings.Contains(controller, publisherNonce) || strings.Contains(controller, scmNonce) {
		t.Fatalf("controller rollout annotations are incorrect:\n%s", controller)
	}
	if !strings.Contains(publisher, publisherNonce) || !strings.Contains(publisher, scmNonce) {
		t.Fatalf("publisher rollout annotations are incorrect:\n%s", publisher)
	}
	if strings.Contains(scmProxy, publisherNonce) || !strings.Contains(scmProxy, scmNonce) {
		t.Fatalf("SCM proxy rollout annotations are incorrect:\n%s", scmProxy)
	}
	if strings.Contains(providerProxy, publisherNonce) || strings.Contains(providerProxy, scmNonce) {
		t.Fatalf("provider proxy received unrelated auth rollout annotations:\n%s", providerProxy)
	}
	for name, rendered := range map[string]string{
		"controller": controller,
		"publisher":  publisher,
		"SCM proxy":  scmProxy,
	} {
		if strings.Contains(rendered, "secret-material") {
			t.Fatalf("%s Pod template annotation render exposed Secret material", name)
		}
	}
}
