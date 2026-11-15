/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workerenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAzureDeploymentEnvRoundTrip(t *testing.T) {
	for _, deployment := range []string{"primary-slot", ""} {
		t.Run("deployment="+deployment, func(t *testing.T) {
			values := map[string]string{
				"ORKA_AI_MODEL":                            "logical-model",
				"ORKA_AI_AZURE_DEPLOYMENT_NAME":            deployment,
				"ORKA_AI_AZURE_API_VERSION":                "2024-06-01",
				"ORKA_AI_FALLBACK_COUNT":                   "1",
				"ORKA_AI_FALLBACK_0_MODEL":                 "fallback-logical",
				"ORKA_AI_FALLBACK_0_AZURE_DEPLOYMENT_NAME": "fallback-slot",
				"ORKA_AI_FALLBACK_0_AZURE_API_VERSION":     "2024-10-21",
			}
			parsed := ParseAIWorkerEnv(func(name string) string { return values[name] })
			rendered := map[string]string{}
			for _, envVar := range parsed.EnvVars() {
				rendered[envVar.Name] = envVar.Value
			}
			for name, want := range values {
				assert.Equal(t, want, rendered[name], name)
			}
			if deployment == "" {
				assert.NotContains(t, rendered, "ORKA_AI_AZURE_DEPLOYMENT_NAME")
			}
		})
	}
}
