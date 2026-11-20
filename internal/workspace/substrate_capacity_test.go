/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSubstrateControlErrorMapsResourceExhaustedToARetryableCapacityKind(t *testing.T) {
	err := substrateControlError("resume", status.Error(codes.ResourceExhausted, "no worker capacity"))
	if !IsKind(err, ErrorKindResourceExhausted) {
		t.Fatalf("kind = %s, want ResourceExhausted", KindOf(err))
	}
	var workspaceErr *Error
	if !errors.As(err, &workspaceErr) || !workspaceErr.Retryable {
		t.Fatal("capacity exhaustion must stay retryable")
	}
	if other := substrateControlError("resume", status.Error(codes.Internal, "boom")); IsKind(other, ErrorKindResourceExhausted) {
		t.Fatal("only ResourceExhausted maps to the capacity kind")
	}
}
