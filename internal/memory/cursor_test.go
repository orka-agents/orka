package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

func mustProtocolBinding(t *testing.T, binding *store.MemoryBackendBinding) protocol.Binding {
	t.Helper()
	identity, err := protocolBinding(binding)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestRemoteSearchCursorIsOpaqueAndBoundToQueryAndAuthority(t *testing.T) {
	governed := newMemoryTestStore(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	binding := &store.MemoryBackendBinding{
		ClusterID: "cluster-a", NamespaceUID: "namespace-a",
		BackendUID:     "22222222-2222-4222-8222-222222222222",
		AuthorityEpoch: 1, RoutingEpoch: 2,
		TenantID:  protocol.DeriveTenantID("cluster-a", "namespace-a"),
		StoreUUID: "44444444-4444-4444-8444-444444444444",
	}
	state := remoteSearchCursor{
		ProviderToken: "placeholder", PageSize: 4, ActualMode: protocol.SearchModeKeyword,
		SeenRecordState: newRemoteSearchSeenRecordState(),
		Pending: []remoteSearchCursorRecord{{
			MemoryID: "mem-2", UpsertKey: protocol.CanonicalUpsertKey(mustProtocolBinding(t, binding), "mem-2"),
			Generation: 2, BackendMemoryID: "backend-record-id",
			ContentDigest: protocol.ContentDigest("two"),
		}},
	}
	cursor, err := saveRemoteSearchCursor(context.Background(), governed, binding, "query-a", state, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cursor, "provider") || strings.Contains(cursor, "mem-2") ||
		!strings.HasPrefix(cursor, remoteSearchCursorPrefix) {
		t.Fatalf("cursor exposed provider state: %q", cursor)
	}
	decoded, err := loadRemoteSearchCursor(context.Background(), governed, binding, "query-a", cursor, now.Add(time.Minute))
	if err != nil || decoded.ProviderToken != "placeholder" || decoded.PageSize != 4 || len(decoded.Pending) != 1 {
		t.Fatalf("decode = %#v, %v", decoded, err)
	}
	if !remoteSearchSeenRecordStatePresent(decoded.SeenRecordState) {
		t.Fatalf("decoded cursor is missing its seen-record filter: %#v", decoded)
	}
	if _, err := loadRemoteSearchCursor(context.Background(), governed, binding, "query-b", cursor, now.Add(time.Minute)); err == nil {
		t.Fatal("cursor accepted a different query")
	}
	changed := *binding
	changed.RoutingEpoch++
	if _, err := loadRemoteSearchCursor(context.Background(), governed, &changed, "query-a", cursor, now.Add(time.Minute)); err == nil {
		t.Fatal("cursor accepted a different routing epoch")
	}
	if _, err := loadRemoteSearchCursor(
		context.Background(), governed, binding, "query-a", cursor, now.Add(remoteSearchCursorTTL+time.Second),
	); err == nil {
		t.Fatal("cursor accepted after expiry")
	}
}

func TestRemoteSearchCursorRejectsLegacyFormat(t *testing.T) {
	binding := &store.MemoryBackendBinding{
		ClusterID: "cluster-a", NamespaceUID: "namespace-a",
		BackendUID:     "22222222-2222-4222-8222-222222222222",
		AuthorityEpoch: 1, RoutingEpoch: 2,
		TenantID:  protocol.DeriveTenantID("cluster-a", "namespace-a"),
		StoreUUID: "44444444-4444-4444-8444-444444444444",
	}
	_, err := loadRemoteSearchCursor(
		t.Context(), newGovernedSearchStore(nil), binding, "query-a", "msc-legacy-format", time.Now().UTC(),
	)
	if !errors.Is(err, errLegacyRemoteSearchCursor) {
		t.Fatalf("legacy cursor error = %v, want %v", err, errLegacyRemoteSearchCursor)
	}
}

func TestRemoteSearchSeenRecordStateTracksExactBoundedDigests(t *testing.T) {
	state := newRemoteSearchSeenRecordState()
	for i := range remoteSearchSeenRecordDigestMaximum {
		seen, err := rememberRemoteSearchRecordIdentity(&state, fmt.Sprintf("identity-%04d", i))
		if err != nil || seen {
			t.Fatalf("remember identity %d: seen=%v err=%v", i, seen, err)
		}
	}
	wantBytes := 1 + remoteSearchSeenRecordDigestMaximum*remoteSearchSeenRecordDigestBytes
	if len(state) != wantBytes || !validRemoteSearchSeenRecordState(state) {
		t.Fatalf("seen-record state bytes = %d, want valid %d", len(state), wantBytes)
	}
	seen, err := rememberRemoteSearchRecordIdentity(&state, "identity-0000")
	if err != nil || !seen {
		t.Fatalf("repeat identity: seen=%v err=%v", seen, err)
	}
	seen, err = rememberRemoteSearchRecordIdentity(&state, "identity-over-capacity")
	if seen || !errors.Is(err, errRemoteSearchIdentityCapacity) {
		t.Fatalf("over-capacity identity: seen=%v err=%v", seen, err)
	}
}

func TestServiceLegacyCursorStoreBoundsReplayState(t *testing.T) {
	service := &Service{}
	cursorStore := serviceLegacyCursorStore{service: service}
	now := time.Date(2026, time.August, 3, 18, 0, 0, 0, time.UTC)
	for index := range legacySearchCursorReplayRows {
		cursor := store.MemorySearchCursorState{
			ID: fmt.Sprintf("lsc-replay-%04d", index), NamespaceUID: "namespace-a",
			State: []byte{byte(index)}, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
		}
		if err := cursorStore.SaveMemorySearchCursor(t.Context(), cursor); err != nil {
			t.Fatalf("save replay cursor %d: %v", index, err)
		}
		if err := cursorStore.RetireMemorySearchCursor(t.Context(), cursor.NamespaceUID, cursor.ID, now); err != nil {
			t.Fatalf("retire replay cursor %d: %v", index, err)
		}
	}
	overflow := store.MemorySearchCursorState{
		ID: "lsc-replay-overflow", NamespaceUID: "namespace-a", State: []byte("overflow"),
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := cursorStore.SaveMemorySearchCursor(t.Context(), overflow); err != nil {
		t.Fatalf("save active overflow cursor: %v", err)
	}
	if err := cursorStore.RetireMemorySearchCursor(
		t.Context(), overflow.NamespaceUID, overflow.ID, now,
	); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("retire overflow cursor error = %v, want ErrCapacity", err)
	}
	if len(service.legacyCursorRetired) != legacySearchCursorReplayRows {
		t.Fatalf("retired fallback cursor rows = %d, want %d",
			len(service.legacyCursorRetired), legacySearchCursorReplayRows)
	}
}

func TestLegacySearchCursorIsBoundToNamespaceQueryAndExpiry(t *testing.T) {
	governed := newMemoryTestStore(t)
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	state := legacySearchCursor{
		PageSize: 2, BeforeUpdatedAt: now, BeforeID: "mem-a", PendingIDs: []string{"mem-pending"},
	}
	cursor, err := saveLegacySearchCursor(t.Context(), governed, authority, "query-a", state, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cursor, legacySearchCursorPrefix) || strings.Contains(cursor, "mem-a") || strings.Contains(cursor, "query-a") {
		t.Fatalf("legacy cursor was not opaque: %q", cursor)
	}
	decoded, err := loadLegacySearchCursor(t.Context(), governed, authority, "query-a", cursor, now.Add(time.Minute))
	if err != nil || decoded.PageSize != 2 || !decoded.BeforeUpdatedAt.Equal(now) ||
		decoded.BeforeID != "mem-a" || len(decoded.PendingIDs) != 1 || decoded.PendingIDs[0] != "mem-pending" {
		t.Fatalf("decode = %#v, %v", decoded, err)
	}
	changed := *authority
	changed.NamespaceUID = "namespace-b"
	if _, err := loadLegacySearchCursor(t.Context(), governed, &changed, "query-a", cursor, now.Add(time.Minute)); err == nil {
		t.Fatal("legacy cursor accepted a different namespace")
	}
	if _, err := loadLegacySearchCursor(t.Context(), governed, authority, "query-b", cursor, now.Add(time.Minute)); err == nil {
		t.Fatal("legacy cursor accepted a different query")
	}
	if _, err := loadLegacySearchCursor(
		t.Context(), governed, authority, "query-a", cursor, now.Add(remoteSearchCursorTTL+time.Second),
	); err == nil {
		t.Fatal("legacy cursor accepted after expiry")
	}
	if _, err := loadLegacySearchCursor(t.Context(), governed, authority, "query-a", legacySearchCursorPrefix+"forged", now); err == nil {
		t.Fatal("legacy cursor accepted a forged identifier")
	}
	parts := strings.Split(cursor, ".")
	parts[1] = "2"
	if _, err := loadLegacySearchCursor(t.Context(), governed, authority, "query-a", strings.Join(parts, "."), now); err == nil {
		t.Fatal("legacy cursor accepted a tampered offset")
	}
	if _, err := loadLegacySearchCursor(
		t.Context(), governed, authority, "query-a", legacySearchCursorPrefix+strings.Repeat(".", 1024), now,
	); err == nil {
		t.Fatal("legacy cursor accepted an oversized value")
	}
}
