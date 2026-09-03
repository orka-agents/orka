package service

import (
	"strings"
	"testing"
)

func TestLoadConfigFromEnvRejectsLocalArtifactFallbackByDefault(t *testing.T) {
	clearPublisherEnvForTest(t)
	configureRequiredPublisherEnvForTest(t)
	artifact := writePublisherSecretForTest(t, "artifact", strings.Repeat("a", MinSecretBytes))
	t.Setenv(EnvArtifactCapabilitySecretFile, artifact)
	t.Setenv(EnvCredentialBrokerURL, "https://orka-api.example")

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), EnvArtifactAuthorizationBrokerURL) ||
		!strings.Contains(err.Error(), EnvAllowDevelopmentFallbacks) {
		t.Fatalf("LoadConfigFromEnv error = %v", err)
	}
}

func TestLoadConfigFromEnvRejectsCredentialRootFallbackByDefault(t *testing.T) {
	clearPublisherEnvForTest(t)
	configureRequiredPublisherEnvForTest(t)
	t.Setenv(EnvArtifactAuthorizationBrokerURL, "https://orka-api.example")
	t.Setenv(EnvCredentialRoot, t.TempDir())

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), EnvCredentialBrokerURL) ||
		!strings.Contains(err.Error(), EnvAllowDevelopmentFallbacks) {
		t.Fatalf("LoadConfigFromEnv error = %v", err)
	}
}

func TestLoadConfigFromEnvAllowsExplicitDevelopmentFallbacks(t *testing.T) {
	clearPublisherEnvForTest(t)
	configureRequiredPublisherEnvForTest(t)
	artifact := writePublisherSecretForTest(t, "artifact", strings.Repeat("a", MinSecretBytes))
	credentialRoot := t.TempDir()
	t.Setenv(EnvAllowDevelopmentFallbacks, "true")
	t.Setenv(EnvArtifactCapabilitySecretFile, artifact)
	t.Setenv(EnvCredentialRoot, credentialRoot)

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if len(config.ArtifactCapabilitySecret) != MinSecretBytes || config.ArtifactAuthorizationBrokerURL != "" {
		t.Fatalf("artifact fallback config = %#v", config)
	}
	if config.CredentialRoot != credentialRoot || config.CredentialBrokerURL != "" {
		t.Fatalf("credential fallback config = %#v", config)
	}
	if config.ProxyEnvironment.HTTPSProxy != "" {
		t.Fatal("development fallback unexpectedly required the SCM egress proxy")
	}
}

func TestLoadConfigFromEnvRequiresSCMEgressProxyInProduction(t *testing.T) {
	clearPublisherEnvForTest(t)
	configureRequiredPublisherEnvForTest(t)
	t.Setenv(EnvArtifactAuthorizationBrokerURL, "https://orka-api.example")
	t.Setenv(EnvCredentialBrokerURL, "https://orka-api.example")

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), EnvSCMEgressProxyRequired) ||
		!strings.Contains(err.Error(), EnvAllowDevelopmentFallbacks) {
		t.Fatalf("LoadConfigFromEnv error = %v", err)
	}
}

func configureRequiredPublisherEnvForTest(t *testing.T) {
	t.Helper()
	controller := writePublisherSecretForTest(t, "controller", strings.Repeat("c", 32))
	operation := writePublisherSecretForTest(t, "operation", strings.Repeat("o", MinSecretBytes))
	t.Setenv(EnvControllerTokenFile, controller)
	t.Setenv(EnvOperationCapabilitySecretFile, operation)
	t.Setenv(EnvArtifactAPIURL, "https://orka-api.example")
}
