/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestDeterministicSubstrateTaskActorID(t *testing.T) {
	tests := []struct {
		name    string
		uid     string
		attempt int32
		want    string
	}{
		{
			name:    "standard kubernetes uid uses full uid hash",
			uid:     "abcdef01-2345-6789-abcd-ef0123456789",
			attempt: 1,
			want:    "orka-t-" + substrateActorIDHashPrefix("abcdef01-2345-6789-abcd-ef0123456789") + "-1",
		},
		{
			name:    "normalizes uid case and attempt",
			uid:     " ABC_DEF.123 ",
			attempt: 0,
			want:    "orka-t-" + substrateActorIDHashPrefix("ABC_DEF.123") + "-1",
		},
		{
			name:    "hashes punctuation uid",
			uid:     "!!!",
			attempt: 2,
			want:    "orka-t-" + substrateActorIDHashPrefix("!!!") + "-2",
		},
		{
			name:    "different suffix changes actor id",
			uid:     "abcdef01-2345-6789-abcd-ef0123456780",
			attempt: 1,
			want:    "orka-t-" + substrateActorIDHashPrefix("abcdef01-2345-6789-abcd-ef0123456780") + "-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deterministicSubstrateTaskActorID(tt.uid, tt.attempt)
			if got != tt.want {
				t.Fatalf("deterministicSubstrateTaskActorID() = %q, want %q", got, tt.want)
			}
			if len(strings.TrimPrefix(got, "orka-t-")) < 34 {
				t.Fatalf("deterministicSubstrateTaskActorID() = %q, want hash plus attempt suffix", got)
			}
		})
	}
}

func TestDeterministicSubstrateSessionActorID(t *testing.T) {
	got := deterministicSubstrateSessionActorID(" task-ns ", " template-ns ", " codex ", " session-1 ")
	if got != deterministicSubstrateSessionActorID("task-ns", "template-ns", "codex", "session-1") {
		t.Fatalf("deterministicSubstrateSessionActorID() did not trim inputs consistently")
	}
	if !strings.HasPrefix(got, "orka-s-") || len(got) != len("orka-s-")+32 {
		t.Fatalf("deterministicSubstrateSessionActorID() = %q, want orka-s- plus 32 hex chars", got)
	}
	if got == deterministicSubstrateSessionActorID("task-ns", "template-ns", "codex", "session-2") {
		t.Fatalf("deterministicSubstrateSessionActorID() did not include reuse key in hash")
	}
}

func substrateActorIDHashPrefix(value string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(value))))
	return hex.EncodeToString(sum[:])[:32]
}
