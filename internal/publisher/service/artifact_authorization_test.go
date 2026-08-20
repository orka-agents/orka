package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestBrokerArtifactAuthorizerRequestsExactCapability(t *testing.T) {
	secret := []byte(strings.Repeat("a", artifactcap.MinSecretBytes))
	bearer := strings.Repeat("b", 32)
	data := []byte("workspace")
	digest := artifactcap.DigestBytes(data)
	artifactID, err := artifactcap.ArtifactIDForDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	request := ArtifactAuthorizationRequest{
		ParentOperation:   OperationWorkspacePrepare,
		Metadata:          OperationMetadata{Namespace: "default", TaskID: "task", OperationID: "workspace-prepare-prompt"},
		ArtifactOperation: artifactcap.OperationUpload,
		Artifact:          harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(artifactID), Digest: digest, SizeBytes: int64(len(data)), MediaType: artifactcap.MediaTypeWorkspaceTar},
		Attempt:           1,
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {
		if httpRequest.URL.Path != ArtifactAuthorizationBrokerPath || httpRequest.Header.Get("Authorization") != "Bearer "+bearer {
			t.Errorf("request path/auth = %q %q", httpRequest.URL.Path, httpRequest.Header.Get("Authorization"))
		}
		var got ArtifactAuthorizationRequest
		if err := json.NewDecoder(httpRequest.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		if got != request {
			t.Errorf("request = %#v, want %#v", got, request)
		}
		binding, err := ArtifactBinding(got)
		if err != nil {
			t.Errorf("binding: %v", err)
			return
		}
		authorization, err := artifactcap.Issue(secret, binding, time.Now().UTC(), time.Minute)
		if err != nil {
			t.Errorf("issue: %v", err)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(ArtifactAuthorizationResponse{Capability: authorization.Capability, RequestDigest: authorization.RequestDigest})
	}))
	defer server.Close()
	authorizer, err := newBrokerArtifactAuthorizer(server.URL, server.Client(), []byte(bearer), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authorizer.Authorize(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := ArtifactBinding(request)
	if err != nil {
		t.Fatal(err)
	}
	path, err := artifactcap.ObjectPath(binding.ObjectDigest)
	if err != nil {
		t.Fatal(err)
	}
	presented := artifactcap.PresentedRequest{Method: binding.Method(), Path: path, ObjectDigest: binding.ObjectDigest, ContentLength: binding.ContentLength, MediaType: binding.MediaType, RequestDigest: issued.RequestDigest}
	if _, err := artifactcap.Verify(secret, issued.Capability, presented, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactClientPreservesBrokerAuthorizationClassification(t *testing.T) {
	data := []byte("workspace")
	digest := artifactcap.DigestBytes(data)
	artifactID, err := artifactcap.ArtifactIDForDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	reference := harnessv2.ArtifactReference{
		ArtifactID: harnessv2.ArtifactID(artifactID), Digest: digest,
		SizeBytes: int64(len(data)), MediaType: artifactcap.MediaTypeWorkspaceTar,
	}
	metadata := OperationMetadata{Namespace: "default", TaskID: "task", OperationID: "workspace-prepare-prompt"}
	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{name: "transient broker outage", status: http.StatusServiceUnavailable, retryable: true},
		{name: "broker authorization denial", status: http.StatusForbidden, retryable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorizer := failingArtifactAuthorizerForTest{err: apiError(
				ErrArtifactTransport, "artifact_authorization_rejected", "broker rejected authorization",
				test.status, test.retryable, nil,
			)}
			client, err := newArtifactClient("http://artifact.example", nil, authorizer, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			err = client.upload(context.Background(), OperationWorkspacePrepare, metadata, 1, reference, bytes.NewReader(data))
			var operationErr *operationError
			if !errors.As(err, &operationErr) {
				t.Fatalf("upload error = %v, want operationError", err)
			}
			if operationErr.status != test.status || operationErr.retryable != test.retryable || operationErr.code != "artifact_authorization_failed" {
				t.Fatalf("upload authorization classification = status %d retryable %t code %q, want %d/%t/artifact_authorization_failed", operationErr.status, operationErr.retryable, operationErr.code, test.status, test.retryable)
			}
		})
	}
}

type failingArtifactAuthorizerForTest struct {
	err error
}

func (a failingArtifactAuthorizerForTest) Authorize(context.Context, ArtifactAuthorizationRequest) (artifactcap.Authorization, error) {
	return artifactcap.Authorization{}, a.err
}

func TestLoadConfigFromEnvUsesArtifactBrokerWithoutSigningKey(t *testing.T) {
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
	if config.ArtifactAuthorizationBrokerURL != "https://orka-api.example" || len(config.ArtifactCapabilitySecret) != 0 {
		t.Fatalf("artifact authorization config = %#v", config)
	}
}
