package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrokerCredentialProviderUsesExactOperationBinding(t *testing.T) {
	bearer := strings.Repeat("b", 32)
	request := CredentialMaterialRequest{
		ParentOperation: OperationWorkspacePrepare,
		Metadata:        OperationMetadata{Namespace: "default", TaskID: "task", OperationID: "workspace-prepare-prompt"},
		Reference:       CredentialReference{Name: "git-token", Kind: CredentialHTTPExtraHeader},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {
		if httpRequest.URL.Path != CredentialBrokerPath || httpRequest.Header.Get("Authorization") != "Bearer "+bearer {
			t.Errorf("request path/auth = %q %q", httpRequest.URL.Path, httpRequest.Header.Get("Authorization"))
		}
		var got CredentialMaterialRequest
		if err := json.NewDecoder(httpRequest.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		if got != request {
			t.Errorf("request = %#v, want %#v", got, request)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(CredentialMaterialResponse{Material: "Authorization: Bearer canary", ResourceVersion: "123"})
	}))
	defer server.Close()
	provider, err := newBrokerCredentialProvider(server.URL, server.Client(), []byte(bearer), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	material, resourceVersion, err := provider.Read(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(material) != "Authorization: Bearer canary" || resourceVersion != "123" {
		t.Fatalf("material/resourceVersion = %q %q", material, resourceVersion)
	}
}

func TestLoadConfigFromEnvUsesCredentialBrokerWithoutCredentialVolume(t *testing.T) {
	clearPublisherEnvForTest(t)
	controller := writePublisherSecretForTest(t, "controller", strings.Repeat("c", 32))
	operation := writePublisherSecretForTest(t, "operation", strings.Repeat("o", MinSecretBytes))
	t.Setenv(EnvControllerTokenFile, controller)
	t.Setenv(EnvOperationCapabilitySecretFile, operation)
	t.Setenv(EnvArtifactAuthorizationBrokerURL, "https://orka-api.example")
	t.Setenv(EnvCredentialBrokerURL, "https://orka-api.example")
	t.Setenv(EnvArtifactAPIURL, "https://orka-api.example")
	setProductionPublisherProxyForTest(t)

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.CredentialBrokerURL != "https://orka-api.example" || config.CredentialRoot != "" {
		t.Fatalf("credential delivery config = %#v", config)
	}
}
