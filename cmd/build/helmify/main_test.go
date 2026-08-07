package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for static chart render tests")
	}

	commandArgs := []string{"template", "test", "static", "--namespace", "orka-test"}
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
			`(list (dict "key" "` + state.authKey + `" "path" "token")))))))) }}`,
		}, " ")
	}
	forced := strings.Replace(string(hook), lookup, forcedLookup, 1)
	if forced == string(hook) {
		t.Fatalf("wrapper drain hook is not gated by the exact existing Deployment lookup")
	}
	if state.controllerState != "" {
		var controllerArgs []string
		switch state.controllerState {
		case "enabled", "disabled":
			enabled := state.controllerState == "enabled"
			controllerArgs = []string{fmt.Sprintf(`"--harness-v1-enabled=%t"`, enabled)}
			if enabled {
				controllerArgs = append(controllerArgs,
					`"--harness-v1-auth-secret-name=`+state.authSecret+`"`,
					`"--harness-v1-auth-secret-key=`+state.authKey+`"`,
				)
			}
		case "legacy-v2-disabled":
			controllerArgs = []string{`"--acp-runtime-enabled=true"`}
		case "damaged-dual":
			controllerArgs = []string{
				`"--acp-runtime-enabled=true"`,
				`"--agent-execution-snapshot-key-file=/var/run/orka/agent-execution-snapshot/key"`,
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

	commandArgs := []string{"template", "test", chartDir, "--namespace", "orka-test", "--is-upgrade"}
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

func TestStaticChartProviderProxyConfigurationIsFixedToSupportedBoundary(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0", 64)
	rendered := requireHelmRender(t,
		"--set", "providerProxy.enabled=true",
		"--set", "controller.acpRuntime.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "controller.image.digest="+digest,
		"--set-string", "publisher.image.digest="+digest,
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--set-string", "controller.acpRuntime.providerProxyNamespace=orka-test",
		"--set-string", "providerProxy.upstreamBaseURL=http://vekil.vekil-system.svc:1337/",
	)

	for _, marker := range []string{
		"--acp-provider-proxy-base-url=http://test-orka-provider-auth-proxy.orka-test.svc:8080",
		"--acp-provider-proxy-namespace=orka-test",
		"--upstream-base-url=http://vekil.vekil-system.svc:1337",
	} {
		if !strings.Contains(rendered, marker) {
			t.Fatalf("rendered provider proxy configuration is missing %q", marker)
		}
	}
	if strings.Contains(rendered, "--upstream-base-url=http://vekil.vekil-system.svc:1337/") {
		t.Fatalf("provider upstream trailing slash was not normalized")
	}

	providerPolicy := requireHelmRender(t,
		"--set", "providerProxy.enabled=true",
		"--show-only", "templates/provider-proxy-networkpolicy.yaml",
	)
	for _, marker := range []string{
		"kubernetes.io/metadata.name: vekil-system",
		"app.kubernetes.io/name: vekil",
		"ports: [{protocol: TCP, port: 1337}]",
	} {
		if !strings.Contains(providerPolicy, marker) {
			t.Fatalf("provider proxy NetworkPolicy lost fixed Vekil boundary %q:\n%s", marker, providerPolicy)
		}
	}

	vekilPolicy := requireHelmRender(t,
		"--set", "providerProxy.enabled=true",
		"--show-only", "templates/vekil-ingress-networkpolicy.yaml",
	)
	for _, marker := range []string{
		"namespace: vekil-system",
		"kubernetes.io/metadata.name: orka-test",
		"ports: [{protocol: TCP, port: 1337}]",
	} {
		if !strings.Contains(vekilPolicy, marker) {
			t.Fatalf("Vekil ingress NetworkPolicy lost fixed boundary %q:\n%s", marker, vekilPolicy)
		}
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
			wantError: "controller.leaderElect must be true when using the SQLite store backend",
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

func TestStaticChartRequiresAgentExecutionSnapshotSecretForACPAdmission(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{
			name: "missing Secret name",
			args: []string{
				"--set", "controller.acpRuntime.enabled=true",
			},
			wantError: "controller.agentExecutionSnapshot.existingSecret is required when agent execution is enabled",
		},
		{
			name: "missing Secret key",
			args: []string{
				"--set", "controller.acpRuntime.enabled=true",
				"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
			},
			wantError: "controller.agentExecutionSnapshot.key is required when agent execution is enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, tt.args...)
			if err == nil {
				t.Fatalf("helm template unexpectedly accepted incomplete snapshot key configuration:\n%s", output)
			}
			if !strings.Contains(output, tt.wantError) {
				t.Fatalf("helm template error does not contain %q:\n%s", tt.wantError, output)
			}
		})
	}
}

func TestStaticChartMountsAgentExecutionSnapshotKeyForACPAdmission(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0", 64)
	rendered := requireHelmRender(t,
		"--set", "controller.acpRuntime.enabled=true",
		"--set", "providerProxy.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "controller.image.digest="+digest,
		"--set-string", "publisher.image.digest="+digest,
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--show-only", "templates/deployment.yaml",
	)

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

func TestStaticChartAdmissionRequiresReplicasTLSAndCA(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	tests := []struct {
		name, want string
		args       []string
	}{
		{
			"replicas", "admission.replicas must be at least 2",
			[]string{"--set", "admission.enabled=true", "--set", "admission.replicas=1"},
		},
		{
			"TLS", "admission.tls.existingSecret is required",
			[]string{"--set", "admission.enabled=true", "--set-string", "controller.image.digest=" + digest},
		},
		{
			"runtime", "admission.enabled must be true",
			[]string{
				"--set", "admission.webhooks.enabled=true",
				"--set-string", "admission.webhooks.caBundle=Y2E=",
			},
		},
		{
			"CA", "requires a nonempty caBundle or caInjectionAnnotations",
			[]string{
				"--set", "admission.enabled=true",
				"--set", "admission.webhooks.enabled=true",
				"--set-string", "controller.image.digest=" + digest,
				"--set-string", "admission.tls.existingSecret=orka-admission-tls",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, tt.args...)
			if err == nil || !strings.Contains(output, tt.want) {
				t.Fatalf("helm render error = %v, want %q:\n%s", err, tt.want, output)
			}
		})
	}
}

func TestStaticChartAdmissionRuntimeAndWebhooksAreIndependent(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	base := []string{
		"--set", "admission.enabled=true",
		"--set-string", "controller.image.digest=" + digest,
		"--set-string", "admission.tls.existingSecret=orka-admission-tls",
	}
	runtime := requireHelmRender(t, base...)
	for _, marker := range []string{
		"replicas: 2", "type: RollingUpdate", "command: [/orka-admission]",
		"port: 443", "targetPort: webhook", "kind: PodDisruptionBudget",
		"kind: NetworkPolicy", "kind: ClusterRole", "podAntiAffinity:",
		"preferredDuringSchedulingIgnoredDuringExecution:", "topologyKey: kubernetes.io/hostname",
	} {
		if !strings.Contains(runtime, marker) {
			t.Fatalf("admission runtime is missing %q:\n%s", marker, runtime)
		}
	}
	if strings.Contains(runtime, "kind: ValidatingWebhookConfiguration") {
		t.Fatalf("admission first wave unexpectedly installed protected webhooks:\n%s", runtime)
	}
	if strings.Contains(runtime, "agent-execution-binding") {
		t.Fatalf("admission first wave unexpectedly installed the parameterized binding policy:\n%s", runtime)
	}
	deployment := requireHelmRender(t,
		append(base, "--show-only", "templates/admission-deployment.yaml")...,
	)
	rbac := requireHelmRender(t, append(base, "--show-only", "templates/admission-rbac.yaml")...)
	for _, forbidden := range []string{
		"--leader-elect", "/data/orka.db", "agent-execution-snapshot",
		"provider-auth", "workspace-publisher", "harness-wrapper",
	} {
		if strings.Contains(deployment, forbidden) {
			t.Fatalf("admission Deployment contains forbidden surface %q:\n%s", forbidden, deployment)
		}
	}
	if !strings.Contains(deployment, "--admin-groups=system:masters") {
		t.Fatalf("admission Deployment is missing the configured administrator group:\n%s", deployment)
	}
	for _, forbidden := range []string{"coordination.k8s.io", "leases", "update", "patch", "delete", "watch"} {
		if strings.Contains(rbac, forbidden) {
			t.Fatalf("admission RBAC contains forbidden authority %q:\n%s", forbidden, rbac)
		}
	}
	for _, required := range []string{
		"apiGroups: [authorization.k8s.io]",
		"resources: [subjectaccessreviews]",
		"verbs: [create]",
	} {
		if !strings.Contains(rbac, required) {
			t.Fatalf("admission RBAC is missing workspace authorization marker %q:\n%s", required, rbac)
		}
	}

	webhookArgs := append(append([]string{}, base...),
		"--set", "admission.webhooks.enabled=true",
		"--set-string", "admission.webhooks.caBundle=Y2E=",
		"--show-only", "templates/admission-validating-webhook.yaml",
	)
	webhooks := requireHelmRender(t, webhookArgs...)
	if got := strings.Count(webhooks, "failurePolicy: Fail"); got != 10 {
		t.Fatalf("fail-closed webhook count = %d, want 10:\n%s", got, webhooks)
	}
	for _, marker := range []string{
		"/validate-core-orka-ai-v1alpha1-agent-contract",
		"/validate-core-orka-ai-v1alpha1-agentruntime-contract",
		"/validate-core-orka-ai-v1alpha1-task-execution-authority",
		"resources: [tasks, tasks/status]",
		"/validate-core-orka-ai-v1alpha1-agentexecutionadjudication",
		"operations: [CREATE, UPDATE, DELETE]",
		"resources: [agentexecutionadjudications]",
		"resources: [agentexecutionadjudications/status]",
		"route-unless-adjudication-controller",
		"/validate-core-orka-ai-v1alpha1-agentexecution-control-policy",
		"resources: [agentexecutioncontrols, agentexecutionpolicies]",
		"route-unless-controller-cleanup-safe",
		"/validate-core-orka-ai-v1alpha1-session-resolution",
		"resources: [runtimesessioncontrols/status]",
	} {
		if !strings.Contains(webhooks, marker) {
			t.Fatalf("admission webhooks are missing %q:\n%s", marker, webhooks)
		}
	}

	bindingPolicyArgs := append(append([]string{}, base...),
		"--set", "admission.webhooks.enabled=true",
		"--set-string", "admission.webhooks.caBundle=Y2E=",
		"--show-only", "templates/agent-execution-binding-admission-policy.yaml",
	)
	bindingPolicy := requireHelmRender(t, bindingPolicyArgs...)
	for _, marker := range []string{
		"kind: ValidatingAdmissionPolicy",
		"kind: ValidatingAdmissionPolicyBinding",
		"failurePolicy: Fail",
		"resources: [tasks, tasks/status]",
		"params.status.observedGeneration == params.metadata.generation",
		"backendControl.name == params.metadata.name",
		"backendControl.generation == params.metadata.generation",
		"backendControl.admittedMode == 'enabled'",
		"effectiveMode == 'enabled'",
		"admissionClosedAt.hasValue()",
		"backendControl.modeRevision == params.status.backends.v1.modeRevision",
		"backendControl.modeRevision == params.status.backends.v2.modeRevision",
		"provenance == 'legacy-cleanup-only'",
		"classification.state == 'Open'",
		"classification.controlUID) == string(params.metadata.uid)",
		"classification.controlGeneration == params.metadata.generation",
		"classification.inventoryID == object.status.agentExecutionBinding.migrationInventoryID",
		"classification.inventoryDigest.matches('^sha256:[a-f0-9]{64}$')",
		"parameterNotFoundAction: Deny",
		"name: cluster",
		"namespace: orka-system",
	} {
		if !strings.Contains(bindingPolicy, marker) {
			t.Fatalf("binding admission policy is missing %q:\n%s", marker, bindingPolicy)
		}
	}
	if strings.Contains(bindingPolicy, "classification.state == 'Sealed'") {
		t.Fatalf("binding admission policy permits post-seal legacy adoption:\n%s", bindingPolicy)
	}
}

func TestStaticChartHarnessV1IsDisabledByDefault(t *testing.T) {
	rendered := requireHelmRender(t)
	if strings.Contains(rendered, "agent-harness-wrapper") {
		t.Fatalf("default chart render unexpectedly enabled the harness v1 data plane")
	}
	control := requireHelmRender(t, "--show-only", "templates/agent-execution-control.yaml")
	for _, marker := range []string{
		"kind: AgentExecutionControl",
		"name: cluster",
		"namespace: orka-system",
		"desiredMode: \"disabled\"",
		"desiredMode: \"enabled\"",
	} {
		if !strings.Contains(control, marker) {
			t.Fatalf("default execution control is missing %q:\n%s", marker, control)
		}
	}
}

func TestStaticChartManagesFixedAgentExecutionOwnershipNamespace(t *testing.T) {
	namespace := requireHelmRender(t,
		"--show-only", "templates/agent-execution-ownership-namespace.yaml",
	)
	for _, marker := range []string{
		"kind: Namespace",
		"name: orka-system",
		"helm.sh/resource-policy: keep",
		"app.kubernetes.io/component: agent-execution-ownership",
	} {
		if !strings.Contains(namespace, marker) {
			t.Fatalf("ownership namespace is missing %q:\n%s", marker, namespace)
		}
	}

	rendered := requireHelmRender(t,
		"--set", "controller.agentExecutionControl.createNamespace=false",
	)
	if strings.Contains(rendered, "app.kubernetes.io/component: agent-execution-ownership") {
		t.Fatalf("chart rendered the ownership Namespace despite an operator-declared prerequisite:\n%s", rendered)
	}
	if !strings.Contains(rendered, "kind: AgentExecutionControl") ||
		!strings.Contains(rendered, "namespace: orka-system") {
		t.Fatalf("operator-managed Namespace mode lost the fixed control resource:\n%s", rendered)
	}
}

func TestStaticChartManagedHarnessV1PolicyCanBeCleanupOnly(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	policy := requireHelmRender(t,
		"--set", "harnessV1.enabled=true",
		"--set", "controller.agentExecutionControl.v1Mode=drain-only",
		"--set", "controller.agentExecutionControl.v2Mode=disabled",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest="+digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
		"--show-only", "templates/agent-execution-policy.yaml",
	)
	if !strings.Contains(policy, "allowNewV1Bindings: false") {
		t.Fatalf("managed harness v1 policy is not cleanup-only by default:\n%s", policy)
	}
	if strings.Contains(policy, "allowNewV1Bindings: true") {
		t.Fatalf("cleanup-only managed policy unexpectedly admits new v1 bindings:\n%s", policy)
	}
}

func TestStaticChartHarnessV1UpgradeDrainHookIsExistingDeploymentGated(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	args := []string{
		"--set", "harnessV1.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
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

	legacyV2, err := helmTemplateHarnessV1UpgradeDrainHook(t, harnessV1UpgradeState{
		wrapperMissing:  true,
		controllerState: "legacy-v2-disabled",
	}, args...)
	if err != nil {
		t.Fatalf("active legacy v2 route could not upgrade to dual mode: %v\n%s", err, legacyV2)
	}
	if strings.Contains(legacyV2, "agent-harness-wrapper-rollover-drain") {
		t.Fatalf("legacy v2-only route unexpectedly triggered a v1 rollover drain:\n%s", legacyV2)
	}

	damagedDual, err := helmTemplateHarnessV1UpgradeDrainHook(t, harnessV1UpgradeState{
		wrapperMissing:  true,
		controllerState: "damaged-dual",
	}, args...)
	if err == nil {
		t.Fatalf("dual-era controller missing its v1 state rendered successfully:\n%s", damagedDual)
	}
	if !strings.Contains(damagedDual, "cannot determine the previously deployed harness v1 state during upgrade") {
		t.Fatalf("damaged dual state did not fail closed:\n%s", damagedDual)
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
		`- "--endpoint=http://test-orka-agent-harness-wrapper:8080"`,
		"- --bearer-token-file=/var/run/orka/harness-wrapper/token",
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
		`- "--endpoint=http://test-orka-agent-harness-wrapper:8080"`,
		"- --bearer-token-file=/var/run/orka/harness-wrapper/token",
		`secretName: "harness-wrapper-auth"`,
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
		"--set", "harnessV1.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
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

	firstEnableArgs := append(append([]string{}, baseArgs...),
		"--set-string", "harnessV1.auth.existingSecret=first-enable-auth")
	firstEnable, err := helmTemplateHarnessV1UpgradeDrainHook(t, harnessV1UpgradeState{
		wrapperMissing:  true,
		controllerState: "disabled",
	}, firstEnableArgs...)
	if err != nil {
		t.Fatalf("recorded disabled route blocked first enablement: %v\n%s", err, firstEnable)
	}
}

func TestStaticChartHarnessV1GenerationTracksOnlyPodTemplate(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	args := []string{
		"--set", "harnessV1.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
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
}

func TestStaticChartHarnessV1UsesOnlyExistingSecretReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	args := []string{
		"--set", "harnessV1.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
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

func TestStaticChartRendersExplicitAgentExecutionBackendModes(t *testing.T) {
	digest := "sha256:" + strings.Repeat("2", 64)
	v1Args := []string{
		"--set", "harnessV1.enabled=true",
		"--set", "harnessV1.policy.allowNewV1Bindings=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}
	v2Args := []string{
		"--set", "controller.acpRuntime.enabled=true",
		"--set", "providerProxy.enabled=true",
		"--set", "store.persistence.enabled=true",
		"--set-string", "controller.image.digest=" + digest,
		"--set-string", "publisher.image.digest=" + digest,
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}

	tests := []struct {
		name             string
		args             []string
		v1Mode           string
		v2Mode           string
		wantHarnessV1    bool
		wantManagedACPV2 bool
	}{
		{
			name: "v1 only",
			args: append(append([]string{}, v1Args...),
				"--set", "controller.agentExecutionControl.v1Mode=enabled",
				"--set", "controller.agentExecutionControl.v2Mode=disabled"),
			v1Mode:           "enabled",
			v2Mode:           "disabled",
			wantHarnessV1:    true,
			wantManagedACPV2: false,
		},
		{
			name: "v2 only",
			args: append(append([]string{}, v2Args...),
				"--set", "controller.agentExecutionControl.v1Mode=disabled",
				"--set", "controller.agentExecutionControl.v2Mode=enabled"),
			v1Mode:           "disabled",
			v2Mode:           "enabled",
			wantHarnessV1:    false,
			wantManagedACPV2: true,
		},
		{
			name: "dual",
			args: append(append(append([]string{}, v1Args...), v2Args...),
				"--set", "controller.agentExecutionControl.v1Mode=enabled",
				"--set", "controller.agentExecutionControl.v2Mode=enabled"),
			v1Mode:           "enabled",
			v2Mode:           "enabled",
			wantHarnessV1:    true,
			wantManagedACPV2: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := requireHelmRender(t, tt.args...)
			for marker, want := range map[string]bool{
				"--harness-v1-enabled=true":                          tt.wantHarnessV1,
				"--acp-runtime-enabled=true":                         tt.wantManagedACPV2,
				"app.kubernetes.io/component: agent-harness-wrapper": tt.wantHarnessV1,
				"app.kubernetes.io/component: acp-runtime":           tt.wantManagedACPV2,
			} {
				if got := strings.Contains(rendered, marker); got != want {
					t.Fatalf("render marker %q present = %t, want %t", marker, got, want)
				}
			}

			controlArgs := append(append([]string{}, tt.args...),
				"--show-only", "templates/agent-execution-control.yaml")
			control := requireHelmRender(t, controlArgs...)
			for _, marker := range []string{
				"v1:\n      desiredMode: \"" + tt.v1Mode + "\"",
				"v2:\n      desiredMode: \"" + tt.v2Mode + "\"",
			} {
				if !strings.Contains(control, marker) {
					t.Fatalf("%s control is missing %q:\n%s", tt.name, marker, control)
				}
			}
		})
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
				"--set", "harnessV1.enabled=true",
			},
			wantError: "harnessV1.image.digest must be a sha256 digest when harnessV1.enabled=true",
		},
		{
			name: "mutable tag-shaped digest",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=latest",
			},
			wantError: "harnessV1.image.digest must be a sha256 digest when harnessV1.enabled=true",
		},
		{
			name: "inline bearer token",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.auth.token=" + strings.Repeat("x", 32),
			},
			wantError: "harnessV1.auth.token is unsupported",
		},
		{
			name: "short bearer token",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.token=too-short",
			},
			wantError: "harnessV1.auth.token is unsupported",
		},
		{
			name: "missing existing auth Secret",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
			},
			wantError: "harnessV1.auth.existingSecret is required when harnessV1.enabled=true",
		},
		{
			name: "missing ledger capacity",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.ledger.size=",
			},
			wantError: "harnessV1.ledger.size is required when harnessV1.enabled=true",
		},
		{
			name: "unsupported Codex sandbox",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.codexSandboxMode=unrestricted",
			},
			wantError: "harnessV1.codexSandboxMode must be read-only, workspace-write, or danger-full-access",
		},
		{
			name: "unsupported built-in Copilot runtime",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set", "harnessV1.policy.allowedBuiltInRuntimeTypes={copilot}",
			},
			wantError: "harnessV1.policy.allowedBuiltInRuntimeTypes may contain only codex or claude",
		},
		{
			name: "invalid upgrade drain timeout",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.upgradeDrain.timeout=0s",
			},
			wantError: "harnessV1.upgradeDrain.timeout must be a positive Go duration",
		},
		{
			name: "invalid upgrade drain poll interval",
			args: []string{
				"--set", "harnessV1.enabled=true",
				"--set-string", "harnessV1.image.digest=" + digest,
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
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
		"--set", "harnessV1.enabled=true",
		"--set", "harnessV1.policy.allowNewV1Bindings=true",
		"--set-string", "harnessV1.image.digest=" + digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "controller.agentExecutionSnapshot.existingSecret=snapshot-key",
		"--set-string", "controller.agentExecutionSnapshot.key=encryption-key",
	}

	policy := requireHelmRender(t, append(args, "--show-only", "templates/agent-execution-policy.yaml")...)
	for _, marker := range []string{
		"kind: AgentExecutionPolicy",
		"name: compatibility",
		"namespace: orka-test",
		"allowNewV1Bindings: true",
		"- codex",
		"- claude",
		"allowedBrokeredToolClasses: []",
		"- transaction-tokens",
		"- observed-write-tools",
		"networkIsolationProfile: \"default-deny\"",
	} {
		if !strings.Contains(policy, marker) {
			t.Fatalf("harness v1 policy is missing %q:\n%s", marker, policy)
		}
	}
	if strings.Contains(policy, "- copilot") {
		t.Fatalf("harness v1 policy unexpectedly advertises unsupported Copilot:\n%s", policy)
	}

	deployment := requireHelmRender(t, append(args, "--show-only", "templates/harness-wrapper-deployment.yaml")...)
	for _, marker := range []string{
		"replicas: 1",
		"strategy:\n    type: Recreate",
		`image: "ghcr.io/orka-agents/orka/agent-harness-wrapper@` + digest + `"`,
		"serviceAccountName: test-orka-agent-harness-wrapper",
		"automountServiceAccountToken: false",
		"name: ORKA_HARNESS_WRAPPER_ADMISSION_LEDGER_PATH",
		"value: /var/lib/orka/harness-v1/admission-ledger.db",
		"name: ORKA_HARNESS_WRAPPER_LEDGER_GENERATION",
		"mountPath: /var/lib/orka/harness-v1",
		"claimName: test-orka-harness-v1-ledger",
		`secretName: "harness-wrapper-auth"`,
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
			"port: 8080",
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
			"app.kubernetes.io/component: controller",
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

func TestStaticChartRejectsUnsupportedProviderProxyOverrides(t *testing.T) {
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
			name: "different upstream host",
			args: []string{
				"--set", "providerProxy.enabled=true",
				"--set-string", "providerProxy.upstreamBaseURL=http://other.vekil-system.svc:1337",
			},
			wantError: "providerProxy.upstreamBaseURL must be http://vekil.vekil-system.svc:1337",
		},
		{
			name: "different upstream port",
			args: []string{
				"--set", "providerProxy.enabled=true",
				"--set-string", "providerProxy.upstreamBaseURL=http://vekil.vekil-system.svc:8080",
			},
			wantError: "providerProxy.upstreamBaseURL must be http://vekil.vekil-system.svc:1337",
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

func TestStaticChartUsesRegisteredContextTokenTTSEndpointFlag(t *testing.T) {
	rendered := requireHelmRender(t,
		"--set-string", "controller.contextToken.tts.url=https://tts.example.test/oauth/token",
		"--show-only", "templates/deployment.yaml",
	)

	if !strings.Contains(rendered, "--context-token-tts-endpoint=https://tts.example.test/oauth/token") {
		t.Fatalf("controller deployment is missing the registered TTS endpoint flag:\n%s", rendered)
	}
	if strings.Contains(rendered, "--context-token-tts-url=") {
		t.Fatalf("controller deployment rendered the unregistered TTS URL flag:\n%s", rendered)
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
