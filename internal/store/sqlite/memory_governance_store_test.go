package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	testMemoryDispatcher   = "dispatcher"
	testMetadataNewTag     = "new"
	testMetadataNewAgent   = "new-agent"
	testMetadataNewSession = "new-session"
	testMetadataAbandoned  = "abandoned"
	testMetadataImport     = "import"
)

func TestMemoryOperationPayloadCapIsImmutable512KiB(t *testing.T) {
	if store.MaxMemoryOperationPayloadBytes != 512<<10 {
		t.Fatalf("MaxMemoryOperationPayloadBytes = %d, want %d", store.MaxMemoryOperationPayloadBytes, 512<<10)
	}
	if store.MaxMemoryOperationPayloadBytes >= protocol.MaxHTTPBodyBytes {
		t.Fatalf("durable cap %d must remain below wire cap %d", store.MaxMemoryOperationPayloadBytes, protocol.MaxHTTPBodyBytes)
	}
}

func TestMemoryGovernanceMigrationIdempotent(t *testing.T) {
	s := setupTestStore(t)
	for range 2 {
		if err := migrate(s.db); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}

	for _, table := range []string{
		"memory_governance_migrations", "legacy_memory_archive", "memory_legacy_fences", "controller_feature_heartbeats",
		"memory_backend_bindings", "remote_memory_catalog", "remote_memory_generation_watermarks", "memory_operations",
		"memory_idempotency", "memory_audit",
	} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("lookup table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
	for _, column := range []string{"apply_operation_id", "application_abandoned_at"} {
		var count int
		rows, err := s.db.Query(`PRAGMA table_info(memory_proposals)`)
		if err != nil {
			t.Fatalf("PRAGMA memory_proposals: %v", err)
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				rows.Close() //nolint:errcheck
				t.Fatalf("scan memory_proposals column: %v", err)
			}
			if name == column {
				count++
			}
		}
		_ = rows.Close()
		if count != 1 {
			t.Fatalf("memory_proposals column %s count = %d, want 1", column, count)
		}
	}
}

func TestMemoryGovernanceMigrationTightensOperationPayloadConstraint(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-payload-migration", "team-payload-migration-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "payload-migration", "payload-migration-request", []byte(`{"content":"v1"}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, legacyLimit := range []int{1 << 20, 2 << 20} {
		rebuildMemoryOperationsPayloadLimitForTest(t, s.db, legacyLimit)
		if err := migrate(s.db); err != nil {
			t.Fatalf("migrate legacy payload constraint %d: %v", legacyLimit, err)
		}
	}
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
	if err != nil || operation.ID != admitted.Operation.ID {
		t.Fatalf("preserved operation = %+v, %v", operation, err)
	}
	var schema string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'memory_operations'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(schema), ""))
	if !strings.Contains(normalized, fmt.Sprintf("check(length(payload)<=%d)", store.MaxMemoryOperationPayloadBytes)) {
		t.Fatalf("migrated memory_operations schema did not tighten payload constraint: %s", schema)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("second migrate after payload rebuild: %v", err)
	}
}

func TestRemoteMemoryAdmissionRetainsMaxEscapedOMSRequest(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 14, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-max-payload", "team-max-payload-uid", now)
	omsBinding := protocol.Binding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: uint64(binding.AuthorityEpoch), RoutingEpoch: uint64(binding.RoutingEpoch),
		TenantID:  protocol.DeriveTenantID(binding.ClusterID, binding.NamespaceUID),
		StoreUUID: "11111111-1111-4111-8111-111111111111",
	}
	mutation := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version, OperationID: "mop-max-escaped-content", Binding: omsBinding,
		MemoryID: "mem-max-escaped-content", Kind: protocol.MutationKindCreate, Generation: 1,
		State: &protocol.MutationState{
			Content: strings.Repeat("\n", protocol.MaxContentBytes),
			Tags:    []string{}, Metadata: map[string]string{},
		},
	}
	if err := protocol.PrepareMutation(&mutation); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= store.MaxMemoryOperationPayloadBytes || len(payload) > protocol.MaxHTTPBodyBytes {
		t.Fatalf("escaped max-content payload bytes = %d, want > durable cap %d and <= wire cap %d",
			len(payload), store.MaxMemoryOperationPayloadBytes, protocol.MaxHTTPBodyBytes)
	}
	canonicalPayload, err := protocol.EncodeJSON(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonicalPayload) <= store.MaxMemoryOperationPayloadBytes || len(canonicalPayload) > protocol.MaxHTTPBodyBytes {
		t.Fatalf("canonical payload bytes = %d, want > durable stored cap %d and <= wire cap %d",
			len(canonicalPayload), store.MaxMemoryOperationPayloadBytes, protocol.MaxHTTPBodyBytes)
	}
	admission := store.RemoteMemoryCreateAdmission{
		Mutation: newMemoryMutationFromEnvelope(binding, now.Add(time.Second), mutation.MemoryID,
			"max-escaped-content", "max-escaped-request", mutation, payload),
		Memory: store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{}},
	}
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate(max escaped content): %v", err)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: "max-payload-dispatcher", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimed.Payload, canonicalPayload) {
		t.Fatalf("claimed payload was not canonicalized: got %d bytes, want %d", len(claimed.Payload), len(canonicalPayload))
	}
	if admitted.Operation.PayloadBytes != len(canonicalPayload) || claimed.Operation.PayloadBytes != len(canonicalPayload) {
		t.Fatalf("logical payload bytes admission=%d claim=%d want=%d",
			admitted.Operation.PayloadBytes, claimed.Operation.PayloadBytes, len(canonicalPayload))
	}
	var storedBytes int
	if err := s.db.QueryRow(`SELECT length(payload) FROM memory_operations WHERE id = ?`, admitted.Operation.ID).Scan(&storedBytes); err != nil {
		t.Fatal(err)
	}
	if storedBytes > store.MaxMemoryOperationPayloadBytes || storedBytes >= len(canonicalPayload) {
		t.Fatalf("stored payload bytes = %d, canonical = %d", storedBytes, len(canonicalPayload))
	}
}

func rebuildMemoryOperationsPayloadLimitForTest(t *testing.T, db *sql.DB, limit int) {
	t.Helper()
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'memory_operations'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	open := strings.Index(schema, "(")
	if open < 0 {
		t.Fatalf("memory_operations schema is malformed: %s", schema)
	}
	rebuildSchema := "CREATE TABLE memory_operations_rebuild " + schema[open:]
	rebuildSchema = strings.Replace(rebuildSchema, fmt.Sprint(store.MaxMemoryOperationPayloadBytes), fmt.Sprint(limit), 1)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, statement := range []string{
		`DROP TABLE IF EXISTS memory_operations_rebuild`, rebuildSchema,
		`INSERT INTO memory_operations_rebuild SELECT * FROM memory_operations`,
		`DROP TABLE memory_operations`,
		`ALTER TABLE memory_operations_rebuild RENAME TO memory_operations`,
		`CREATE UNIQUE INDEX idx_memory_operations_unresolved_memory ON memory_operations(namespace_uid, memory_id)
			WHERE state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		`CREATE UNIQUE INDEX idx_memory_operations_operation_idempotency
			ON memory_operations(namespace_uid, operation_idempotency_key)`,
		`CREATE INDEX idx_memory_operations_claim
			ON memory_operations(namespace_uid, backend_uid, authority_epoch, routing_epoch, state, next_retry_at, sequence)`,
		`CREATE INDEX idx_memory_operations_proposal ON memory_operations(namespace_uid, proposal_id, sequence DESC)
			WHERE proposal_id <> ''`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatalf("rebuild test memory_operations schema: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendMemoryAuditIsInsertOnly(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	audit := store.MemoryAuditRecord{
		ID: "audit-intent-1", Namespace: "team-a", NamespaceUID: "team-a-uid",
		Actor: "operator", Action: "backend.decommission.intent", Reason: "planned migration",
		AuthorityEpoch: 1, RoutingEpoch: 2, RequestID: "request-1",
		CreatedAt: time.Date(2026, 7, 28, 11, 0, 0, 0, time.UTC),
	}
	if err := s.AppendMemoryAudit(ctx, audit); err != nil {
		t.Fatalf("AppendMemoryAudit: %v", err)
	}
	if err := s.AppendMemoryAudit(ctx, audit); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate AppendMemoryAudit error = %v, want ErrConflict", err)
	}
	if _, err := s.db.Exec(`UPDATE memory_audit SET reason = 'tampered' WHERE id = ?`, audit.ID); err == nil {
		t.Fatalf("memory_audit update unexpectedly succeeded")
	}
	listed, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: audit.NamespaceUID})
	if err != nil || len(listed) != 1 || listed[0].Reason != audit.Reason {
		t.Fatalf("ListMemoryAudit = %+v, %v", listed, err)
	}
}

func TestMemoryGovernanceMigrationUpgradesLegacyProposalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY, namespace TEXT NOT NULL, session_name TEXT NOT NULL DEFAULT '',
		agent_name TEXT NOT NULL DEFAULT '', task_name TEXT NOT NULL DEFAULT '', parent_task TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '', source_proposal_id TEXT NOT NULL DEFAULT '', content TEXT NOT NULL,
		tags_json TEXT NOT NULL DEFAULT '[]', disabled BOOLEAN NOT NULL DEFAULT FALSE,
		deleted BOOLEAN NOT NULL DEFAULT FALSE, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, last_recalled_at TIMESTAMP,
		recalled_count INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create legacy memories: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE memory_proposals (
		id TEXT PRIMARY KEY, namespace TEXT NOT NULL, task_name TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL, skill_name TEXT NOT NULL DEFAULT '', title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '', patch TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending',
		reviewer TEXT NOT NULL DEFAULT '', review_note TEXT NOT NULL DEFAULT '', applied_memory_id TEXT NOT NULL DEFAULT '',
		applied_by TEXT NOT NULL DEFAULT '', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, reviewed_at TIMESTAMP, applied_at TIMESTAMP
	)`); err != nil {
		t.Fatalf("create legacy memory_proposals: %v", err)
	}

	appliedAt := time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO memories
		(id, namespace, source, source_proposal_id, content, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"mem-legacy-reviewed", "team-legacy", memorySourceProposal, "proposal-legacy-reviewed",
		"reviewed legacy content", appliedAt.Add(-time.Hour), appliedAt); err != nil {
		t.Fatalf("insert legacy reviewed memory: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_proposals
		(id, namespace, type, title, content, status, reviewer, applied_memory_id, applied_by,
		 created_at, updated_at, reviewed_at, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"proposal-legacy-reviewed", "team-legacy", proposalTypeMemory, "legacy reviewed proposal",
		"reviewed legacy content", proposalStatusApplied, "reviewer", "mem-legacy-reviewed", "applier",
		appliedAt.Add(-2*time.Hour), appliedAt, appliedAt.Add(-time.Hour), appliedAt); err != nil {
		t.Fatalf("insert legacy reviewed proposal: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memories
		(id, namespace, source, source_proposal_id, content) VALUES (?, ?, ?, ?, ?)`,
		"mem-legacy-mismatch", "team-legacy", memorySourceProposal, "proposal-legacy-mismatch",
		"edited after review"); err != nil {
		t.Fatalf("insert legacy mismatched memory: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_proposals
		(id, namespace, type, title, content, status, reviewer, applied_memory_id, applied_by, reviewed_at, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"proposal-legacy-mismatch", "team-legacy", proposalTypeMemory, "legacy mismatched proposal",
		"original reviewed content", proposalStatusApplied, "reviewer", "mem-legacy-mismatch", "applier",
		appliedAt.Add(-time.Hour), appliedAt); err != nil {
		t.Fatalf("insert legacy mismatched proposal: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate legacy schema: %v", err)
	}
	columns := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(memory_proposals)`)
	if err != nil {
		t.Fatalf("PRAGMA memory_proposals: %v", err)
	}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close() //nolint:errcheck
			t.Fatalf("scan memory_proposals: %v", err)
		}
		columns[name] = true
	}
	_ = rows.Close()
	for _, column := range []string{"apply_operation_id", "application_abandoned_by", "application_abandoned_reason", "application_abandoned_at"} {
		if !columns[column] {
			t.Fatalf("missing migrated memory_proposals column %s", column)
		}
	}

	var operationID string
	if err := db.QueryRow(`SELECT apply_operation_id FROM memory_proposals WHERE id = ?`,
		"proposal-legacy-reviewed").Scan(&operationID); err != nil {
		t.Fatalf("read backfilled operation id: %v", err)
	}
	if operationID != legacyAppliedProposalBackfillPrefix+"proposal-legacy-reviewed" {
		t.Fatalf("backfilled operation id = %q", operationID)
	}
	if err := db.QueryRow(`SELECT apply_operation_id FROM memory_proposals WHERE id = ?`,
		"proposal-legacy-mismatch").Scan(&operationID); err != nil {
		t.Fatalf("read mismatched operation id: %v", err)
	}
	if operationID != "" {
		t.Fatalf("mismatched proposal operation id = %q, want empty", operationID)
	}

	storeAfterUpgrade := NewStore(db, path)
	reviewed, err := storeAfterUpgrade.GetMemory(context.Background(), "team-legacy", "mem-legacy-reviewed")
	if err != nil {
		t.Fatalf("GetMemory after legacy upgrade: %v", err)
	}
	if reviewed.Trust != store.MemoryTrustReviewed {
		t.Fatalf("legacy upgraded memory trust = %q, want reviewed", reviewed.Trust)
	}
	mismatched, err := storeAfterUpgrade.GetMemory(context.Background(), "team-legacy", "mem-legacy-mismatch")
	if err != nil {
		t.Fatalf("GetMemory mismatched after legacy upgrade: %v", err)
	}
	if mismatched.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("legacy mismatched memory trust = %q, want untrusted", mismatched.Trust)
	}

	if _, err := db.Exec(`INSERT INTO memories
		(id, namespace, source, source_proposal_id, content) VALUES (?, ?, ?, ?, ?)`,
		"mem-post-migration-spoof", "team-legacy", memorySourceProposal, "proposal-post-migration-spoof",
		"post migration spoof"); err != nil {
		t.Fatalf("insert post-migration spoofed memory: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_proposals
		(id, namespace, type, title, content, status, reviewer, applied_memory_id, apply_operation_id,
		 applied_by, reviewed_at, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		"proposal-post-migration-spoof", "team-legacy", proposalTypeMemory, "post-migration spoof",
		"post migration spoof", proposalStatusApplied, "reviewer", "mem-post-migration-spoof", "applier",
		appliedAt.Add(-time.Hour), appliedAt); err != nil {
		t.Fatalf("insert post-migration spoofed proposal: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate legacy schema: %v", err)
	}
	spoofed, err := storeAfterUpgrade.GetMemory(context.Background(), "team-legacy", "mem-post-migration-spoof")
	if err != nil {
		t.Fatalf("GetMemory post-migration spoof: %v", err)
	}
	if spoofed.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("post-migration spoof trust = %q, want untrusted", spoofed.Trust)
	}
}

func TestMemoryBackendActivationFencesLegacyAndRestorePreview(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	legacy := &store.Memory{Namespace: "team-a", Content: "legacy content", Source: "manual"}
	if err := s.CreateMemory(ctx, legacy); err != nil {
		t.Fatalf("CreateMemory legacy: %v", err)
	}
	if err := s.CreateMemory(ctx, &store.Memory{Namespace: "other", Content: "unfenced", Source: "manual"}); err != nil {
		t.Fatalf("CreateMemory other: %v", err)
	}
	binding := activateMemoryBackendForTest(t, s, "team-a", "team-a-uid", now)
	bindings, err := s.ListMemoryBackendBindings(ctx, store.MemoryBackendBindingFilter{
		Modes:  []store.MemoryBackendMode{store.MemoryBackendModeRemote},
		States: []store.MemoryBackendBindingState{store.MemoryBackendBindingAccepting},
	})
	if err != nil || len(bindings) != 1 || bindings[0].NamespaceUID != binding.NamespaceUID {
		t.Fatalf("ListMemoryBackendBindings = %+v, %v", bindings, err)
	}

	if _, err := s.GetMemory(ctx, "team-a", legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetMemory archived error = %v, want ErrNotFound", err)
	}
	if err := s.CreateMemory(ctx, &store.Memory{Namespace: "team-a", Content: "must be fenced"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateMemory fenced error = %v, want ErrConflict", err)
	}
	if _, err := s.db.Exec(`INSERT INTO memories(id, namespace, content) VALUES ('direct-fenced', 'team-a', 'blocked')`); err == nil || !strings.Contains(err.Error(), legacyMemoryFenceMessage) {
		t.Fatalf("direct fenced insert error = %v, want trigger rejection", err)
	}
	if err := s.CreateMemory(ctx, &store.Memory{Namespace: "other", Content: "still works"}); err != nil {
		t.Fatalf("CreateMemory unfenced after activation: %v", err)
	}

	transitioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "clean decommission", Now: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("TransitionMemoryBackendBinding: %v", err)
	}
	preview, err := s.PreviewLegacyMemoryRestore(ctx, binding.NamespaceUID, binding.BackendUID)
	if err != nil {
		t.Fatalf("PreviewLegacyMemoryRestore: %v", err)
	}
	if !preview.Restorable || preview.ArchivedMemories != 1 || preview.ConflictingMemories != 0 || preview.UnresolvedOperations != 0 {
		t.Fatalf("unexpected restore preview: %+v", preview)
	}
	if _, err := s.GetMemory(ctx, "team-a", legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("preview mutated legacy rows: %v", err)
	}

	restored, err := s.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedAuthorityEpoch: preview.AuthorityEpoch, ExpectedRoutingEpoch: preview.RoutingEpoch,
		ExpectedTenantID: preview.TenantID, ExpectedStoreName: preview.StoreName,
		ExpectedStoreUUID: preview.StoreUUID, PreviewDigest: preview.PreviewDigest,
		Actor: "operator", Reason: "return to sqlite", Now: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("RestoreLegacyMemories: %v", err)
	}
	if restored.RestoredMemories != 1 || restored.Binding.State != store.MemoryBackendBindingLegacy ||
		restored.Binding.RoutingEpoch != transitioned.RoutingEpoch+1 ||
		!restored.Binding.UpdatedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("unexpected restore result: %+v", restored)
	}
	got, err := s.GetMemory(ctx, "team-a", legacy.ID)
	if err != nil {
		t.Fatalf("GetMemory restored: %v", err)
	}
	if got.Content != legacy.Content || got.Generation != 1 || !got.ContentAvailable {
		t.Fatalf("restored memory = %+v", got)
	}
	if err := s.CreateMemory(ctx, &store.Memory{Namespace: "team-a", Content: "legacy resumed"}); err != nil {
		t.Fatalf("CreateMemory after restore: %v", err)
	}
}

func TestLegacyRestoreBlocksRemoteDeletedArchivedIDs(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	legacy := &store.Memory{Namespace: "team-restore-delete", Content: "must stay deleted", Source: "manual"}
	if err := s.CreateMemory(ctx, legacy); err != nil {
		t.Fatalf("CreateMemory legacy: %v", err)
	}
	binding := activateMemoryBackendForTest(t, s, legacy.Namespace, "team-restore-delete-uid", now)
	create := newRemoteCreateAdmission(binding, now.Add(time.Second), "restore-delete-create", "restore-delete-create-request", []byte("remote"))
	create.Mutation = newCanonicalMemoryMutation(binding, now.Add(time.Second), legacy.ID, "restore-delete-create",
		"restore-delete-create-request", store.MemoryOperationCreate, 1, 0, "", "remote")
	created, err := s.AdmitRemoteMemoryCreate(ctx, create)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	completeOperationForTest(t, s, binding, created.Operation, now.Add(2*time.Second), "backend-v1", "remote-restore-delete")
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(6*time.Second), legacy.ID, "restore-delete", "restore-delete-request",
			store.MemoryOperationDelete, 2, 1, "backend-v1", ""),
		ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
	})
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryDelete: %v", err)
	}
	completeOperationForTest(t, s, binding, deleted.Operation, now.Add(7*time.Second), "backend-v2", "remote-restore-delete")
	deleteOperation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, deleted.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	retentionNow := now.Add(40 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-restore-delete", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: deleteOperation.Sequence, CheckpointDigest: protocol.ContentDigest("restore delete checkpoint"),
		Actor: "operator", Reason: "verified deletion checkpoint", VerifiedAt: retentionNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: deleteOperation.Sequence,
		Before: now.Add(35 * 24 * time.Hour), PurgePayloads: true, PurgeReceipts: true,
		PurgeExpiredIdempotency: true, PurgeTombstones: true,
		Actor: "operator", Reason: "retention after remote deletion", Now: retentionNow,
	})
	if err != nil || purged.TombstonesPurged != 1 {
		t.Fatalf("PurgeMemoryGovernance = %+v, %v", purged, err)
	}
	transitioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "test retained deletion restore barrier", Now: retentionNow.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("TransitionMemoryBackendBinding: %v", err)
	}

	preview, err := s.PreviewLegacyMemoryRestore(ctx, binding.NamespaceUID, binding.BackendUID)
	if err != nil {
		t.Fatalf("PreviewLegacyMemoryRestore: %v", err)
	}
	if !preview.Restorable || preview.ArchivedMemories != 0 || preview.RemoteDeletedMemories != 0 {
		t.Fatalf("purged deletion did not suppress archived resurrection: %+v", preview)
	}
	if _, err := s.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedAuthorityEpoch: preview.AuthorityEpoch, ExpectedRoutingEpoch: preview.RoutingEpoch,
		ExpectedTenantID: preview.TenantID, ExpectedStoreName: preview.StoreName,
		ExpectedStoreUUID: preview.StoreUUID, PreviewDigest: preview.PreviewDigest,
		Actor: "operator", Reason: "restore without deleted archive", Now: retentionNow.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("RestoreLegacyMemories: %v", err)
	}
	if transitioned.State != store.MemoryBackendBindingDecommissioned {
		t.Fatalf("transitioned binding = %+v", transitioned)
	}
	if _, err := s.GetMemory(ctx, legacy.Namespace, legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remote-deleted legacy memory was resurrected: %v", err)
	}

}

func TestTombstonePurgePreservesArchivedLegacyMemoryForNeverAppliedCreate(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
	legacy := &store.Memory{Namespace: "team-restore-never-applied", Content: "legacy authority", Source: "manual"}
	if err := s.CreateMemory(ctx, legacy); err != nil {
		t.Fatalf("CreateMemory legacy: %v", err)
	}
	binding := activateMemoryBackendForTest(t, s, legacy.Namespace, "team-restore-never-applied-uid", now)
	create := newRemoteCreateAdmission(binding, now.Add(time.Second), "restore-never-applied", "restore-never-applied-request", []byte("replacement"))
	create.Mutation = newCanonicalMemoryMutation(binding, now.Add(time.Second), legacy.ID, "restore-never-applied",
		"restore-never-applied-request", store.MemoryOperationCreate, 1, 0, "", "replacement")
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, create)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "PROVIDER_NEVER_APPLIED", ErrorMessage: "create was not applied",
		Now: now.Add(4 * time.Second),
	})
	if err != nil || dead.State != store.MemoryOperationDeadLettered {
		t.Fatalf("dead letter = %+v, %v", dead, err)
	}
	abandoned, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "provider proved create was never applied", ProviderNeverApplied: true,
		Fenced: true, Now: now.Add(5 * time.Second),
	})
	if err != nil || abandoned.State != store.MemoryOperationAbandoned {
		t.Fatalf("AbandonMemoryOperation = %+v, %v", abandoned, err)
	}

	retentionNow := now.Add(40 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-restore-never-applied", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: abandoned.Sequence, CheckpointDigest: protocol.ContentDigest("restore never-applied checkpoint"),
		Actor: "operator", Reason: "verified never-applied checkpoint", VerifiedAt: retentionNow,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint: %v", err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: abandoned.Sequence,
		Before: now.Add(35 * 24 * time.Hour), PurgePayloads: true, PurgeReceipts: true,
		PurgeExpiredIdempotency: true, PurgeTombstones: true,
		Actor: "operator", Reason: "retention after never-applied create", Now: retentionNow,
	})
	if err != nil || purged.TombstonesPurged != 1 {
		t.Fatalf("PurgeMemoryGovernance = %+v, %v", purged, err)
	}
	if _, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphaned catalog was not purged: %v", err)
	}
	var archived int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_memory_archive WHERE namespace_uid = ? AND id = ?`,
		binding.NamespaceUID, legacy.ID).Scan(&archived); err != nil || archived != 1 {
		t.Fatalf("legacy archive count = %d, %v; want 1", archived, err)
	}

	if _, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "restore preserved legacy authority", Now: retentionNow.Add(time.Second),
	}); err != nil {
		t.Fatalf("TransitionMemoryBackendBinding: %v", err)
	}
	preview, err := s.PreviewLegacyMemoryRestore(ctx, binding.NamespaceUID, binding.BackendUID)
	if err != nil {
		t.Fatalf("PreviewLegacyMemoryRestore: %v", err)
	}
	if !preview.Restorable || preview.ArchivedMemories != 1 || preview.RemoteDeletedMemories != 0 {
		t.Fatalf("unexpected restore preview: %+v", preview)
	}
	if _, err := s.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedAuthorityEpoch: preview.AuthorityEpoch, ExpectedRoutingEpoch: preview.RoutingEpoch,
		ExpectedTenantID: preview.TenantID, ExpectedStoreName: preview.StoreName,
		ExpectedStoreUUID: preview.StoreUUID, PreviewDigest: preview.PreviewDigest,
		Actor: "operator", Reason: "restore never-replaced legacy memory", Now: retentionNow.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("RestoreLegacyMemories: %v", err)
	}
	restored, err := s.GetMemory(ctx, legacy.Namespace, legacy.ID)
	if err != nil {
		t.Fatalf("GetMemory restored: %v", err)
	}
	if restored.Content != legacy.Content {
		t.Fatalf("restored content = %q, want %q", restored.Content, legacy.Content)
	}
}

