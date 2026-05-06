/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/store"
)

const (
	testScanRunID2 = "scan-2"
	testStateOpen  = "open"
)

func TestSaveThreatModelReplacesCurrentModel(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	first := &store.ThreatModel{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Content:        "first threat model",
		Source:         "generated",
	}
	if err := s.SaveThreatModel(ctx, first); err != nil {
		t.Fatalf("SaveThreatModel(first): %v", err)
	}

	second := &store.ThreatModel{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Content:        "updated threat model",
		Source:         "edited",
	}
	if err := s.SaveThreatModel(ctx, second); err != nil {
		t.Fatalf("SaveThreatModel(second): %v", err)
	}

	got, err := s.GetLatestThreatModel(ctx, "ns1", "repo1")
	if err != nil {
		t.Fatalf("GetLatestThreatModel: %v", err)
	}
	if got.Content != "updated threat model" {
		t.Fatalf("Content = %q, want %q", got.Content, "updated threat model")
	}
	if got.Source != "edited" {
		t.Fatalf("Source = %q, want %q", got.Source, "edited")
	}
	if got.Version != 2 {
		t.Fatalf("Version = %d, want 2", got.Version)
	}

	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM security_threat_models WHERE namespace = ? AND repository_scan = ?`,
		"ns1", "repo1",
	).Scan(&count); err != nil {
		t.Fatalf("count threat models: %v", err)
	}
	if count != 1 {
		t.Fatalf("threat model row count = %d, want 1", count)
	}
}

func TestSaveThreatModelCollapsesExistingVersions(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	createdAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	for version, content := range map[int]string{
		1: "older model",
		2: "newer model",
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO security_threat_models
			 (namespace, repository_scan, version, content, source, generated_by_scan, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"ns1", "repo1", version, content, "generated", "", createdAt, createdAt,
		); err != nil {
			t.Fatalf("seed threat model version %d: %v", version, err)
		}
	}

	current := &store.ThreatModel{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Content:        "singleton threat model",
		Source:         "edited",
	}
	if err := s.SaveThreatModel(ctx, current); err != nil {
		t.Fatalf("SaveThreatModel(current): %v", err)
	}

	got, err := s.GetLatestThreatModel(ctx, "ns1", "repo1")
	if err != nil {
		t.Fatalf("GetLatestThreatModel: %v", err)
	}
	if got.Content != "singleton threat model" {
		t.Fatalf("Content = %q, want %q", got.Content, "singleton threat model")
	}
	if got.Version != 3 {
		t.Fatalf("Version = %d, want 3", got.Version)
	}

	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM security_threat_models WHERE namespace = ? AND repository_scan = ?`,
		"ns1", "repo1",
	).Scan(&count); err != nil {
		t.Fatalf("count threat models: %v", err)
	}
	if count != 1 {
		t.Fatalf("threat model row count = %d, want 1", count)
	}
}

func TestUpsertFindingPreservesMostAdvancedStateAndPRMetadata(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	prNumber := 123
	initial := &store.Finding{
		ID:               "fnd-123",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:unauthenticated-preview",
		Title:            "Preview disclosure",
		Summary:          "initial summary",
		Severity:         "medium",
		Confidence:       "high",
		ValidationStatus: "validated",
		State:            "pr_open",
		PatchProposalID:  "patch-123",
		PRNumber:         &prNumber,
		PRURL:            "https://github.com/example/repo/pull/123",
	}
	if err := s.UpsertFinding(ctx, initial); err != nil {
		t.Fatalf("UpsertFinding(initial): %v", err)
	}

	laterStage := &store.Finding{
		ID:               "fnd-123",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        testScanRunID2,
		Fingerprint:      initial.Fingerprint,
		Title:            initial.Title,
		Summary:          "later summary",
		Severity:         initial.Severity,
		Confidence:       initial.Confidence,
		ValidationStatus: "pending",
		State:            "patch_ready",
	}
	if err := s.UpsertFinding(ctx, laterStage); err != nil {
		t.Fatalf("UpsertFinding(laterStage): %v", err)
	}

	got, err := s.GetFinding(ctx, "ns1", "fnd-123")
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.State != "pr_open" {
		t.Fatalf("State = %q, want %q", got.State, "pr_open")
	}
	if got.ValidationStatus != "validated" {
		t.Fatalf("ValidationStatus = %q, want %q", got.ValidationStatus, "validated")
	}
	if got.PatchProposalID != "patch-123" {
		t.Fatalf("PatchProposalID = %q, want %q", got.PatchProposalID, "patch-123")
	}
	if got.PRNumber == nil || *got.PRNumber != prNumber {
		t.Fatalf("PRNumber = %#v, want %d", got.PRNumber, prNumber)
	}
	if got.PRURL != "https://github.com/example/repo/pull/123" {
		t.Fatalf("PRURL = %q, want preserved PR URL", got.PRURL)
	}
	if got.Summary != "later summary" {
		t.Fatalf("Summary = %q, want later summary to keep newer descriptive fields", got.Summary)
	}
}

