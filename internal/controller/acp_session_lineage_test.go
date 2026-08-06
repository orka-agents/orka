/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func newACPLineageTestContinuity(t *testing.T, s *sqlite.Store) *ACPSessionContinuity {
	t.Helper()
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: s, Transcripts: s, Publications: s, BranchClaims: s,
		Lineages:      s,
		NewSessionUID: func() (string, error) { return "acp-lineage-session-uid", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return continuity
}

func acpLineageLeaseRequest(control *store.SessionControl, fence store.ControllerEpochFence, taskUID, runtimeIdentity string) ACPAcquireSessionLeaseRequest {
	return ACPAcquireSessionLeaseRequest{
		Session: *control, Fence: fence, TaskUID: taskUID, Attempt: 1, PromptID: "prompt-" + taskUID,
		PromptRequestDigest: acpSessionTestDigest("request-" + taskUID),
		AcquiredAt:          time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		NamespaceUID:        "ns-uid-1",
		RuntimeIdentity:     runtimeIdentity,
		ConfigDigest:        acpSessionTestDigest("profile-1"),
	}
}

func TestAcquireMutationLeaseClaimsSessionLineageAtomically(t *testing.T) {
	ctx := context.Background()
	s, fence, cleanup := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "lineage.db"))
	defer cleanup()
	continuity := newACPLineageTestContinuity(t, s)
	control := ensureACPSessionForTest(t, continuity, fence, "lineage-session")

	lease, err := continuity.AcquireMutationLease(ctx, acpLineageLeaseRequest(control, fence, "task-1", "codex"))
	if err != nil {
		t.Fatalf("acquire with lineage: %v", err)
	}

	lineage, err := s.GetSessionLineage(ctx, "ns", "lineage-session")
	if err != nil {
		t.Fatalf("get lineage: %v", err)
	}
	if lineage.ContractVersion != string(corev1alpha1.AgentRuntimeContractHarnessV2) ||
		lineage.RuntimeIdentity != "codex" || lineage.SessionUID != control.SessionUID ||
		lineage.Provenance != store.SessionLineageFirstUse || lineage.NamespaceUID != "ns-uid-1" {
		t.Fatalf("unexpected lineage: %+v", lineage)
	}

	// Release, then continuation on the same runtime verifies the lineage.
	if _, err := continuity.ReleaseMutationLease(ctx, ACPReleaseSessionLeaseRequest{Lease: *lease, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.GetSessionControl(ctx, "ns", "lineage-session")
	if err != nil {
		t.Fatal(err)
	}
	nextLease, err := continuity.AcquireMutationLease(ctx, acpLineageLeaseRequest(refreshed, fence, "task-2", "codex"))
	if err != nil {
		t.Fatalf("continuation acquire: %v", err)
	}
	if _, err := continuity.ReleaseMutationLease(ctx, ACPReleaseSessionLeaseRequest{Lease: *nextLease, Fence: fence}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireMutationLeaseRejectsLineageConflictAndReleasesLease(t *testing.T) {
	ctx := context.Background()
	s, fence, cleanup := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "lineage-conflict.db"))
	defer cleanup()
	continuity := newACPLineageTestContinuity(t, s)
	control := ensureACPSessionForTest(t, continuity, fence, "conflict-session")

	lease, err := continuity.AcquireMutationLease(ctx, acpLineageLeaseRequest(control, fence, "task-1", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := continuity.ReleaseMutationLease(ctx, ACPReleaseSessionLeaseRequest{Lease: *lease, Fence: fence}); err != nil {
		t.Fatal(err)
	}

	// A different runtime identity can never silently continue this Session.
	refreshed, err := s.GetSessionControl(ctx, "ns", "conflict-session")
	if err != nil {
		t.Fatal(err)
	}
	_, err = continuity.AcquireMutationLease(ctx, acpLineageLeaseRequest(refreshed, fence, "task-2", "opencode"))
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected lineage conflict, got %v", err)
	}

	// The failed acquisition released its lease so the Session is not stuck.
	after, err := s.GetSessionControl(ctx, "ns", "conflict-session")
	if err != nil {
		t.Fatal(err)
	}
	if after.Lease != nil {
		t.Fatalf("lease must be released after a lineage conflict: %+v", after.Lease)
	}
	if after.Availability != store.SessionAvailable {
		t.Fatalf("session availability = %s", after.Availability)
	}

	// The original lineage is untouched.
	lineage, err := s.GetSessionLineage(ctx, "ns", "conflict-session")
	if err != nil || lineage.RuntimeIdentity != "codex" {
		t.Fatalf("lineage after conflict = %+v, %v", lineage, err)
	}
}

func TestAcquireMutationLeaseRequiresLineageIdentityWhenRecording(t *testing.T) {
	ctx := context.Background()
	s, fence, cleanup := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "lineage-required.db"))
	defer cleanup()
	continuity := newACPLineageTestContinuity(t, s)
	control := ensureACPSessionForTest(t, continuity, fence, "identity-required")

	request := acpLineageLeaseRequest(control, fence, "task-1", "")
	if _, err := continuity.AcquireMutationLease(ctx, request); err == nil {
		t.Fatal("missing runtime identity must fail lineage recording closed")
	}
	after, err := s.GetSessionControl(ctx, "ns", "identity-required")
	if err != nil {
		t.Fatal(err)
	}
	if after.Lease != nil {
		t.Fatal("lease must be released when lineage identity is missing")
	}
}

func TestAcquireMutationLeaseAdoptsPreexistingTranscriptAsV2(t *testing.T) {
	ctx := context.Background()
	s, fence, cleanup := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "lineage-adopt.db"))
	defer cleanup()
	continuity := newACPLineageTestContinuity(t, s)
	control := ensureACPSessionForTest(t, continuity, fence, "adopted-session")

	// Simulate a pre-upgrade v2 session: transcript exists, no lineage row.
	if err := s.AppendMessages(ctx, "ns", "adopted-session", []store.SessionMessage{
		{Role: "user", Content: "earlier prompt"},
		{Role: "assistant", Content: "earlier answer"},
	}); err != nil {
		t.Fatal(err)
	}

	lease, err := continuity.AcquireMutationLease(ctx, acpLineageLeaseRequest(control, fence, "task-1", "claude"))
	if err != nil {
		t.Fatalf("adoption acquire: %v", err)
	}
	defer func() {
		_, _ = continuity.ReleaseMutationLease(ctx, ACPReleaseSessionLeaseRequest{Lease: *lease, Fence: fence})
	}()

	lineage, err := s.GetSessionLineage(ctx, "ns", "adopted-session")
	if err != nil {
		t.Fatal(err)
	}
	if lineage.Provenance != store.SessionLineageLegacyAdopted || lineage.RuntimeIdentity != "claude" {
		t.Fatalf("adopted lineage = %+v", lineage)
	}
}