func TestMemoryBackendSameUIDReactivationRequiresNamespaceNameMatch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-reactivation", "team-reactivation-uid", now)
	decommissioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "prepare legacy restore", Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("TransitionMemoryBackendBinding: %v", err)
	}
	if _, err := s.PreviewLegacyMemoryRestore(ctx, binding.NamespaceUID, binding.BackendUID); err != nil {
		t.Fatalf("PreviewLegacyMemoryRestore: %v", err)
	}
	restored, err := s.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		Actor: "operator", Reason: "return to legacy", Now: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("RestoreLegacyMemories: %v", err)
	}

	reactivation := restored.Binding
	reactivation.Namespace = "different-namespace-name"
	reactivation.Mode = store.MemoryBackendModeRemote
	reactivation.State = store.MemoryBackendBindingAccepting
	reactivation.BackendGeneration++
	reactivation.AuthorityEpoch = decommissioned.AuthorityEpoch + 1
	reactivation.RoutingEpoch = restored.Binding.RoutingEpoch + 1
	reactivation.ActivationEpoch = reactivation.AuthorityEpoch
	reactivation.ValidationExpiresAt = now.Add(30 * time.Minute)
	reactivation.DecommissionedAt = nil
	recordActivationRecoveryReceiptForTest(t, s, reactivation, now.Add(3*time.Second))
	if _, err := s.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
		Binding: reactivation, RequiredFeatureEpoch: 1, Actor: "operator",
		Reason: "invalid same uid rename", Now: now.Add(3 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ActivateMemoryBackend renamed same uid error = %v, want ErrConflict", err)
	}
	current, err := s.GetMemoryBackendBinding(ctx, binding.NamespaceUID)
	if err != nil {
		t.Fatalf("GetMemoryBackendBinding: %v", err)
	}
	if current.Namespace != binding.Namespace || current.Mode != store.MemoryBackendModeLegacy {
		t.Fatalf("same uid reactivation mutated binding: %+v", current)
	}
}

func TestMemoryAdmissionRequiresPreallocatedIDsBeforePayloadValidation(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-preallocated", "team-preallocated-uid", now)

	for name, clearID := range map[string]func(*store.MemoryMutationAdmission){
		"memory id":    func(m *store.MemoryMutationAdmission) { m.MemoryID = "" },
		"operation id": func(m *store.MemoryMutationAdmission) { m.OperationID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "missing-"+name, "request", nil)
			admission.Mutation.MutationDigest = ""
			clearID(&admission.Mutation)
			_, err := s.AdmitRemoteMemoryCreate(context.Background(), admission)
			if !errors.Is(err, store.ErrValidation) || !strings.Contains(err.Error(), "preallocated memory and operation ids") {
				t.Fatalf("AdmitRemoteMemoryCreate error = %v, want preallocated-id validation before payload/digest validation", err)
			}
		})
	}
}

func TestMarkMemoryOperationSendStartedRejectsExpiredLease(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 8, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-expired-send", "team-expired-send-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "expired-send", "expired-send-request", []byte(`{"content":"v1"}`)))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "expired-dispatcher", Now: now.Add(2 * time.Second), LeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch,
		Now: now.Add(4 * time.Second), RequestDeadline: now.Add(time.Minute),
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("MarkMemoryOperationSendStarted error = %v, want ErrConflict", err)
	}
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
	if err != nil || operation.State != store.MemoryOperationLeased || operation.SendStartedAt != nil {
		t.Fatalf("expired lease operation changed = %+v, %v", operation, err)
	}
}

func TestManualDeadLetterRetryRequiresAndRetainsPayload(t *testing.T) {
	for name, settings := range map[string]struct {
		duration time.Duration
		recovery bool
	}{
		"new-max-age":     {duration: 10 * 24 * time.Hour},
		"recovery-window": {duration: 24 * time.Hour, recovery: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-retry-"+name, "team-retry-uid-"+name, now)
			admitted, err := s.AdmitRemoteMemoryCreate(ctx,
				newRemoteCreateAdmission(binding, now.Add(time.Second), "retain-"+name, "retain-request", []byte(`{"content":"v1"}`)))
			if err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
				NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
				RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "retry-dispatcher", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
				NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
				LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch,
				DeadLetter: true, Now: now.Add(3 * time.Second),
			})
			if err != nil || dead.State != store.MemoryOperationDeadLettered {
				t.Fatalf("dead letter = %+v, %v", dead, err)
			}
			retryAt := now.Add(4 * time.Second)
			maxAgeAt := retryAt.Add(settings.duration)
			if _, err := s.db.ExecContext(ctx, `UPDATE memory_operations SET payload_retain_until = ? WHERE namespace_uid = ? AND id = ?`,
				retryAt.Add(-time.Second), binding.NamespaceUID, admitted.Operation.ID); err != nil {
				t.Fatal(err)
			}
			retried, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
				NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, Manual: true,
				Actor: "operator", Reason: "retry retained payload", Now: retryAt, MaxAgeAt: maxAgeAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			wantRetainUntil := maxAgeAt
			if settings.recovery {
				wantRetainUntil = retryAt.Add(defaultOperationRecoveryWindow)
			}
			if !retried.PayloadRetainUntil.Equal(wantRetainUntil) {
				t.Fatalf("payload retain until = %v, want %v", retried.PayloadRetainUntil, wantRetainUntil)
			}
		})
	}

	t.Run("missing-payload", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		now := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
		binding := activateMemoryBackendForTest(t, s, "team-retry-missing", "team-retry-missing-uid", now)
		admitted, err := s.AdmitRemoteMemoryCreate(ctx,
			newRemoteCreateAdmission(binding, now.Add(time.Second), "missing-payload", "missing-payload-request", []byte(`{"content":"v1"}`)))
		if err != nil {
			t.Fatal(err)
		}
		claim, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
			NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
			RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "retry-dispatcher", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
			NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
			AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
			LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch, DeadLetter: true, Now: now.Add(3 * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memory_operations SET payload = X'' WHERE namespace_uid = ? AND id = ?`,
			binding.NamespaceUID, admitted.Operation.ID); err != nil {
			t.Fatal(err)
		}
		_, err = s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
			NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
			AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, Manual: true,
			Actor: "operator", Reason: "payload missing", Now: now.Add(4 * time.Second),
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("manual retry without payload error = %v, want ErrConflict", err)
		}
	})
}

func TestRemoteMemoryAdmissionConcurrentIdempotencyConverges(t *testing.T) {
	t.Run("identical requests replay the committed winner", func(t *testing.T) {
		stores := setupConcurrentGovernedMemoryStores(t)
		ctx := context.Background()
		now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
		binding := activateMemoryBackendForTest(t, stores[0], "team-idem-race", "team-idem-race-uid", now)
		admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "idem-race", "request-same", []byte(`{"content":"same"}`))

		const workers = 16
		type result struct {
			admission *store.MemoryMutationAdmissionResult
			err       error
		}
		results := make(chan result, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(s *Store) {
				defer wg.Done()
				<-start
				admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
				results <- result{admission: admitted, err: err}
			}(stores[i%len(stores)])
		}
		close(start)
		wg.Wait()
		close(results)

		firstAdmissions := 0
		for result := range results {
			if result.err != nil {
				t.Fatalf("concurrent identical admission error = %v", result.err)
			}
			if result.admission == nil || result.admission.Memory.ID != admission.Mutation.MemoryID ||
				result.admission.Operation.ID != admission.Mutation.OperationID {
				t.Fatalf("concurrent identical admission = %#v", result.admission)
			}
			if !result.admission.Replayed {
				firstAdmissions++
			}
		}
		if firstAdmissions != 1 {
			t.Fatalf("non-replayed admissions = %d, want 1", firstAdmissions)
		}
	})

	t.Run("mismatched requests return duplicate mismatch", func(t *testing.T) {
		stores := setupConcurrentGovernedMemoryStores(t)
		ctx := context.Background()
		now := time.Date(2026, 7, 30, 9, 30, 0, 0, time.UTC)
		binding := activateMemoryBackendForTest(t, stores[0], "team-idem-mismatch", "team-idem-mismatch-uid", now)
		first := newRemoteCreateAdmission(binding, now.Add(time.Second), "idem-mismatch", "request-a", []byte(`{"content":"a"}`))
		second := newRemoteCreateAdmission(binding, now.Add(time.Second), "idem-mismatch-b", "request-b", []byte("b"))
		second.Mutation.IdempotencyKey = first.Mutation.IdempotencyKey

		type result struct {
			admission *store.MemoryMutationAdmissionResult
			err       error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, admission := range []store.RemoteMemoryCreateAdmission{first, second} {
			wg.Add(1)
			go func(s *Store, admission store.RemoteMemoryCreateAdmission) {
				defer wg.Done()
				<-start
				admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
				results <- result{admission: admitted, err: err}
			}(stores[i], admission)
		}
		close(start)
		wg.Wait()
		close(results)

		successes := 0
		mismatches := 0
		for result := range results {
			switch {
			case result.err == nil:
				successes++
				if result.admission == nil || result.admission.Replayed {
					t.Fatalf("winning mismatched admission = %#v", result.admission)
				}
			case errors.Is(result.err, store.ErrDuplicateMismatch):
				mismatches++
			default:
				t.Fatalf("concurrent mismatched admission error = %v", result.err)
			}
		}
		if successes != 1 || mismatches != 1 {
			t.Fatalf("successes = %d, mismatches = %d, want 1 each", successes, mismatches)
		}
	})
}

func TestRemoteMemoryAdmissionIdempotencyReplayAndConflict(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-b", "team-b-uid", now)
	admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "idem-create", "request-a", []byte(`{"content":"raw-payload-only"}`))

	first, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	if first.Replayed || first.Memory.Generation != 0 || first.Memory.DesiredGeneration != 1 ||
		first.Memory.MaterializationState != store.MemoryMaterializationPending || first.Memory.ContentAvailable ||
		first.Memory.PendingOperationID != first.Operation.ID {
		t.Fatalf("unexpected first admission: %+v", first)
	}
	second, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate replay: %v", err)
	}
	if !second.Replayed || second.Memory.ID != first.Memory.ID || second.Operation.ID != first.Operation.ID {
		t.Fatalf("replay allocated different identities: first=%+v second=%+v", first, second)
	}
	conflict := admission
	conflict.Mutation.RequestDigest = "request-b"
	if _, err := s.AdmitRemoteMemoryCreate(ctx, conflict); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("idempotency key conflict error = %v, want ErrDuplicateMismatch", err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, first.Operation, now.Add(2*time.Second))
	completion := completionForDispatch(binding, dispatch, now.Add(4*time.Second), "backend-v1", "remote-idempotent")
	completion.FinalizeIdempotencyOutcome = true
	completion.IdempotencyOutcome = store.MemoryIdempotencyOutcome{
		Status: 201, ResponseType: store.MemoryIdempotencyMemory,
		Location: "/api/v1/memories/" + first.Memory.ID, ResponseDigest: "response-digest",
	}
	if _, err := s.CompleteMemoryOperation(ctx, completion); err != nil {
		t.Fatalf("CompleteMemoryOperation immediate: %v", err)
	}
	immediateReplay, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate immediate replay: %v", err)
	}
	if !immediateReplay.Replayed || immediateReplay.Idempotency.OriginalStatus != 201 ||
		immediateReplay.Idempotency.ResponseType != store.MemoryIdempotencyMemory ||
		immediateReplay.Idempotency.Location != completion.IdempotencyOutcome.Location ||
		immediateReplay.Idempotency.ResponseDigest != completion.IdempotencyOutcome.ResponseDigest {
		t.Fatalf("immediate replay outcome = %+v", immediateReplay.Idempotency)
	}
	operations, err := s.ListMemoryOperations(ctx, store.MemoryOperationFilter{NamespaceUID: binding.NamespaceUID})
	if err != nil {
		t.Fatalf("ListMemoryOperations: %v", err)
	}
	if len(operations) != 1 || operations[0].PayloadBytes != len(admission.Mutation.Payload) {
		t.Fatalf("operations = %+v, want one summary", operations)
	}
	var auditContainsRaw int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_audit WHERE reason LIKE '%raw-payload-only%'
		OR request_digest LIKE '%raw-payload-only%' OR mutation_digest LIKE '%raw-payload-only%'`).Scan(&auditContainsRaw); err != nil {
		t.Fatalf("query memory audit: %v", err)
	}
	if auditContainsRaw != 0 {
		t.Fatalf("raw payload leaked into memory_audit")
	}
}

//nolint:gocyclo // The test intentionally exercises one end-to-end generation/supersession sequence.
func TestRemoteMemoryGenerationsDeleteSupersessionAndGovernance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-c", "team-c-uid", now)
	created, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(binding, now.Add(time.Second), "create-key", "create-request", []byte(`{"content":"v1"}`)))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	completeOperationForTest(t, s, binding, created.Operation, now.Add(2*time.Second), "backend-v1", "remote-1")

	current, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatalf("GetRemoteMemory after create: %v", err)
	}
	if current.Generation != 1 || current.DesiredGeneration != 1 || current.ContentDigest != created.Operation.ContentDigest || !current.ContentAvailable {
		t.Fatalf("materialized create = %+v", current)
	}
	disabled, err := s.SetRemoteMemoryDisabled(ctx, store.RemoteMemoryDisabledChange{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, Disabled: true,
		ExpectedGovernanceRevision: current.GovernanceRevision, Actor: "operator", Reason: "suppress", Now: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("SetRemoteMemoryDisabled: %v", err)
	}
	if disabled.Generation != 1 || disabled.GovernanceRevision != current.GovernanceRevision+1 || !disabled.Disabled {
		t.Fatalf("disable changed wrong revision: before=%+v after=%+v", current, disabled)
	}
	trusted, err := s.SetRemoteMemoryTrust(ctx, store.RemoteMemoryTrustChange{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, Trust: store.MemoryTrustTrusted,
		ExpectedGovernanceRevision: disabled.GovernanceRevision, Actor: "operator", Reason: "reviewed", Now: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatalf("SetRemoteMemoryTrust: %v", err)
	}
	if trusted.Generation != 1 || trusted.GovernanceRevision != disabled.GovernanceRevision+1 || trusted.Trust != store.MemoryTrustTrusted {
		t.Fatalf("trust changed wrong revision: %+v", trusted)
	}

	replaceAdmission := store.RemoteMemoryReplaceAdmission{
		Mutation:           newCanonicalMemoryMutation(binding, now.Add(5*time.Second), current.ID, "replace-key", "replace-request", store.MemoryOperationReplace, 2, 1, "backend-v1", "v2"),
		Memory:             store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{"updated"}},
		ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
	}
	replaced, err := s.AdmitRemoteMemoryReplace(ctx, replaceAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryReplace: %v", err)
	}
	if replaced.Memory.Generation != 1 || replaced.Memory.DesiredGeneration != 2 || replaced.Memory.PendingOperationID != replaced.Operation.ID {
		t.Fatalf("pending replacement = %+v", replaced)
	}
	leased, err := s.ClaimMemoryOperation(ctx, replaced.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: testMemoryDispatcher, Now: now.Add(6 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil || leased.Operation.ID != replaced.Operation.ID {
		t.Fatalf("ClaimMemoryOperation = %+v, %v", leased, err)
	}
	deleteAdmission := store.RemoteMemoryDeleteAdmission{
		Mutation:           newCanonicalMemoryMutation(binding, now.Add(7*time.Second), current.ID, "delete-key", "delete-request", store.MemoryOperationDelete, 3, 1, "backend-v1", ""),
		ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, deleteAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryDelete: %v", err)
	}
	if !deleted.Memory.Deleted || deleted.Memory.DesiredGeneration != 3 || deleted.Operation.DesiredGeneration != 3 {
		t.Fatalf("delete did not install generation-three tombstone: %+v", deleted)
	}
	replaceSummary, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, replaced.Operation.ID)
	if err != nil {
		t.Fatalf("GetMemoryOperation replace: %v", err)
	}
	if replaceSummary.State != store.MemoryOperationSuperseded {
		t.Fatalf("replace state = %q, want superseded", replaceSummary.State)
	}
	completeOperationForTest(t, s, binding, deleted.Operation, now.Add(9*time.Second), "backend-v3", "remote-1")
	final, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, current.ID)
	if err != nil {
		t.Fatalf("GetRemoteMemory after delete: %v", err)
	}
	if final.Generation != 3 || final.DesiredGeneration != 3 || final.MaterializationState != store.MemoryMaterializationDeleted ||
		!final.Deleted || final.ContentAvailable || final.PendingOperationID != "" {
		t.Fatalf("final tombstone = %+v", final)
	}
}

func TestMemoryOperationLeaseSendAmbiguityAndStaleCompletion(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-d", "team-d-uid", now)
	payload := []byte(`{"content":"ambiguous"}`)
	admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "ambiguous-key", "ambiguous-request", payload)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	firstClaim, err := s.ClaimNextMemoryOperation(ctx, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "dispatcher-a", Now: now.Add(2 * time.Second), LeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatalf("ClaimNextMemoryOperation first: %v", err)
	}
	if !bytes.Equal(firstClaim.Payload, admission.Mutation.Payload) {
		t.Fatalf("claim payload = %q", firstClaim.Payload)
	}
	firstSend, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: "dispatcher-a", LeaseEpoch: firstClaim.Operation.LeaseEpoch,
		Now: now.Add(2500 * time.Millisecond), RequestDeadline: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted first: %v", err)
	}
	secondClaim, err := s.ClaimNextMemoryOperation(ctx, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "dispatcher-b", Now: now.Add(4 * time.Second), LeaseDuration: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ClaimNextMemoryOperation after ambiguity: %v", err)
	}
	if secondClaim.Operation.ID != firstClaim.Operation.ID || secondClaim.Operation.LeaseEpoch <= firstClaim.Operation.LeaseEpoch {
		t.Fatalf("ambiguous replay claim = %+v, first = %+v", secondClaim.Operation, firstClaim.Operation)
	}
	stale := store.MemoryOperationCompletion{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: "dispatcher-a", LeaseEpoch: firstSend.LeaseEpoch, Now: now.Add(4500 * time.Millisecond),
		Receipt: store.MemoryOperationReceipt{
			BindingIdentityDigest: memoryBackendBindingDigest(&binding), AppliedGeneration: 1, BackendVersion: "v1",
			BackendMemoryID: "remote-ambiguous", ContentDigest: admitted.Operation.ContentDigest,
			MutationDigest: admitted.Operation.MutationDigest, CompletedAt: now.Add(4500 * time.Millisecond),
		}, Actor: "dispatcher-a",
	}
	if _, err := s.CompleteMemoryOperation(ctx, stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale completion error = %v, want ErrConflict", err)
	}
	secondSend, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: "dispatcher-b", LeaseEpoch: secondClaim.Operation.LeaseEpoch,
		Now: now.Add(5 * time.Second), RequestDeadline: now.Add(6 * time.Second),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted second: %v", err)
	}
	stale.LeaseOwner = "dispatcher-b"
	stale.LeaseEpoch = secondSend.LeaseEpoch
	stale.Now = now.Add(5500 * time.Millisecond)
	stale.Receipt.CompletedAt = now.Add(5500 * time.Millisecond)
	if _, err := s.CompleteMemoryOperation(ctx, stale); err != nil {
		t.Fatalf("CompleteMemoryOperation second: %v", err)
	}
}

func TestRemoteMaterializationIssueSuppressesRecallAndAudits(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 15, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-mismatch", "team-mismatch-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(binding, now.Add(time.Second), "mismatch-key", "mismatch-request", []byte(`{"content":"verified"}`)))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	completeOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second), "backend-v1", "remote-mismatch")
	current, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, admitted.Memory.ID)
	if err != nil {
		t.Fatalf("GetRemoteMemory: %v", err)
	}
	trusted, err := s.SetRemoteMemoryTrust(ctx, store.RemoteMemoryTrustChange{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, Trust: store.MemoryTrustTrusted,
		ExpectedGovernanceRevision: current.GovernanceRevision, Actor: "operator", Reason: "reviewed",
		Now: now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("SetRemoteMemoryTrust: %v", err)
	}
	if err := s.MarkRemoteMemoriesRecalled(ctx, binding.NamespaceUID, []string{current.ID}, now.Add(6*time.Second)); err != nil {
		t.Fatalf("MarkRemoteMemoriesRecalled active: %v", err)
	}
	marked, err := s.MarkRemoteMemoryMaterializationIssue(ctx, store.RemoteMemoryMaterializationIssue{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		ExpectedGeneration: current.Generation, ExpectedBackendVersion: current.BackendVersion,
		State: store.MemoryMaterializationDiverged, Actor: "hydrator", Reason: "content digest mismatch",
		RequestID: "request-mismatch", Now: now.Add(7 * time.Second),
	})
	if err != nil {
		t.Fatalf("MarkRemoteMemoryMaterializationIssue: %v", err)
	}
	if marked.MaterializationState != store.MemoryMaterializationDiverged || !marked.Disabled || marked.ContentAvailable ||
		marked.GovernanceRevision != trusted.GovernanceRevision+1 {
		t.Fatalf("materialization issue did not suppress catalog: %+v", marked)
	}
	if err := s.MarkRemoteMemoriesRecalled(ctx, binding.NamespaceUID, []string{current.ID}, now.Add(8*time.Second)); err != nil {
		t.Fatalf("MarkRemoteMemoriesRecalled diverged: %v", err)
	}
	after, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, current.ID)
	if err != nil {
		t.Fatalf("GetRemoteMemory after mismatch: %v", err)
	}
	if after.RecalledCount != 1 {
		t.Fatalf("diverged recall count = %d, want 1", after.RecalledCount)
	}
	if _, err := s.MarkRemoteMemoryMaterializationIssue(ctx, store.RemoteMemoryMaterializationIssue{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		ExpectedGeneration: 0, ExpectedBackendVersion: current.BackendVersion,
		State: store.MemoryMaterializationLost, Actor: "hydrator", Reason: "stale observation", Now: now.Add(9 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale materialization issue error = %v, want ErrConflict", err)
	}
	audit, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: binding.NamespaceUID, MemoryID: current.ID})
	if err != nil {
		t.Fatalf("ListMemoryAudit: %v", err)
	}
	var found bool
	for _, record := range audit {
		if record.Action == "memory.materialization.issue" && record.NewState == string(store.MemoryMaterializationDiverged) {
			found = true
		}
	}
	if !found {
		t.Fatalf("materialization issue audit missing: %+v", audit)
	}
}

func TestOrphanMemoryOperationsRequiresCompleteEgressBarrierAndCoversPriorRoutingEpoch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 15, 45, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-orphan", "team-orphan-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "pending")
	admitted, err := s.AdmitRemoteMemoryProposalApply(ctx,
		newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "orphan-key"))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryProposalApply: %v", err)
	}
	if _, err := s.OrphanMemoryOperations(ctx, store.MemoryOperationOrphaning{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "missing barrier", Now: now.Add(1500 * time.Millisecond),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("OrphanMemoryOperations without barrier error = %v, want ErrConflict", err)
	}
	transitioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "egress barrier", Now: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("TransitionMemoryBackendBinding: %v", err)
	}
	count, err := s.OrphanMemoryOperations(ctx, store.MemoryOperationOrphaning{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: transitioned.RoutingEpoch,
		Actor: "operator", Reason: "force orphan", Now: now.Add(3 * time.Second),
	})
	if err != nil || count != 1 {
		t.Fatalf("OrphanMemoryOperations = %d, %v", count, err)
	}
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
	if err != nil || operation.State != store.MemoryOperationOrphaned {
		t.Fatalf("orphaned operation = %+v, %v", operation, err)
	}
	catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, admitted.Memory.ID)
	if err != nil || catalog.MaterializationState != store.MemoryMaterializationOrphaned || !catalog.Disabled {
		t.Fatalf("orphaned catalog = %+v, %v", catalog, err)
	}
	abandoned, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
	if err != nil || abandoned.Status != proposalStatusApplicationAbandoned || abandoned.ApplicationAbandonedAt == nil {
		t.Fatalf("orphaned proposal = %+v, %v", abandoned, err)
	}
	audit, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: binding.NamespaceUID, OperationID: admitted.Operation.ID})
	if err != nil {
		t.Fatalf("ListMemoryAudit: %v", err)
	}
	var orphanAudit *store.MemoryAuditRecord
	for i := range audit {
		if audit[i].Action == "memory.operation.orphan" {
			orphanAudit = &audit[i]
			break
		}
	}
	if orphanAudit == nil || orphanAudit.RoutingEpoch != admitted.Operation.RoutingEpoch {
		t.Fatalf("orphan audit = %+v, want original routing epoch %d", orphanAudit, admitted.Operation.RoutingEpoch)
	}
}