func TestUpsertFindingAllowsPendingValidationToBecomeTerminal(t *testing.T) {
	for _, tc := range []struct {
		status         string
		validationJSON string
	}{
		{
			status:         "failed",
			validationJSON: `{"status":"failed","summary":"validation failed"}`,
		},
		{
			status:         "skipped",
			validationJSON: `{"status":"skipped","summary":"validation skipped"}`,
		},
	} {
		t.Run(tc.status, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()

			initial := &store.Finding{
				ID:               "fnd-" + tc.status,
				Namespace:        "ns1",
				RepositoryScan:   "repo1",
				ScanRunID:        "scan-1",
				Fingerprint:      "repo1:file.go:" + tc.status,
				Title:            "Finding",
				Summary:          "pending validation",
				Severity:         "high",
				Confidence:       "medium",
				ValidationStatus: "pending",
				State:            testStateOpen,
				ValidationJSON:   `{"status":"pending"}`,
			}
			if err := s.UpsertFinding(ctx, initial); err != nil {
				t.Fatalf("UpsertFinding(initial): %v", err)
			}

			terminal := *initial
			terminal.ScanRunID = testScanRunID2
			terminal.Summary = "terminal validation"
			terminal.ValidationStatus = tc.status
			terminal.ValidationJSON = tc.validationJSON
			if err := s.UpsertFinding(ctx, &terminal); err != nil {
				t.Fatalf("UpsertFinding(terminal): %v", err)
			}

			got, err := s.GetFinding(ctx, "ns1", initial.ID)
			if err != nil {
				t.Fatalf("GetFinding: %v", err)
			}
			if got.ValidationStatus != tc.status {
				t.Fatalf("ValidationStatus = %q, want %q", got.ValidationStatus, tc.status)
			}
			if got.ValidationJSON != tc.validationJSON {
				t.Fatalf("ValidationJSON = %q, want %q", got.ValidationJSON, tc.validationJSON)
			}
		})
	}
}

func TestUpsertFindingKeepsValidationJSONWhenValidatedStatusIsPreserved(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	initial := &store.Finding{
		ID:               "fnd-validated",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:validated",
		Title:            "Finding",
		Summary:          "validated",
		Severity:         "high",
		Confidence:       "medium",
		ValidationStatus: "validated",
		State:            testStateOpen,
		ValidationJSON:   `{"status":"validated","summary":"confirmed"}`,
	}
	if err := s.UpsertFinding(ctx, initial); err != nil {
		t.Fatalf("UpsertFinding(initial): %v", err)
	}

	lowerStatus := *initial
	lowerStatus.ScanRunID = testScanRunID2
	lowerStatus.ValidationStatus = "failed"
	lowerStatus.ValidationJSON = `{"status":"failed","summary":"later failure"}`
	if err := s.UpsertFinding(ctx, &lowerStatus); err != nil {
		t.Fatalf("UpsertFinding(lowerStatus): %v", err)
	}

	got, err := s.GetFinding(ctx, "ns1", initial.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.ValidationStatus != "validated" {
		t.Fatalf("ValidationStatus = %q, want validated", got.ValidationStatus)
	}
	if got.ValidationJSON != initial.ValidationJSON {
		t.Fatalf("ValidationJSON = %q, want %q", got.ValidationJSON, initial.ValidationJSON)
	}
}

func TestUpsertFindingAllowsPatchPendingToReturnOpen(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	initial := &store.Finding{
		ID:               "fnd-patch-pending",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:patch-pending",
		Title:            "Finding",
		Summary:          "patch pending",
		Severity:         "high",
		Confidence:       "medium",
		ValidationStatus: "validated",
		State:            "patch_pending",
		PatchProposalID:  "patch-123",
	}
	if err := s.UpsertFinding(ctx, initial); err != nil {
		t.Fatalf("UpsertFinding(initial): %v", err)
	}

	open := *initial
	open.ScanRunID = testScanRunID2
	open.State = testStateOpen
	if err := s.UpsertFinding(ctx, &open); err != nil {
		t.Fatalf("UpsertFinding(open): %v", err)
	}

	got, err := s.GetFinding(ctx, "ns1", initial.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.State != testStateOpen {
		t.Fatalf("State = %q, want open", got.State)
	}
}

func TestUpsertFindingPreservesFinalStatesOverOpen(t *testing.T) {
	for _, finalState := range []string{"fixed", "resolved", "dismissed", "suppressed", "false_positive"} {
		t.Run(finalState, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()

			initial := &store.Finding{
				ID:               "fnd-" + finalState,
				Namespace:        "ns1",
				RepositoryScan:   "repo1",
				ScanRunID:        "scan-1",
				Fingerprint:      "repo1:file.go:" + finalState,
				Title:            "Finding",
				Summary:          "final state",
				Severity:         "high",
				Confidence:       "medium",
				ValidationStatus: "validated",
				State:            finalState,
			}
			if err := s.UpsertFinding(ctx, initial); err != nil {
				t.Fatalf("UpsertFinding(initial): %v", err)
			}

			reopened := *initial
			reopened.ScanRunID = testScanRunID2
			reopened.State = testStateOpen
			if err := s.UpsertFinding(ctx, &reopened); err != nil {
				t.Fatalf("UpsertFinding(reopened): %v", err)
			}

			got, err := s.GetFinding(ctx, "ns1", initial.ID)
			if err != nil {
				t.Fatalf("GetFinding: %v", err)
			}
			if got.State != finalState {
				t.Fatalf("State = %q, want %q", got.State, finalState)
			}
		})
	}
}
