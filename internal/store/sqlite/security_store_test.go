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
