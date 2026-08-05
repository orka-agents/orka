package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
