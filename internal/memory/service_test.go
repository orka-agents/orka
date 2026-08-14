package memory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/apierror"
	"github.com/orka-agents/orka/internal/store"
	storesqlite "github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

type staticAuthorityResolver struct{ authority *ResolvedAuthority }

func (r staticAuthorityResolver) Resolve(context.Context, string) (*ResolvedAuthority, error) {
	return r.authority, nil
}
func (r staticAuthorityResolver) ResolveLocal(context.Context, string) (*ResolvedAuthority, error) {
	return r.authority, nil
}

type transientLegacySearchResolver struct {
	authority         *ResolvedAuthority
	resolveCalls      int
	resolveLocalCalls int
}

func (r *transientLegacySearchResolver) Resolve(context.Context, string) (*ResolvedAuthority, error) {
	r.resolveCalls++
	return r.authority, nil
}

func (r *transientLegacySearchResolver) ResolveLocal(context.Context, string) (*ResolvedAuthority, error) {
	r.resolveLocalCalls++
	return nil, errors.New("transient local resolver failure")
}

func TestServiceLegacyCreatePreservesSynchronousBehavior(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	service := &Service{Legacy: storeImpl, Proposals: storeImpl}
	result, err := service.CreateMemory(context.Background(), "team-a", CreateRequest{
		Content: " durable guidance ", Source: "cli", Tags: []string{"Storage", "storage"},
	}, MutationContext{})
	if err != nil {
		t.Fatalf("CreateMemory() error = %v", err)
	}
	if result.StatusCode != http.StatusCreated || result.Memory == nil || result.Operation != nil {
		t.Fatalf("result = %#v", result)
	}
	if result.Memory.Trust != store.MemoryTrustUntrusted || len(result.Memory.Tags) != 1 || result.Memory.Tags[0] != "storage" {
		t.Fatalf("memory = %#v", result.Memory)
	}
}

