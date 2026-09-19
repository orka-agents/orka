package controller

import (
	"errors"
	"strings"

	"testing"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestACPWorkspaceValidationFailureMessageProjectsSupervisorReason(t *testing.T) {
	t.Parallel()
	const generic = "workspace validation failed before a trusted delta was established"
	if got := acpWorkspaceValidationFailureMessage(errors.New("dial tcp: connection refused")); got != generic {
		t.Fatalf("non-client error message = %q, want %q", got, generic)
	}
	clientErr := &harnessv2.ClientError{StatusCode: 409, Code: harnessv2.ErrorCodeSessionPoisoned, Message: "workspace validation failed: validate: reserved workspace path"}
	got := acpWorkspaceValidationFailureMessage(clientErr)
	if !strings.HasPrefix(got, generic+": ") || !strings.Contains(got, "reserved workspace path") {
		t.Fatalf("client error message = %q", got)
	}
	if got := acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{StatusCode: 409, Message: "   "}); got != generic {
		t.Fatalf("blank client message = %q, want %q", got, generic)
	}
	long := &harnessv2.ClientError{StatusCode: 409, Code: harnessv2.ErrorCodeSessionPoisoned, Message: "workspace validation failed: " + strings.Repeat("x", 2*acpPromptFailureMessageLimit)}
	if got := acpWorkspaceValidationFailureMessage(long); len(got) > acpPromptFailureMessageLimit || !strings.HasPrefix(got, generic+": ") {
		t.Fatalf("len(long client message) = %d (limit %d), prefix ok=%v", len(got), acpPromptFailureMessageLimit, strings.HasPrefix(got, generic+": "))
	}
}

func TestACPWorkspaceValidationFailureMessageTruncatesOnRuneBoundary(t *testing.T) {
	t.Parallel()
	// Three-byte runes put the workspace status byte limit inside a rune.
	multibyte := strings.Repeat("界", acpPromptFailureMessageLimit)
	clientErr := &harnessv2.ClientError{StatusCode: 409, Code: harnessv2.ErrorCodeSessionPoisoned, Message: multibyte}
	got := acpWorkspaceValidationFailureMessage(clientErr)
	const generic = "workspace validation failed before a trusted delta was established: "
	// The complete projected message, including its prefix, is bounded on a
	// rune boundary.
	if !strings.HasPrefix(got, generic) || !utf8.ValidString(got) || len(got) > acpPromptFailureMessageLimit || len(got) < acpPromptFailureMessageLimit-utf8.UTFMax {
		t.Fatalf("acpWorkspaceValidationFailureMessage() = %q (%d bytes)", got, len(got))
	}
}

func TestACPWorkspaceValidationFailureMessageRedactsCredentialShapedDetail(t *testing.T) {
	t.Parallel()
	const (
		leakedKey = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
		leakedJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJvcmthLXRlc3QifQ.c2lnbmF0dXJlLXRoYXQtbXVzdC1ub3QtbGVhaw"
	)
	detail := "upstream rejected api_key=" + leakedKey + " and Authorization: Bearer " + leakedJWT + " for the model"
	assertRedacted := func(label, got string) {
		t.Helper()
		if strings.Contains(got, leakedKey) || strings.Contains(got, leakedJWT) {
			t.Fatalf("%s leaked a credential: %q", label, got)
		}
		if !strings.Contains(got, "upstream rejected") || !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%s lost its surrounding prose: %q", label, got)
		}
	}
	assertRedacted("acpWorkspaceValidationFailureMessage", acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{
		StatusCode: 409, Code: harnessv2.ErrorCodeSessionPoisoned, Message: detail,
	}))
}

func TestACPWorkspaceValidationFailureMessageRedactsCredentialsSplitByControlRunes(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	// C0, DEL, and C1 controls inside the token must be dropped (not
	// replaced) so the token reassembles and is redacted as a whole.
	split := "upstream rejected api_key=" + key[:10] + "\x00" + key[10:20] + "\x7f" + key[20:30] + "\u0085" + key[30:] + " for the model"
	got := acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{
		StatusCode: 409, Code: harnessv2.ErrorCodeSessionPoisoned, Message: split,
	})
	for _, fragment := range []string{key[:10], key[10:20], key[20:30], key[30:]} {
		if strings.Contains(got, fragment) {
			t.Fatalf("acpWorkspaceValidationFailureMessage() leaked credential fragment %q: %q", fragment, got)
		}
	}
	if !strings.Contains(got, "upstream rejected") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("acpWorkspaceValidationFailureMessage() = %q, want redacted prose", got)
	}
}

func TestStripACPControlRunesReassemblesLineWrappedCredential(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	split := "upstream rejected " + key[:11] + "\n" + key[11:] + " for the model"
	got := redact.SensitiveText(stripACPControlRunes(split))
	if strings.Contains(got, key[:11]) || strings.Contains(got, key[11:]) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitized detail = %q, want line-wrapped credential redacted", got)
	}
}

func TestStripACPControlRunesDropsFormatRunesBeforeRedaction(t *testing.T) {
	t.Parallel()
	// A zero-width space (U+200B, category Cf) inside a credential must not
	// split the token past the redactor in persisted status or logs.
	const secret = "ak-live-0123456789abcdef"
	split := "api_key=" + secret[:10] + "\u200b" + secret[10:]
	got := redact.SensitiveText(stripACPControlRunes(split))
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitized detail = %q, want the reassembled credential redacted", got)
	}
}