func TestOrphanMemoryOperationsInDrainingIsIrreversibleAndRejectsStaleCompletion(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 16, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-orphan-delete", "team-orphan-delete-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "orphan-delete")
	current, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(4*time.Second), current.ID, "orphan-delete-op", "orphan-delete-request",
			store.MemoryOperationDelete, current.Generation+1, current.Generation, current.BackendVersion, ""),
		ExpectedGeneration: current.Generation, ExpectedBackendVersion: current.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, deleted.Operation, now.Add(5*time.Second))
	draining, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "stop new content egress", Now: now.Add(7 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.OrphanMemoryOperations(ctx, store.MemoryOperationOrphaning{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: draining.RoutingEpoch,
		Actor: "operator", Reason: "force orphan after egress barrier", Now: now.Add(8 * time.Second),
	})
	if err != nil || count != 1 {
		t.Fatalf("OrphanMemoryOperations = %d, %v", count, err)
	}
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, deleted.Operation.ID)
	if err != nil || operation.State != store.MemoryOperationOrphaned {
		t.Fatalf("orphaned operation = %+v, %v", operation, err)
	}
	completion := completionForDispatch(binding, dispatch, now.Add(9*time.Second), "backend-v2", "remote-orphaned")
	if _, err := s.CompleteMemoryOperation(ctx, completion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale completion after force-orphan error = %v, want ErrConflict", err)
	}
	decommissioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: draining.NamespaceUID, BackendUID: draining.BackendUID,
		ExpectedState: draining.State, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: draining.RoutingEpoch, RoutingEpoch: draining.RoutingEpoch + 1,
		Actor: "operator", Reason: "finish orphan transition", Now: now.Add(10 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewLegacyMemoryRestore(ctx, decommissioned.NamespaceUID, decommissioned.BackendUID)
	if err != nil || preview.Restorable || !strings.Contains(preview.Reason, "force-orphan") {
		t.Fatalf("restore preview after force-orphan = %+v, %v", preview, err)
	}
}

func TestProviderNeverAppliedDeleteAbandonmentRestoresActiveCatalog(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-delete-abandon", "team-delete-abandon-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "delete-abandon")
	before, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(4*time.Second), before.ID, "delete-abandon-op", "delete-abandon-request",
			store.MemoryOperationDelete, before.Generation+1, before.Generation, before.BackendVersion, ""),
		ExpectedGeneration: before.Generation, ExpectedBackendVersion: before.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Memory.Deleted || deleted.Memory.MaterializationState != before.MaterializationState ||
		deleted.Memory.ContentAvailable != before.ContentAvailable || deleted.Memory.Disabled != before.Disabled {
		t.Fatalf("delete admission discarded prior materialization state: before=%+v admitted=%+v", before, deleted.Memory)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, deleted.Operation, now.Add(5*time.Second))
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: deleted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "PROVIDER_MISMATCH", ErrorMessage: "provider proved delete was not applied",
		Now: now.Add(7 * time.Second),
	})
	if err != nil || dead.State != store.MemoryOperationDeadLettered {
		t.Fatalf("dead letter = %+v, %v", dead, err)
	}
	if _, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: deleted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "provider proved delete was never applied", ProviderNeverApplied: true,
		Fenced: true, Now: now.Add(8 * time.Second),
	}); err != nil {
		t.Fatalf("AbandonMemoryOperation: %v", err)
	}
	restored, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Deleted || restored.MaterializationState != before.MaterializationState ||
		restored.Generation != before.Generation || restored.DesiredGeneration != before.Generation ||
		restored.BackendMemoryID != before.BackendMemoryID || restored.BackendVersion != before.BackendVersion ||
		restored.ContentDigest != before.ContentDigest || restored.ContentAvailable != before.ContentAvailable ||
		restored.Disabled != before.Disabled || restored.PendingOperationID != "" {
		t.Fatalf("catalog after proved delete abandonment: before=%+v restored=%+v", before, restored)
	}
}

func TestUnmaterializedDeleteAbandonmentMarksCatalogOrphaned(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 13, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-delete-orphan", "team-delete-orphan-uid", now)
	pending, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "delete-orphan", "delete-orphan-create", []byte(`{"content":"pending"}`)))
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(2*time.Second), pending.Memory.ID, "delete-orphan-op", "delete-orphan-request",
			store.MemoryOperationDelete, 2, 0, "", ""),
		ExpectedGeneration: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, deleted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: testMemoryDispatcher, Now: now.Add(3 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: deleted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claimed.Operation.LeaseOwner, LeaseEpoch: claimed.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "LOCAL_REJECT", ErrorMessage: "delete was never sent", Now: now.Add(4 * time.Second),
	})
	if err != nil || dead.State != store.MemoryOperationDeadLettered {
		t.Fatalf("dead letter = %+v, %v", dead, err)
	}
	if _, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: deleted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "delete was never sent", Fenced: true, Now: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("AbandonMemoryOperation: %v", err)
	}
	catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, pending.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Deleted || catalog.MaterializationState != store.MemoryMaterializationOrphaned ||
		!catalog.Disabled || catalog.ContentAvailable || catalog.PendingOperationID != "" {
		t.Fatalf("unmaterialized catalog after delete abandonment = %+v", catalog)
	}
}

func TestRemoteProposalApplyRequiresCanonicalReviewedProposalContentAndMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *store.RemoteMemoryProposalApplyAdmission)
	}{
		{
			name: "content differs from reviewed proposal",
			mutate: func(t *testing.T, admission *store.RemoteMemoryProposalApplyAdmission) {
				t.Helper()
				envelope, err := protocol.DecodeMutationEnvelope(admission.Mutation.Payload)
				if err != nil {
					t.Fatal(err)
				}
				envelope.State.Content = "tampered content"
				if err := protocol.PrepareMutation(envelope); err != nil {
					t.Fatal(err)
				}
				admission.Mutation.Payload, err = protocol.EncodeJSON(envelope)
				if err != nil {
					t.Fatal(err)
				}
				admission.Mutation.ContentDigest = envelope.ContentDigest
				admission.Mutation.MutationDigest = envelope.MutationDigest
				admission.Memory.ContentDigest = envelope.ContentDigest
			},
		},
		{
			name: "metadata differs from reviewed proposal",
			mutate: func(t *testing.T, admission *store.RemoteMemoryProposalApplyAdmission) {
				t.Helper()
				envelope, err := protocol.DecodeMutationEnvelope(admission.Mutation.Payload)
				if err != nil {
					t.Fatal(err)
				}
				envelope.State.Metadata["agentname"] = "attacker"
				if err := protocol.PrepareMutation(envelope); err != nil {
					t.Fatal(err)
				}
				admission.Mutation.Payload, err = protocol.EncodeJSON(envelope)
				if err != nil {
					t.Fatal(err)
				}
				admission.Mutation.MutationDigest = envelope.MutationDigest
				admission.Memory.AgentName = "attacker"
			},
		},
		{
			name: "catalog metadata differs from payload",
			mutate: func(t *testing.T, admission *store.RemoteMemoryProposalApplyAdmission) {
				t.Helper()
				admission.Memory.TaskName = "different-task"
			},
		},
		{
			name: "payload is not a closed mutation envelope",
			mutate: func(t *testing.T, admission *store.RemoteMemoryProposalApplyAdmission) {
				t.Helper()
				var object map[string]any
				if err := json.Unmarshal(admission.Mutation.Payload, &object); err != nil {
					t.Fatal(err)
				}
				object["unexpected"] = true
				payload, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				admission.Mutation.Payload = payload
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-proposal-verify", "team-proposal-verify-uid", now)
			proposal := &store.MemoryProposal{
				Namespace: binding.Namespace, Type: "memory", Title: "reviewed proposal",
				Description: "Tags: Beta, alpha", Content: "reviewed content", AgentName: "agent-a", TaskName: "task-a",
			}
			if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
				t.Fatal(err)
			}
			if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
				Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
			}); err != nil {
				t.Fatal(err)
			}
			admission := newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "verify-"+strings.ReplaceAll(tt.name, " ", "-"))
			tt.mutate(t, &admission)
			if _, err := s.AdmitRemoteMemoryProposalApply(ctx, admission); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("AdmitRemoteMemoryProposalApply error = %v, want ErrValidation", err)
			}
			persisted, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != proposalStatusAccepted || persisted.ApplyOperationID != "" || persisted.AppliedMemoryID != "" {
				t.Fatalf("proposal changed after rejected admission: %+v", persisted)
			}
			if _, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, admission.Mutation.MemoryID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("catalog row exists after rejected admission: %v", err)
			}
		})
	}
}

