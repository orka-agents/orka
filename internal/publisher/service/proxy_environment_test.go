package service

import (
	"strings"
	"testing"
)

func TestLoadConfigFromEnvRequiresValidatedSCMEgressProxy(t *testing.T) {
	clearPublisherEnvForTest(t)
	controller := writePublisherSecretForTest(t, "controller", strings.Repeat("c", 32))
	operation := writePublisherSecretForTest(t, "operation", strings.Repeat("o", MinSecretBytes))
	t.Setenv(EnvControllerTokenFile, controller)
	t.Setenv(EnvOperationCapabilitySecretFile, operation)
	t.Setenv(EnvArtifactAuthorizationBrokerURL, "https://orka-api.example")
	t.Setenv(EnvCredentialBrokerURL, "https://orka-api.example")
	t.Setenv(EnvArtifactAPIURL, "https://orka-api.example")
	t.Setenv(EnvSCMEgressProxyRequired, "true")
	password := strings.Repeat("p", 64)
	t.Setenv("HTTPS_PROXY", "http://orka-publisher:"+password+"@orka-scm-egress-proxy.orka-system.svc:8080")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1,::1,.svc,.cluster.local")

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if config.ProxyEnvironment.HTTPSProxy == "" || config.ProxyEnvironment.NoProxy == "" {
		t.Fatal("SCM egress proxy environment was not retained")
	}
}

func TestLoadConfigFromEnvRejectsUnsafeSCMEgressBypass(t *testing.T) {
	clearPublisherEnvForTest(t)
	controller := writePublisherSecretForTest(t, "controller", strings.Repeat("c", 32))
	operation := writePublisherSecretForTest(t, "operation", strings.Repeat("o", MinSecretBytes))
	t.Setenv(EnvControllerTokenFile, controller)
	t.Setenv(EnvOperationCapabilitySecretFile, operation)
	t.Setenv(EnvArtifactAuthorizationBrokerURL, "https://orka-api.example")
	t.Setenv(EnvCredentialBrokerURL, "https://orka-api.example")
	t.Setenv(EnvArtifactAPIURL, "https://orka-api.example")
	t.Setenv(EnvSCMEgressProxyRequired, "true")
	t.Setenv("HTTPS_PROXY", "http://orka-publisher:"+strings.Repeat("p", 64)+"@proxy.orka-system.svc:8080")
	t.Setenv("NO_PROXY", "github.com")

	if _, err := LoadConfigFromEnv(); err == nil {
		t.Fatal("LoadConfigFromEnv accepted a public NO_PROXY bypass")
	}
}
