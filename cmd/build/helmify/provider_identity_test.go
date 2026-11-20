package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/provideridentity"
)

const testProviderIdentities = `[{"name":"models","tenantID":"11111111-1111-1111-1111-111111111111",` +
	`"clientID":"22222222-2222-2222-2222-222222222222","allowedEndpoints":["https://example-resource.openai.azure.com"]}]`

func TestStaticChartProviderIdentitiesAreOffByDefault(t *testing.T) {
	deployment, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", deployment)
	require.NotContains(t, deployment, "--provider-identities")
	require.NotContains(t, deployment, "provider-identity-token")
}

func TestStaticChartRendersProviderIdentitiesTheControllerAccepts(t *testing.T) {
	rendered, err := helmTemplateStaticChart(t,
		"--set-json", "controller.providerIdentities.identities="+testProviderIdentities,
		"--set-string", "controller.providerIdentities.tokenAudience=api://AzureADTokenExchange",
		"--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", rendered)
	var deployment appsv1.Deployment
	require.NoError(t, yaml.Unmarshal([]byte(requireRenderedDocument(t, rendered, "kind: Deployment\n")), &deployment))

	var flagValue string
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, arg := range append(append([]string{}, container.Command...), container.Args...) {
		if value, ok := strings.CutPrefix(arg, "--provider-identities="); ok {
			flagValue = value
		}
	}
	require.NotEmpty(t, flagValue, "the controller must receive the identities")
	config, err := provideridentity.Parse(flagValue)
	require.NoError(t, err, "the rendered flag must be a configuration the controller accepts: %s", flagValue)
	require.Len(t, config.Identities, 1)
	require.Equal(t, "models", config.Identities[0].Name)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(flagValue), &raw))
	require.Equal(t, "api://AzureADTokenExchange", raw["tokenAudience"])

	var projected bool
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "provider-identity-token" {
			require.NotNil(t, volume.Projected)
			token := volume.Projected.Sources[0].ServiceAccountToken
			require.NotNil(t, token)
			require.Equal(t, "api://AzureADTokenExchange", token.Audience)
			projected = true
		}
	}
	require.True(t, projected, "the controller needs its projected token")
	require.Contains(t, rendered, "mountPath: /var/run/secrets/orka.ai/provider-identity")

	usGov, err := helmTemplateStaticChart(t,
		"--set-json", "controller.providerIdentities.identities="+testProviderIdentities,
		"--set-string", "controller.providerIdentities.tokenAudience=api://AzureADTokenExchangeUSGov",
		"--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", usGov)
	require.Contains(t, usGov, `audience: "api://AzureADTokenExchangeUSGov"`)
}

// The identities trust the controller ServiceAccount, so the chart installs
// the same protection it installs for agent identities.
func TestStaticChartProtectsTheControllerServiceAccountWithProviderIdentities(t *testing.T) {
	const template = "templates/controller-serviceaccount-admission-policy.yaml"
	rendered, err := helmTemplateStaticChartForRelease(t, "orka", "orka-system",
		"--set-json", "controller.providerIdentities.identities="+testProviderIdentities,
		"--show-only", template)
	require.NoError(t, err, "%s", rendered)
	require.Contains(t, rendered, "kind: ValidatingAdmissionPolicy\n")
	require.Contains(t, rendered, "kind: ValidatingAdmissionPolicyBinding\n")
}