func TestRemoteProposalApplyRequiresAuthoritativeReview(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-proposal-review", "team-proposal-review-uid", now)
	proposal := &store.MemoryProposal{Namespace: binding.Namespace, Type: "memory", Title: "proposal", Content: "content"}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_proposals SET status = ? WHERE namespace = ? AND id = ?`,
		proposalStatusAccepted, proposal.Namespace, proposal.ID); err != nil {
		t.Fatal(err)
	}
	admission := newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "missing-review")
	if _, err := s.AdmitRemoteMemoryProposalApply(ctx, admission); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("AdmitRemoteMemoryProposalApply error = %v, want ErrValidation", err)
	}
}

func TestRemoteCreateAlwaysForcesUntrustedWithoutProposalProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-direct-trust", "team-direct-trust-uid", now)
	for index, requestedTrust := range []store.MemoryTrust{store.MemoryTrustReviewed, store.MemoryTrustTrusted} {
		admission := newRemoteCreateAdmission(binding, now.Add(time.Duration(index+1)*time.Second),
			fmt.Sprintf("direct-trust-%d", index), fmt.Sprintf("direct-request-%d", index), []byte(`{"content":"direct"}`))
		admission.Mutation.ProposalID = "mprop-spoofed"
		admission.Memory.Trust = requestedTrust
		admission.Memory.SourceProposalID = "mprop-spoofed"
		admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
		if err != nil {
			t.Fatal(err)
		}
		if admitted.Memory.Trust != store.MemoryTrustUntrusted || admitted.Memory.SourceProposalID != "" || admitted.Operation.ProposalID != "" {
			t.Fatalf("direct create retained caller-owned trust/proposal provenance: %+v %+v", admitted.Memory, admitted.Operation)
		}
	}
}

func TestRemoteReplacementContentChangeDemotesTrustAndInvalidatesTrustCAS(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-replace-trust", "team-replace-trust-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "reviewed content")
	applied, err := s.AdmitRemoteMemoryProposalApply(ctx, newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "replace-trust-proposal"))
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, applied.Operation, now.Add(2*time.Second), "proposal-v1", "proposal-memory")
	before, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, applied.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Trust != store.MemoryTrustReviewed || before.SourceProposalID != proposal.ID {
		t.Fatalf("proposal memory not reviewed: %+v", before)
	}

	replace := store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), before.ID, "replace-trust", "replace-trust-request",
			store.MemoryOperationReplace, before.Generation+1, before.Generation, before.BackendVersion, "changed content"),
		Memory: *before, ExpectedGeneration: before.Generation, ExpectedBackendVersion: before.BackendVersion,
	}
	admitted, err := s.AdmitRemoteMemoryReplace(ctx, replace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemoteMemoryTrust(ctx, store.RemoteMemoryTrustChange{
		NamespaceUID: binding.NamespaceUID, ID: before.ID, Trust: store.MemoryTrustReviewed,
		ExpectedGovernanceRevision: before.GovernanceRevision, Actor: "operator", Now: now.Add(6 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("SetRemoteMemoryTrust while replacement pending error = %v, want ErrConflict", err)
	}
	completeOperationForTest(t, s, binding, admitted.Operation, now.Add(7*time.Second), "proposal-v2", "proposal-memory")
	after, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Trust != store.MemoryTrustUntrusted || after.SourceProposalID != "" || after.Source == memorySourceProposal ||
		after.GovernanceRevision != before.GovernanceRevision+1 || after.Generation != before.Generation+1 {
		t.Fatalf("content replacement did not demote trust and clear provenance: before=%+v after=%+v", before, after)
	}
	if _, err := s.SetRemoteMemoryTrust(ctx, store.RemoteMemoryTrustChange{
		NamespaceUID: binding.NamespaceUID, ID: after.ID, Trust: store.MemoryTrustTrusted,
		ExpectedGovernanceRevision: before.GovernanceRevision, Actor: "operator", Now: now.Add(10 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale post-replacement trust CAS error = %v, want ErrConflict", err)
	}
}

func TestRemoteReplacementMetadataChangeDemotesReviewedProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-metadata-trust", "team-metadata-trust-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "reviewed content")
	applied, err := s.AdmitRemoteMemoryProposalApply(ctx, newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "metadata-trust-proposal"))
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, applied.Operation, now.Add(2*time.Second), "proposal-v1", "proposal-memory")
	before, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, applied.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	desired := *before
	desired.TaskName = "metadata-only"
	replace := store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), before.ID, "metadata-trust", "metadata-trust-request",
			store.MemoryOperationReplace, before.Generation+1, before.Generation, before.BackendVersion, "reviewed content"),
		Memory: desired, ExpectedGeneration: before.Generation, ExpectedBackendVersion: before.BackendVersion,
	}
	admitted, err := s.AdmitRemoteMemoryReplace(ctx, replace)
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, admitted.Operation, now.Add(6*time.Second), "proposal-v2", "proposal-memory")
	after, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Trust != store.MemoryTrustUntrusted || after.Source == memorySourceProposal || after.SourceProposalID != "" ||
		after.GovernanceRevision != before.GovernanceRevision+1 {
		t.Fatalf("manual replacement retained reviewed governance: before=%+v after=%+v", before, after)
	}
}

func TestRemoteNoOpReplacementPreservesReviewedProposalProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-noop-reviewed", "team-noop-reviewed-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "reviewed content")
	applied, err := s.AdmitRemoteMemoryProposalApply(ctx,
		newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "noop-reviewed-proposal"))
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, applied.Operation, now.Add(2*time.Second), "proposal-v1", "proposal-memory")
	before, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, applied.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}

	desired := *before
	replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), before.ID, "noop-reviewed", "noop-reviewed-request",
			store.MemoryOperationReplace, before.Generation+1, before.Generation, before.BackendVersion, proposal.Content),
		Memory: desired, ExpectedGeneration: before.Generation, ExpectedBackendVersion: before.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, replace.Operation, now.Add(6*time.Second), "proposal-v2", "proposal-memory")
	after, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Trust != store.MemoryTrustReviewed || after.Source != memorySourceProposal ||
		after.SourceProposalID != proposal.ID || after.GovernanceRevision != before.GovernanceRevision ||
		after.Generation != before.Generation+1 {
		t.Fatalf("no-op replacement changed reviewed provenance: before=%+v after=%+v", before, after)
	}
}

func TestRemoteNoOpReplacementPreservesTrustedGovernance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 6, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-noop-trusted", "team-noop-trusted-uid", now)
	materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "noop-trusted")
	before, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, "mem-noop-trusted")
	if err != nil {
		t.Fatal(err)
	}
	before, err = s.SetRemoteMemoryTrust(ctx, store.RemoteMemoryTrustChange{
		NamespaceUID: binding.NamespaceUID, ID: before.ID, Trust: store.MemoryTrustTrusted,
		ExpectedGovernanceRevision: before.GovernanceRevision, Actor: "operator", Reason: "trusted guidance",
		Now: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	desired := *before
	replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), before.ID, "noop-trusted-replace", "noop-trusted-request",
			store.MemoryOperationReplace, before.Generation+1, before.Generation, before.BackendVersion, `{"content":"v1"}`),
		Memory: desired, ExpectedGeneration: before.Generation, ExpectedBackendVersion: before.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, replace.Operation, now.Add(6*time.Second), "backend-v2", "remote-noop-trusted")
	after, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Trust != store.MemoryTrustTrusted || after.Source != before.Source ||
		after.SourceProposalID != before.SourceProposalID || after.GovernanceRevision != before.GovernanceRevision ||
		after.Generation != before.Generation+1 {
		t.Fatalf("no-op replacement changed trusted governance: before=%+v after=%+v", before, after)
	}
}

func TestRemoteProposalApplyingCompletionAndAbandonment(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-e", "team-e-uid", now)

	proposal := acceptedMemoryProposalForTest(t, s, "team-e", "proposal content")
	apply := newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "proposal-apply-key")
	apply.Mutation.Actor = "request-actor"
	apply.AppliedBy = "applying-operator"
	admitted, err := s.AdmitRemoteMemoryProposalApply(ctx, apply)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryProposalApply: %v", err)
	}
	applying, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal applying: %v", err)
	}
	if applying.Status != proposalStatusApplying || applying.ApplyOperationID != admitted.Operation.ID || applying.AppliedMemoryID != "" ||
		applying.AppliedBy != apply.AppliedBy || admitted.Operation.Actor != apply.Mutation.Actor {
		t.Fatalf("proposal applying state = %+v", applying)
	}
	if err := s.ArchiveMemoryProposal(ctx, proposal.Namespace, proposal.ID); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("ArchiveMemoryProposal applying error = %v, want validation", err)
	}
	completeOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second), "proposal-v1", "proposal-remote")
	applied, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal applied: %v", err)
	}
	if applied.Status != proposalStatusApplied || applied.AppliedMemoryID != admitted.Memory.ID || applied.ApplyOperationID != admitted.Operation.ID ||
		applied.AppliedBy != apply.AppliedBy || applied.AppliedAt == nil {
		t.Fatalf("proposal applied state = %+v", applied)
	}

	abandonProposal := acceptedMemoryProposalForTest(t, s, "team-e", "abandon content")
	abandonAdmission := newRemoteProposalApplyAdmission(binding, abandonProposal, now.Add(3*time.Second), "proposal-abandon-key")
	abandonAdmitted, err := s.AdmitRemoteMemoryProposalApply(ctx, abandonAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryProposalApply abandon: %v", err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, abandonAdmitted.Operation, now.Add(4*time.Second))
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: abandonAdmitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "PROVIDER_REJECTED", ErrorMessage: "not applied",
		Actor: testMemoryDispatcher, Reason: "terminal provider proof", Now: now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("RetryMemoryOperation dead letter: %v", err)
	}
	if dead.State != store.MemoryOperationDeadLettered {
		t.Fatalf("dead letter state = %q", dead.State)
	}
	if _, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: abandonAdmitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "provider proved no fence", ProviderNeverApplied: true, Fenced: true,
		Now: now.Add(6 * time.Second),
	}); err != nil {
		t.Fatalf("AbandonMemoryOperation: %v", err)
	}
	abandoned, err := s.GetMemoryProposal(ctx, abandonProposal.Namespace, abandonProposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal abandoned: %v", err)
	}
	if abandoned.Status != proposalStatusApplicationAbandoned || abandoned.ApplicationAbandonedAt == nil || abandoned.ApplyOperationID != abandonAdmitted.Operation.ID {
		t.Fatalf("proposal abandonment state = %+v", abandoned)
	}
}

func TestDeleteRejectsSentUnresolvedReplacementState(t *testing.T) {
	for _, targetState := range []store.MemoryOperationState{
		store.MemoryOperationDispatching,
		store.MemoryOperationAmbiguous,
		store.MemoryOperationDeadLettered,
	} {
		t.Run(string(targetState), func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 28, 16, 30, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-delete-"+string(targetState), "team-delete-uid-"+string(targetState), now)
			created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "delete-base-"+string(targetState))
			replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
				Mutation: newCanonicalMemoryMutation(binding, now.Add(4*time.Second), created.Memory.ID, "replace-"+string(targetState), "replace-request-"+string(targetState),
					store.MemoryOperationReplace, 2, 1, "backend-v1", "v2"),
				Memory:             store.RemoteMemoryCatalogEntry{SessionName: testMetadataNewSession, Source: "manual", Tags: []string{testMetadataNewTag}},
				ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
			})
			if err != nil {
				t.Fatal(err)
			}
			dispatch := claimAndSendOperationForTest(t, s, binding, replace.Operation, now.Add(5*time.Second))
			switch targetState {
			case store.MemoryOperationDispatching:
			case store.MemoryOperationAmbiguous:
				if _, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
					NamespaceUID: binding.NamespaceUID, ID: replace.Operation.ID, BackendUID: binding.BackendUID,
					AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
					LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
					Ambiguous: true, ErrorCode: "TIMEOUT", ErrorMessage: "unknown", Now: now.Add(7 * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			case store.MemoryOperationDeadLettered:
				if _, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
					NamespaceUID: binding.NamespaceUID, ID: replace.Operation.ID, BackendUID: binding.BackendUID,
					AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
					LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
					DeadLetter: true, ErrorCode: "UNKNOWN", ErrorMessage: "operator required", Now: now.Add(7 * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			}
			deleteMutation := newCanonicalMemoryMutation(binding, now.Add(8*time.Second), created.Memory.ID,
				"delete-"+string(targetState), "delete-request-"+string(targetState),
				store.MemoryOperationDelete, 3, 1, "backend-v1", "")
			if _, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
				Mutation: deleteMutation, ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("delete with %s predecessor error = %v, want ErrConflict", targetState, err)
			}
			predecessor, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, replace.Operation.ID)
			if err != nil || predecessor.State != targetState || predecessor.SendStartedAt == nil {
				t.Fatalf("predecessor after rejected delete = %+v, %v", predecessor, err)
			}
			catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
			if err != nil || catalog.Deleted || catalog.PendingOperationID != replace.Operation.ID {
				t.Fatalf("catalog after rejected delete = %+v, %v", catalog, err)
			}
		})
	}
}

func TestDeleteRejectsEmptyCanonicalPayload(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-delete-empty", "team-delete-empty-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "delete-empty")
	mutation := newCanonicalMemoryMutation(binding, now.Add(4*time.Second), created.Memory.ID, "delete-empty-op", "delete-empty-request",
		store.MemoryOperationDelete, 2, 1, "backend-v1", "")
	mutation.Payload = nil
	if _, err := s.AdmitRemoteMemoryDelete(context.Background(), store.RemoteMemoryDeleteAdmission{
		Mutation: mutation, ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("empty delete payload error = %v, want ErrValidation", err)
	}
}

func TestReplacementMetadataPromotesOnlyOnCompletionAndRestoresOnAbandon(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-metadata", "team-metadata-uid", now)
	create := newRemoteCreateAdmission(binding, now.Add(time.Second), "metadata-create", "metadata-create-request", []byte(`{"content":"v1"}`))
	create.Memory = store.RemoteMemoryCatalogEntry{SessionName: "old-session", AgentName: "old-agent", Source: "manual", Tags: []string{"old"}}
	created, err := s.AdmitRemoteMemoryCreate(ctx, create)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	completeOperationForTest(t, s, binding, created.Operation, now.Add(2*time.Second), "backend-v1", "remote-metadata")
	replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), created.Memory.ID, "metadata-replace", "metadata-replace-request",
			store.MemoryOperationReplace, 2, 1, "backend-v1", "v2"),
		Memory:             store.RemoteMemoryCatalogEntry{SessionName: testMetadataNewSession, AgentName: testMetadataNewAgent, Source: testMetadataImport, Tags: []string{"new"}},
		ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
	})
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryReplace: %v", err)
	}
	if replace.Memory.SessionName != "old-session" || replace.Memory.AgentName != "old-agent" || len(replace.Memory.Tags) != 0 {
		t.Fatalf("replacement metadata became visible before completion: %+v", replace.Memory)
	}
	completeOperationForTest(t, s, binding, replace.Operation, now.Add(6*time.Second), "backend-v2", "remote-metadata")
	promoted, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil || promoted.SessionName != testMetadataNewSession || promoted.AgentName != testMetadataNewAgent ||
		promoted.Source != testMetadataImport || len(promoted.Tags) != 0 {
		t.Fatalf("promoted replacement metadata = %+v, %v", promoted, err)
	}
	replaceAgain, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(9*time.Second), created.Memory.ID, "metadata-abandon", "metadata-abandon-request",
			store.MemoryOperationReplace, 3, 2, "backend-v2", "v3"),
		Memory:             store.RemoteMemoryCatalogEntry{SessionName: testMetadataAbandoned, AgentName: testMetadataAbandoned, Source: "manual", Tags: []string{testMetadataAbandoned}},
		ExpectedGeneration: 2, ExpectedBackendVersion: "backend-v2",
	})
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryReplace abandon: %v", err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, replaceAgain.Operation, now.Add(10*time.Second))
	if _, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: replaceAgain.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		DeadLetter: true, Now: now.Add(12 * time.Second),
	}); err != nil {
		t.Fatalf("RetryMemoryOperation: %v", err)
	}
	if _, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: replaceAgain.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "fenced", ProviderNeverApplied: true, Fenced: true, Now: now.Add(13 * time.Second),
	}); err != nil {
		t.Fatalf("AbandonMemoryOperation: %v", err)
	}
	restored, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil || restored.SessionName != testMetadataNewSession || restored.AgentName != testMetadataNewAgent ||
		len(restored.Tags) != 0 || restored.DesiredGeneration != restored.Generation {
		t.Fatalf("metadata after abandonment = %+v, %v", restored, err)
	}
}

func TestRefreshMemoryBackendBindingAtomicallyResumesWithValidatedRoute(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-resume-refresh", "team-resume-refresh-uid", now)
	draining, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "install drain barrier", Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("TransitionMemoryBackendBinding draining: %v", err)
	}

	unsafe := *draining
	unsafe.State = store.MemoryBackendBindingAccepting
	unsafe.ValidationExpiresAt = now.Add(time.Hour)
	if _, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: unsafe, ExpectedRoutingEpoch: draining.RoutingEpoch,
		Actor: "operator", Reason: "unsafe same-route resume", Now: now.Add(2 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same-route resume error = %v, want ErrConflict", err)
	}

	resumed := *draining
	resumed.State = store.MemoryBackendBindingAccepting
	resumed.BackendGeneration++
	resumed.RoutingEpoch++
	resumed.SpecDigest = "resumed-spec"
	resumed.EndpointDigest = "resumed-endpoint"
	resumed.SecretName = "resumed-auth"
	resumed.SecretKey = "resumed-token"
	resumed.SecretUID = "resumed-secret-uid"
	resumed.SecretResourceVersion = "7"
	resumed.OwnershipClaim = "resumed-claim"
	resumed.CapabilityRevision = "resumed-capabilities"
	resumed.ValidationExpiresAt = now.Add(2 * time.Hour)
	refreshed, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: resumed, ExpectedRoutingEpoch: draining.RoutingEpoch,
		Actor: "operator", Reason: "resume after remote fence", Now: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("RefreshMemoryBackendBinding resume: %v", err)
	}
	if refreshed.State != store.MemoryBackendBindingAccepting ||
		refreshed.BackendGeneration != resumed.BackendGeneration || refreshed.RoutingEpoch != resumed.RoutingEpoch ||
		refreshed.SpecDigest != resumed.SpecDigest || refreshed.EndpointDigest != resumed.EndpointDigest ||
		refreshed.SecretName != resumed.SecretName || refreshed.SecretKey != resumed.SecretKey ||
		refreshed.SecretUID != resumed.SecretUID || refreshed.SecretResourceVersion != resumed.SecretResourceVersion ||
		refreshed.OwnershipClaim != resumed.OwnershipClaim || refreshed.CapabilityRevision != resumed.CapabilityRevision ||
		!refreshed.ValidationExpiresAt.Equal(resumed.ValidationExpiresAt) {
		t.Fatalf("atomic resume binding = %#v", refreshed)
	}
}

func TestRefreshMemoryBackendBindingIdentityAndCatalogRebase(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 17, 30, 0, 0, time.UTC)
	s := setupTestStore(t)
	binding := activateMemoryBackendForTest(t, s, "team-refresh", "team-refresh-uid", now)
	fresh := binding
	fresh.ValidationExpiresAt = now.Add(45 * time.Minute)
	refreshed, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: fresh, ExpectedRoutingEpoch: binding.RoutingEpoch, Actor: "operator", Reason: "fresh", Now: now.Add(time.Second),
	})
	if err != nil || refreshed.EndpointDigest != binding.EndpointDigest || !refreshed.ValidationExpiresAt.Equal(fresh.ValidationExpiresAt) {
		t.Fatalf("equal-epoch refresh = %+v, %v", refreshed, err)
	}
	changed := *refreshed
	changed.EndpointDigest = "changed-endpoint"
	changed.ValidationExpiresAt = now.Add(50 * time.Minute)
	if _, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: changed, ExpectedRoutingEpoch: refreshed.RoutingEpoch, Actor: "operator", Reason: "unsafe equal route", Now: now.Add(2 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("equal route identity change error = %v, want ErrConflict", err)
	}
	changed.RoutingEpoch++
	changed.TenantID = "different-tenant"
	if _, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: changed, ExpectedRoutingEpoch: refreshed.RoutingEpoch, Actor: "operator", Reason: "new authority", Now: now.Add(3 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("tenant identity change error = %v, want ErrConflict", err)
	}
	pending, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(*refreshed, now.Add(4*time.Second), "refresh-pending", "refresh-pending-request", []byte(`{"content":"pending"}`)))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate pending: %v", err)
	}
	changed = *refreshed
	changed.RoutingEpoch++
	changed.EndpointDigest = "next-endpoint"
	changed.ValidationExpiresAt = now.Add(55 * time.Minute)
	if _, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: changed, ExpectedRoutingEpoch: refreshed.RoutingEpoch, Actor: "operator", Reason: "blocked rebase", Now: now.Add(5 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("refresh with unresolved old-route operation error = %v, want ErrConflict", err)
	}
	if operation, getErr := s.GetMemoryOperation(ctx, binding.NamespaceUID, pending.Operation.ID); getErr != nil || operation.RoutingEpoch != binding.RoutingEpoch {
		t.Fatalf("pending operation changed during blocked refresh = %+v, %v", operation, getErr)
	}

	safe := setupTestStore(t)
	safeBinding := activateMemoryBackendForTest(t, safe, "team-rebase", "team-rebase-uid", now)
	materialized := materializedRemoteMemoryForTest(t, safe, safeBinding, now.Add(time.Second), "rebase")
	next := safeBinding
	next.RoutingEpoch++
	next.EndpointDigest = "rebound-endpoint"
	next.SecretResourceVersion = "2"
	next.ValidationExpiresAt = now.Add(50 * time.Minute)
	rebased, err := safe.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: next, ExpectedRoutingEpoch: safeBinding.RoutingEpoch, Actor: "operator", Reason: "rotate route", Now: now.Add(5 * time.Second),
	})
	if err != nil || rebased.RoutingEpoch != next.RoutingEpoch {
		t.Fatalf("routing refresh = %+v, %v", rebased, err)
	}
	catalog, err := safe.GetRemoteMemory(ctx, safeBinding.NamespaceUID, materialized.Memory.ID)
	if err != nil || catalog.RoutingEpoch != next.RoutingEpoch {
		t.Fatalf("rebased catalog = %+v, %v", catalog, err)
	}
}

func TestSentReplacementAbandonmentRequiresNeverAppliedProofAndPreservesCatalog(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-abandon-proof", "team-abandon-proof-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "abandon-proof")
	before, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(4*time.Second), before.ID, "abandon-proof-replace", "abandon-proof-request",
			store.MemoryOperationReplace, before.Generation+1, before.Generation, before.BackendVersion, "v2"),
		Memory: store.RemoteMemoryCatalogEntry{SessionName: testMetadataAbandoned, AgentName: testMetadataAbandoned,
			Source: "manual", Tags: []string{testMetadataAbandoned}},
		ExpectedGeneration: before.Generation, ExpectedBackendVersion: before.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, replace.Operation, now.Add(5*time.Second))
	ambiguous, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: replace.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		Ambiguous: true, ErrorCode: "TIMEOUT", ErrorMessage: "provider outcome unknown", Now: now.Add(6 * time.Second),
	})
	if err != nil || ambiguous.State != store.MemoryOperationAmbiguous {
		t.Fatalf("ambiguous retry = %+v, %v", ambiguous, err)
	}
	secondDispatch := claimAndSendOperationForTest(t, s, binding, *ambiguous, now.Add(7*time.Second))
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: replace.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: secondDispatch.Operation.LeaseOwner, LeaseEpoch: secondDispatch.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "UNKNOWN", ErrorMessage: "operator reconciliation required", Now: now.Add(9 * time.Second),
	})
	if err != nil || dead.State != store.MemoryOperationDeadLettered || dead.SendStartedAt == nil {
		t.Fatalf("dead letter = %+v, %v", dead, err)
	}

	_, err = s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: replace.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "fenced but provider outcome remains uncertain", Fenced: true, Now: now.Add(10 * time.Second),
	})
	if !errors.Is(err, store.ErrValidation) {
		t.Fatalf("abandon uncertain sent replacement error = %v, want ErrValidation", err)
	}
	unchanged, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Generation != before.Generation || unchanged.DesiredGeneration != replace.Operation.DesiredGeneration ||
		unchanged.PendingOperationID != replace.Operation.ID || unchanged.ContentDigest != before.ContentDigest ||
		unchanged.SessionName != before.SessionName || unchanged.AgentName != before.AgentName {
		t.Fatalf("uncertain sent replacement rolled catalog back: before=%+v after=%+v", before, unchanged)
	}

	abandoned, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: replace.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "provider proved mutation was never applied", ProviderNeverApplied: true,
		Fenced: true, Now: now.Add(11 * time.Second),
	})
	if err != nil || abandoned.State != store.MemoryOperationAbandoned {
		t.Fatalf("proved abandonment = %+v, %v", abandoned, err)
	}
	restored, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, before.ID)
	if err != nil || restored.Generation != before.Generation || restored.DesiredGeneration != before.Generation ||
		restored.PendingOperationID != "" || restored.ContentDigest != before.ContentDigest {
		t.Fatalf("catalog after proved abandonment = %+v, %v", restored, err)
	}
}

func TestProviderUncertaintySurvivesManualRetry(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 11, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-retry-proof", "team-retry-proof-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "retry-proof", "retry-proof-request", []byte(`{"content":"v1"}`)))
	if err != nil {
		t.Fatal(err)
	}
	firstDispatch := claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: firstDispatch.Operation.LeaseOwner, LeaseEpoch: firstDispatch.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "UNKNOWN", ErrorMessage: "first send uncertain", Now: now.Add(3 * time.Second),
	})
	if err != nil || dead.SendStartedAt == nil {
		t.Fatalf("first dead letter = %+v, %v", dead, err)
	}
	retried, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Manual: true, Actor: "operator", Reason: "retry same operation id", Now: now.Add(4 * time.Second),
	})
	if err != nil || retried.SendStartedAt == nil {
		t.Fatalf("manual retry lost provider uncertainty: %+v, %v", retried, err)
	}
	secondClaim, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: "dispatcher-2", Now: now.Add(5 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadAgain, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: secondClaim.Operation.LeaseOwner, LeaseEpoch: secondClaim.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "LOCAL_FAILURE", ErrorMessage: "second attempt never sent", Now: now.Add(6 * time.Second),
	})
	if err != nil || deadAgain.SendStartedAt == nil {
		t.Fatalf("second dead letter lost provider uncertainty: %+v, %v", deadAgain, err)
	}
	if _, err := s.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "fenced only", Fenced: true, Now: now.Add(7 * time.Second),
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("abandon after historical send error = %v, want ErrValidation", err)
	}
}

//nolint:gocyclo // Namespace reuse covers terminal binding, archive, proposal, fence, and audit rotation.
func TestMemoryBackendActivationReusesNamespaceNameOnlyAfterRemovedUID(t *testing.T) {
	const reusedNamespace = "team-reused"
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := s.CreateMemory(ctx, &store.Memory{ID: "legacy-reused", Namespace: reusedNamespace, Content: "restorable"}); err != nil {
		t.Fatalf("seed legacy memory: %v", err)
	}
	oldBinding := activateMemoryBackendForTest(t, s, reusedNamespace, "team-reused-old-uid", now)
	oldProposal := acceptedMemoryProposalForTest(t, s, reusedNamespace, "old namespace proposal")
	if _, err := s.db.Exec(`UPDATE memory_proposals SET created_at = ?, updated_at = ? WHERE id = ?`,
		now.Add(500*time.Millisecond), now.Add(500*time.Millisecond), oldProposal.ID); err != nil {
		t.Fatal(err)
	}
	decommissioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: oldBinding.NamespaceUID, BackendUID: oldBinding.BackendUID,
		ExpectedState: oldBinding.State, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: oldBinding.RoutingEpoch, RoutingEpoch: oldBinding.RoutingEpoch + 1,
		Actor: "operator", Reason: "old namespace deleted", Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	newBinding := oldBinding
	newBinding.NamespaceUID = "team-reused-new-uid"
	newBinding.BackendUID = "backend-new"
	newBinding.BackendGeneration = 1
	newBinding.AuthorityEpoch = 1
	newBinding.RoutingEpoch = 1
	newBinding.ActivationEpoch = 1
	newBinding.SpecDigest = "spec-new"
	newBinding.EndpointDigest = "endpoint-new"
	newBinding.SecretUID = "secret-new"
	newBinding.SecretResourceVersion = "1"
	newBinding.TenantID = "tenant-new"
	newBinding.StoreUUID = "store-new"
	newBinding.OwnershipClaim = "claim-new"
	newBinding.CapabilityRevision = "cap-new"
	newBinding.State = store.MemoryBackendBindingAccepting
	newBinding.ValidationExpiresAt = now.Add(time.Hour)
	newBinding.ActivatedAt = time.Time{}
	newBinding.UpdatedAt = time.Time{}
	newBinding.DecommissionedAt = nil
	activation := store.MemoryBackendActivation{
		Binding: newBinding, RequiredFeatureEpoch: 1, Actor: "operator", Reason: "new namespace incarnation", Now: now.Add(2 * time.Second),
	}
	recordActivationRecoveryReceiptForTest(t, s, newBinding, now.Add(2*time.Second))
	if _, err := s.ActivateMemoryBackend(ctx, activation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("decommissioned namespace reuse error = %v, want ErrConflict", err)
	}
	preview, err := s.PreviewLegacyMemoryRestore(ctx, oldBinding.NamespaceUID, oldBinding.BackendUID)
	if err != nil || !preview.Restorable || preview.ArchivedMemories != 1 {
		t.Fatalf("decommissioned archive preview = %+v, %v", preview, err)
	}
	terminal, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: oldBinding.NamespaceUID, BackendUID: oldBinding.BackendUID,
		ExpectedState: decommissioned.State, State: store.MemoryBackendBindingRemoved,
		ExpectedRoutingEpoch: decommissioned.RoutingEpoch, RoutingEpoch: decommissioned.RoutingEpoch + 1,
		Actor: "operator", Reason: "old namespace removal complete", Now: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("remove old namespace binding: %v", err)
	}
	newProposal := &store.MemoryProposal{
		Namespace: reusedNamespace, Type: proposalTypeMemory, Title: "new namespace proposal", Content: "new content",
		CreatedAt: now.Add(3500 * time.Millisecond), UpdatedAt: now.Add(3500 * time.Millisecond),
	}
	if err := s.CreateMemoryProposal(ctx, newProposal); err != nil {
		t.Fatal(err)
	}
	activation.Now = now.Add(4 * time.Second)
	activated, err := s.ActivateMemoryBackend(ctx, activation)
	if err != nil {
		t.Fatalf("ActivateMemoryBackend new namespace incarnation: %v", err)
	}
	if activated.Binding.NamespaceUID != newBinding.NamespaceUID {
		t.Fatalf("new binding = %+v", activated.Binding)
	}
	current, err := s.GetMemoryBackendBindingByNamespace(ctx, reusedNamespace)
	if err != nil || current.NamespaceUID != newBinding.NamespaceUID {
		t.Fatalf("current binding by namespace = %+v, %v", current, err)
	}
	historical, err := s.GetMemoryBackendBinding(ctx, oldBinding.NamespaceUID)
	if err != nil || historical.State != terminal.State || historical.Namespace == reusedNamespace {
		t.Fatalf("historical binding = %+v, %v", historical, err)
	}
	if _, err := s.GetMemoryProposal(ctx, reusedNamespace, oldProposal.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old proposal remained visible to reused namespace: %v", err)
	}
	if proposal, err := s.GetMemoryProposal(ctx, historical.Namespace, oldProposal.ID); err != nil || proposal.ID != oldProposal.ID {
		t.Fatalf("historical proposal = %+v, %v", proposal, err)
	}
	if proposal, err := s.GetMemoryProposal(ctx, reusedNamespace, newProposal.ID); err != nil || proposal.ID != newProposal.ID {
		t.Fatalf("new namespace proposal = %+v, %v", proposal, err)
	}
	var oldFenceName, newFenceName string
	if err := s.db.QueryRow(`SELECT namespace_name FROM memory_legacy_fences WHERE namespace_uid = ?`, oldBinding.NamespaceUID).Scan(&oldFenceName); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT namespace_name FROM memory_legacy_fences WHERE namespace_uid = ?`, newBinding.NamespaceUID).Scan(&newFenceName); err != nil {
		t.Fatal(err)
	}
	if oldFenceName == reusedNamespace || oldFenceName != historical.Namespace || newFenceName != reusedNamespace {
		t.Fatalf("rotated fence names old=%q historical=%q new=%q", oldFenceName, historical.Namespace, newFenceName)
	}
	audit, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: oldBinding.NamespaceUID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	foundRotation := false
	for _, record := range audit {
		if record.Action == "backend.namespace_identity.rotate" {
			foundRotation = true
			break
		}
	}
	if !foundRotation {
		t.Fatalf("historical namespace rotation audit missing: %+v", audit)
	}
	if err := s.CreateMemory(ctx, &store.Memory{Namespace: "team-reused", Content: "must remain fenced"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("new namespace write fence error = %v, want ErrConflict", err)
	}
}

