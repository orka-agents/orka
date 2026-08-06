package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestSecurityRunThreatModelImmutableReplay(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan", Version: 1, Content: "# Threat model\nBounded.",
	}
	created, err := s.SaveSecurityRunThreatModel(ctx, model)
	if err != nil || !created {
		t.Fatalf("first save = %v, %v", created, err)
	}
	created, err = s.SaveSecurityRunThreatModel(ctx, model)
	if err != nil || created {
		t.Fatalf("replay = %v, %v", created, err)
	}
	conflict := *model
	conflict.Content = "# Different"
	conflict.ContentDigest = ""
	if _, err := s.SaveSecurityRunThreatModel(ctx, &conflict); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("conflict error = %v", err)
	}
	got, err := s.GetSecurityRunThreatModel(ctx, "ns", model.RunUID)
	if err != nil || got.Content != model.Content {
		t.Fatalf("get = %#v, %v", got, err)
	}
}

func TestSecurityRunThreatModelRedactsCredentialContent(t *testing.T) {
	s := setupTestStore(t)
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-1", Version: 1,
		Content: strings.Join([]string{"Author", "ization: ", "Bear", "er run-value-for-redaction"}, ""),
	}
	if _, err := s.SaveSecurityRunThreatModel(context.Background(), model); err != nil {
		t.Fatalf("SaveSecurityRunThreatModel() error = %v", err)
	}
	got, err := s.GetSecurityRunThreatModel(context.Background(), "ns", model.RunUID)
	if err != nil {
		t.Fatalf("GetSecurityRunThreatModel() error = %v", err)
	}
	if strings.Contains(got.Content, "run-value-for-redaction") || !strings.Contains(got.Content, "[REDACTED]") {
		t.Fatalf("persisted threat model retained credential: %q", got.Content)
	}
}

func TestSecurityRunThreatModelVerifiesSubmittedDigestBeforeNormalization(t *testing.T) {
	s := setupTestStore(t)
	submitted := strings.Join([]string{
		"  # Threat model\r\n",
		"Author", "ization: ", "Bear", "er submitted-value-for-redaction\r\n  ",
	}, "")
	submittedDigest := securityDigestBytes([]byte(submitted))
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-raw-digest", Version: 1,
		Content: submitted, ContentDigest: submittedDigest,
	}
	created, err := s.SaveSecurityRunThreatModel(context.Background(), model)
	if err != nil || !created {
		t.Fatalf("SaveSecurityRunThreatModel() = (%v, %v), want (true, nil)", created, err)
	}
	got, err := s.GetSecurityRunThreatModel(context.Background(), "ns", model.RunUID)
	if err != nil {
		t.Fatalf("GetSecurityRunThreatModel() error = %v", err)
	}
	if strings.Contains(got.Content, "submitted-value-for-redaction") || !strings.Contains(got.Content, "[REDACTED]") {
		t.Fatalf("persisted threat model retained credential: %q", got.Content)
	}
	wantPersistedDigest := securityDigestBytes([]byte(got.Content))
	if got.ContentDigest != wantPersistedDigest {
		t.Fatalf("persisted contentDigest = %q, want %q", got.ContentDigest, wantPersistedDigest)
	}
	if got.ContentDigest == submittedDigest {
		t.Fatalf("persisted contentDigest unexpectedly retained submitted digest %q", submittedDigest)
	}
}

func TestSecurityRunThreatModelNormalizesActualCRLFAndPreservesLiteralEscapes(t *testing.T) {
	s := setupTestStore(t)
	submitted := "first\r\nsecond\\r\\nthird\r\n"
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-newlines", Version: 1,
		Content: submitted, ContentDigest: securityDigestBytes([]byte(submitted)),
	}
	created, err := s.SaveSecurityRunThreatModel(context.Background(), model)
	if err != nil || !created {
		t.Fatalf("SaveSecurityRunThreatModel() = (%v, %v), want (true, nil)", created, err)
	}
	got, err := s.GetSecurityRunThreatModel(context.Background(), "ns", model.RunUID)
	if err != nil {
		t.Fatalf("GetSecurityRunThreatModel() error = %v", err)
	}
	const want = "first\nsecond\\r\\nthird"
	if got.Content != want {
		t.Fatalf("persisted content = %q, want %q", got.Content, want)
	}
	if got.ContentDigest != securityDigestBytes([]byte(want)) {
		t.Fatalf("persisted contentDigest = %q, want normalized content digest", got.ContentDigest)
	}
}

