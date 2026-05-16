/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package redact

import (
	"strings"
	"testing"
)

func TestSensitiveTextRedactsTxnTokenHeader(t *testing.T) {
	input := `curl -H "Txn-Token: opaque-secret-token" https://orka.example.test`
	got := SensitiveText(input)
	if strings.Contains(got, "opaque-secret-token") {
		t.Fatalf("SensitiveText leaked Txn-Token value: %q", got)
	}
	if !strings.Contains(got, "Txn-Token: "+redactedValue) {
		t.Fatalf("SensitiveText() = %q, want redacted Txn-Token header", got)
	}
}

func TestSensitiveTextRedactsJWT(t *testing.T) {
	input := "token eyJhbGciOiJSUzI1NiIsInR5cCI6InR4bnRva2VuK2p3dCJ9.eyJzdWIiOiJ3b3JrbG9hZCIsInR4biI6InR4bi0xMjMifQ.signaturevalue1234567890"
	got := SensitiveText(input)
	if strings.Contains(got, "eyJhbGci") {
		t.Fatalf("SensitiveText leaked JWT: %q", got)
	}
}