func TestCatalogRebaseIgnoresPriorTerminalAuthority(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-authority-rebase", "team-authority-rebase-uid", now)
	binding.AuthorityEpoch = 2
	binding.RoutingEpoch = 3
	binding.ActivationEpoch = 2
	if _, err := s.db.Exec(`UPDATE memory_backend_bindings SET authority_epoch = ?, routing_epoch = ?, activation_epoch = ? WHERE namespace_uid = ?`,
		binding.AuthorityEpoch, binding.RoutingEpoch, binding.ActivationEpoch, binding.NamespaceUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE memory_legacy_fences SET authority_epoch = ?, routing_epoch = ? WHERE namespace_uid = ?`,
		binding.AuthorityEpoch, binding.RoutingEpoch, binding.NamespaceUID); err != nil {
		t.Fatal(err)
	}
	oldCatalog := store.RemoteMemoryCatalogEntry{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, ID: "old-authority-memory", ClusterID: binding.ClusterID,
		BackendUID: "retired-backend", AuthorityEpoch: 1, RoutingEpoch: 1, TenantID: "retired-tenant", StoreUUID: "retired-store",
		BackendMemoryID: "retired-provider-id", BackendVersion: "retired-version", Generation: 1, DesiredGeneration: 1,
		GovernanceRevision: 1, MaterializationState: store.MemoryMaterializationOrphaned, Disabled: true,
		Trust: store.MemoryTrustReviewed, ContentDigest: "retired-digest", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRemoteMemoryCatalog(ctx, tx, &oldCatalog); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	next := binding
	next.RoutingEpoch++
	next.EndpointDigest = "endpoint-current-authority-next"
	next.SecretResourceVersion = "2"
	next.ValidationExpiresAt = now.Add(time.Hour)
	refreshed, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: next, ExpectedRoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "rotate current authority route", Now: now.Add(time.Second),
	})
	if err != nil || refreshed.RoutingEpoch != next.RoutingEpoch {
		t.Fatalf("current authority refresh = %+v, %v", refreshed, err)
	}
	prior, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, oldCatalog.ID)
	if err != nil || prior.RoutingEpoch != oldCatalog.RoutingEpoch || prior.AuthorityEpoch != oldCatalog.AuthorityEpoch {
		t.Fatalf("prior authority catalog changed = %+v, %v", prior, err)
	}
}

func TestMemoryOperationDeadlinesAreCappedAtMaximumAge(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-deadline-cap", "team-deadline-cap-uid", now)
	admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "deadline-cap", "deadline-cap-request", []byte(`{"content":"deadline"}`))
	admission.Mutation.MaxAgeAt = now.Add(10 * time.Second)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: now.Add(2 * time.Second), LeaseDuration: time.Hour,
	})
	if err != nil {
		t.Fatalf("ClaimMemoryOperation: %v", err)
	}
	if claimed.Operation.LeaseExpiresAt == nil || !claimed.Operation.LeaseExpiresAt.Equal(admission.Mutation.MaxAgeAt) {
		t.Fatalf("lease expiry = %v, want max age %v", claimed.Operation.LeaseExpiresAt, admission.Mutation.MaxAgeAt)
	}
	sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claimed.Operation.LeaseOwner, LeaseEpoch: claimed.Operation.LeaseEpoch,
		Now: now.Add(3 * time.Second), RequestDeadline: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted: %v", err)
	}
	if sent.RequestDeadline == nil || !sent.RequestDeadline.Equal(admission.Mutation.MaxAgeAt.Add(-time.Nanosecond)) ||
		sent.LeaseExpiresAt == nil || !sent.LeaseExpiresAt.Equal(admission.Mutation.MaxAgeAt) {
		t.Fatalf("send deadlines were not capped at max age: %+v", sent)
	}
}

func TestMemoryOperationDeadlinesAreCappedAtBindingValidationExpiry(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 11, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-validation-cap", "team-validation-cap-uid", now)
	binding.ValidationExpiresAt = now.Add(10 * time.Second)
	refreshed, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: binding, ExpectedRoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "short validation window", Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("RefreshMemoryBackendBinding: %v", err)
	}
	binding = *refreshed
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(2*time.Second), "validation-cap", "validation-cap-request", []byte(`{"content":"deadline"}`)))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: now.Add(3 * time.Second), LeaseDuration: time.Hour,
	})
	if err != nil {
		t.Fatalf("ClaimMemoryOperation: %v", err)
	}
	if claimed.Operation.LeaseExpiresAt == nil || !claimed.Operation.LeaseExpiresAt.Equal(binding.ValidationExpiresAt) {
		t.Fatalf("lease expiry = %v, want validation expiry %v", claimed.Operation.LeaseExpiresAt, binding.ValidationExpiresAt)
	}
	sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claimed.Operation.LeaseOwner, LeaseEpoch: claimed.Operation.LeaseEpoch,
		Now: now.Add(4 * time.Second), RequestDeadline: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted: %v", err)
	}
	if sent.RequestDeadline == nil || !sent.RequestDeadline.Equal(binding.ValidationExpiresAt.Add(-time.Nanosecond)) ||
		sent.LeaseExpiresAt == nil || !sent.LeaseExpiresAt.Equal(binding.ValidationExpiresAt) {
		t.Fatalf("send deadlines were not capped at validation expiry: %+v", sent)
	}
	claimed.Operation = *sent
	completion := completionForDispatch(binding, *claimed, binding.ValidationExpiresAt.Add(-time.Second), "backend-v1", "remote-validation-cap")
	if _, err := s.CompleteMemoryOperation(ctx, completion); err != nil {
		t.Fatalf("CompleteMemoryOperation before validation expiry: %v", err)
	}
}

func TestCompleteMemoryOperationEnforcesDispatchTemporalFence(t *testing.T) {
	for _, test := range []struct {
		name       string
		slug       string
		completeAt func(time.Time) time.Time
		wantState  store.MemoryOperationState
	}{
		{
			name:       "just before deadline",
			slug:       "before",
			completeAt: func(deadline time.Time) time.Time { return deadline.Add(-time.Millisecond) },
			wantState:  store.MemoryOperationSucceeded,
		},
		{
			name:       "after deadline",
			slug:       "after",
			completeAt: func(deadline time.Time) time.Time { return deadline.Add(time.Millisecond) },
			wantState:  store.MemoryOperationDispatching,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 29, 11, 45, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-completion-fence-"+test.slug, "team-completion-fence-uid-"+test.slug, now)
			admitted, err := s.AdmitRemoteMemoryCreate(ctx,
				newRemoteCreateAdmission(binding, now.Add(time.Second), "completion-fence-"+test.slug, "completion-fence-request-"+test.slug, []byte(`{"content":"deadline"}`)))
			if err != nil {
				t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
			}
			claimed, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
				NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
				LeaseOwner: testMemoryDispatcher, Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
			})
			if err != nil {
				t.Fatalf("ClaimMemoryOperation: %v", err)
			}
			deadline := now.Add(10 * time.Second)
			sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
				NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
				LeaseOwner: claimed.Operation.LeaseOwner, LeaseEpoch: claimed.Operation.LeaseEpoch,
				Now: now.Add(3 * time.Second), RequestDeadline: deadline,
			})
			if err != nil {
				t.Fatalf("MarkMemoryOperationSendStarted: %v", err)
			}
			claimed.Operation = *sent
			completionAt := test.completeAt(deadline)
			completion := completionForDispatch(binding, *claimed, completionAt, "backend-v1", "remote-completion-fence")
			completed, err := s.CompleteMemoryOperation(ctx, completion)
			if test.wantState == store.MemoryOperationSucceeded {
				if err != nil {
					t.Fatalf("CompleteMemoryOperation just before deadline: %v", err)
				}
				if completed.State != store.MemoryOperationSucceeded {
					t.Fatalf("completed state = %q, want %q", completed.State, store.MemoryOperationSucceeded)
				}
				return
			}
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("CompleteMemoryOperation after deadline error = %v, want ErrConflict", err)
			}
			operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
			if err != nil {
				t.Fatalf("GetMemoryOperation after rejected completion: %v", err)
			}
			if operation.State != store.MemoryOperationDispatching || operation.ReceiptCompletedAt != nil ||
				operation.LeaseExpiresAt == nil || operation.RequestDeadline == nil {
				t.Fatalf("rejected completion mutated dispatch state: %+v", operation)
			}
			catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, admitted.Memory.ID)
			if err != nil {
				t.Fatalf("GetRemoteMemory after rejected completion: %v", err)
			}
			if catalog.PendingOperationID != admitted.Operation.ID || catalog.MaterializationState != store.MemoryMaterializationPending {
				t.Fatalf("rejected completion advanced catalog: %+v", catalog)
			}
			_, err = s.ClaimMemoryOperation(ctx, "missing-operation", store.MemoryOperationClaim{
				NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
				LeaseOwner: "recovery-dispatcher", Now: deadline.Add(2 * time.Millisecond), LeaseDuration: time.Minute,
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("maintenance claim error = %v, want ErrNotFound", err)
			}
			operation, err = s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
			if err != nil {
				t.Fatalf("GetMemoryOperation after recovery: %v", err)
			}
			if operation.State != store.MemoryOperationAmbiguous || operation.LeaseExpiresAt != nil || operation.RequestDeadline != nil {
				t.Fatalf("expired completion did not remain recoverable as ambiguous: %+v", operation)
			}
		})
	}
}

func TestCompleteMemoryOperationRejectsEitherExpiredDispatchDeadline(t *testing.T) {
	for _, test := range []struct {
		name   string
		expire func(context.Context, *Store, string, string, time.Time) error
	}{
		{
			name: "lease",
			expire: func(ctx context.Context, s *Store, namespaceUID, operationID string, expiresAt time.Time) error {
				_, err := s.db.ExecContext(ctx, `UPDATE memory_operations SET lease_expires_at = ? WHERE namespace_uid = ? AND id = ?`,
					expiresAt, namespaceUID, operationID)
				return err
			},
		},
		{
			name: "request deadline",
			expire: func(ctx context.Context, s *Store, namespaceUID, operationID string, expiresAt time.Time) error {
				_, err := s.db.ExecContext(ctx, `UPDATE memory_operations SET request_deadline = ? WHERE namespace_uid = ? AND id = ?`,
					expiresAt, namespaceUID, operationID)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 29, 11, 48, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-completion-expired-"+test.name, "team-completion-expired-uid-"+test.name, now)
			admitted, err := s.AdmitRemoteMemoryCreate(ctx,
				newRemoteCreateAdmission(binding, now.Add(time.Second), "completion-expired-"+test.name, "completion-expired-request-"+test.name, []byte(`{"content":"deadline"}`)))
			if err != nil {
				t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
			}
			dispatch := claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
			completionAt := now.Add(5 * time.Second)
			if err := test.expire(ctx, s, binding.NamespaceUID, admitted.Operation.ID, completionAt.Add(-time.Millisecond)); err != nil {
				t.Fatalf("expire %s: %v", test.name, err)
			}
			completion := completionForDispatch(binding, dispatch, completionAt, "backend-v1", "remote-completion-expired")
			if _, err := s.CompleteMemoryOperation(ctx, completion); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("CompleteMemoryOperation with expired %s error = %v, want ErrConflict", test.name, err)
			}
			operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
			if err != nil {
				t.Fatalf("GetMemoryOperation: %v", err)
			}
			if operation.State != store.MemoryOperationDispatching || operation.ReceiptCompletedAt != nil {
				t.Fatalf("expired %s completion mutated operation: %+v", test.name, operation)
			}
		})
	}
}

func TestCompleteMemoryOperationTemporalFenceIsInSQLCAS(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 11, 50, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-completion-cas", "team-completion-cas-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "completion-cas", "completion-cas-request", []byte(`{"content":"cas"}`)))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate: %v", err)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("ClaimMemoryOperation: %v", err)
	}
	sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claimed.Operation.LeaseOwner, LeaseEpoch: claimed.Operation.LeaseEpoch,
		Now: now.Add(3 * time.Second), RequestDeadline: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted: %v", err)
	}
	claimed.Operation = *sent
	if _, err := s.db.ExecContext(ctx, `CREATE TEMP TRIGGER expire_completion_dispatch_before_operation_cas
		AFTER UPDATE OF generation ON remote_memory_catalog
		WHEN NEW.namespace_uid = 'team-completion-cas-uid' AND NEW.id = 'mem-completion-cas'
		BEGIN
			UPDATE memory_operations
			SET lease_expires_at = NEW.updated_at, request_deadline = NEW.updated_at
			WHERE namespace_uid = NEW.namespace_uid AND id = 'mop-completion-cas';
		END`); err != nil {
		t.Fatalf("create completion expiry trigger: %v", err)
	}
	completionAt := now.Add(5 * time.Second)
	completion := completionForDispatch(binding, *claimed, completionAt, "backend-v1", "remote-completion-cas")
	if _, err := s.CompleteMemoryOperation(ctx, completion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CompleteMemoryOperation raced expiry error = %v, want ErrConflict", err)
	}
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
	if err != nil {
		t.Fatalf("GetMemoryOperation after CAS rejection: %v", err)
	}
	if operation.State != store.MemoryOperationDispatching || operation.ReceiptCompletedAt != nil ||
		operation.LeaseExpiresAt == nil || !operation.LeaseExpiresAt.After(completionAt) ||
		operation.RequestDeadline == nil || !operation.RequestDeadline.After(completionAt) {
		t.Fatalf("CAS rejection did not roll back completion transaction: %+v", operation)
	}
	catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, admitted.Memory.ID)
	if err != nil {
		t.Fatalf("GetRemoteMemory after CAS rejection: %v", err)
	}
	if catalog.PendingOperationID != admitted.Operation.ID || catalog.MaterializationState != store.MemoryMaterializationPending {
		t.Fatalf("CAS rejection advanced catalog: %+v", catalog)
	}
}

func TestExpiryMaintenanceWaitsForLeaseAndRequestDeadlines(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-expiry-barrier", "team-expiry-barrier-uid", now)
	claimAt := func(at time.Time, id string) error {
		_, err := s.ClaimMemoryOperation(ctx, id, store.MemoryOperationClaim{
			NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
			AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
			LeaseOwner: "maintenance-dispatcher", Now: at, LeaseDuration: time.Second,
		})
		return err
	}

	leasedAdmission := newRemoteCreateAdmission(binding, now.Add(time.Second), "lease-expiry-barrier", "lease-expiry-request", []byte(`{"content":"lease"}`))
	leasedAdmission.Mutation.MaxAgeAt = now.Add(3 * time.Second)
	leased, err := s.AdmitRemoteMemoryCreate(ctx, leasedAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate leased: %v", err)
	}
	if err := claimAt(now.Add(2*time.Second), leased.Operation.ID); err != nil {
		t.Fatalf("initial leased claim: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_operations SET lease_expires_at = ? WHERE namespace_uid = ? AND id = ?`,
		now.Add(6*time.Second), binding.NamespaceUID, leased.Operation.ID); err != nil {
		t.Fatalf("extend historical lease deadline: %v", err)
	}
	if err := claimAt(now.Add(4*time.Second), leased.Operation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim during active lease error = %v, want ErrNotFound", err)
	}
	leasedState, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, leased.Operation.ID)
	if err != nil || leasedState.State != store.MemoryOperationLeased {
		t.Fatalf("max-age maintenance crossed active lease: %+v, %v", leasedState, err)
	}
	if err := claimAt(now.Add(7*time.Second), leased.Operation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim after lease deadline error = %v, want ErrNotFound", err)
	}
	leasedState, err = s.GetMemoryOperation(ctx, binding.NamespaceUID, leased.Operation.ID)
	if err != nil || leasedState.State != store.MemoryOperationDeadLettered {
		t.Fatalf("expired leased operation state = %+v, %v", leasedState, err)
	}

	dispatchBase := now.Add(20 * time.Second)
	dispatchAdmission := newRemoteCreateAdmission(binding, dispatchBase, "request-expiry-barrier", "request-expiry-request", []byte(`{"content":"dispatch"}`))
	dispatchAdmission.Mutation.MaxAgeAt = dispatchBase.Add(3 * time.Second)
	dispatching, err := s.AdmitRemoteMemoryCreate(ctx, dispatchAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate dispatching: %v", err)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, dispatching.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: dispatchBase.Add(time.Second), LeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatalf("ClaimMemoryOperation dispatching: %v", err)
	}
	if _, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: dispatching.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claimed.Operation.LeaseOwner, LeaseEpoch: claimed.Operation.LeaseEpoch,
		Now: dispatchBase.Add(1500 * time.Millisecond), RequestDeadline: dispatchBase.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_operations SET lease_expires_at = ?, request_deadline = ?
		WHERE namespace_uid = ? AND id = ?`, dispatchBase.Add(2*time.Second), dispatchBase.Add(6*time.Second),
		binding.NamespaceUID, dispatching.Operation.ID); err != nil {
		t.Fatalf("extend historical request deadline: %v", err)
	}
	if err := claimAt(dispatchBase.Add(4*time.Second), dispatching.Operation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim during active request error = %v, want ErrNotFound", err)
	}
	dispatchState, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, dispatching.Operation.ID)
	if err != nil || dispatchState.State != store.MemoryOperationDispatching {
		t.Fatalf("max-age maintenance crossed active request: %+v, %v", dispatchState, err)
	}
	if err := claimAt(dispatchBase.Add(7*time.Second), dispatching.Operation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim after request deadline error = %v, want ErrNotFound", err)
	}
	dispatchState, err = s.GetMemoryOperation(ctx, binding.NamespaceUID, dispatching.Operation.ID)
	if err != nil || dispatchState.State != store.MemoryOperationDeadLettered {
		t.Fatalf("expired dispatching operation state = %+v, %v", dispatchState, err)
	}
}

func TestClaimMaintenanceCommitsBeforeNotFound(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-maintenance", "team-maintenance-uid", now)
	expiredAdmission := newRemoteCreateAdmission(binding, now.Add(time.Second), "expired", "expired-request", []byte(`{"content":"expired"}`))
	expiredAdmission.Mutation.MaxAgeAt = now.Add(2 * time.Second)
	expired, err := s.AdmitRemoteMemoryCreate(ctx, expiredAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate expired: %v", err)
	}
	claim := store.MemoryOperationClaim{NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: now.Add(3 * time.Second), LeaseDuration: time.Minute}
	if _, err := s.ClaimMemoryOperation(ctx, expired.Operation.ID, claim); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ClaimMemoryOperation expired error = %v, want ErrNotFound", err)
	}
	expiredState, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, expired.Operation.ID)
	if err != nil || expiredState.State != store.MemoryOperationDeadLettered {
		t.Fatalf("expired maintenance state = %+v, %v", expiredState, err)
	}
	leasedAdmission := newRemoteCreateAdmission(binding, now.Add(4*time.Second), "leased", "leased-request", []byte(`{"content":"leased"}`))
	leased, err := s.AdmitRemoteMemoryCreate(ctx, leasedAdmission)
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate leased: %v", err)
	}
	if _, err := s.ClaimMemoryOperation(ctx, leased.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: testMemoryDispatcher, Now: now.Add(5 * time.Second), LeaseDuration: time.Second,
	}); err != nil {
		t.Fatalf("ClaimMemoryOperation leased: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_backend_bindings SET state = ? WHERE namespace_uid = ?`,
		store.MemoryBackendBindingDraining, binding.NamespaceUID); err != nil {
		t.Fatalf("set draining test barrier: %v", err)
	}
	claim.Now = now.Add(7 * time.Second)
	if _, err := s.ClaimMemoryOperation(ctx, "missing-operation", claim); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ClaimMemoryOperation draining maintenance error = %v, want ErrNotFound", err)
	}
	recovered, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, leased.Operation.ID)
	if err != nil || recovered.State != store.MemoryOperationQueued {
		t.Fatalf("recovered maintenance state = %+v, %v", recovered, err)
	}
}

func TestClaimMaintenanceUsesActualNowAfterValidationExpiry(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-expired-validation", "team-expired-validation-uid", now)
	admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "expired-validation", "expired-validation-request", []byte(`{"content":"expired"}`))
	admission.Mutation.MaxAgeAt = now.Add(2 * time.Second)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_backend_bindings SET validation_expires_at = ? WHERE namespace_uid = ?`,
		now.Add(1500*time.Millisecond), binding.NamespaceUID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: now.Add(3 * time.Second), LeaseDuration: time.Minute,
		AllowExpiredValidationMaintenance: true,
	})
	if !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("ClaimMemoryOperation() error = %v, want ErrNotReady", err)
	}
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
	if err != nil || operation.State != store.MemoryOperationDeadLettered {
		t.Fatalf("operation = %#v, err = %v", operation, err)
	}
}

func TestPreAdmittedMutationsSendAndCompleteWhileDrainingButNewAdmissionIsClosed(t *testing.T) {
	for _, kind := range []store.MemoryOperationKind{
		store.MemoryOperationCreate,
		store.MemoryOperationReplace,
		store.MemoryOperationDelete,
	} {
		t.Run(string(kind), func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-drain-"+string(kind), "team-drain-uid-"+string(kind), now)

			var admitted *store.MemoryMutationAdmissionResult
			var untouched *store.MemoryMutationAdmissionResult
			switch kind {
			case store.MemoryOperationCreate:
				var err error
				admitted, err = s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(
					binding, now.Add(time.Second), "drain-create", "drain-create-request", []byte(`{"content":"create"}`),
				))
				if err != nil {
					t.Fatal(err)
				}
			case store.MemoryOperationReplace, store.MemoryOperationDelete:
				current := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "drain-current-"+string(kind))
				untouched = materializedRemoteMemoryForTest(t, s, binding, now.Add(5*time.Second), "drain-untouched-"+string(kind))
				var err error
				if kind == store.MemoryOperationReplace {
					mutation := newCanonicalMemoryMutation(binding, now.Add(9*time.Second), current.Memory.ID, "drain-replace", "drain-replace-request",
						store.MemoryOperationReplace, 2, 1, "backend-v1", "replace")
					admitted, err = s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
						Mutation: mutation, Memory: store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{"replace"}},
						ExpectedGeneration: current.Operation.DesiredGeneration, ExpectedBackendVersion: "backend-v1",
					})
				} else {
					mutation := newCanonicalMemoryMutation(binding, now.Add(9*time.Second), current.Memory.ID, "drain-delete", "drain-delete-request",
						store.MemoryOperationDelete, 2, 1, "backend-v1", "")
					admitted, err = s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
						Mutation: mutation, ExpectedGeneration: current.Operation.DesiredGeneration, ExpectedBackendVersion: "backend-v1",
					})
				}
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unsupported test kind %q", kind)
			}

			draining, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
				NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
				ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
				ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch,
				Actor: "operator", Reason: "drain accepted operations", Now: now.Add(10 * time.Second),
			})
			if err != nil {
				t.Fatal(err)
			}

			switch kind {
			case store.MemoryOperationCreate:
				_, err = s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(
					*draining, now.Add(11*time.Second), "drain-new-create", "drain-new-create-request", []byte(`{"content":"new"}`),
				))
			case store.MemoryOperationReplace:
				mutation := newCanonicalMemoryMutation(*draining, now.Add(11*time.Second), untouched.Memory.ID, "drain-new-replace", "drain-new-replace-request",
					store.MemoryOperationReplace, 2, 1, "backend-v1", "new")
				_, err = s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
					Mutation: mutation, Memory: store.RemoteMemoryCatalogEntry{Source: "manual"},
					ExpectedGeneration: untouched.Operation.DesiredGeneration, ExpectedBackendVersion: "backend-v1",
				})
			case store.MemoryOperationDelete:
				mutation := newCanonicalMemoryMutation(*draining, now.Add(11*time.Second), untouched.Memory.ID, "drain-new-delete", "drain-new-delete-request",
					store.MemoryOperationDelete, 2, 1, "backend-v1", "")
				_, err = s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
					Mutation: mutation, ExpectedGeneration: untouched.Operation.DesiredGeneration, ExpectedBackendVersion: "backend-v1",
				})
			}
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("new %s admission while draining error = %v, want ErrConflict", kind, err)
			}

			dispatch := claimAndSendExactOperationForTest(t, s, *draining, admitted.Operation, now.Add(12*time.Second))
			completion := completionForDispatch(binding, dispatch, now.Add(14*time.Second), "backend-drained", "remote-drained-"+string(kind))
			if _, err := s.CompleteMemoryOperation(ctx, completion); err != nil {
				t.Fatalf("CompleteMemoryOperation(%s) while draining: %v", kind, err)
			}
		})
	}
}

func TestOrdinaryMutationsClearProposalIdentity(t *testing.T) {
	t.Run("replace", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		now := time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC)
		binding := activateMemoryBackendForTest(t, s, "team-proposal-spoof-replace", "team-proposal-spoof-replace-uid", now)
		current := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "proposal-spoof-replace-current")
		proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "unrelated proposal")
		mutation := newCanonicalMemoryMutation(binding, now.Add(5*time.Second), current.Memory.ID, "proposal-spoof-replace", "proposal-spoof-replace-request",
			store.MemoryOperationReplace, 2, 1, "backend-v1", "replace")
		mutation.ProposalID = proposal.ID
		admitted, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
			Mutation:           mutation,
			Memory:             store.RemoteMemoryCatalogEntry{Source: "manual", SourceProposalID: proposal.ID, Tags: []string{"replace"}},
			ExpectedGeneration: current.Operation.DesiredGeneration, ExpectedBackendVersion: "backend-v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if admitted.Operation.ProposalID != "" || admitted.Idempotency.OperationID != admitted.Operation.ID {
			t.Fatalf("ordinary replace retained proposal identity: %#v", admitted)
		}
		completeOperationForTest(t, s, binding, admitted.Operation, now.Add(6*time.Second), "backend-v2", "remote-replaced")
		unchanged, err := s.GetMemoryProposal(ctx, binding.Namespace, proposal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Status != proposalStatusAccepted || unchanged.ApplyOperationID != "" || unchanged.AppliedMemoryID != "" {
			t.Fatalf("ordinary replace changed proposal = %#v", unchanged)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
		binding := activateMemoryBackendForTest(t, s, "team-proposal-spoof-delete", "team-proposal-spoof-delete-uid", now)
		current := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "proposal-spoof-delete-current")
		proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "unrelated proposal")
		mutation := newCanonicalMemoryMutation(binding, now.Add(5*time.Second), current.Memory.ID, "proposal-spoof-delete", "proposal-spoof-delete-request",
			store.MemoryOperationDelete, 2, 1, "backend-v1", "")
		mutation.ProposalID = proposal.ID
		admitted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
			Mutation: mutation, ExpectedGeneration: current.Operation.DesiredGeneration, ExpectedBackendVersion: "backend-v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if admitted.Operation.ProposalID != "" {
			t.Fatalf("ordinary delete retained proposal identity: %#v", admitted.Operation)
		}
		completeOperationForTest(t, s, binding, admitted.Operation, now.Add(6*time.Second), "backend-v2", "remote-deleted")
		unchanged, err := s.GetMemoryProposal(ctx, binding.Namespace, proposal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Status != proposalStatusAccepted || unchanged.ApplyOperationID != "" || unchanged.AppliedMemoryID != "" {
			t.Fatalf("ordinary delete changed proposal = %#v", unchanged)
		}
	})
}

func TestStateOnlyDrainingBarrierClaimsPreviouslyAcceptedWorkAtOldEpoch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-state-only-drain", "team-state-only-drain-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(
		binding, now.Add(time.Second), "state-only-drain", "state-only-drain-request", []byte(`{"content":"queued"}`),
	))
	if err != nil {
		t.Fatal(err)
	}
	draining, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "install admission barrier before drain", Now: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.RoutingEpoch != binding.RoutingEpoch || draining.State != store.MemoryBackendBindingDraining {
		t.Fatalf("draining binding = %#v", draining)
	}
	claimed, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: testMemoryDispatcher, Now: now.Add(3 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil || claimed.Operation.Kind != store.MemoryOperationCreate {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
}

func TestBindingHelpersAndGenericLegacyTransitionRejected(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	first := activateMemoryBackendForTest(t, s, "team-helper-000", "team-helper-uid-000", now)
	for i := 1; i <= 200; i++ {
		activateMemoryBackendForTest(t, s, fmt.Sprintf("team-helper-%03d", i), fmt.Sprintf("team-helper-uid-%03d", i), now)
	}
	byName, err := s.GetMemoryBackendBindingByNamespace(ctx, first.Namespace)
	if err != nil || byName.NamespaceUID != first.NamespaceUID {
		t.Fatalf("GetMemoryBackendBindingByNamespace = %+v, %v", byName, err)
	}
	seen := 0
	if err := s.ForEachMemoryBackendBinding(ctx, store.MemoryBackendBindingFilter{Modes: []store.MemoryBackendMode{store.MemoryBackendModeRemote}},
		func(store.MemoryBackendBinding) error { seen++; return nil }); err != nil || seen != 201 {
		t.Fatalf("ForEachMemoryBackendBinding seen = %d, err = %v", seen, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_backend_bindings SET minimum_feature_epoch = 7 WHERE namespace_uid = ?`, first.NamespaceUID); err != nil {
		t.Fatalf("update feature epoch: %v", err)
	}
	maximum, err := s.MaxRequiredMemoryFeatureEpoch(ctx)
	if err != nil || maximum != 7 {
		t.Fatalf("MaxRequiredMemoryFeatureEpoch = %d, %v", maximum, err)
	}
	decommissioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: first.NamespaceUID, BackendUID: first.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: first.RoutingEpoch, RoutingEpoch: first.RoutingEpoch + 1,
		Actor: "operator", Reason: "decommission", Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("decommission: %v", err)
	}
	if _, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: first.NamespaceUID, BackendUID: first.BackendUID,
		ExpectedState: store.MemoryBackendBindingDecommissioned, State: store.MemoryBackendBindingLegacy,
		ExpectedRoutingEpoch: decommissioned.RoutingEpoch, RoutingEpoch: decommissioned.RoutingEpoch + 1,
		Actor: "operator", Reason: "generic restore", Now: now.Add(2 * time.Second),
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("generic decommissioned-to-legacy transition error = %v, want ErrValidation", err)
	}
}

func TestProposalApplySupersessionAbandonsProposal(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 19, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-proposal-delete", "team-proposal-delete-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "proposal")
	applied, err := s.AdmitRemoteMemoryProposalApply(ctx, newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "proposal-delete"))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryProposalApply: %v", err)
	}
	if _, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(2*time.Second), applied.Memory.ID, "proposal-delete-tombstone", "proposal-delete-request",
			store.MemoryOperationDelete, 2, 0, "", ""),
		ExpectedGeneration: 0,
	}); err != nil {
		t.Fatalf("AdmitRemoteMemoryDelete: %v", err)
	}
	superseded, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, applied.Operation.ID)
	if err != nil || superseded.State != store.MemoryOperationSuperseded {
		t.Fatalf("superseded proposal operation = %+v, %v", superseded, err)
	}
	abandoned, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
	if err != nil || abandoned.Status != proposalStatusApplicationAbandoned || abandoned.ApplicationAbandonedAt == nil {
		t.Fatalf("superseded proposal = %+v, %v", abandoned, err)
	}
}

func TestAdmissionStrictlyBindsCanonicalMutationEnvelope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.RemoteMemoryCreateAdmission)
	}{
		{
			name: "operation id",
			mutate: func(admission *store.RemoteMemoryCreateAdmission) {
				envelope, _ := protocol.DecodeMutationEnvelope(admission.Mutation.Payload)
				envelope.OperationID = "mop-different-operation"
				_ = protocol.PrepareMutation(envelope)
				admission.Mutation.Payload, _ = protocol.EncodeJSON(envelope)
			},
		},
		{
			name: "memory id",
			mutate: func(admission *store.RemoteMemoryCreateAdmission) {
				envelope, _ := protocol.DecodeMutationEnvelope(admission.Mutation.Payload)
				envelope.MemoryID = "mem-different-memory"
				envelope.UpsertKey = ""
				_ = protocol.PrepareMutation(envelope)
				admission.Mutation.Payload, _ = protocol.EncodeJSON(envelope)
			},
		},
		{
			name: "binding",
			mutate: func(admission *store.RemoteMemoryCreateAdmission) {
				envelope, _ := protocol.DecodeMutationEnvelope(admission.Mutation.Payload)
				envelope.Binding.StoreUUID = "22222222-2222-4222-8222-222222222222"
				_ = protocol.PrepareMutation(envelope)
				admission.Mutation.Payload, _ = protocol.EncodeJSON(envelope)
			},
		},
		{
			name: "generation",
			mutate: func(admission *store.RemoteMemoryCreateAdmission) {
				envelope, _ := protocol.DecodeMutationEnvelope(admission.Mutation.Payload)
				envelope.Generation = 2
				_ = protocol.PrepareMutation(envelope)
				admission.Mutation.Payload, _ = protocol.EncodeJSON(envelope)
			},
		},
		{
			name: "mutation digest",
			mutate: func(admission *store.RemoteMemoryCreateAdmission) {
				admission.Mutation.MutationDigest = protocol.ContentDigest("different mutation")
			},
		},
		{
			name: "content digest",
			mutate: func(admission *store.RemoteMemoryCreateAdmission) {
				admission.Mutation.ContentDigest = protocol.ContentDigest("different content")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-envelope-"+tt.name, "team-envelope-uid-"+tt.name, now)
			admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "envelope-"+tt.name, "request-"+tt.name, []byte("content"))
			tt.mutate(&admission)
			if _, err := s.AdmitRemoteMemoryCreate(context.Background(), admission); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("AdmitRemoteMemoryCreate error = %v, want ErrValidation", err)
			}
			var catalogRows, operationRows int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM remote_memory_catalog WHERE namespace_uid = ?`, binding.NamespaceUID).Scan(&catalogRows); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_operations WHERE namespace_uid = ?`, binding.NamespaceUID).Scan(&operationRows); err != nil {
				t.Fatal(err)
			}
			if catalogRows != 0 || operationRows != 0 {
				t.Fatalf("invalid envelope committed catalog=%d operations=%d", catalogRows, operationRows)
			}
		})
	}
}

