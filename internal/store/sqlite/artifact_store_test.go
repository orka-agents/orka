/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestSaveAndGetArtifact(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	data := []byte("hello artifact")
	contentType := "text/plain"

	if err := s.SaveArtifact(ctx, "ns1", "task1", "output.txt", contentType, data); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}

	got, gotCT, err := s.GetArtifact(ctx, "ns1", "task1", "output.txt")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("data = %q, want %q", got, data)
	}
	if gotCT != contentType {
		t.Errorf("contentType = %q, want %q", gotCT, contentType)
	}
}

func TestGetArtifactNotFound(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, _, err := s.GetArtifact(ctx, "ns1", "task1", "nonexistent.txt")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound, got %v", err)
	}
}

func TestListArtifacts(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	files := []struct {
		name        string
		contentType string
		data        []byte
	}{
		{"file1.txt", "text/plain", []byte("one")},
		{"file2.json", "application/json", []byte(`{"two":2}`)},
		{"file3.bin", "application/octet-stream", []byte{0x00, 0x01, 0x02}},
	}

	for _, f := range files {
		if err := s.SaveArtifact(ctx, "ns1", "task1", f.name, f.contentType, f.data); err != nil {
			t.Fatalf("SaveArtifact(%s): %v", f.name, err)
		}
	}

	list, err := s.ListArtifacts(ctx, "ns1", "task1")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d artifacts, want 3", len(list))
	}

	byName := map[string]store.ArtifactMetadata{}
	for _, m := range list {
		byName[m.Filename] = m
	}
	for _, f := range files {
		m, ok := byName[f.name]
		if !ok {
			t.Errorf("missing artifact %q in list", f.name)
			continue
		}
		if m.ContentType != f.contentType {
			t.Errorf("artifact %q: contentType = %q, want %q", f.name, m.ContentType, f.contentType)
		}
		if m.Size != int64(len(f.data)) {
			t.Errorf("artifact %q: size = %d, want %d", f.name, m.Size, len(f.data))
		}
	}
}

func TestListArtifactsEmpty(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	list, err := s.ListArtifacts(ctx, "ns1", "no-artifacts-task")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if list == nil {
		// Implementation returns nil for empty; accept nil or empty slice.
		list = []store.ArtifactMetadata{}
	}
	if len(list) != 0 {
		t.Errorf("got %d artifacts, want 0", len(list))
	}
}

func TestDeleteArtifacts(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.SaveArtifact(ctx, "ns1", "task1", "a.txt", "text/plain", []byte("aaa")); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	if err := s.SaveArtifact(ctx, "ns1", "task1", "b.txt", "text/plain", []byte("bbb")); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}

	if err := s.DeleteArtifacts(ctx, "ns1", "task1"); err != nil {
		t.Fatalf("DeleteArtifacts: %v", err)
	}

	// Get should return ErrNotFound
	_, _, err := s.GetArtifact(ctx, "ns1", "task1", "a.txt")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetArtifact after delete: expected ErrNotFound, got %v", err)
	}

	// List should return empty
	list, err := s.ListArtifacts(ctx, "ns1", "task1")
	if err != nil {
		t.Fatalf("ListArtifacts after delete: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("got %d artifacts after delete, want 0", len(list))
	}
}

func TestSaveArtifactOverwrite(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.SaveArtifact(ctx, "ns1", "task1", "data.bin", "application/octet-stream", []byte("original")); err != nil {
		t.Fatalf("SaveArtifact (original): %v", err)
	}

	newData := []byte("updated content")
	newCT := "text/plain"
	if err := s.SaveArtifact(ctx, "ns1", "task1", "data.bin", newCT, newData); err != nil {
		t.Fatalf("SaveArtifact (overwrite): %v", err)
	}

	got, gotCT, err := s.GetArtifact(ctx, "ns1", "task1", "data.bin")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Errorf("data = %q, want %q", got, newData)
	}
	if gotCT != newCT {
		t.Errorf("contentType = %q, want %q", gotCT, newCT)
	}

	// Should still be only one artifact in the list
	list, err := s.ListArtifacts(ctx, "ns1", "task1")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("got %d artifacts after overwrite, want 1", len(list))
	}
}

func TestSaveArtifactSizeLimit(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Exactly at the limit (10MB) should succeed
	atLimit := make([]byte, 10<<20)
	if err := s.SaveArtifact(ctx, "ns1", "task1", "big.bin", "application/octet-stream", atLimit); err != nil {
		t.Fatalf("SaveArtifact at 10MB limit: %v", err)
	}

	// Over the limit (10MB + 1) should fail
	overLimit := make([]byte, 10<<20+1)
	err := s.SaveArtifact(ctx, "ns1", "task1", "toobig.bin", "application/octet-stream", overLimit)
	if err == nil {
		t.Fatal("expected error for artifact exceeding 10MB, got nil")
	}

	// The over-limit artifact should not be stored
	_, _, err = s.GetArtifact(ctx, "ns1", "task1", "toobig.bin")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("over-limit artifact should not exist, got err: %v", err)
	}
}

func TestArtifactIsolation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.SaveArtifact(ctx, "ns1", "task-a", "shared.txt", "text/plain", []byte("from task-a")); err != nil {
		t.Fatalf("SaveArtifact task-a: %v", err)
	}
	if err := s.SaveArtifact(ctx, "ns1", "task-b", "shared.txt", "text/plain", []byte("from task-b")); err != nil {
		t.Fatalf("SaveArtifact task-b: %v", err)
	}

	// task-a should see its own data
	data, _, err := s.GetArtifact(ctx, "ns1", "task-a", "shared.txt")
	if err != nil {
		t.Fatalf("GetArtifact task-a: %v", err)
	}
	if string(data) != "from task-a" {
		t.Errorf("task-a data = %q, want %q", data, "from task-a")
	}

	// task-b should see its own data
	data, _, err = s.GetArtifact(ctx, "ns1", "task-b", "shared.txt")
	if err != nil {
		t.Fatalf("GetArtifact task-b: %v", err)
	}
	if string(data) != "from task-b" {
		t.Errorf("task-b data = %q, want %q", data, "from task-b")
	}

	// List for task-a should only have 1 artifact
	listA, err := s.ListArtifacts(ctx, "ns1", "task-a")
	if err != nil {
		t.Fatalf("ListArtifacts task-a: %v", err)
	}
	if len(listA) != 1 {
		t.Errorf("task-a: got %d artifacts, want 1", len(listA))
	}

	// Deleting task-a's artifacts should not affect task-b
	if err := s.DeleteArtifacts(ctx, "ns1", "task-a"); err != nil {
		t.Fatalf("DeleteArtifacts task-a: %v", err)
	}
	data, _, err = s.GetArtifact(ctx, "ns1", "task-b", "shared.txt")
	if err != nil {
		t.Fatalf("GetArtifact task-b after deleting task-a: %v", err)
	}
	if string(data) != "from task-b" {
		t.Errorf("task-b data after delete = %q, want %q", data, "from task-b")
	}
}
