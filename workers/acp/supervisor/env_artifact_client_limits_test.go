package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/artifactcap"
)

func TestLoadConfigFromEnvConfiguresInboundPublisherAndOutboundDeltaLimits(t *testing.T) {
	t.Run("controller propagated default", func(t *testing.T) {
		setArtifactClientSupervisorEnv(t)

		cfg, client := loadArtifactClientConfigFromEnv(t)
		if client.maxDownloadBytes != defaultWorkspaceArtifactDownloadBytes {
			t.Fatalf("inbound workspace artifact limit = %d, want propagated default %d", client.maxDownloadBytes, defaultWorkspaceArtifactDownloadBytes)
		}
		if client.maxUploadBytes != cfg.Capabilities.Limits.MaxWorkspaceDeltaBytes {
			t.Fatalf("outbound delta upload limit = %d, want advertised %d", client.maxUploadBytes, cfg.Capabilities.Limits.MaxWorkspaceDeltaBytes)
		}
	})

	t.Run("raised Publisher capability propagated by controller", func(t *testing.T) {
		setArtifactClientSupervisorEnv(t)
		raised := defaultWorkspaceArtifactDownloadBytes + 2<<20
		t.Setenv(EnvWorkspaceArtifactMaxBytes, strconv.FormatInt(raised, 10))

		cfg, client := loadArtifactClientConfigFromEnv(t)
		if client.maxDownloadBytes != raised {
			t.Fatalf("inbound workspace artifact limit = %d, want propagated Publisher capability %d", client.maxDownloadBytes, raised)
		}
		if client.maxUploadBytes != cfg.Capabilities.Limits.MaxWorkspaceDeltaBytes || client.maxUploadBytes == raised {
			t.Fatalf("outbound delta upload limit = %d, advertised=%d, inbound=%d", client.maxUploadBytes, cfg.Capabilities.Limits.MaxWorkspaceDeltaBytes, raised)
		}
	})
}

func TestLoadConfigFromEnvRejectsMissingOrInvalidWorkspaceArtifactLimit(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "missing", value: "", want: "is required"},
		{name: "zero", value: "0", want: "positive integer"},
		{name: "negative", value: "-1", want: "positive integer"},
		{name: "malformed", value: "large", want: "positive integer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setArtifactClientSupervisorEnv(t)
			t.Setenv(EnvWorkspaceArtifactMaxBytes, test.value)
			if _, err := LoadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfigFromEnv() error = %v, want %q", err, test.want)
			}
		})
	}
}

func loadArtifactClientConfigFromEnv(t *testing.T) (Config, *ArtifactClient) {
	t.Helper()
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	materializer, ok := cfg.WorkspaceMaterializer.(*remoteWorkspaceMaterializer)
	if !ok {
		t.Fatalf("workspace materializer type = %T", cfg.WorkspaceMaterializer)
	}
	uploader, ok := cfg.ArtifactUploader.(*remoteArtifactUploader)
	if !ok {
		t.Fatalf("artifact uploader type = %T", cfg.ArtifactUploader)
	}
	if uploader.client != materializer.client {
		t.Fatal("workspace materializer and uploader do not share the configured artifact client")
	}
	return cfg, materializer.client
}

func setArtifactClientSupervisorEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	controllerFile := filepath.Join(dir, "controller-auth")
	capabilityFile := filepath.Join(dir, "capability-auth")
	providerFile := filepath.Join(dir, "provider-auth")
	for path, value := range map[string]string{
		controllerFile: artifactcap.DigestBytes([]byte("controller test credential")),
		capabilityFile: artifactcap.DigestBytes([]byte("capability test credential")),
		providerFile:   artifactcap.DigestBytes([]byte("provider test credential")),
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		EnvRuntimeInstanceID:        "runtime-instance",
		EnvSupervisorBootID:         "boot",
		EnvControllerEpoch:          "1",
		EnvRuntimePoolUID:           "pool-uid",
		EnvRuntimePoolGeneration:    "1",
		EnvProvider:                 providerKindCodex,
		EnvModel:                    "gpt-test",
		EnvWorkspaceIntent:          "read",
		EnvAgentConfigurationDigest: artifactcap.DigestBytes([]byte("agent")),
		EnvToolPolicyDigest:         artifactcap.DigestBytes([]byte("tool")),
		EnvApprovalPolicyDigest:     artifactcap.DigestBytes([]byte("approval")),
		EnvMCPConfigurationDigest:   artifactcap.DigestBytes([]byte("mcp")),
		EnvProxyCredentialRole:      "provider",
		EnvProxyCredentialScope:     "model:gpt-test",
		EnvResourceClass:            "standard",
		EnvControllerTokenFile:      controllerFile,
		EnvCapabilitySecretFile:     capabilityFile,
		EnvProviderTokenFile:        providerFile,
		EnvArtifactAPIURL:           "http://127.0.0.1:1",
		EnvMCPBrokerURL:             "http://127.0.0.1:1",
		EnvTrustNamespace:           "default",
		EnvSessionBaseDir:           filepath.Join(dir, "sessions"),
		EnvFirstSessionUID:          "20000",
		EnvLastSessionUID:           "20010",
		EnvSessionGID:               "20000",
	} {
		t.Setenv(name, value)
	}
	t.Setenv(EnvWorkspaceArtifactMaxBytes, strconv.FormatInt(defaultWorkspaceArtifactDownloadBytes, 10))
}