func TestReplaceAdmissionCrossChecksExpectedGenerationAndVersionEnvelope(t *testing.T) {
	for _, field := range []string{"expected generation", "expected backend version"} {
		t.Run(field, func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Date(2026, 7, 30, 8, 30, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-replace-envelope-"+field, "team-replace-envelope-uid-"+field, now)
			current := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "replace-envelope-"+field)
			mutation := newCanonicalMemoryMutation(binding, now.Add(4*time.Second), current.Memory.ID, "replace-mismatch-"+field,
				"replace-request-"+field, store.MemoryOperationReplace, 2, 1, "backend-v1", "v2")
			envelope, err := protocol.DecodeMutationEnvelope(mutation.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if field == "expected generation" {
				envelope.ExpectedGeneration = 2
				envelope.Generation = 3
			} else {
				envelope.ExpectedBackendVersion = "different-version"
			}
			if err := protocol.PrepareMutation(envelope); err != nil {
				t.Fatal(err)
			}
			mutation.Payload, _ = protocol.EncodeJSON(envelope)
			if _, err := s.AdmitRemoteMemoryReplace(context.Background(), store.RemoteMemoryReplaceAdmission{
				Mutation: mutation, Memory: store.RemoteMemoryCatalogEntry{Source: "manual"},
				ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
			}); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("replace mismatch error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestIdempotencyReplayUsesImmutableSnapshotAndExtendsTerminalExpiry(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-idem-snapshot", "team-idem-snapshot-uid", now)
	createAdmission := newRemoteCreateAdmission(binding, now.Add(time.Second), "idem-snapshot-create", "idem-snapshot-request", []byte("v1"))
	createAdmission.Mutation.IdempotencyExpiresAt = now.Add(24 * time.Hour)
	created, err := s.AdmitRemoteMemoryCreate(ctx, createAdmission)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, created.Operation, now.Add(2*time.Second))
	completionAt := now.Add(4 * time.Second)
	completion := completionForDispatch(binding, dispatch, completionAt, "backend-v1", "remote-idem-snapshot")
	completion.FinalizeIdempotencyOutcome = true
	completion.IdempotencyOutcome = store.MemoryIdempotencyOutcome{
		Status: 201, ResponseType: store.MemoryIdempotencyMemory,
		Location: "/api/v1/memories/" + created.Memory.ID, ResponseDigest: "response-v1",
	}
	if _, err := s.CompleteMemoryOperation(ctx, completion); err != nil {
		t.Fatal(err)
	}
	materialized, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), materialized.ID, "idem-snapshot-replace", "idem-snapshot-replace-request",
			store.MemoryOperationReplace, 2, 1, "backend-v1", "v2"),
		Memory: store.RemoteMemoryCatalogEntry{Source: "manual"}, ExpectedGeneration: 1, ExpectedBackendVersion: "backend-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, replace.Operation, now.Add(6*time.Second), "backend-v2", "remote-idem-snapshot")

	replayed, err := s.AdmitRemoteMemoryCreate(ctx, createAdmission)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Memory.Generation != 1 || replayed.Memory.DesiredGeneration != 1 ||
		replayed.Operation.State != store.MemoryOperationSucceeded || replayed.Operation.DesiredGeneration != 1 {
		t.Fatalf("immutable replay = %+v", replayed)
	}
	if len(replayed.Idempotency.ResponseSnapshot) == 0 || replayed.Idempotency.OriginalStatus != 201 ||
		replayed.Idempotency.ExpiresAt.Before(completionAt.Add(minimumIdempotencyRetention)) {
		t.Fatalf("idempotency outcome = %+v", replayed.Idempotency)
	}
	var snapshot memoryIdempotencySnapshot
	if err := json.Unmarshal(replayed.Idempotency.ResponseSnapshot, &snapshot); err != nil {
		t.Fatalf("decode idempotency response snapshot: %v", err)
	}
	if string(snapshot.Content) != "v1" {
		t.Fatalf("idempotency response content = %q, want original v1", snapshot.Content)
	}
}

func TestExpiredIdempotencyKeyIsAtomicallyReplaceable(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 9, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-idem-expiry", "team-idem-expiry-uid", now)
	first := newRemoteCreateAdmission(binding, now.Add(time.Second), "idem-expiry-a", "request-a", []byte("a"))
	first.Mutation.MaxAgeAt = now.Add(2 * time.Second)
	first.Mutation.IdempotencyExpiresAt = first.Mutation.MaxAgeAt
	firstAdmission, err := s.AdmitRemoteMemoryCreate(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	second := newRemoteCreateAdmission(binding, now.Add(3*time.Second), "idem-expiry-b", "request-b", []byte("b"))
	second.Mutation.IdempotencyKey = first.Mutation.IdempotencyKey
	if _, err := s.AdmitRemoteMemoryCreate(context.Background(), second); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("unresolved operation allowed expired key reuse: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memory_operations SET state = ?, completed_at = ?, updated_at = ?
		WHERE namespace_uid = ? AND id = ?`, store.MemoryOperationAbandoned, now.Add(3*time.Second),
		now.Add(3*time.Second), binding.NamespaceUID, firstAdmission.Operation.ID); err != nil {
		t.Fatal(err)
	}
	second.Mutation.Now = now.Add(4 * time.Second)
	second.Mutation.MaxAgeAt = second.Mutation.Now.Add(24 * time.Hour)
	second.Mutation.IdempotencyExpiresAt = second.Mutation.Now.Add(30 * 24 * time.Hour)
	admitted, err := s.AdmitRemoteMemoryCreate(context.Background(), second)
	if err != nil {
		t.Fatalf("reuse expired terminal idempotency key: %v", err)
	}
	if admitted.Replayed || admitted.Memory.ID == first.Mutation.MemoryID {
		t.Fatalf("expired terminal key replayed stale outcome: %+v", admitted)
	}
}

func TestDependencyFailuresDoNotConsumeSemanticAttemptBudgetOrExtendLease(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-dependency-attempt", "team-dependency-attempt-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "dependency-attempt", "dependency-attempt-request", []byte("v1")))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "dispatcher-a", Now: now.Add(2 * time.Second), LeaseDuration: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseExpiry := *claim.Operation.LeaseExpiresAt
	sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch,
		Now: now.Add(3 * time.Second), RequestDeadline: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Dispatches != 1 || sent.Attempts != 0 || sent.LeaseExpiresAt == nil || !sent.LeaseExpiresAt.Equal(leaseExpiry) ||
		sent.RequestDeadline == nil || sent.RequestDeadline.Sub(now.Add(3*time.Second)) > 60*time.Second ||
		!sent.RequestDeadline.Before(leaseExpiry) {
		t.Fatalf("send accounting/deadlines = %+v", sent)
	}
	retried, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: sent.LeaseOwner, LeaseEpoch: sent.LeaseEpoch, Ambiguous: true, DependencyFailure: true,
		ErrorCode: "DNS_OUTAGE", ErrorMessage: "dependency unavailable", Now: now.Add(4 * time.Second),
	})
	if err != nil || retried.Attempts != 0 || retried.State != store.MemoryOperationAmbiguous {
		t.Fatalf("dependency retry = %+v, %v", retried, err)
	}
	claim, err = s.ClaimMemoryOperation(ctx, admitted.Operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: "dispatcher-b", Now: now.Add(5 * time.Second), LeaseDuration: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	sent, err = s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch,
		Now: now.Add(6 * time.Second), RequestDeadline: now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: sent.LeaseOwner, LeaseEpoch: sent.LeaseEpoch, Ambiguous: true,
		ErrorCode: "RECORD_CONFLICT", ErrorMessage: "operation-specific failure", Now: now.Add(7 * time.Second),
	})
	if err != nil || semantic.Dispatches != 2 || semantic.Attempts != 1 {
		t.Fatalf("semantic retry = %+v, %v", semantic, err)
	}
}

func TestRetryMemoryOperationRejectsReplayedAmbiguousCallback(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 10, 15, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-retry-replay", "team-retry-replay-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx,
		newRemoteCreateAdmission(binding, now.Add(time.Second), "retry-replay", "retry-replay-request", []byte("v1")))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
	retry := store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		Ambiguous: true, ErrorCode: "UNKNOWN", ErrorMessage: "provider outcome unknown", Now: now.Add(4 * time.Second),
	}
	ambiguous, err := s.RetryMemoryOperation(ctx, retry)
	if err != nil {
		t.Fatalf("first ambiguous callback: %v", err)
	}
	if ambiguous.State != store.MemoryOperationAmbiguous || ambiguous.Attempts != 1 || ambiguous.Dispatches != 1 {
		t.Fatalf("first ambiguous callback = %+v", ambiguous)
	}

	retry.Now = now.Add(5 * time.Second)
	if _, err := s.RetryMemoryOperation(ctx, retry); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replayed ambiguous callback error = %v, want ErrConflict", err)
	}
	unchanged, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, admitted.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != store.MemoryOperationAmbiguous || unchanged.Attempts != 1 || unchanged.Dispatches != 1 {
		t.Fatalf("replayed callback changed operation: %+v", unchanged)
	}

	secondDispatch := claimAndSendOperationForTest(t, s, binding, *unchanged, now.Add(6*time.Second))
	second, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: secondDispatch.Operation.LeaseOwner, LeaseEpoch: secondDispatch.Operation.LeaseEpoch,
		Ambiguous: true, ErrorCode: "UNKNOWN", ErrorMessage: "second provider outcome unknown", Now: now.Add(8 * time.Second),
	})
	if err != nil {
		t.Fatalf("fresh ambiguous callback: %v", err)
	}
	if second.State != store.MemoryOperationAmbiguous || second.Attempts != 2 || second.Dispatches != 2 {
		t.Fatalf("fresh ambiguous callback = %+v", second)
	}
}

func TestTrustReviewedIsProposalOnlyAndDisabledNoOpRequiresCAS(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-governance-cas", "team-governance-cas-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "governance-cas")
	current, err := s.GetRemoteMemory(context.Background(), binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemoteMemoryTrust(context.Background(), store.RemoteMemoryTrustChange{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, Trust: store.MemoryTrustReviewed,
		ExpectedGovernanceRevision: current.GovernanceRevision, Actor: "operator", Reason: "not a proposal", Now: now.Add(4 * time.Second),
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("direct reviewed trust error = %v, want ErrValidation", err)
	}
	if _, err := s.SetRemoteMemoryDisabled(context.Background(), store.RemoteMemoryDisabledChange{
		NamespaceUID: binding.NamespaceUID, ID: current.ID, Disabled: current.Disabled,
		ExpectedGovernanceRevision: current.GovernanceRevision + 1, Actor: "operator", Reason: "stale no-op", Now: now.Add(5 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("disabled stale no-op error = %v, want ErrConflict", err)
	}
}

func TestRestoreApplyRequiresFreshIdentityBoundPreview(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	legacy := &store.Memory{Namespace: "team-restore-preview", Content: "legacy"}
	if err := s.CreateMemory(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	binding := activateMemoryBackendForTest(t, s, legacy.Namespace, "team-restore-preview-uid", now)
	decommissioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "preview restore", Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewLegacyMemoryRestore(ctx, binding.NamespaceUID, binding.BackendUID)
	if err != nil || !preview.Restorable || preview.PreviewDigest == "" {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if _, err := s.db.Exec(`UPDATE memory_backend_bindings SET authority_epoch = authority_epoch + 1,
		routing_epoch = routing_epoch + 1, tenant_id = 'rotated-tenant', store_uuid = '22222222-2222-4222-8222-222222222222'
		WHERE namespace_uid = ?`, binding.NamespaceUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedAuthorityEpoch: decommissioned.AuthorityEpoch, ExpectedRoutingEpoch: decommissioned.RoutingEpoch,
		ExpectedTenantID: decommissioned.TenantID, ExpectedStoreName: decommissioned.StoreName,
		ExpectedStoreUUID: decommissioned.StoreUUID, PreviewDigest: preview.PreviewDigest,
		Actor: "operator", Reason: "delayed restore", Now: now.Add(2 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale preview restore error = %v, want ErrConflict", err)
	}
	if _, err := s.GetMemory(ctx, legacy.Namespace, legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale preview resurrected legacy memory: %v", err)
	}
}

func TestIncompleteGovernancePaginationTuplesAreRejected(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 11, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-pagination-tuples", "team-pagination-tuples-uid", now)
	for name, run := range map[string]func() error{
		"catalog timestamp only": func() error {
			_, err := s.ListRemoteMemories(context.Background(), store.RemoteMemoryCatalogFilter{NamespaceUID: binding.NamespaceUID, BeforeUpdatedAt: &now})
			return err
		},
		"catalog id only": func() error {
			_, err := s.ListRemoteMemories(context.Background(), store.RemoteMemoryCatalogFilter{NamespaceUID: binding.NamespaceUID, BeforeID: "mem-x"})
			return err
		},
		"operation timestamp only": func() error {
			_, err := s.ListMemoryOperations(context.Background(), store.MemoryOperationFilter{NamespaceUID: binding.NamespaceUID, BeforeCreatedAt: &now})
			return err
		},
		"operation sequence only": func() error {
			_, err := s.ListMemoryOperations(context.Background(), store.MemoryOperationFilter{NamespaceUID: binding.NamespaceUID, BeforeSequence: 1})
			return err
		},
		"audit timestamp only": func() error {
			_, err := s.ListMemoryAudit(context.Background(), store.MemoryAuditFilter{NamespaceUID: binding.NamespaceUID, BeforeCreatedAt: &now})
			return err
		},
		"audit id only": func() error {
			_, err := s.ListMemoryAudit(context.Background(), store.MemoryAuditFilter{NamespaceUID: binding.NamespaceUID, BeforeID: "maudit-x"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("pagination error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestForceOrphanTerminalizesUncertainProposalAsOrphaned(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-proposal-orphan", "team-proposal-orphan-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "uncertain proposal")
	admitted, err := s.AdmitRemoteMemoryProposalApply(ctx,
		newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "proposal-orphan"))
	if err != nil {
		t.Fatal(err)
	}
	_ = claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
	draining, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "egress barrier", Now: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrphanMemoryOperations(ctx, store.MemoryOperationOrphaning{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: draining.RoutingEpoch,
		Actor: "operator", Reason: "provider outcome uncertain", Now: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != proposalStatusApplicationOrphaned || got.ApplicationAbandonedAt == nil || got.ApplicationAbandonedBy == "" {
		t.Fatalf("uncertain proposal was not terminally orphaned: %+v", got)
	}
}

func TestVerifiedCheckpointPurgesCoveredPayloadReceiptsAndAudit(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-checkpoint-purge", "team-checkpoint-purge-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "checkpoint-purge")
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if operation.PayloadBytes == 0 || operation.ReceiptBindingDigest == "" {
		t.Fatalf("operation missing retained redo/receipt before purge: %+v", operation)
	}
	if _, err := s.db.Exec(`DELETE FROM memory_audit WHERE namespace_uid = ?`, binding.NamespaceUID); err == nil {
		t.Fatal("direct memory audit deletion unexpectedly succeeded")
	}
	checkpointAt := now.Add(8 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-purge", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: operation.Sequence, CheckpointDigest: protocol.ContentDigest("checkpoint"),
		Actor: "operator", Reason: "verified matched checkpoint", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint: %v", err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: operation.Sequence,
		Before: now.Add(24 * time.Hour), PurgePayloads: true, PurgeReceipts: true, PurgeAudit: true,
		Actor: "operator", Reason: "retention policy", Now: checkpointAt,
	})
	if err != nil {
		t.Fatalf("PurgeMemoryGovernance: %v", err)
	}
	if purged.PayloadsPurged != 1 || purged.ReceiptsPurged != 1 || purged.AuditRowsPurged == 0 || purged.PurgeDigest == "" {
		t.Fatalf("purge result = %+v", purged)
	}
	operation, err = s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if operation.PayloadBytes != 0 || operation.ReceiptBindingDigest != "" || operation.ReceiptAppliedGeneration != 0 ||
		operation.ReceiptBackendVersion != "" || operation.ReceiptBackendMemoryID != "" ||
		operation.ReceiptContentDigest != "" || operation.ReceiptMutationDigest != "" || operation.ReceiptCompletedAt != nil {
		t.Fatalf("operation retained purged data: %+v", operation)
	}
	var watermarkCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_governance_purge_watermarks WHERE namespace_uid = ? AND purge_digest = ?`,
		binding.NamespaceUID, purged.PurgeDigest).Scan(&watermarkCount); err != nil || watermarkCount != 1 {
		t.Fatalf("purge watermark count = %d, %v", watermarkCount, err)
	}
}

func TestReceiptPurgeSelectsAppliedGenerationAndCompletionTimestamp(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 45, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-partial-receipt", "team-partial-receipt-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "partial-receipt")
	if _, err := s.db.Exec(`UPDATE memory_operations SET receipt_binding_digest = '', receipt_backend_version = '',
		receipt_backend_memory_id = '', receipt_content_digest = '', receipt_mutation_digest = ''
		WHERE namespace_uid = ? AND id = ?`, binding.NamespaceUID, created.Operation.ID); err != nil {
		t.Fatalf("clear string receipt fields: %v", err)
	}
	partial, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if partial.ReceiptAppliedGeneration == 0 || partial.ReceiptCompletedAt == nil || partial.ReceiptBindingDigest != "" ||
		partial.ReceiptBackendVersion != "" || partial.ReceiptBackendMemoryID != "" ||
		partial.ReceiptContentDigest != "" || partial.ReceiptMutationDigest != "" {
		t.Fatalf("partial receipt fixture = %+v", partial)
	}

	checkpointAt := now.Add(8 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-partial-receipt", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: partial.Sequence, CheckpointDigest: protocol.ContentDigest("partial receipt checkpoint"),
		Actor: "operator", Reason: "verified partial receipt checkpoint", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint: %v", err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: partial.Sequence,
		Before: now.Add(24 * time.Hour), PurgeReceipts: true,
		Actor: "operator", Reason: "purge partial receipt", Now: checkpointAt,
	})
	if err != nil || purged.ReceiptsPurged != 1 {
		t.Fatalf("PurgeMemoryGovernance = %+v, %v", purged, err)
	}
	after, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReceiptAppliedGeneration != 0 || after.ReceiptCompletedAt != nil {
		t.Fatalf("partial receipt fields survived purge: %+v", after)
	}
}

func TestCheckpointAndPurgeAuditUseVerifiedRoutingEpoch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-old-route-audit", "team-old-route-audit-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "old-route-audit")
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "advance route before checkpoint verification", Now: now.Add(3 * time.Second),
	})
	if err != nil || advanced.RoutingEpoch != binding.RoutingEpoch+1 {
		t.Fatalf("advance binding route = %+v, %v", advanced, err)
	}
	checkpointAt := now.Add(8 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-old-route-audit", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: operation.Sequence, CheckpointDigest: protocol.ContentDigest("old route checkpoint"),
		Actor: "operator", Reason: "verify old route", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint: %v", err)
	}
	if _, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: operation.Sequence,
		Before: now.Add(24 * time.Hour), PurgePayloads: true,
		Actor: "operator", Reason: "purge verified old route", Now: checkpointAt,
	}); err != nil {
		t.Fatalf("PurgeMemoryGovernance: %v", err)
	}
	audit, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: binding.NamespaceUID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"memory.checkpoint.verify": false, "memory.governance.purge": false}
	for _, record := range audit {
		if _, exists := wanted[record.Action]; !exists {
			continue
		}
		wanted[record.Action] = true
		if record.RoutingEpoch != binding.RoutingEpoch {
			t.Fatalf("%s audit routing epoch = %d, want verified route %d", record.Action, record.RoutingEpoch, binding.RoutingEpoch)
		}
	}
	for action, found := range wanted {
		if !found {
			t.Fatalf("missing %s audit record", action)
		}
	}
}

func TestGovernanceOperationQuotaFailsClosed(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	governedMemoryQuotas.NamespaceOperationRows = 1
	governedMemoryQuotas.GlobalOperationRows = 100
	governedMemoryQuotas.SafetyReserveRows = 0
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-operation-quota", "team-operation-quota-uid", now)
	if _, err := s.AdmitRemoteMemoryCreate(context.Background(),
		newRemoteCreateAdmission(binding, now.Add(time.Second), "quota-a", "quota-request-a", []byte("a"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitRemoteMemoryCreate(context.Background(),
		newRemoteCreateAdmission(binding, now.Add(2*time.Second), "quota-b", "quota-request-b", []byte("b"))); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("second operation quota error = %v, want ErrCapacity", err)
	}
}

func setupConcurrentGovernedMemoryStores(t *testing.T) []*Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory-governance.db")
	stores := make([]*Store, 0, 2)
	for range 2 {
		db, err := NewDB(path)
		if err != nil {
			t.Fatalf("NewDB(%s): %v", path, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(`PRAGMA busy_timeout=1`); err != nil {
			t.Fatalf("set busy timeout: %v", err)
		}
		stores = append(stores, NewStore(db, path))
	}
	return stores
}

func claimAndSendExactOperationForTest(t *testing.T, s *Store, binding store.MemoryBackendBinding, operation store.MemoryOperation, now time.Time) store.MemoryOperationDispatch {
	t.Helper()
	ctx := context.Background()
	claim, err := s.ClaimMemoryOperation(ctx, operation.ID, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: testMemoryDispatcher, Now: now, LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("ClaimMemoryOperation(%s): %v", operation.ID, err)
	}
	sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch,
		Now: now.Add(time.Second), RequestDeadline: now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted(%s): %v", operation.ID, err)
	}
	claim.Operation = *sent
	return *claim
}

func activateMemoryBackendForTest(t *testing.T, s *Store, namespace, namespaceUID string, now time.Time) store.MemoryBackendBinding {
	t.Helper()
	namespaceUID = strings.ReplaceAll(namespaceUID, " ", "-")
	ctx := context.Background()
	if err := s.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: "controller-a", Role: "serving_dispatching", FeatureEpoch: 1,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("UpsertControllerFeatureHeartbeat: %v", err)
	}
	binding := store.MemoryBackendBinding{
		Namespace: namespace, NamespaceUID: namespaceUID, ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "backend-a", BackendGeneration: 1, AuthorityEpoch: 1, RoutingEpoch: 1,
		SpecDigest: "spec-digest", EndpointDigest: "endpoint-digest",
		ResolvedAddressDigest: "address-digest", ServerCertificateDigest: "certificate-digest",
		SecretName: "memory-auth", SecretKey: "token",
		SecretUID: "secret-uid", SecretResourceVersion: "1", TenantID: protocol.DeriveTenantID("cluster-a", namespaceUID), StoreName: "memories", StoreUUID: "11111111-1111-4111-8111-111111111111",
		OwnershipClaim: "claim-a", CapabilityRevision: "cap-a", Protocol: "oms/v0alpha1",
		State: store.MemoryBackendBindingAccepting, ActivationEpoch: 1, MinimumFeatureEpoch: 1,
		ValidationExpiresAt: now.Add(30 * time.Minute),
	}
	recordActivationRecoveryReceiptForTest(t, s, binding, now)
	result, err := s.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
		Binding: binding, RequiredFeatureEpoch: 1, Actor: "operator", Reason: "test activation", Now: now,
	})
	if err != nil {
		t.Fatalf("ActivateMemoryBackend: %v", err)
	}
	return result.Binding
}

func recordActivationRecoveryReceiptForTest(t *testing.T, s *Store, binding store.MemoryBackendBinding, now time.Time) {
	t.Helper()
	if _, err := s.RecordMemoryActivationRecoveryReceipt(context.Background(), store.MemoryActivationRecoveryReceipt{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		RouteDigest: binding.RecoveryRouteIdentity().Digest(), StoreUUID: binding.StoreUUID,
		ManifestDigest: protocol.ContentDigest("matched recovery manifest:" + binding.NamespaceUID + ":" + binding.BackendUID),
		Actor:          "operator", Reason: "test matched recovery prerequisite", VerifiedAt: now,
	}); err != nil {
		t.Fatalf("RecordMemoryActivationRecoveryReceipt: %v", err)
	}
}

func materializedRemoteMemoryForTest(t *testing.T, s *Store, binding store.MemoryBackendBinding, now time.Time, key string) *store.MemoryMutationAdmissionResult {
	t.Helper()
	admitted, err := s.AdmitRemoteMemoryCreate(context.Background(),
		newRemoteCreateAdmission(binding, now, key, key+"-request", []byte(`{"content":"v1"}`)))
	if err != nil {
		t.Fatalf("AdmitRemoteMemoryCreate(%s): %v", key, err)
	}
	completeOperationForTest(t, s, binding, admitted.Operation, now.Add(time.Second), "backend-v1", "remote-"+key)
	return admitted
}