func TestServiceLegacySearchPaginatesWithOpaqueCursor(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		memory := &store.Memory{
			ID: fmt.Sprintf("mem-legacy-page-%d", i), Namespace: "team-a",
			Content: "legacy pagination", Source: "test",
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := storeImpl.CreateMemory(t.Context(), memory); err != nil {
			t.Fatal(err)
		}
	}
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	service := &Service{
		Legacy: storeImpl, Governed: storeImpl, Resolver: staticAuthorityResolver{authority: authority},
		Now: func() time.Time { return now },
	}
	request := SearchRequest{Query: "legacy", Limit: 1, Mode: protocol.SearchModeKeyword}
	searchContext := SearchContext{PreserveEmptyTrust: true}
	seen := make(map[string]struct{})
	pages := make([]*SearchResponse, 0, 3)
	for range 3 {
		page, err := service.Search(t.Context(), authority.Namespace, request, searchContext)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Complete != true {
			t.Fatalf("legacy page = %#v", page)
		}
		if _, duplicate := seen[page.Items[0].Memory.ID]; duplicate {
			t.Fatalf("legacy pagination repeated %q", page.Items[0].Memory.ID)
		}
		seen[page.Items[0].Memory.ID] = struct{}{}
		pages = append(pages, page)
		request.Cursor = page.Cursor
		if len(pages) == 1 {
			if err := storeImpl.DeleteMemory(t.Context(), authority.Namespace, page.Items[0].Memory.ID); err != nil {
				t.Fatal(err)
			}
			inserted := &store.Memory{
				ID: "mem-legacy-page-inserted", Namespace: authority.Namespace,
				Content: "legacy pagination", Source: "test",
				CreatedAt: now.Add(10 * time.Second), UpdatedAt: now.Add(10 * time.Second),
			}
			if err := storeImpl.CreateMemory(t.Context(), inserted); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(seen) != 3 || !pages[2].Exhausted || pages[2].Cursor != "" ||
		pages[0].Exhausted || pages[0].Cursor == "" || pages[1].Exhausted || pages[1].Cursor == "" {
		t.Fatalf("legacy pagination pages = %#v", pages)
	}
	if _, included := seen["mem-legacy-page-inserted"]; included {
		t.Fatal("legacy continuation included a record created after the first page")
	}
	replayRequest := SearchRequest{Query: "legacy", Limit: 1, Mode: protocol.SearchModeKeyword, Cursor: pages[0].Cursor}
	replayed, err := service.Search(t.Context(), authority.Namespace, replayRequest, searchContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Items) != 1 || replayed.Items[0].Memory.ID != pages[1].Items[0].Memory.ID {
		t.Fatalf("legacy cursor replay = %#v, want %#v", replayed, pages[1])
	}
	replayRequest.Query = "different"
	if _, err := service.Search(t.Context(), authority.Namespace, replayRequest, searchContext); err == nil {
		t.Fatal("legacy cursor was accepted for a different query")
	}
}

func TestServiceLegacySearchContinuationHonorsSmallerLimit(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		memory := &store.Memory{
			ID: fmt.Sprintf("mem-legacy-limit-%d", i), Namespace: "team-a",
			Content: "legacy limit", Source: "test",
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := storeImpl.CreateMemory(t.Context(), memory); err != nil {
			t.Fatal(err)
		}
	}
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	service := &Service{
		Legacy: storeImpl, Governed: storeImpl, Resolver: staticAuthorityResolver{authority: authority},
		Now: func() time.Time { return now },
	}
	first, err := service.Search(t.Context(), authority.Namespace, SearchRequest{
		Query: "legacy", Limit: 2, Mode: protocol.SearchModeKeyword,
	}, SearchContext{PreserveEmptyTrust: true})
	if err != nil || len(first.Items) != 2 || first.Cursor == "" {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	second, err := service.Search(t.Context(), authority.Namespace, SearchRequest{
		Query: "legacy", Limit: 1, Mode: protocol.SearchModeKeyword, Cursor: first.Cursor,
	}, SearchContext{PreserveEmptyTrust: true})
	if err != nil || len(second.Items) != 1 {
		t.Fatalf("smaller continuation page = %#v, %v", second, err)
	}
}

func TestServiceRemoteCreateRequiresIdempotencyAndReplays(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	binding := activateServiceTestBinding(t, storeImpl, now)
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	authority := &ResolvedAuthority{Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding, Backend: backend}
	service := &Service{
		Legacy: storeImpl, Proposals: storeImpl, Governed: storeImpl,
		Resolver: staticAuthorityResolver{authority: authority}, Now: func() time.Time { return now.Add(time.Minute) },
	}
	request := CreateRequest{Content: "remote durable guidance", Source: "api", Tags: []string{"storage"}}
	_, err := service.CreateMemory(context.Background(), binding.Namespace, request, MutationContext{Actor: "alice", Principal: "alice", Route: "createMemory"})
	var structured *apierror.Error
	if !errors.As(err, &structured) || structured.Status != http.StatusPreconditionRequired || structured.Reason != ReasonIdempotencyKeyRequired {
		t.Fatalf("unkeyed error = %#v", err)
	}
	ctx := MutationContext{Actor: "alice", Principal: "alice", Route: "createMemory", IdempotencyKey: "key-1"}
	first, err := service.CreateMemory(context.Background(), binding.Namespace, request, ctx)
	if err != nil {
		t.Fatalf("first CreateMemory() error = %v", err)
	}
	if first.StatusCode != http.StatusAccepted || first.Operation == nil || first.Memory != nil {
		t.Fatalf("first result = %#v", first)
	}
	second, err := service.CreateMemory(context.Background(), binding.Namespace, request, ctx)
	if err != nil {
		t.Fatalf("replay CreateMemory() error = %v", err)
	}
	if !second.Replayed || second.Operation == nil || second.Operation.ID != first.Operation.ID {
		t.Fatalf("replay result = %#v, first = %#v", second, first)
	}
	_, err = service.CreateMemory(context.Background(), binding.Namespace, CreateRequest{Content: "different"}, ctx)
	if !errors.As(err, &structured) || structured.Status != http.StatusConflict || structured.Reason != ReasonIdempotencyKeyReuse {
		t.Fatalf("reuse error = %#v", err)
	}
}

func newMemoryTestStore(t *testing.T) *storesqlite.Store {
	t.Helper()
	db, err := storesqlite.NewDB(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return storesqlite.NewStore(db, filepath.Join(t.TempDir(), "memory.db"))
}

func activateServiceTestBinding(t *testing.T, governed store.GovernedMemoryStore, now time.Time) store.MemoryBackendBinding {
	t.Helper()
	binding := store.MemoryBackendBinding{
		Namespace: "team-remote", NamespaceUID: "11111111-1111-4111-8111-111111111111",
		ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "22222222-2222-4222-8222-222222222222", BackendGeneration: 1,
		AuthorityEpoch: 1, RoutingEpoch: 1, SpecDigest: digestString("spec"), EndpointDigest: digestString("endpoint"),
		ResolvedAddressDigest: digestString("addresses"), ServerCertificateDigest: digestString("certificate"),
		SecretName: "memory-auth", SecretKey: "token",
		SecretUID: "33333333-3333-4333-8333-333333333333", SecretResourceVersion: "1",
		TenantID:  protocol.DeriveTenantID("cluster-a", "11111111-1111-4111-8111-111111111111"),
		StoreName: "store-a", StoreUUID: "44444444-4444-4444-8444-444444444444",
		OwnershipClaim: "claim-a", CapabilityRevision: "cap-a", Protocol: "orka.oms.v0alpha1",
		State: store.MemoryBackendBindingAccepting, ActivationEpoch: 1, MinimumFeatureEpoch: 0,
		ValidationExpiresAt: now.Add(time.Hour),
	}
	if _, err := governed.RecordMemoryActivationRecoveryReceipt(context.Background(), store.MemoryActivationRecoveryReceipt{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		RouteDigest: binding.RecoveryRouteIdentity().Digest(), StoreUUID: binding.StoreUUID,
		ManifestDigest: protocol.ContentDigest("service test recovery manifest"),
		Actor:          "test", Reason: "test recovery prerequisite", VerifiedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := governed.ActivateMemoryBackend(context.Background(), store.MemoryBackendActivation{
		Binding: binding, Actor: "test", Reason: "test activation", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Binding
}

func TestServiceLegacyTrustUsesDurableAuditOverlay(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	service := &Service{
		Legacy: storeImpl, Governed: storeImpl,
		Resolver: staticAuthorityResolver{authority: authority},
		Now:      func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) },
	}
	created, err := service.CreateMemory(context.Background(), authority.Namespace, CreateRequest{
		Content: "legacy guidance", Source: "api",
	}, MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := service.SetMemoryTrust(context.Background(), authority.Namespace, created.Memory.ID, TrustRequest{
		Trust: store.MemoryTrustReviewed, Reason: "initial review",
	}, TrustContext{Actor: "alice", RequestID: "request-a"})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Trust != store.MemoryTrustReviewed || reviewed.GovernanceRevision != 2 {
		t.Fatalf("reviewed memory = %#v", reviewed)
	}
	updated, err := service.SetMemoryTrust(context.Background(), authority.Namespace, created.Memory.ID, TrustRequest{
		Trust: store.MemoryTrustTrusted, Reason: "operator review",
	}, TrustContext{Actor: "alice", RequestID: "request-b"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Trust != store.MemoryTrustTrusted || updated.GovernanceRevision != 3 {
		t.Fatalf("updated memory = %#v", updated)
	}
	got, err := service.GetMemory(context.Background(), authority.Namespace, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Trust != store.MemoryTrustTrusted || got.GovernanceRevision != 3 {
		t.Fatalf("persisted memory = %#v", got)
	}
	if err := service.SetMemoryDisabled(context.Background(), authority.Namespace, created.Memory.ID, true, "alice", "request-c"); err != nil {
		t.Fatal(err)
	}
	disabled, err := service.GetMemory(context.Background(), authority.Namespace, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled.Disabled || disabled.Trust != store.MemoryTrustTrusted || disabled.GovernanceRevision != 4 {
		t.Fatalf("disabled memory = %#v", disabled)
	}
	if err := service.SetMemoryDisabled(context.Background(), authority.Namespace, created.Memory.ID, false, "alice", "request-d"); err != nil {
		t.Fatal(err)
	}

	tags := []string{"updated"}
	mutation, err := service.UpdateMemory(context.Background(), authority.Namespace, created.Memory.ID, UpdateRequest{Tags: &tags}, MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Memory.Disabled || mutation.Memory.Trust != store.MemoryTrustUntrusted || mutation.Memory.GovernanceRevision != 6 {
		t.Fatalf("updated mutation memory = %#v", mutation.Memory)
	}
	reloaded, err := service.GetMemory(context.Background(), authority.Namespace, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Disabled || reloaded.Trust != store.MemoryTrustUntrusted || reloaded.GovernanceRevision != 6 {
		t.Fatalf("reloaded memory = %#v", reloaded)
	}
	listed, err := service.ListMemories(context.Background(), store.MemoryFilter{
		Namespace: authority.Namespace, Trust: []store.MemoryTrust{store.MemoryTrustTrusted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("trusted list after provenance change = %#v, want empty", listed)
	}
}

func TestServiceLegacyTrustFilterFailsClosedBeyondScanCap(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	service := &Service{
		Legacy: storeImpl, Governed: storeImpl,
		Resolver: staticAuthorityResolver{authority: authority},
	}

	var oldestID string
	for i := range maxRemoteCatalogLimit + 1 {
		created, err := service.CreateMemory(context.Background(), authority.Namespace, CreateRequest{
			Content: "legacy guidance", Source: "api", Tags: []string{"storage"},
		}, MutationContext{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldestID = created.Memory.ID
		}
	}
	if _, err := service.SetMemoryTrust(context.Background(), authority.Namespace, oldestID, TrustRequest{
		Trust: store.MemoryTrustReviewed, Reason: "review older memory",
	}, TrustContext{Actor: "alice", RequestID: "request-a"}); err != nil {
		t.Fatal(err)
	}

	memories, err := service.ListMemories(context.Background(), store.MemoryFilter{
		Namespace: authority.Namespace,
		Trust:     []store.MemoryTrust{store.MemoryTrustReviewed},
		Limit:     1,
	})
	var structured *apierror.Error
	if !errors.As(err, &structured) || structured.Status != http.StatusServiceUnavailable ||
		structured.Reason != ReasonResultSetIncomplete {
		t.Fatalf("ListMemories() = (%#v, %#v), want strict incomplete error", memories, err)
	}
}

func TestServiceLegacyProposalReplayReturnsGovernanceOverlay(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	service := &Service{
		Legacy: storeImpl, Proposals: storeImpl, Governed: storeImpl,
		Resolver: staticAuthorityResolver{authority: authority},
	}
	proposal := &store.MemoryProposal{
		Namespace: authority.Namespace, Type: "memory", Title: "reviewed guidance",
		Content: "Keep durable memory governance explicit.",
	}
	if err := storeImpl.CreateMemoryProposal(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if err := storeImpl.ReviewMemoryProposal(context.Background(), store.MemoryProposalReview{
		Namespace: authority.Namespace, ID: proposal.ID, Status: "accepted", Reviewer: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := service.ApplyMemoryProposal(context.Background(), authority.Namespace, proposal.ID, "alice", MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetMemoryTrust(context.Background(), authority.Namespace, first.Memory.ID, TrustRequest{
		Trust: store.MemoryTrustTrusted, Reason: "operator approval",
	}, TrustContext{Actor: "alice", RequestID: "request-a"}); err != nil {
		t.Fatal(err)
	}

	replayed, err := service.ApplyMemoryProposal(context.Background(), authority.Namespace, proposal.ID, "alice", MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Memory.Trust != store.MemoryTrustTrusted || replayed.Memory.GovernanceRevision != 2 {
		t.Fatalf("replayed proposal memory = %#v", replayed.Memory)
	}
}

func TestServiceLegacySearchReusesResolvedAuthorityWhenLocalResolverFails(t *testing.T) {
	ctx := context.Background()
	storeImpl := newMemoryTestStore(t)
	authority := &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a"}
	service := &Service{
		Legacy: storeImpl, Proposals: storeImpl, Governed: storeImpl,
		Resolver: staticAuthorityResolver{authority: authority},
	}
	proposal := &store.MemoryProposal{
		Namespace: authority.Namespace, Type: "memory", Title: "demoted proposal guidance",
		Content: "Never return this demoted proposal memory from default search.",
	}
	if err := storeImpl.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if err := storeImpl.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: authority.Namespace, ID: proposal.ID, Status: "accepted", Reviewer: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := service.ApplyMemoryProposal(ctx, authority.Namespace, proposal.ID, "alice", MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	demoted, err := service.SetMemoryTrust(ctx, authority.Namespace, applied.Memory.ID, TrustRequest{
		Trust: store.MemoryTrustUntrusted, Reason: "proposal no longer approved",
	}, TrustContext{Actor: "alice", RequestID: "request-demote"})
	if err != nil {
		t.Fatal(err)
	}
	if demoted.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("demoted memory trust = %q, want untrusted", demoted.Trust)
	}
	raw, err := storeImpl.GetMemory(ctx, authority.Namespace, applied.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Trust != store.MemoryTrustReviewed {
		t.Fatalf("raw proposal memory trust = %q, want reviewed so the audit overlay is required", raw.Trust)
	}

	resolver := &transientLegacySearchResolver{authority: authority}
	service.Resolver = resolver
	response, err := service.Search(ctx, authority.Namespace, SearchRequest{
		Query: "demoted proposal memory", Limit: 10,
	}, SearchContext{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(response.Items) != 0 {
		t.Fatalf("Search() returned demoted proposal memory: %#v", response.Items)
	}
	if resolver.resolveCalls != 1 || resolver.resolveLocalCalls != 0 {
		t.Fatalf("resolver calls fresh/local = %d/%d, want 1/0", resolver.resolveCalls, resolver.resolveLocalCalls)
	}
}

func TestServiceLegacyUpdatePreservesPointerCompatibilityFields(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	service := &Service{Legacy: storeImpl}
	created, err := service.CreateMemory(context.Background(), "team-a", CreateRequest{
		Content: "legacy guidance", SessionName: "session-old", AgentName: "agent-old",
		TaskName: "task-old", ParentTask: "parent-old",
	}, MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	newAgent := "agent-new"
	newParent := "parent-new"
	updated, err := service.UpdateMemory(context.Background(), "team-a", created.Memory.ID, UpdateRequest{
		SessionName: &empty, AgentName: &newAgent, ParentTask: &newParent,
	}, MutationContext{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Memory.SessionName != "" || updated.Memory.AgentName != newAgent ||
		updated.Memory.TaskName != "task-old" || updated.Memory.ParentTask != newParent {
		t.Fatalf("updated memory = %#v", updated.Memory)
	}
}

func TestRemoteListExcludesPendingCreateWithoutHydration(t *testing.T) {
	storeImpl := newMemoryTestStore(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	binding := activateServiceTestBinding(t, storeImpl, now)
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	authority := &ResolvedAuthority{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding, Backend: backend,
		Adapter: &fakeOMSAdapter{},
	}
	service := &Service{Governed: storeImpl, Resolver: staticAuthorityResolver{authority: authority}, Now: func() time.Time { return now.Add(time.Minute) }}
	created, err := service.CreateMemory(context.Background(), binding.Namespace, CreateRequest{Content: "pending"}, MutationContext{
		Actor: "alice", Principal: "alice", Route: "createMemory", IdempotencyKey: "pending-key",
	})
	if err != nil || created.Operation == nil {
		t.Fatalf("CreateMemory() = %#v, %v", created, err)
	}
	page, err := service.ListMemoriesPageWithSearchContext(context.Background(), store.MemoryFilter{
		Namespace: binding.Namespace, Limit: 10,
	}, SearchContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 || !page.Paginated || !page.Exhausted || !page.Complete {
		t.Fatalf("page = %#v", page)
	}
}
