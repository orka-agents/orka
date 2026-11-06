package v2

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

func operatorTestMetadata(t *testing.T) MutationMetadata {
	t.Helper()
	return MutationMetadata{
		Fence:       testFence(t),
		OperationID: "operator-exec-op-1",
		ExpiresAt:   time.Now().UTC().Add(OperatorCapabilityTTL),
	}
}

func TestOperatorExecRequestSealDecodeAndVerify(t *testing.T) {
	secret := bytes.Repeat([]byte("s"), MinCapabilitySecretBytes)
	request := OperatorExecRequest{
		Protocol: ProtocolVersion, Metadata: operatorTestMetadata(t), AuditID: "op-1",
		Exec: operatoraccess.ExecOptions{Argv: []string{"ls", "-la"}},
	}
	if err := request.Exec.Normalize(); err != nil {
		t.Fatal(err)
	}
	header, capability, err := SealOperatorRequest(secret, &request, &request.Metadata)
	if err != nil {
		t.Fatalf("SealOperatorRequest: %v", err)
	}
	var decoded OperatorExecRequest
	if err := DecodeOperatorRequestHeader(header, &decoded); err != nil {
		t.Fatalf("DecodeOperatorRequestHeader: %v", err)
	}
	if err := decoded.ValidateAt(time.Now().UTC()); err != nil {
		t.Fatalf("ValidateAt: %v", err)
	}
	if err := VerifyOperationCapability(secret, capability, decoded.Metadata, true, time.Now().UTC()); err != nil {
		t.Fatalf("VerifyOperationCapability: %v", err)
	}
	if err := VerifyOperationCapability(bytes.Repeat([]byte("x"), MinCapabilitySecretBytes), capability, decoded.Metadata, true, time.Now().UTC()); err == nil {
		t.Fatal("a capability signed with another pool's secret must be rejected")
	}

	// Changing argv after signing breaks the content digest even when the
	// attacker re-encodes canonical JSON.
	tampered := decoded
	tampered.Exec.Argv = []string{"rm", "-rf", "/"}
	raw, err := CanonicalValue(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeOperatorRequestHeader(base64.RawURLEncoding.EncodeToString(raw), &OperatorExecRequest{}); err == nil ||
		!strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered argv must fail the digest check, got %v", err)
	}
}

func TestOperatorRequestHeaderRejectsMalformedInput(t *testing.T) {
	for _, header := range []string{"", "%%%", strings.Repeat("a", MaxOperatorRequestHeaderBytes+1), base64.RawURLEncoding.EncodeToString([]byte(`{"metadata":{}}`))} {
		if err := DecodeOperatorRequestHeader(header, &OperatorExecRequest{}); err == nil {
			t.Fatalf("header %q must be rejected", header)
		}
	}
	// Unknown fields are refused, so a newer client cannot smuggle options
	// an older supervisor would ignore.
	request := map[string]any{"protocol": ProtocolVersion, "metadata": map[string]any{"requestDigest": ""}, "unexpected": true}
	digest, err := CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request["metadata"] = map[string]any{"requestDigest": string(digest)}
	raw, _ := CanonicalValue(request)
	if err := DecodeOperatorRequestHeader(base64.RawURLEncoding.EncodeToString(raw), &OperatorExecRequest{}); err == nil {
		t.Fatal("unknown request fields must be rejected")
	}
}

func TestOperatorRequestRequiresSessionFenceAndBoundedExpiry(t *testing.T) {
	metadata := operatorTestMetadata(t)
	metadata.Fence.RuntimeSessionUID = ""
	metadata.Fence.RuntimeSessionGeneration = 0
	request := OperatorExecRequest{Protocol: ProtocolVersion, Metadata: metadata, AuditID: "op-1", Exec: operatoraccess.ExecOptions{Argv: []string{"ls"}}}
	request.Metadata.RequestDigestSchemaVersion = RequestDigestSchemaVersion
	request.Metadata.RequestDigest = testSHA256Digest()
	if err := request.ValidateAt(time.Now().UTC()); err == nil {
		t.Fatal("operator requests must name an exact RuntimeSession")
	}
	request.Metadata = operatorTestMetadata(t)
	request.Metadata.RequestDigestSchemaVersion = RequestDigestSchemaVersion
	request.Metadata.RequestDigest = testSHA256Digest()
	request.Metadata.ExpiresAt = time.Now().UTC().Add(24 * time.Hour)
	if err := request.ValidateAt(time.Now().UTC()); err == nil {
		t.Fatal("long-lived operator capabilities must be rejected")
	}
}

func TestOperatorFileRequestValidation(t *testing.T) {
	base := func() OperatorFileRequest {
		metadata := operatorTestMetadata(t)
		metadata.RequestDigestSchemaVersion = RequestDigestSchemaVersion
		metadata.RequestDigest = testSHA256Digest()
		return OperatorFileRequest{Protocol: ProtocolVersion, Metadata: metadata, AuditID: "op-1", Path: "src/main.go", MaxBytes: 1024}
	}
	ok := base()
	if err := ok.ValidateAt(time.Now().UTC()); err != nil {
		t.Fatalf("valid read: %v", err)
	}
	write := base()
	write.Write, write.Size = true, 1024
	if err := write.ValidateAt(time.Now().UTC()); err != nil {
		t.Fatalf("valid write: %v", err)
	}
	for name, mutate := range map[string]func(*OperatorFileRequest){
		"traversal":     func(r *OperatorFileRequest) { r.Path = "../etc/passwd" },
		"non-canonical": func(r *OperatorFileRequest) { r.Path = "/src//main.go" },
		"zero bound":    func(r *OperatorFileRequest) { r.MaxBytes = 0 },
		"huge bound":    func(r *OperatorFileRequest) { r.MaxBytes = operatoraccess.MaxFileBytes + 1 },
		"read size":     func(r *OperatorFileRequest) { r.Size = 1 },
		"write oversize": func(r *OperatorFileRequest) {
			r.Write, r.Size = true, r.MaxBytes+1
		},
	} {
		request := base()
		mutate(&request)
		if err := request.ValidateAt(time.Now().UTC()); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}
}

func testSHA256Digest() RequestDigest {
	return RequestDigest("sha256:" + strings.Repeat("a", 64))
}