func newRemoteCreateAdmission(binding store.MemoryBackendBinding, now time.Time, key, requestDigest string, content []byte) store.RemoteMemoryCreateAdmission {
	return store.RemoteMemoryCreateAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now, "", key, requestDigest,
			store.MemoryOperationCreate, 1, 0, "", string(content)),
		Memory: store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{}},
	}
}

func newCanonicalMemoryMutation(
	binding store.MemoryBackendBinding,
	now time.Time,
	memoryID, key, requestDigest string,
	kind store.MemoryOperationKind,
	desiredGeneration, expectedGeneration int64,
	expectedBackendVersion, content string,
) store.MemoryMutationAdmission {
	safeKey := strings.ReplaceAll(key, " ", "-")
	if memoryID == "" {
		memoryID = "mem-" + safeKey
	}
	operationID := "mop-" + safeKey
	protocolKind := ""
	var state *protocol.MutationState
	switch kind {
	case store.MemoryOperationCreate:
		protocolKind = protocol.MutationKindCreate
		state = &protocol.MutationState{Content: content, Tags: []string{}, Metadata: map[string]string{}}
	case store.MemoryOperationReplace:
		protocolKind = protocol.MutationKindReplace
		state = &protocol.MutationState{Content: content, Tags: []string{}, Metadata: map[string]string{}}
	case store.MemoryOperationDelete:
		protocolKind = protocol.MutationKindDelete
	default:
		panic("unsupported test mutation kind: " + kind)
	}
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version,
		OperationID:     operationID,
		Binding: protocol.Binding{
			ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
			AuthorityEpoch: uint64(binding.AuthorityEpoch), RoutingEpoch: uint64(binding.RoutingEpoch),
			TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
		},
		MemoryID: memoryID, Kind: protocolKind, Generation: uint64(desiredGeneration),
		ExpectedGeneration: uint64(expectedGeneration), ExpectedBackendVersion: expectedBackendVersion, State: state,
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		panic(err)
	}
	payload, err := protocol.EncodeJSON(envelope)
	if err != nil {
		panic(err)
	}
	return newMemoryMutationFromEnvelope(binding, now, memoryID, key, requestDigest, envelope, payload)
}

func newMemoryMutationFromEnvelope(
	binding store.MemoryBackendBinding,
	now time.Time,
	memoryID, key, requestDigest string,
	envelope protocol.MutationEnvelope,
	payload []byte,
) store.MemoryMutationAdmission {
	operationID := envelope.OperationID
	return store.MemoryMutationAdmission{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, ClusterID: binding.ClusterID,
		BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		MemoryID: memoryID, OperationID: operationID, OperationIdempotencyKey: operationID,
		Principal: "user:test", Route: "/api/v1/memories", IdempotencyKey: key,
		RequestDigest: requestDigest, MutationDigest: envelope.MutationDigest, ContentDigest: envelope.ContentDigest,
		Payload: payload, Actor: "user:test", Reason: "test mutation", OriginalStatus: 202,
		ResponseType: store.MemoryIdempotencyOperation, Location: "/api/v1/memory-operations/pending",
		Now: now, MaxAgeAt: now.Add(24 * time.Hour), IdempotencyExpiresAt: now.Add(30 * 24 * time.Hour),
	}
}

func claimAndSendOperationForTest(t *testing.T, s *Store, binding store.MemoryBackendBinding, operation store.MemoryOperation, now time.Time) store.MemoryOperationDispatch {
	t.Helper()
	ctx := context.Background()
	claim, err := s.ClaimNextMemoryOperation(ctx, store.MemoryOperationClaim{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, LeaseOwner: testMemoryDispatcher, Now: now, LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("ClaimNextMemoryOperation(%s): %v", operation.ID, err)
	}
	if claim.Operation.ID != operation.ID {
		t.Fatalf("claimed operation %s, want %s", claim.Operation.ID, operation.ID)
	}
	sent, err := s.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID: binding.NamespaceUID, ID: operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: claim.Operation.LeaseOwner, LeaseEpoch: claim.Operation.LeaseEpoch,
		Now: now.Add(time.Second), RequestDeadline: now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("MarkMemoryOperationSendStarted(%s): %v", operation.ID, err)
	}
	claim.Operation = *sent
	return *claim
}

func completeOperationForTest(t *testing.T, s *Store, binding store.MemoryBackendBinding, operation store.MemoryOperation, now time.Time, backendVersion, backendMemoryID string) {
	t.Helper()
	dispatch := claimAndSendOperationForTest(t, s, binding, operation, now)
	completion := completionForDispatch(binding, dispatch, now.Add(2*time.Second), backendVersion, backendMemoryID)
	if _, err := s.CompleteMemoryOperation(context.Background(), completion); err != nil {
		t.Fatalf("CompleteMemoryOperation(%s): %v", operation.ID, err)
	}
}

func completionForDispatch(
	binding store.MemoryBackendBinding,
	dispatch store.MemoryOperationDispatch,
	completedAt time.Time,
	backendVersion, backendMemoryID string,
) store.MemoryOperationCompletion {
	return store.MemoryOperationCompletion{
		NamespaceUID: dispatch.Operation.NamespaceUID, ID: dispatch.Operation.ID, BackendUID: dispatch.Operation.BackendUID,
		AuthorityEpoch: dispatch.Operation.AuthorityEpoch, RoutingEpoch: dispatch.Operation.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch, Now: completedAt,
		Receipt: store.MemoryOperationReceipt{
			BindingIdentityDigest: memoryBackendBindingDigest(&binding), AppliedGeneration: dispatch.Operation.DesiredGeneration,
			BackendVersion: backendVersion, BackendMemoryID: backendMemoryID,
			ContentDigest: dispatch.Operation.ContentDigest, MutationDigest: dispatch.Operation.MutationDigest,
			CompletedAt: completedAt,
		}, Actor: dispatch.Operation.LeaseOwner, Reason: "adapter acknowledged",
	}
}

func acceptedMemoryProposalForTest(t *testing.T, s *Store, namespace, content string) *store.MemoryProposal {
	t.Helper()
	ctx := context.Background()
	proposal := &store.MemoryProposal{Namespace: namespace, Type: "memory", Title: "proposal", Content: content}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
	}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}
	return proposal
}

func newRemoteProposalApplyAdmission(binding store.MemoryBackendBinding, proposal *store.MemoryProposal, now time.Time, key string) store.RemoteMemoryProposalApplyAdmission {
	memoryID := "mem-" + key
	operationID := "mop-" + key
	tags, _ := canonicalRemoteProposalTags(proposal.Description)
	metadata, _ := canonicalRemoteProposalMetadata(proposal)
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version,
		OperationID:     operationID,
		Binding: protocol.Binding{
			ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
			AuthorityEpoch: uint64(binding.AuthorityEpoch), RoutingEpoch: uint64(binding.RoutingEpoch),
			TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
		},
		MemoryID: memoryID, Kind: protocol.MutationKindCreate, Generation: 1,
		ExpectedGeneration: 0, ExpectedBackendVersion: "",
		State: &protocol.MutationState{Content: proposal.Content, Tags: tags, Metadata: metadata},
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		panic(err)
	}
	payload, err := protocol.EncodeJSON(envelope)
	if err != nil {
		panic(err)
	}
	mutation := newMemoryMutationFromEnvelope(binding, now, memoryID, key, "request-"+key, envelope, payload)
	mutation.ProposalID = proposal.ID
	mutation.Route = "/api/v1/memory-proposals/:id/apply"
	return store.RemoteMemoryProposalApplyAdmission{
		Mutation: mutation,
		Memory: store.RemoteMemoryCatalogEntry{
			Source: memorySourceProposal, SourceProposalID: proposal.ID,
			AgentName: metadata["agentname"], TaskName: metadata["taskname"], Tags: tags,
			ContentDigest: envelope.ContentDigest,
		},
		AppliedBy: "operator",
	}
}

func TestMemoryBackendLifecycleTransitionsRebaseIdleCatalogRows(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-rebase", "team-rebase-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "rebase")

	draining, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "read only", Now: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatalf("transition to draining: %v", err)
	}
	catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil || catalog.RoutingEpoch != draining.RoutingEpoch {
		t.Fatalf("catalog after draining = %+v, %v", catalog, err)
	}

	resume := *draining
	resume.State = store.MemoryBackendBindingAccepting
	resume.BackendGeneration++
	resume.RoutingEpoch++
	resume.ValidationExpiresAt = now.Add(time.Hour)
	accepting, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: resume, ExpectedRoutingEpoch: draining.RoutingEpoch,
		Actor: "operator", Reason: "resume with fully validated route", Now: now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("transition to accepting: %v", err)
	}
	catalog, err = s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil || catalog.RoutingEpoch != accepting.RoutingEpoch {
		t.Fatalf("catalog after accepting = %+v, %v", catalog, err)
	}

	if _, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(*accepting, now.Add(6*time.Second), catalog.ID, "rebase-replace", "rebase-replace-request",
			store.MemoryOperationReplace, catalog.Generation+1, catalog.Generation, catalog.BackendVersion, "v2"),
		Memory:             store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{"updated"}},
		ExpectedGeneration: catalog.Generation, ExpectedBackendVersion: catalog.BackendVersion,
	}); err != nil {
		t.Fatalf("replace after accepting/draining/accepting: %v", err)
	}
}

func TestMarkMaterializationIssueRequiresIdleActiveMaterialization(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-issue", "team-issue-uid", now)
	pending, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(binding, now.Add(time.Second), "pending-issue", "pending-issue-request", []byte(`{"content":"pending"}`)))
	if err != nil {
		t.Fatal(err)
	}
	issue := store.RemoteMemoryMaterializationIssue{
		NamespaceUID: binding.NamespaceUID, ID: pending.Memory.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		ExpectedGeneration: pending.Memory.Generation, ExpectedBackendVersion: pending.Memory.BackendVersion,
		State: store.MemoryMaterializationLost, Actor: "hydrator", Reason: "missing", Now: now.Add(2 * time.Second),
	}
	if _, err := s.MarkRemoteMemoryMaterializationIssue(ctx, issue); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pending materialization issue error = %v, want ErrConflict", err)
	}

	completeOperationForTest(t, s, binding, pending.Operation, now.Add(3*time.Second), "backend-v1", "remote-pending-issue")
	active, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, pending.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	replace, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(6*time.Second), active.ID, "pending-replace", "pending-replace-request",
			store.MemoryOperationReplace, active.Generation+1, active.Generation, active.BackendVersion, "replacement"),
		Memory: store.RemoteMemoryCatalogEntry{Source: "manual"}, ExpectedGeneration: active.Generation,
		ExpectedBackendVersion: active.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	issue.ExpectedGeneration = replace.Memory.Generation
	issue.ExpectedBackendVersion = replace.Memory.BackendVersion
	issue.Now = now.Add(7 * time.Second)
	if _, err := s.MarkRemoteMemoryMaterializationIssue(ctx, issue); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("active materialization with pending operation error = %v, want ErrConflict", err)
	}
}

func TestCompleteMemoryOperationRequiresBoundedBackendIdentity(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-receipt", "team-receipt-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(binding, now.Add(time.Second), "receipt", "receipt-request", []byte(`{"content":"receipt"}`)))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
	for name, values := range map[string][2]string{
		"empty version": {"", "memory-id"}, "empty memory id": {"version", ""},
		"oversized version":   {strings.Repeat("v", maxMemoryReceiptIdentityBytes+1), "memory-id"},
		"oversized memory id": {"version", strings.Repeat("m", maxMemoryReceiptIdentityBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			completion := completionForDispatch(binding, dispatch, now.Add(4*time.Second), values[0], values[1])
			if _, err := s.CompleteMemoryOperation(ctx, completion); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("CompleteMemoryOperation error = %v, want ErrValidation", err)
			}
		})
	}
	if _, err := s.CompleteMemoryOperation(ctx, completionForDispatch(binding, dispatch, now.Add(5*time.Second), "version", "memory-id")); err != nil {
		t.Fatalf("valid completion after rejected receipts: %v", err)
	}
}

func TestManualRetryRequiresCurrentMemoryBinding(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 10, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-manual-retry", "team-manual-retry-uid", now)
	admitted, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(binding, now.Add(time.Second), "manual-retry", "manual-retry-request", []byte(`{"content":"retry"}`)))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, admitted.Operation, now.Add(2*time.Second))
	dead, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		LeaseOwner: dispatch.Operation.LeaseOwner, LeaseEpoch: dispatch.Operation.LeaseEpoch,
		DeadLetter: true, ErrorCode: "FAILED", ErrorMessage: "failed", Now: now.Add(4 * time.Second),
	})
	if err != nil || dead.State != store.MemoryOperationDeadLettered {
		t.Fatalf("dead letter = %+v, %v", dead, err)
	}
	if _, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "fence old route", Now: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("transition binding: %v", err)
	}
	if _, err := s.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Manual: true, Actor: "operator", Reason: "retry", Now: now.Add(6 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("manual retry with stale binding error = %v, want ErrConflict", err)
	}
}