func TestSecurityRunThreatModelRejectsMismatchedSubmittedDigestWithoutMutation(t *testing.T) {
	s := setupTestStore(t)
	submitted := "  # Threat model\r\nBounded.  "
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-bad-digest", Version: 1,
		Content: submitted, ContentDigest: securityDigestBytes([]byte("different")),
	}
	if _, err := s.SaveSecurityRunThreatModel(context.Background(), model); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SaveSecurityRunThreatModel() error = %v, want ErrValidation", err)
	}
	if model.Content != submitted {
		t.Fatalf("content mutated before digest rejection: %q", model.Content)
	}
}

func TestSecurityRunThreatModelRejectsOversizedRawContentBeforeNormalization(t *testing.T) {
	s := setupTestStore(t)
	submitted := strings.Repeat(" ", maxSecurityPayloadBytes) + "x"
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-raw-oversized", Version: 1,
		Content: submitted, ContentDigest: securityDigestBytes([]byte(submitted)),
	}
	originalDigest := model.ContentDigest
	if _, err := s.SaveSecurityRunThreatModel(context.Background(), model); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SaveSecurityRunThreatModel() error = %v, want ErrValidation", err)
	}
	if model.Content != submitted || model.ContentDigest != originalDigest {
		t.Fatalf("oversized raw content mutated before rejection: content bytes=%d digest=%q", len(model.Content), model.ContentDigest)
	}
	if _, err := s.GetSecurityRunThreatModel(context.Background(), model.Namespace, model.RunUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSecurityRunThreatModel(after rejection) error = %v, want ErrNotFound", err)
	}
}

func TestSecurityRunThreatModelRejectsInvalidUTF8BeforeNormalization(t *testing.T) {
	s := setupTestStore(t)
	submitted := string([]byte{'#', ' ', 'T', 'h', 'r', 'e', 'a', 't', 0xff})
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-invalid-utf8", Version: 1,
		Content: submitted, ContentDigest: securityDigestBytes([]byte(submitted)),
	}
	originalDigest := model.ContentDigest
	if _, err := s.SaveSecurityRunThreatModel(context.Background(), model); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SaveSecurityRunThreatModel() error = %v, want ErrValidation", err)
	}
	if model.Content != submitted || model.ContentDigest != originalDigest {
		t.Fatalf("invalid UTF-8 content mutated before rejection: content=%q digest=%q", model.Content, model.ContentDigest)
	}
	if _, err := s.GetSecurityRunThreatModel(context.Background(), model.Namespace, model.RunUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSecurityRunThreatModel(after rejection) error = %v, want ErrNotFound", err)
	}
}

func TestSecurityRunThreatModelRetainsPostNormalizationSizeBound(t *testing.T) {
	s := setupTestStore(t)
	const credentialUnit = "token=x "
	submitted := strings.Repeat(credentialUnit, maxSecurityPayloadBytes/len(credentialUnit))
	if len(submitted) > maxSecurityPayloadBytes {
		t.Fatalf("test input bytes = %d, want at most %d", len(submitted), maxSecurityPayloadBytes)
	}
	model := &store.SecurityRunThreatModel{
		RunUID:    "run_9999999999999999999999999999999999999999999999999999999999999999",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-normalized-oversized", Version: 1,
		Content: submitted,
	}
	if _, err := s.SaveSecurityRunThreatModel(context.Background(), model); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SaveSecurityRunThreatModel() error = %v, want ErrValidation", err)
	}
}
