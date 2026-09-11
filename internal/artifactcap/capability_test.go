package artifactcap

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCapabilityBindsExactRequestAndExpires(t *testing.T) {
	t.Parallel()
	secret := []byte(strings.Repeat("s", MinSecretBytes))
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	request := testOperationRequest(OperationUpload, []byte("artifact"), "operation-1")
	authorization, err := Issue(secret, request, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	presented := present(request, authorization)
	claims, err := Verify(secret, authorization.Capability, presented, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claims.Request != request {
		t.Fatalf("claims request = %#v, want %#v", claims.Request, request)
	}

	mutations := []struct {
		name string
		edit func(*PresentedRequest)
	}{
		{"method", func(value *PresentedRequest) { value.Method = "GET" }},
		{"path", func(value *PresentedRequest) { value.Path += "/extra" }},
		{"digest", func(value *PresentedRequest) { value.ObjectDigest = DigestBytes([]byte("different")) }},
		{"length", func(value *PresentedRequest) { value.ContentLength++ }},
		{"media", func(value *PresentedRequest) { value.MediaType = "application/json" }},
		{"request digest", func(value *PresentedRequest) { value.RequestDigest = DigestBytes([]byte("different binding")) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := presented
			mutation.edit(&changed)
			if _, err := Verify(secret, authorization.Capability, changed, now.Add(30*time.Second)); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Verify() error = %v, want ErrUnauthorized", err)
			}
		})
	}
	if _, err := Verify(secret, authorization.Capability, presented, now.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired Verify() error = %v, want ErrExpired", err)
	}
}

func TestCapabilityRejectsTamperingWithoutSecretDisclosure(t *testing.T) {
	t.Parallel()
	secretText := strings.Repeat("super-secret-value-", 2)
	secret := []byte(secretText)
	now := time.Now().UTC()
	request := testOperationRequest(OperationDownload, []byte("artifact"), "operation-2")
	authorization, err := Issue(secret, request, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	replacement := "A"
	if strings.HasSuffix(authorization.Capability, replacement) {
		replacement = "B"
	}
	tampered := authorization.Capability[:len(authorization.Capability)-1] + replacement
	_, err = Verify(secret, tampered, present(request, authorization), now)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Verify() error = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), secretText) || strings.Contains(err.Error(), authorization.Capability) {
		t.Fatalf("authorization error disclosed a secret: %q", err)
	}
}

func testOperationRequest(operation Operation, data []byte, operationID string) OperationRequest {
	return OperationRequest{
		Operation:     operation,
		ObjectDigest:  DigestBytes(data),
		Identity:      Identity{Namespace: "default", TaskID: "task-uid-123"},
		ContentLength: int64(len(data)),
		MediaType:     "application/octet-stream",
		OperationID:   operationID,
	}
}

func present(request OperationRequest, authorization Authorization) PresentedRequest {
	return PresentedRequest{
		Method:        request.Method(),
		Path:          request.Path(),
		ObjectDigest:  request.ObjectDigest,
		ContentLength: request.ContentLength,
		MediaType:     request.MediaType,
		RequestDigest: authorization.RequestDigest,
	}
}
