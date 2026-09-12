package service

import (
	"strings"
	"testing"
	"time"
)

func TestWorkspaceResolveCapabilityClaimsAreValid(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	metadata := OperationMetadata{
		Namespace:   "orka-acp-e2e-20260725115056-80978",
		TaskID:      "7bc95937-99a0-4a9c-924c-aa147606066b",
		OperationID: "workspace-resolve-prompt-7bc95937-99a0-4a9c-924c-aa147606066b-1",
	}
	claims := NewClaims(
		OperationWorkspaceResolve,
		metadata,
		"sha256:"+strings.Repeat("0", 64),
		now,
		time.Minute,
	)
	secret := []byte(strings.Repeat("s", MinSecretBytes))
	capability, err := SignCapability(secret, claims)
	if err != nil {
		t.Fatalf("SignCapability(workspace.resolve): %v", err)
	}
	if err := VerifyCapability(secret, capability, claims, now.Add(time.Second)); err != nil {
		t.Fatalf("VerifyCapability(workspace.resolve): %v", err)
	}
}