func TestUnsentMemoryOperationReleasePreservesLeaseOriginWithoutSemanticBudget(t *testing.T) {
	for _, origin := range []store.MemoryOperationState{store.MemoryOperationQueued, store.MemoryOperationAmbiguous} {
		t.Run(string(origin), func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			binding := activateMemoryBackendForTest(t, s, "team-unsent-"+string(origin), "uid-unsent-"+string(origin), now)
			admitted, err := s.AdmitRemoteMemoryCreate(context.Background(), newRemoteCreateAdmission(
				binding, now.Add(time.Second), "unsent-"+string(origin), "request-"+string(origin), []byte(`{"content":"v1"}`),
			))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE memory_operations SET state = ?, attempts = 50, next_retry_at = ? WHERE namespace_uid = ? AND id = ?`,
				origin, now.Add(2*time.Second), binding.NamespaceUID, admitted.Operation.ID); err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimMemoryOperation(context.Background(), admitted.Operation.ID, store.MemoryOperationClaim{
				NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
				LeaseOwner: "dispatcher-a", Now: now.Add(3 * time.Second), LeaseDuration: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			if claim.Operation.LeaseOriginState != origin {
				t.Fatalf("lease origin = %q, want %q", claim.Operation.LeaseOriginState, origin)
			}
			released, err := s.RetryMemoryOperation(context.Background(), store.MemoryOperationRetry{
				NamespaceUID: binding.NamespaceUID, ID: admitted.Operation.ID, BackendUID: binding.BackendUID,
				AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
				LeaseOwner: "dispatcher-a", LeaseEpoch: claim.Operation.LeaseEpoch,
				UnsentRelease: true, Ambiguous: origin == store.MemoryOperationAmbiguous,
				NextRetryAt: now.Add(4 * time.Second), Now: now.Add(4 * time.Second),
				Actor: "dispatcher-a", Reason: "backend unavailable before send",
			})
			if err != nil {
				t.Fatal(err)
			}
			if released.State != origin {
				t.Fatalf("released state = %q, want %q", released.State, origin)
			}
		})
	}
}

func TestRefreshMemoryBackendBindingRejectsEmptySecretRoute(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-refresh-secret", "team-refresh-secret-uid", now)
	invalid := binding
	invalid.RoutingEpoch++
	invalid.SecretName = ""
	invalid.ValidationExpiresAt = now.Add(time.Hour)
	_, err := s.RefreshMemoryBackendBinding(context.Background(), store.MemoryBackendBindingRefresh{
		Binding: invalid, ExpectedRoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "invalid secret rotation", Now: now.Add(time.Minute),
	})
	if !errors.Is(err, store.ErrValidation) {
		t.Fatalf("RefreshMemoryBackendBinding() error = %v, want ErrValidation", err)
	}
}

func TestIdempotencyExpiryCoversOperationMaximumAge(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 14, 15, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-idempotency-floor", "team-idempotency-floor-uid", now)
	admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "idempotency-floor", "request", []byte("content"))
	admission.Mutation.MaxAgeAt = now.Add(48 * time.Hour)
	admission.Mutation.IdempotencyExpiresAt = now.Add(time.Hour)
	admitted, err := s.AdmitRemoteMemoryCreate(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Idempotency.ExpiresAt.Before(admission.Mutation.MaxAgeAt) {
		t.Fatalf("idempotency expiry = %s, operation max age = %s", admitted.Idempotency.ExpiresAt, admission.Mutation.MaxAgeAt)
	}
}

func TestDeleteCompletionRejectsWrongBindingAndContentDigests(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 14, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-delete-receipt", "team-delete-receipt-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "delete-receipt")
	active, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), active.ID, "delete-receipt-op", "delete-request",
			store.MemoryOperationDelete, active.Generation+1, active.Generation, active.BackendVersion, ""),
		ExpectedGeneration: active.Generation, ExpectedBackendVersion: active.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := claimAndSendOperationForTest(t, s, binding, deleted.Operation, now.Add(6*time.Second))
	completion := completionForDispatch(binding, dispatch, now.Add(8*time.Second), "backend-v2", "remote-delete")
	completion.Receipt.BindingIdentityDigest = protocol.ContentDigest("wrong binding")
	if _, err := s.CompleteMemoryOperation(ctx, completion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong binding digest error = %v, want ErrConflict", err)
	}
	completion.Receipt.BindingIdentityDigest = memoryBackendBindingDigest(&binding)
	completion.Receipt.ContentDigest = ""
	if _, err := s.CompleteMemoryOperation(ctx, completion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong delete content digest error = %v, want ErrConflict", err)
	}
	completion.Receipt.ContentDigest = deleted.Operation.ContentDigest
	if _, err := s.CompleteMemoryOperation(ctx, completion); err != nil {
		t.Fatalf("valid delete completion: %v", err)
	}
}

func TestHistoricalRoutingCheckpointCanPurgeRetainedOperation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-historical-checkpoint", "team-historical-checkpoint-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "historical-checkpoint")
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "rotate route", Now: now.Add(4 * time.Second),
	})
	if err != nil || current.RoutingEpoch != binding.RoutingEpoch+1 {
		t.Fatalf("routing transition = %+v, %v", current, err)
	}
	checkpointAt := now.Add(8 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-historical", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: operation.Sequence, CheckpointDigest: protocol.ContentDigest("historical checkpoint"),
		Actor: "operator", Reason: "verified old route checkpoint", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint(old route): %v", err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: operation.Sequence,
		Before: now.Add(24 * time.Hour), PurgePayloads: true, PurgeReceipts: true,
		Actor: "operator", Reason: "purge old route redo", Now: checkpointAt,
	})
	if err != nil || purged.PayloadsPurged != 1 || purged.ReceiptsPurged != 1 {
		t.Fatalf("historical route purge = %+v, %v", purged, err)
	}
}

func TestCurrentRoutingCheckpointCoversPriorFencedPayloads(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 11, 15, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-current-checkpoint", "team-current-checkpoint-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "current-checkpoint")
	operation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldCheckpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-prior-route", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: operation.Sequence, CheckpointDigest: protocol.ContentDigest("prior route checkpoint"),
		Actor: "operator", Reason: "verify prior route", VerifiedAt: now.Add(2 * time.Second),
	})
	if err != nil || oldCheckpoint.RoutingEpoch != binding.RoutingEpoch {
		t.Fatalf("prior checkpoint = %+v, %v", oldCheckpoint, err)
	}
	current, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "fence prior route", Now: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointAt := now.Add(8 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-current-route", NamespaceUID: current.NamespaceUID, BackendUID: current.BackendUID,
		AuthorityEpoch: current.AuthorityEpoch, RoutingEpoch: current.RoutingEpoch, StoreUUID: current.StoreUUID,
		MaximumOperationSequence: operation.Sequence, CheckpointDigest: protocol.ContentDigest("current route checkpoint"),
		Actor: "operator", Reason: "verify current route and prior fenced ledger", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint(current route): %v", err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: current.NamespaceUID, BackendUID: current.BackendUID,
		AuthorityEpoch: current.AuthorityEpoch, RoutingEpoch: current.RoutingEpoch, StoreUUID: current.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: operation.Sequence,
		Before: now.Add(24 * time.Hour), PurgePayloads: true, PurgeReceipts: true,
		Actor: "operator", Reason: "reclaim prior route payload", Now: checkpointAt,
	})
	if err != nil || purged.PayloadsPurged != 1 || purged.ReceiptsPurged != 1 {
		t.Fatalf("current route purge = %+v, %v", purged, err)
	}
	retained, err := s.GetMemoryOperation(ctx, current.NamespaceUID, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.PayloadBytes != 0 || retained.ReceiptBindingDigest != "" || retained.ReceiptAppliedGeneration != 0 {
		t.Fatalf("prior route operation was not compacted: %+v", retained)
	}
}

func TestTombstonePurgeWaitsForIndependentRetentionGates(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-tombstone-retention", "team-tombstone-retention-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "tombstone-retention")
	active, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), active.ID, "tombstone-retention-delete", "delete-request",
			store.MemoryOperationDelete, active.Generation+1, active.Generation, active.BackendVersion, ""),
		ExpectedGeneration: active.Generation, ExpectedBackendVersion: active.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, deleted.Operation, now.Add(6*time.Second), "backend-v2", "remote-tombstone")
	deleteOperation, err := s.GetMemoryOperation(ctx, binding.NamespaceUID, deleted.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpointAt := now.Add(8 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-tombstone", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: deleteOperation.Sequence, CheckpointDigest: protocol.ContentDigest("tombstone checkpoint"),
		Actor: "operator", Reason: "verified checkpoint", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: deleteOperation.Sequence,
		Before: now.Add(24 * time.Hour), PurgeTombstones: true,
		Actor: "operator", Reason: "tombstone only", Now: checkpointAt,
	})
	if err != nil || first.TombstonesPurged != 0 {
		t.Fatalf("early tombstone purge = %+v, %v", first, err)
	}
	if _, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, active.ID); err != nil {
		t.Fatalf("tombstone disappeared before replay/redo retention elapsed: %v", err)
	}
	late := now.Add(40 * 24 * time.Hour)
	second, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, MaximumOperationSequence: deleteOperation.Sequence,
		Before: now.Add(35 * 24 * time.Hour), PurgePayloads: true, PurgeReceipts: true,
		PurgeExpiredIdempotency: true, PurgeTombstones: true,
		Actor: "operator", Reason: "all retention gates elapsed", Now: late,
	})
	if err != nil || second.TombstonesPurged != 1 {
		t.Fatalf("late tombstone purge = %+v, %v", second, err)
	}
	if _, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, active.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("purged tombstone lookup error = %v, want ErrNotFound", err)
	}
	watermark, err := s.GetRemoteMemoryGenerationWatermark(
		ctx, binding.NamespaceUID, active.ID, binding.BackendUID, binding.AuthorityEpoch, binding.StoreUUID,
	)
	if err != nil || watermark != deleteOperation.DesiredGeneration {
		t.Fatalf("purged tombstone watermark = %d, %v; want %d", watermark, err, deleteOperation.DesiredGeneration)
	}
	refreshedBinding := binding
	refreshedBinding.BackendGeneration++
	refreshedBinding.ValidationExpiresAt = late.Add(time.Hour)
	refreshed, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: refreshedBinding, ExpectedRoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "refresh validation for recreation", Now: late,
	})
	if err != nil {
		t.Fatalf("refresh binding before recreation: %v", err)
	}
	recreated, err := s.AdmitRemoteMemoryCreate(ctx, store.RemoteMemoryCreateAdmission{
		Mutation: newCanonicalMemoryMutation(
			*refreshed, late.Add(time.Second), active.ID, "tombstone-recreate", "recreate-request",
			store.MemoryOperationCreate, watermark+1, watermark, "", "recreated content",
		),
		Memory: store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{}},
	})
	if err != nil {
		t.Fatalf("recreate after tombstone purge: %v", err)
	}
	if recreated.Memory.Generation != watermark || recreated.Memory.DesiredGeneration != watermark+1 ||
		recreated.Operation.ExpectedMaterializedGeneration != watermark || recreated.Operation.DesiredGeneration != watermark+1 {
		t.Fatalf("recreated generations = memory %+v operation %+v", recreated.Memory, recreated.Operation)
	}
	completeOperationForTest(
		t, s, *refreshed, recreated.Operation, late.Add(2*time.Second), "backend-v3", "remote-recreated",
	)
	materialized, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, active.ID)
	if err != nil || materialized.Generation != watermark+1 || materialized.MaterializationState != store.MemoryMaterializationActive {
		t.Fatalf("materialized recreation = %+v, %v", materialized, err)
	}
	watermark, err = s.GetRemoteMemoryGenerationWatermark(
		ctx, binding.NamespaceUID, active.ID, binding.BackendUID, binding.AuthorityEpoch, binding.StoreUUID,
	)
	if err != nil || watermark != 0 {
		t.Fatalf("generation watermark remained after recreation: %d, %v", watermark, err)
	}
}

func TestActivationUsesExactBindingMinimumFeatureEpoch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 17, 30, 0, 0, time.UTC)
	if err := s.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: "old-controller", Role: "serving_dispatching", FeatureEpoch: 1,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	binding := store.MemoryBackendBinding{
		Namespace: "team-feature-epoch", NamespaceUID: "team-feature-epoch-uid", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, BackendUID: "backend-a", BackendGeneration: 1,
		AuthorityEpoch: 1, RoutingEpoch: 1, SpecDigest: "spec", EndpointDigest: "endpoint",
		ResolvedAddressDigest: "addresses", ServerCertificateDigest: "certificate",
		SecretName: "memory-auth", SecretKey: "token", SecretUID: "secret-a", SecretResourceVersion: "1",
		TenantID:  protocol.DeriveTenantID("cluster-a", "team-feature-epoch-uid"),
		StoreName: "memories", StoreUUID: "11111111-1111-4111-8111-111111111111",
		OwnershipClaim: "claim", CapabilityRevision: "cap", Protocol: "oms/v0alpha1",
		State: store.MemoryBackendBindingAccepting, ActivationEpoch: 1, MinimumFeatureEpoch: 2,
		ValidationExpiresAt: now.Add(time.Hour),
	}
	recordActivationRecoveryReceiptForTest(t, s, binding, now)
	if _, err := s.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
		Binding: binding, RequiredFeatureEpoch: 1, Actor: "operator", Reason: "mismatched gate", Now: now,
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("mismatched feature epoch error = %v, want ErrValidation", err)
	}
	if _, err := s.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
		Binding: binding, RequiredFeatureEpoch: 2, Actor: "operator", Reason: "old replica", Now: now,
	}); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("incompatible heartbeat error = %v, want ErrNotReady", err)
	}
}

func TestActivationFeatureEpochRequiresObservedFoundationRelease(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 17, 45, 0, 0, time.UTC)
	binding := store.MemoryBackendBinding{
		Namespace: "team-foundation-history", NamespaceUID: "team-foundation-history-uid", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, BackendUID: "backend-a", BackendGeneration: 1,
		AuthorityEpoch: 1, RoutingEpoch: 1, ActivationEpoch: 1, MinimumFeatureEpoch: 2,
		SpecDigest: "spec", EndpointDigest: "endpoint", ResolvedAddressDigest: "addresses",
		ServerCertificateDigest: "certificate", SecretName: "memory-auth", SecretKey: "token",
		SecretUID: "secret-a", SecretResourceVersion: "1",
		TenantID:  protocol.DeriveTenantID("cluster-a", "team-foundation-history-uid"),
		StoreName: "memories", StoreUUID: "11111111-1111-4111-8111-111111111111",
		OwnershipClaim: "claim", CapabilityRevision: "cap", Protocol: "oms/v0alpha1",
		State: store.MemoryBackendBindingAccepting, ValidationExpiresAt: now.Add(time.Hour),
	}
	recordActivationRecoveryReceiptForTest(t, s, binding, now)
	if err := s.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: "controller-a", Role: "serving_dispatching", FeatureEpoch: 2,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	activation := store.MemoryBackendActivation{
		Binding: binding, RequiredFeatureEpoch: 2, Actor: "operator", Reason: "activate", Now: now,
	}
	if _, err := s.ActivateMemoryBackend(ctx, activation); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("activation without observed foundation epoch error = %v, want ErrNotReady", err)
	}
	if err := s.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: "controller-a", Role: "serving_dispatching", FeatureEpoch: 1,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: "controller-a", Role: "serving_dispatching", FeatureEpoch: 2,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateMemoryBackend(ctx, activation); err != nil {
		t.Fatalf("activation after observed foundation epoch: %v", err)
	}
}

func TestRefreshMemoryBackendBindingRejectsGenerationRegression(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-generation-refresh", "team-generation-refresh-uid", now)
	newer := binding
	newer.BackendGeneration = 2
	newer.ValidationExpiresAt = now.Add(2 * time.Hour)
	refreshed, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: newer, ExpectedRoutingEpoch: binding.RoutingEpoch,
		Actor: "operator", Reason: "new generation", Now: now.Add(time.Minute),
	})
	if err != nil || refreshed.BackendGeneration != 2 {
		t.Fatalf("newer refresh = %+v, %v", refreshed, err)
	}
	stale := binding
	stale.ValidationExpiresAt = now.Add(3 * time.Hour)
	if _, err := s.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: stale, ExpectedRoutingEpoch: binding.RoutingEpoch,
		Actor: "old-controller", Reason: "stale refresh", Now: now.Add(2 * time.Minute),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("generation regression error = %v, want ErrConflict", err)
	}
}

func TestSupersededOperationExtendsIdempotencyTerminalRetention(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 18, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-terminal-idempotency", "team-terminal-idempotency-uid", now)
	create := newRemoteCreateAdmission(binding, now.Add(time.Second), "terminal-retention-create", "create-request", []byte("content"))
	created, err := s.AdmitRemoteMemoryCreate(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	deleteAt := now.Add(2 * time.Second)
	if _, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, deleteAt, created.Memory.ID, "terminal-retention-delete", "delete-request",
			store.MemoryOperationDelete, 2, 0, "", ""),
		ExpectedGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}
	record, err := s.GetMemoryIdempotency(ctx, binding.NamespaceUID, create.Mutation.Principal,
		create.Mutation.Route, create.Mutation.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpiresAt.Before(deleteAt.Add(minimumIdempotencyRetention)) {
		t.Fatalf("superseded idempotency expiry = %s", record.ExpiresAt)
	}
}

func TestRetainedTerminalPayloadsCountTowardAdmissionQuota(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 19, 15, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-retained-payload", "team-retained-payload-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "retained-payload")
	operation, err := s.GetMemoryOperation(context.Background(), binding.NamespaceUID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	governedMemoryQuotas.NamespaceRetainedPayloadBytes = int64(operation.PayloadBytes)
	governedMemoryQuotas.GlobalRetainedPayloadBytes = 1 << 30
	if _, err := s.AdmitRemoteMemoryCreate(context.Background(), newRemoteCreateAdmission(
		binding, now.Add(5*time.Second), "retained-payload-second", "second-request", []byte("second"),
	)); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("retained payload quota error = %v, want ErrCapacity", err)
	}
}

func TestMemorySearchReplayQuotaCoversMaximumSnapshot(t *testing.T) {
	const maximumPerCursorOverhead = 4 << 10
	var requiredBytes int64
	for seen := 1; seen < protocol.MaxSnapshotRecords; seen++ {
		rawIdentityBytes := 1 + seen*sha256.Size
		requiredBytes += int64(base64.StdEncoding.EncodedLen(rawIdentityBytes) + maximumPerCursorOverhead)
	}
	if governedMemoryQuotas.NamespaceSearchReplayRows < int64(protocol.MaxSnapshotRecords-1) {
		t.Fatalf("namespace replay rows = %d, want at least %d",
			governedMemoryQuotas.NamespaceSearchReplayRows, protocol.MaxSnapshotRecords-1)
	}
	if governedMemoryQuotas.NamespaceSearchReplayBytes < requiredBytes {
		t.Fatalf("namespace replay bytes = %d, want at least conservative maximum %d",
			governedMemoryQuotas.NamespaceSearchReplayBytes, requiredBytes)
	}
}

func TestMemorySearchCursorStateByteLimit(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, time.August, 3, 15, 5, 0, 0, time.UTC)
	cursor := store.MemorySearchCursorState{
		ID: "msc-state-limit", NamespaceUID: "namespace-a", BindingDigest: "binding", QueryDigest: "query",
		State:     bytes.Repeat([]byte{'x'}, store.MaxMemorySearchCursorStateBytes),
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := s.SaveMemorySearchCursor(t.Context(), cursor); err != nil {
		t.Fatalf("SaveMemorySearchCursor(maximum) error = %v", err)
	}
	cursor.ID = "msc-state-oversized"
	cursor.State = append(cursor.State, 'x')
	if err := s.SaveMemorySearchCursor(t.Context(), cursor); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SaveMemorySearchCursor(oversized) error = %v, want ErrValidation", err)
	}
}

func TestMemorySearchCursorCapacityIsBounded(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	governedMemoryQuotas.NamespaceSearchCursorRows = 1
	governedMemoryQuotas.GlobalSearchCursorRows = 10
	governedMemoryQuotas.NamespaceSearchCursorBytes = 1024
	governedMemoryQuotas.GlobalSearchCursorBytes = 10240
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 19, 30, 0, 0, time.UTC)
	first := store.MemorySearchCursorState{
		ID: "msc-one", NamespaceUID: "namespace-a", BindingDigest: "binding", QueryDigest: "query",
		State: []byte(`{"page":1}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := s.SaveMemorySearchCursor(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMemorySearchCursor(context.Background(), first); err != nil {
		t.Fatalf("exact cursor replay was not idempotent: %v", err)
	}
	mismatch := first
	mismatch.State = []byte(`{"page":"different"}`)
	if err := s.SaveMemorySearchCursor(context.Background(), mismatch); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched cursor replay error = %v, want ErrConflict", err)
	}
	second := first
	second.ID = "msc-two"
	if err := s.SaveMemorySearchCursor(context.Background(), second); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("second cursor error = %v, want ErrCapacity", err)
	}
}

func TestMemorySearchCursorRetirementSeparatesActiveAndReplayCapacity(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	governedMemoryQuotas.NamespaceSearchCursorRows = 1
	governedMemoryQuotas.GlobalSearchCursorRows = 10
	governedMemoryQuotas.NamespaceSearchCursorBytes = 1024
	governedMemoryQuotas.GlobalSearchCursorBytes = 10240
	governedMemoryQuotas.NamespaceSearchReplayRows = 1
	governedMemoryQuotas.GlobalSearchReplayRows = 10
	governedMemoryQuotas.NamespaceSearchReplayBytes = 1024
	governedMemoryQuotas.GlobalSearchReplayBytes = 10240
	s := setupTestStore(t)
	now := time.Date(2026, time.August, 2, 19, 30, 0, 0, time.UTC)
	first := store.MemorySearchCursorState{
		ID: "msc-retire-one", NamespaceUID: "namespace-a", BindingDigest: "binding", QueryDigest: "query",
		State: []byte(`{"page":1}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := s.SaveMemorySearchCursor(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireMemorySearchCursor(t.Context(), first.NamespaceUID, first.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMemorySearchCursor(t.Context(), first.NamespaceUID, first.ID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("retired cursor was not replayable: %v", err)
	}
	second := first
	second.ID = "msc-retire-two"
	if err := s.SaveMemorySearchCursor(t.Context(), second); err != nil {
		t.Fatalf("retired cursor still consumed active quota: %v", err)
	}
	if err := s.RetireMemorySearchCursor(t.Context(), second.NamespaceUID, second.ID, now.Add(3*time.Second)); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("second retirement error = %v, want replay ErrCapacity", err)
	}
	third := first
	third.ID = "msc-retire-three"
	if err := s.SaveMemorySearchCursor(t.Context(), third); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("active cursor after failed retirement error = %v, want ErrCapacity", err)
	}
}

func TestMemorySearchCursorRetirementAllowsLongPagination(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	governedMemoryQuotas.NamespaceSearchCursorRows = 1
	governedMemoryQuotas.GlobalSearchCursorRows = 10
	governedMemoryQuotas.NamespaceSearchCursorBytes = 1024
	governedMemoryQuotas.GlobalSearchCursorBytes = 10240
	governedMemoryQuotas.NamespaceSearchReplayRows = 256
	governedMemoryQuotas.GlobalSearchReplayRows = 256
	governedMemoryQuotas.NamespaceSearchReplayBytes = 1 << 20
	governedMemoryQuotas.GlobalSearchReplayBytes = 1 << 20
	s := setupTestStore(t)
	now := time.Date(2026, time.August, 2, 20, 0, 0, 0, time.UTC)
	current := store.MemorySearchCursorState{
		ID: "msc-page-000", NamespaceUID: "namespace-a", BindingDigest: "binding", QueryDigest: "query",
		State: []byte(`{"page":0}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := s.SaveMemorySearchCursor(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	first := current
	for page := 1; page <= 150; page++ {
		stepNow := now.Add(time.Duration(page) * time.Millisecond)
		if err := s.RetireMemorySearchCursor(t.Context(), current.NamespaceUID, current.ID, stepNow); err != nil {
			t.Fatalf("retire page %d: %v", page-1, err)
		}
		current = store.MemorySearchCursorState{
			ID: fmt.Sprintf("msc-page-%03d", page), NamespaceUID: first.NamespaceUID,
			BindingDigest: first.BindingDigest, QueryDigest: first.QueryDigest,
			State: fmt.Appendf(nil, `{"page":%d}`, page), CreatedAt: stepNow, ExpiresAt: first.ExpiresAt,
		}
		if err := s.SaveMemorySearchCursor(t.Context(), current); err != nil {
			t.Fatalf("save page %d: %v", page, err)
		}
	}
	if _, err := s.GetMemorySearchCursor(t.Context(), first.NamespaceUID, first.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("first retired page was not replayable: %v", err)
	}
	if err := s.RetireMemorySearchCursor(t.Context(), current.NamespaceUID, current.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("retire terminal page: %v", err)
	}
	governedMemoryQuotas.NamespaceSearchCursorRows = 0
	failed := current
	failed.ID = "msc-successor-fails"
	if err := s.SaveMemorySearchCursor(t.Context(), failed); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("successor error = %v, want active ErrCapacity", err)
	}
	if _, err := s.GetMemorySearchCursor(t.Context(), current.NamespaceUID, current.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("terminal retired page was not replayable after successor failure: %v", err)
	}
}

func TestMaterializationIssueRequiresCurrentRoutingBinding(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 19, 45, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-materialization-fence", "team-materialization-fence-uid", now)
	created := materializedRemoteMemoryForTest(t, s, binding, now.Add(time.Second), "materialization-fence")
	active, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "advance route", Now: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.MarkRemoteMemoryMaterializationIssue(ctx, store.RemoteMemoryMaterializationIssue{
		NamespaceUID: binding.NamespaceUID, ID: active.ID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		ExpectedGeneration: active.Generation, ExpectedBackendVersion: active.BackendVersion,
		State: store.MemoryMaterializationDiverged, Actor: "reader", Reason: "old route observation", Now: now.Add(6 * time.Second),
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("old-route materialization issue error = %v, want ErrConflict", err)
	}
}

func TestBoundedMemoryAuditTextPreservesUTF8(t *testing.T) {
	value := strings.Repeat("a", maxMemoryAuditTextBytes-1) + "é"
	bounded := boundedMemoryAuditText(value)
	if len(bounded) > maxMemoryAuditTextBytes || !utf8.ValidString(bounded) {
		t.Fatalf("bounded audit text is invalid UTF-8: %q", bounded)
	}
}

func TestFeatureHeartbeatTTLAndClockWindowAreBounded(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC)
	if err := s.UpsertControllerFeatureHeartbeat(context.Background(), store.ControllerFeatureHeartbeat{
		InstanceID: "too-long", Role: "serving_dispatching", FeatureEpoch: 1,
		LastHeartbeatAt: now, ExpiresAt: now.Add(maxFeatureHeartbeatTTL + time.Second),
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("oversized heartbeat TTL error = %v, want ErrValidation", err)
	}
	offset := time.FixedZone("offset", 5*60*60)
	if err := s.UpsertControllerFeatureHeartbeat(context.Background(), store.ControllerFeatureHeartbeat{
		InstanceID: "offset", Role: "serving_dispatching", FeatureEpoch: 1,
		LastHeartbeatAt: now.In(offset), ExpiresAt: now.Add(time.Minute).In(offset),
	}); err != nil {
		t.Fatal(err)
	}
	live, err := s.ListLiveControllerFeatureHeartbeats(context.Background(), now.Add(30*time.Second))
	if err != nil || len(live) != 1 || live[0].InstanceID != "offset" || live[0].ExpiresAt.Location() != time.UTC {
		t.Fatalf("live heartbeats = %+v, %v", live, err)
	}
}

func TestCatalogTagsAreDerivedFromCanonicalMutationState(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 20, 15, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-tag-consistency", "team-tag-consistency-uid", now)
	admission := newRemoteCreateAdmission(binding, now.Add(time.Second), "tag-consistency", "request", []byte("content"))
	admission.Memory.Tags = []string{"different"}
	admitted, err := s.AdmitRemoteMemoryCreate(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if len(admitted.Memory.Tags) != 0 {
		t.Fatalf("catalog tags = %#v, want canonical envelope tags", admitted.Memory.Tags)
	}
}

func TestManualReplacementDemotesReviewedProposalProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 20, 30, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-reviewed-replace", "team-reviewed-replace-uid", now)
	proposal := acceptedMemoryProposalForTest(t, s, binding.Namespace, "reviewed content")
	applied, err := s.AdmitRemoteMemoryProposalApply(ctx,
		newRemoteProposalApplyAdmission(binding, proposal, now.Add(time.Second), "reviewed-replace"))
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, applied.Operation, now.Add(2*time.Second), "backend-v1", "remote-reviewed")
	active, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, applied.Memory.ID)
	if err != nil || active.Trust != store.MemoryTrustReviewed || active.SourceProposalID != proposal.ID {
		t.Fatalf("reviewed catalog = %+v, %v", active, err)
	}
	replaced, err := s.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(5*time.Second), active.ID, "reviewed-metadata-replace", "replace-request",
			store.MemoryOperationReplace, active.Generation+1, active.Generation, active.BackendVersion, proposal.Content),
		Memory:             store.RemoteMemoryCatalogEntry{Source: "manual", Tags: []string{}},
		ExpectedGeneration: active.Generation, ExpectedBackendVersion: active.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, replaced.Operation, now.Add(6*time.Second), "backend-v2", "remote-reviewed")
	updated, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Trust != store.MemoryTrustUntrusted || updated.SourceProposalID != "" || updated.Source != memorySourceManual {
		t.Fatalf("manual replacement retained reviewed provenance: %+v", updated)
	}
}

func TestMigrationFencesBindingsMissingValidatedRouteIdentity(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 30, 22, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-migration-route", "team-migration-route-uid", now)
	if _, err := s.db.Exec(`UPDATE memory_backend_bindings
		SET resolved_address_digest = '', server_certificate_digest = '', state = ?, validation_expires_at = ?
		WHERE namespace_uid = ?`, store.MemoryBackendBindingAccepting, now.Add(time.Hour), binding.NamespaceUID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	migrated, err := s.GetMemoryBackendBinding(context.Background(), binding.NamespaceUID)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.State != store.MemoryBackendBindingRecovering {
		t.Fatalf("migrated binding state = %q, want recovering", migrated.State)
	}
}

func TestCheckpointAndAuditPurgeUseReservedAuditCapacity(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 22, 15, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-audit-reserve", "team-audit-reserve-uid", now)
	var rows, size int64
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(length(id) + length(namespace_name) + length(namespace_uid) +
		length(actor) + length(action) + length(reason) + length(previous_state) + length(new_state) +
		length(memory_id) + length(operation_id) + length(proposal_id) + length(request_digest) +
		length(mutation_digest) + length(content_digest) + length(request_id) + 128), 0)
		FROM memory_audit WHERE namespace_uid = ?`, binding.NamespaceUID).Scan(&rows, &size); err != nil {
		t.Fatal(err)
	}
	governedMemoryQuotas.NamespaceAuditRows = rows
	governedMemoryQuotas.GlobalAuditRows = rows
	governedMemoryQuotas.NamespaceAuditBytes = size
	governedMemoryQuotas.GlobalAuditBytes = size
	checkpointAt := now.Add(time.Minute)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		ID: "mcheckpoint-audit-reserve", NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: 0, CheckpointDigest: protocol.ContentDigest("audit reserve checkpoint"),
		Actor: "operator", Reason: "recover audit capacity", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatalf("RecordMemoryVerifiedCheckpoint at audit quota: %v", err)
	}
	purged, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, Before: checkpointAt, PurgeAudit: true,
		Actor: "operator", Reason: "purge audit at quota", Now: checkpointAt.Add(time.Second),
	})
	if err != nil || purged.AuditRowsPurged == 0 {
		t.Fatalf("PurgeMemoryGovernance at audit quota = %+v, %v", purged, err)
	}
}

func TestActivationRequiresFreshMatchedRecoveryReceipt(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC)
	if err := s.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: "controller-a", Role: "serving_dispatching", FeatureEpoch: 1,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	binding := store.MemoryBackendBinding{
		Namespace: "team-recovery-gate", NamespaceUID: "team-recovery-gate-uid", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, BackendUID: "backend-a", BackendGeneration: 1,
		AuthorityEpoch: 1, RoutingEpoch: 1, ActivationEpoch: 1, MinimumFeatureEpoch: 1,
		SpecDigest: "spec", EndpointDigest: "endpoint", ResolvedAddressDigest: "addresses",
		ServerCertificateDigest: "certificate", SecretName: "memory-auth", SecretKey: "token",
		SecretUID: "secret-a", SecretResourceVersion: "1", TenantID: protocol.DeriveTenantID("cluster-a", "team-recovery-gate-uid"),
		StoreName: "memories", StoreUUID: "11111111-1111-4111-8111-111111111111",
		OwnershipClaim: "claim", CapabilityRevision: "cap", Protocol: "oms/v0alpha1",
		State: store.MemoryBackendBindingAccepting, ValidationExpiresAt: now.Add(time.Hour),
	}
	activation := store.MemoryBackendActivation{Binding: binding, RequiredFeatureEpoch: 1, Actor: "operator", Reason: "activate", Now: now}
	if _, err := s.ActivateMemoryBackend(ctx, activation); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("activation without recovery receipt error = %v, want ErrNotReady", err)
	}
	recordActivationRecoveryReceiptForTest(t, s, binding, now)
	if _, err := s.ActivateMemoryBackend(ctx, activation); err != nil {
		t.Fatalf("activation with recovery receipt: %v", err)
	}
}

func TestIdempotencyFromPriorAuthorityRejectedAfterCleanRestoreAndReactivation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 21, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-idempotency-rebind", "team-idempotency-rebind-uid", now)
	created, err := s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(binding, now.Add(time.Second), "stable-key", "stable-request", []byte("v1")))
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, created.Operation, now.Add(2*time.Second), "v1", "remote-stable")
	catalog, err := s.GetRemoteMemory(ctx, binding.NamespaceUID, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation: newCanonicalMemoryMutation(binding, now.Add(3*time.Second), catalog.ID, "delete-stable", "delete-request",
			store.MemoryOperationDelete, catalog.Generation+1, catalog.Generation, catalog.BackendVersion, ""),
		ExpectedGeneration: catalog.Generation, ExpectedBackendVersion: catalog.BackendVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeOperationForTest(t, s, binding, deleted.Operation, now.Add(4*time.Second), "v2", "remote-stable")
	decommissioned, err := s.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: store.MemoryBackendBindingAccepting, State: store.MemoryBackendBindingDecommissioned,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: "operator", Reason: "clean decommission", Now: now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewLegacyMemoryRestore(ctx, binding.NamespaceUID, binding.BackendUID)
	if err != nil || !preview.Restorable {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	restored, err := s.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		Actor: "operator", Reason: "clean restore", Now: now.Add(6 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	reactivated := *decommissioned
	reactivated.Mode = store.MemoryBackendModeRemote
	reactivated.State = store.MemoryBackendBindingAccepting
	reactivated.AuthorityEpoch++
	reactivated.RoutingEpoch = restored.Binding.RoutingEpoch + 1
	reactivated.ActivationEpoch = reactivated.AuthorityEpoch
	reactivated.ValidationExpiresAt = now.Add(time.Hour)
	reactivated.DecommissionedAt = nil
	recordActivationRecoveryReceiptForTest(t, s, reactivated, now.Add(7*time.Second))
	activated, err := s.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
		Binding: reactivated, RequiredFeatureEpoch: 1, Actor: "operator", Reason: "reactivate", Now: now.Add(7 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AdmitRemoteMemoryCreate(ctx, newRemoteCreateAdmission(activated.Binding, now.Add(8*time.Second), "stable-key", "stable-request", []byte("v2")))
	if !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("reused prior-authority idempotency key error = %v, want ErrDuplicateMismatch", err)
	}
}

func TestAuditPurgePreservesAuthoritativeLifecycleAndGovernanceRows(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 22, 0, 0, 0, time.UTC)
	binding := activateMemoryBackendForTest(t, s, "team-audit-authority", "team-audit-authority-uid", now)
	for _, action := range []string{"backend.lifecycle.intent", "backend.lifecycle.requested", "memory.trust", "memory.disable", "memory.transient"} {
		if err := s.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
			Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: "operator",
			Action: action, MemoryID: "mem-a", RequestDigest: protocol.ContentDigest(action), CreatedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkpointAt := now.Add(40 * 24 * time.Hour)
	checkpoint, err := s.RecordMemoryVerifiedCheckpoint(ctx, store.MemoryVerifiedCheckpoint{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		MaximumOperationSequence: 0, CheckpointDigest: protocol.ContentDigest("audit checkpoint"),
		Actor: "operator", Reason: "audit retention", VerifiedAt: checkpointAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeMemoryGovernance(ctx, store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: checkpoint.ID, Before: now.Add(24 * time.Hour), PurgeAudit: true,
		Actor: "operator", Reason: "purge non-authoritative audit", Now: checkpointAt,
	}); err != nil {
		t.Fatal(err)
	}
	records, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: binding.NamespaceUID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, record := range records {
		seen[record.Action] = true
	}
	for _, action := range []string{"backend.lifecycle.intent", "backend.lifecycle.requested", "memory.trust", "memory.disable"} {
		if !seen[action] {
			t.Fatalf("authoritative audit action %q was purged", action)
		}
	}
	if seen["memory.transient"] {
		t.Fatal("non-authoritative audit row was not purged")
	}
}
