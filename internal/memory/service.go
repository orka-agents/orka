package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/apierror"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	defaultMemoryContentBytes         = 64 << 10
	defaultMemoryOperationMaxAge      = 7 * 24 * time.Hour
	defaultMemoryIdempotencyRetention = 30 * 24 * time.Hour
	defaultMemoryOperationRetryAfter  = 2 * time.Second
	defaultRemoteCatalogLimit         = 100
	maxRemoteCatalogLimit             = 200
	maxRemoteSearchCandidates         = 1000
	maxRemoteSearchPages              = 20
	maxRemoteListCandidates           = 1000
	maxRemoteListPages                = 20
	maxRemoteListHydrationConcurrency = omsHTTPMaxConnsPerHost
	remoteListCursorTTL               = 5 * time.Minute
	maxRemoteListCursorBytes          = 4 << 10
	legacyMemoryDisableAuditAction    = "memory.disable"
	legacyMemoryTrustAuditAction      = "memory.trust"
	memorySourceProposal              = "memory_proposal"
	memorySourceManual                = "manual"
)

// Service is the single governed entry point for legacy and remote memory.
type Service struct {
	Legacy       store.MemoryStore
	Proposals    store.MemoryProposalStore
	Governed     store.GovernedMemoryStore
	Resolver     AuthorityResolver
	Dispatcher   *Dispatcher
	Now          func() time.Time
	ContentLimit int

	legacyCursorMu      sync.Mutex
	legacyCursors       map[string]store.MemorySearchCursorState
	legacyCursorRetired map[string]bool
}

type legacyMemoryGovernanceStore interface {
	SetLegacyMemoryDisabledWithAudit(
		ctx context.Context,
		namespace, namespaceUID, id string,
		disabled bool,
		actor, reason, requestID string,
		now time.Time,
	) error
}

type legacyMemoryUpdateGovernanceStore interface {
	UpdateLegacyMemoryWithAudit(
		ctx context.Context,
		memory *store.Memory,
		namespaceUID, actor, reason, requestID string,
		now time.Time,
	) (*store.Memory, error)
}

type legacyMemoryTrustGovernanceStore interface {
	SetLegacyMemoryTrustWithAudit(
		ctx context.Context,
		expected *store.Memory,
		namespaceUID string,
		trust store.MemoryTrust,
		actor, reason, requestID string,
		now time.Time,
	) (*store.Memory, error)
}

type remoteMemoryGenerationWatermarkStore interface {
	GetRemoteMemoryGenerationWatermark(
		ctx context.Context,
		namespaceUID, id, backendUID string,
		authorityEpoch int64,
		storeUUID string,
	) (int64, error)
}

type legacyMemoryProposalGovernanceStore interface {
	ApplyLegacyMemoryProposalWithAudit(
		ctx context.Context,
		apply store.MemoryProposalApply,
		namespaceUID string,
	) (*store.Memory, []store.MemoryAuditRecord, error)
}

// ListMemories lists through the durable authority selected for the namespace incarnation.
func (s *Service) ListMemories(ctx context.Context, filter store.MemoryFilter) ([]store.Memory, error) {
	page, err := s.listMemoriesPage(ctx, filter, SearchContext{})
	if err != nil {
		return nil, err
	}
	return strictListPageItems(page)
}

// ListMemoriesWithSearchContext lists memories while carrying server-derived
// authorization that can be checked at the exact remote-search egress point.
func (s *Service) ListMemoriesWithSearchContext(
	ctx context.Context,
	filter store.MemoryFilter,
	searchContext SearchContext,
) ([]store.Memory, error) {
	page, err := s.listMemoriesPage(ctx, filter, searchContext)
	if err != nil {
		return nil, err
	}
	return strictListPageItems(page)
}

func strictListPageItems(page *ListPage) ([]store.Memory, error) {
	if page == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory list returned no page")
	}
	if !page.Complete {
		memoryIncompleteTotal.Inc()
		return nil, &IncompleteSearchError{
			Cause: apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory list scan budget was exhausted"),
			Cursor: page.Cursor,
		}
	}
	return page.Items, nil
}

// ListMemoriesPageWithSearchContext returns deterministic continuation metadata
// for remote authority while preserving the existing legacy list behavior.
func (s *Service) ListMemoriesPageWithSearchContext(
	ctx context.Context,
	filter store.MemoryFilter,
	searchContext SearchContext,
) (*ListPage, error) {
	return s.listMemoriesPage(ctx, filter, searchContext)
}

func (s *Service) listMemoriesPage(
	ctx context.Context,
	filter store.MemoryFilter,
	searchContext SearchContext,
) (*ListPage, error) {
	authority, err := s.resolve(ctx, filter.Namespace, true)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		if s.Legacy == nil {
			return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
		}
		if len(filter.Trust) > 0 {
			response, searchErr := s.searchLegacy(ctx, authority, filter.Namespace, SearchRequest{
				Query: filter.Query, Tags: filter.Tags, IDs: filter.IDs, Sources: sourceFilterValues(filter.Source),
				SessionName: filter.SessionName, TaskName: filter.TaskName, ParentTask: filter.ParentTask, AgentName: filter.AgentName,
				Trust: filter.Trust, Limit: filter.Limit, Cursor: filter.Cursor,
				IncludeDisabled: filter.IncludeDisabled, IncludeDeleted: filter.IncludeDeleted, AllowIncomplete: true,
			}, protocol.SearchModeKeyword)
			if searchErr != nil {
				return nil, searchErr
			}
			memories := make([]store.Memory, 0, len(response.Items))
			for _, item := range response.Items {
				memories = append(memories, item.Memory)
			}
			return &ListPage{
				Items: memories, Cursor: response.Cursor, Exhausted: response.Exhausted,
				Complete: response.Complete, Paginated: true,
			}, nil
		}
		memories, listErr := s.Legacy.ListMemories(ctx, filter)
		if listErr != nil {
			return nil, mapStoreError(listErr)
		}
		filtered, filterErr := s.applyLegacyGovernanceFilter(ctx, authority, memories, nil, boundedMemoryLimit(filter.Limit))
		if filterErr != nil {
			return nil, filterErr
		}
		return &ListPage{Items: filtered, Complete: true}, nil
	}
	if err := requireRemoteRead(authority); err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter.Query) != "" {
		searchContext.PreserveEmptyTrust = true
		response, searchErr := s.search(ctx, filter.Namespace, SearchRequest{
			Query: filter.Query, Tags: filter.Tags, IDs: filter.IDs,
			Sources: sourceFilterValues(filter.Source), SessionName: filter.SessionName,
			TaskName: filter.TaskName, ParentTask: filter.ParentTask, AgentName: filter.AgentName,
			Trust: filter.Trust, Limit: filter.Limit, Cursor: filter.Cursor,
			IncludeDisabled: filter.IncludeDisabled, IncludeDeleted: filter.IncludeDeleted,
			Mode: protocol.SearchModeKeyword,
		}, searchContext)
		if searchErr != nil {
			return nil, searchErr
		}
		memories := make([]store.Memory, 0, len(response.Items))
		for _, item := range response.Items {
			memories = append(memories, item.Memory)
		}
		return &ListPage{
			Items: memories, Cursor: response.Cursor, Exhausted: response.Exhausted,
			Complete: response.Complete, Paginated: true,
		}, nil
	}
	return s.listRemoteMemoriesPage(ctx, authority, filter)
}

type remoteListCursor struct {
	BeforeUpdatedAt time.Time `json:"t"`
	BeforeID        string    `json:"i"`
}

func (s *Service) listRemoteMemoriesPage(
	ctx context.Context,
	authority *ResolvedAuthority,
	filter store.MemoryFilter,
) (*ListPage, error) {
	limit := boundedMemoryLimit(filter.Limit)
	result := make([]store.Memory, 0, limit)
	queryDigest := remoteListQueryDigest(filter)
	consumedCursor := strings.TrimSpace(filter.Cursor)
	cursor, err := loadRemoteListCursor(ctx, s.Governed, authority.Binding, queryDigest, consumedCursor, s.now())
	if err != nil {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid or expired memory list cursor")
	}
	var beforeUpdatedAt *time.Time
	beforeID := cursor.BeforeID
	if !cursor.BeforeUpdatedAt.IsZero() {
		value := cursor.BeforeUpdatedAt.UTC()
		beforeUpdatedAt = &value
	}
	states := []store.MemoryMaterializationState{store.MemoryMaterializationActive}
	if filter.IncludeDeleted {
		states = append(states, store.MemoryMaterializationDeleted)
	}
	scanned := 0
	pages := 0
	exhausted := false
	for len(result) < limit && scanned < maxRemoteListCandidates && pages < maxRemoteListPages {
		pageSize := min(maxRemoteCatalogLimit, maxRemoteListCandidates-scanned)
		entries, err := s.Governed.ListRemoteMemories(ctx, store.RemoteMemoryCatalogFilter{
			NamespaceUID: authority.NamespaceUID, IDs: filter.IDs, Trust: filter.Trust,
			IncludeDisabled: filter.IncludeDisabled || filter.IncludeDeleted, IncludeDeleted: filter.IncludeDeleted,
			States: states, BeforeUpdatedAt: beforeUpdatedAt, BeforeID: beforeID, Limit: pageSize,
		})
		if err != nil {
			return nil, mapStoreError(err)
		}
		if len(entries) == 0 {
			exhausted = true
			break
		}
		pages++
		processed := 0
		remaining := limit - len(result)
		candidates := make([]store.RemoteMemoryCatalogEntry, 0, min(remaining, len(entries)))
		for i := range entries {
			entry := &entries[i]
			processed++
			scanned++
			updatedAt := entry.UpdatedAt.UTC()
			beforeUpdatedAt = &updatedAt
			beforeID = entry.ID
			if !entryMatchesAuthority(entry, authority.Binding) || !memoryEntryMatchesFilter(entry, filter) {
				continue
			}
			candidates = append(candidates, *entry)
			if len(candidates) == remaining {
				break
			}
		}
		hydrated, hydrateErr := s.hydrateRemoteListEntries(ctx, authority, candidates, filter.IncludeDisabled)
		if hydrateErr != nil {
			return nil, hydrateErr
		}
		result = append(result, hydrated...)
		if processed == len(entries) && len(entries) < pageSize {
			exhausted = true
			break
		}
	}
	nextCursor := ""
	if !exhausted {
		if beforeUpdatedAt == nil || beforeID == "" {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete, "memory catalog pagination did not advance")
		}
		nextCursor, err = saveRemoteListCursor(ctx, s.Governed, authority.Binding, queryDigest, remoteListCursor{
			BeforeUpdatedAt: *beforeUpdatedAt, BeforeID: beforeID,
		}, consumedCursor, s.now())
		if err != nil {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory list continuation could not be preserved")
		}
	} else if consumedCursor != "" {
		if err := s.Governed.RetireMemorySearchCursor(ctx, authority.NamespaceUID, consumedCursor, s.now()); err != nil {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory list cursor could not be retired")
		}
	}
	if len(result) < limit && !exhausted {
		memoryIncompleteTotal.Inc()
		return nil, &IncompleteSearchError{
			Cause: apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory list scan budget was exhausted"),
			Cursor: nextCursor,
		}
	}
	return &ListPage{Items: result, Cursor: nextCursor, Exhausted: exhausted, Complete: true, Paginated: true}, nil
}

func (s *Service) hydrateRemoteListEntries(
	ctx context.Context,
	authority *ResolvedAuthority,
	entries []store.RemoteMemoryCatalogEntry,
	allowDisabled bool,
) ([]store.Memory, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	memories := make([]*store.Memory, len(entries))
	errs := make([]error, len(entries))
	jobs := make(chan int, len(entries))
	for i := range entries {
		jobs <- i
	}
	close(jobs)

	workerCount := min(maxRemoteListHydrationConcurrency, len(entries))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for i := range jobs {
				memories[i], errs[i] = s.hydrate(ctx, authority, &entries[i], allowDisabled)
			}
		}()
	}
	workers.Wait()

	result := make([]store.Memory, 0, len(entries))
	for i := range entries {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if memories[i] == nil {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
				"memory backend hydration returned no record")
		}
		result = append(result, *memories[i])
	}
	return result, nil
}

func remoteListQueryDigest(filter store.MemoryFilter) string {
	return digestJSON(struct {
		SessionName, AgentName, TaskName, ParentTask, Source string
		Tags, IDs                                            []string
		Trust                                                []store.MemoryTrust
		IncludeDisabled, IncludeDeleted                      bool
		Limit                                                int
	}{
		SessionName: filter.SessionName, AgentName: filter.AgentName, TaskName: filter.TaskName,
		ParentTask: filter.ParentTask, Source: filter.Source, Tags: filter.Tags, IDs: filter.IDs,
		Trust: filter.Trust, IncludeDisabled: filter.IncludeDisabled,
		IncludeDeleted: filter.IncludeDeleted, Limit: boundedMemoryLimit(filter.Limit),
	})
}

func saveRemoteListCursor(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	binding *store.MemoryBackendBinding,
	queryDigest string,
	state remoteListCursor,
	consumedCursor string,
	now time.Time,
) (string, error) {
	if governed == nil || binding == nil || state.BeforeUpdatedAt.IsZero() || strings.TrimSpace(state.BeforeID) == "" {
		return "", errors.New("memory list cursor state is unavailable")
	}
	identity, err := protocolBinding(binding)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(state)
	if err != nil || len(payload) == 0 || len(payload) > maxRemoteListCursorBytes {
		return "", errors.New("memory list cursor state is invalid")
	}
	id := "mlc-" + uuid.NewString()
	if consumedCursor = strings.TrimSpace(consumedCursor); consumedCursor != "" {
		id = remoteListSuccessorID(consumedCursor, queryDigest, state)
		if err := governed.RetireMemorySearchCursor(ctx, binding.NamespaceUID, consumedCursor, now); err != nil {
			return "", err
		}
	}
	if err := governed.SaveMemorySearchCursor(ctx, store.MemorySearchCursorState{
		ID: id, NamespaceUID: binding.NamespaceUID, BindingDigest: protocol.BindingDigest(identity),
		QueryDigest: queryDigest, State: payload, CreatedAt: now, ExpiresAt: now.Add(remoteListCursorTTL),
	}); err != nil {
		return "", err
	}
	return id, nil
}

func remoteListSuccessorID(consumedCursor, queryDigest string, state remoteListCursor) string {
	shapeDigest := digestJSON(struct {
		ConsumedCursor string           `json:"cursor"`
		QueryDigest    string           `json:"queryDigest"`
		State          remoteListCursor `json:"state"`
	}{ConsumedCursor: consumedCursor, QueryDigest: queryDigest, State: state})
	return "mlc-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(shapeDigest)).String()
}

func loadRemoteListCursor(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	binding *store.MemoryBackendBinding,
	queryDigest, encoded string,
	now time.Time,
) (remoteListCursor, error) {
	if strings.TrimSpace(encoded) == "" {
		return remoteListCursor{}, nil
	}
	if governed == nil || binding == nil || !strings.HasPrefix(encoded, "mlc-") || len(encoded) > 128 {
		return remoteListCursor{}, errors.New("invalid memory list cursor")
	}
	identity, err := protocolBinding(binding)
	if err != nil {
		return remoteListCursor{}, err
	}
	stored, err := governed.GetMemorySearchCursor(ctx, binding.NamespaceUID, encoded, now)
	if err != nil {
		return remoteListCursor{}, err
	}
	if stored.BindingDigest != protocol.BindingDigest(identity) || stored.QueryDigest != queryDigest ||
		len(stored.State) == 0 || len(stored.State) > maxRemoteListCursorBytes {
		return remoteListCursor{}, errors.New("mismatched memory list cursor")
	}
	var cursor remoteListCursor
	if err := json.Unmarshal(stored.State, &cursor); err != nil || cursor.BeforeUpdatedAt.IsZero() ||
		strings.TrimSpace(cursor.BeforeID) == "" {
		return remoteListCursor{}, errors.New("invalid memory list cursor state")
	}
	return cursor, nil
}

// GetMemory returns exact local suppression metadata while keeping disabled content suppressed.
func (s *Service) GetMemory(ctx context.Context, namespace, id string) (*store.Memory, error) {
	return s.GetMemoryWithVisibility(ctx, namespace, id, false)
}

// GetMemoryWithVisibility returns exact memory content when disabled inspection is explicitly authorized.
func (s *Service) GetMemoryWithVisibility(
	ctx context.Context,
	namespace, id string,
	includeDisabled bool,
) (*store.Memory, error) {
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		if s.Legacy == nil {
			return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
		}
		memory, getErr := s.Legacy.GetMemory(ctx, namespace, id)
		if getErr != nil {
			return nil, mapStoreError(getErr)
		}
		if overlayErr := s.applyLegacyGovernance(ctx, authority, memory); overlayErr != nil {
			return nil, overlayErr
		}
		if memory.Disabled && !includeDisabled {
			memory.Content = ""
			memory.ContentAvailable = false
		}
		return memory, nil
	}
	entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if !entryMatchesAuthority(entry, authority.Binding) {
		return nil, apierror.New(http.StatusConflict, ReasonIdentityMismatch, "memory belongs to a different backend authority")
	}
	if entry.Deleted || entry.MaterializationState == store.MemoryMaterializationDeleted ||
		entry.MaterializationState == store.MemoryMaterializationOrphaned {
		memory := remoteEntryToMemory(entry, "")
		return &memory, nil
	}
	if entry.MaterializationState == store.MemoryMaterializationPending || entry.Generation == 0 {
		memory := remoteEntryToMemory(entry, "")
		return &memory, nil
	}
	if entry.Disabled && !includeDisabled {
		memory := remoteEntryToMemory(entry, "")
		return &memory, nil
	}
	fresh, err := s.resolve(ctx, namespace, true)
	if err != nil {
		return nil, err
	}
	if err := requireRemoteRead(fresh); err != nil {
		return nil, err
	}
	return s.hydrate(ctx, fresh, entry, includeDisabled)
}

// CreateMemory preserves synchronous legacy behavior and durably admits remote work.
func (s *Service) CreateMemory(
	ctx context.Context,
	namespace string,
	request CreateRequest,
	mutationContext MutationContext,
) (*MutationResult, error) {
	if err := validateDirectMemorySource(request.Source); err != nil {
		return nil, err
	}
	localAuthority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !localAuthority.Remote() {
		return s.createLegacy(ctx, namespace, request)
	}
	if err := requireIdempotency(mutationContext); err != nil {
		return nil, err
	}
	requestDigest := digestJSON(request)
	if replay, found, replayErr := s.lookupMutationReplay(ctx, localAuthority, mutationContext, requestDigest); found || replayErr != nil {
		return replay, replayErr
	}

	authority, err := s.resolve(ctx, namespace, true)
	if err != nil {
		return nil, err
	}
	if err := requireRemoteMutation(authority, false); err != nil {
		return nil, err
	}
	content, tags, err := s.normalizeContent(request.Content, request.Tags)
	if err != nil {
		return nil, err
	}
	memoryID := strings.TrimSpace(request.ID)
	if memoryID == "" {
		memoryID = "mem-" + uuid.NewString()
	}
	expectedGeneration, desiredGeneration, err := s.remoteCreateGenerations(ctx, authority, memoryID)
	if err != nil {
		return nil, err
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	now := s.now()
	operationID := "mop-" + uuid.NewString()
	metadata := memoryMetadata(request)
	envelope := protocol.MutationEnvelope{
		ProtocolVersion:        protocol.Version,
		OperationID:            operationID,
		Binding:                binding,
		MemoryID:               memoryID,
		Kind:                   protocol.MutationKindCreate,
		Generation:             uint64(desiredGeneration),
		ExpectedGeneration:     uint64(expectedGeneration),
		ExpectedBackendVersion: "",
		State:                  &protocol.MutationState{Content: content, Tags: tags, Metadata: metadata},
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid memory mutation")
	}
	payload, _ := json.Marshal(envelope)
	if err := validateRemoteMutationLimits(authority, envelope.State, payload); err != nil {
		return nil, err
	}
	catalog := store.RemoteMemoryCatalogEntry{
		ID: memoryID, Namespace: namespace, NamespaceUID: authority.NamespaceUID,
		ClusterID: authority.Binding.ClusterID, BackendUID: authority.Binding.BackendUID,
		AuthorityEpoch: authority.Binding.AuthorityEpoch, RoutingEpoch: authority.Binding.RoutingEpoch,
		TenantID: authority.Binding.TenantID, StoreUUID: authority.Binding.StoreUUID,
		Generation: expectedGeneration, DesiredGeneration: desiredGeneration, GovernanceRevision: 1,
		MaterializationState: store.MemoryMaterializationPending,
		Trust:                store.MemoryTrustUntrusted, SessionName: metadata["sessionName"], AgentName: metadata["agentName"],
		TaskName: metadata["taskName"], ParentTask: metadata["parentTask"], Source: metadata["source"], Tags: tags,
		ContentDigest: envelope.ContentDigest, ContentAvailable: false, PendingOperationID: operationID,
		CreatedAt: now, UpdatedAt: now,
	}
	admission, err := s.Governed.AdmitRemoteMemoryCreate(ctx, store.RemoteMemoryCreateAdmission{
		Mutation: s.mutationAdmission(mutationContext, authority, memoryID, operationID, "", requestDigest, envelope, payload, now),
		Memory:   catalog,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	memoryEnqueueTotal.WithLabelValues(string(store.MemoryOperationCreate)).Inc()
	return s.materializeOrQueue(ctx, authority, admission, http.StatusCreated)
}

// UpdateMemory hydrates and verifies current remote content before admitting a full replacement.
func (s *Service) UpdateMemory(
	ctx context.Context,
	namespace, id string,
	request UpdateRequest,
	mutationContext MutationContext,
) (*MutationResult, error) {
	localAuthority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !localAuthority.Remote() {
		return s.updateLegacy(ctx, localAuthority, namespace, id, request, mutationContext)
	}
	if err := requireIdempotency(mutationContext); err != nil {
		return nil, err
	}
	requestDigest := digestJSON(struct {
		ID      string        `json:"id"`
		Request UpdateRequest `json:"request"`
	}{id, request})
	if replay, found, replayErr := s.lookupMutationReplay(ctx, localAuthority, mutationContext, requestDigest); found || replayErr != nil {
		return replay, replayErr
	}

	authority, err := s.resolve(ctx, namespace, true)
	if err != nil {
		return nil, err
	}
	if err := requireRemoteMutation(authority, false); err != nil {
		return nil, err
	}
	entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if !entryMatchesAuthority(entry, authority.Binding) {
		return nil, identityError()
	}
	if entry.MaterializationState == store.MemoryMaterializationPending || entry.PendingOperationID != "" ||
		entry.DesiredGeneration != entry.Generation {
		return nil, apierror.New(http.StatusConflict, ReasonOperationInProgress,
			"memory content materialization is still in progress")
	}
	current, err := s.hydrate(ctx, authority, entry, true)
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "verified current memory content is unavailable")
	}
	if request.SessionName != nil {
		current.SessionName = *request.SessionName
	}
	if request.AgentName != nil {
		current.AgentName = *request.AgentName
	}
	if request.TaskName != nil {
		current.TaskName = *request.TaskName
	}
	if request.ParentTask != nil {
		current.ParentTask = *request.ParentTask
	}
	if request.Content != nil {
		current.Content = *request.Content
	}
	if request.Source != nil {
		current.Source = *request.Source
	}
	if request.Tags != nil {
		current.Tags = append([]string(nil), (*request.Tags)...)
	}
	content, tags, err := s.normalizeContent(current.Content, current.Tags)
	if err != nil {
		return nil, err
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	now := s.now()
	desired := max(entry.Generation, entry.DesiredGeneration) + 1
	operationID := "mop-" + uuid.NewString()
	metadata := replacementMemoryMetadata(entry, *current, content, tags)
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version, OperationID: operationID, Binding: binding,
		MemoryID: id, Kind: protocol.MutationKindReplace, Generation: uint64(desired),
		ExpectedGeneration: uint64(entry.Generation), ExpectedBackendVersion: entry.BackendVersion,
		State: &protocol.MutationState{Content: content, Tags: tags, Metadata: metadata},
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid memory mutation")
	}
	payload, _ := json.Marshal(envelope)
	if err := validateRemoteMutationLimits(authority, envelope.State, payload); err != nil {
		return nil, err
	}
	updatedEntry := *entry
	updatedEntry.DesiredGeneration = desired
	updatedEntry.PendingOperationID = operationID
	updatedEntry.Tags = tags
	updatedEntry.SessionName = metadata["sessionName"]
	updatedEntry.AgentName = metadata["agentName"]
	updatedEntry.TaskName = metadata["taskName"]
	updatedEntry.ParentTask = metadata["parentTask"]
	updatedEntry.Source = metadata["source"]
	updatedEntry.SourceProposalID = metadata["sourceProposalId"]
	updatedEntry.ContentDigest = envelope.ContentDigest
	updatedEntry.UpdatedAt = now
	admission, err := s.Governed.AdmitRemoteMemoryReplace(ctx, store.RemoteMemoryReplaceAdmission{
		Mutation:               s.mutationAdmission(mutationContext, authority, id, operationID, "", requestDigest, envelope, payload, now),
		Memory:                 updatedEntry,
		ExpectedGeneration:     entry.Generation,
		ExpectedBackendVersion: entry.BackendVersion,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	memoryEnqueueTotal.WithLabelValues(string(store.MemoryOperationReplace)).Inc()
	return s.materializeOrQueue(ctx, authority, admission, http.StatusOK)
}

// DeleteMemory installs local suppression before any remote dependency is contacted.
func (s *Service) DeleteMemory(
	ctx context.Context,
	namespace, id string,
	mutationContext MutationContext,
) (*MutationResult, error) {
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		if s.Legacy == nil {
			return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
		}
		if err := s.Legacy.DeleteMemory(ctx, namespace, id); err != nil {
			return nil, mapStoreError(err)
		}
		return &MutationResult{StatusCode: http.StatusNoContent}, nil
	}
	if err := requireIdempotency(mutationContext); err != nil {
		return nil, err
	}
	requestDigest := digestJSON(struct {
		ID string `json:"id"`
	}{id})
	if replay, found, replayErr := s.lookupMutationReplay(ctx, authority, mutationContext, requestDigest); found || replayErr != nil {
		return replay, replayErr
	}
	if authority.Binding.State != store.MemoryBackendBindingAccepting && authority.Binding.State != store.MemoryBackendBindingDraining {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is not accepting safety mutations")
	}
	entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if !entryMatchesAuthority(entry, authority.Binding) {
		return nil, identityError()
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	now := s.now()
	desired := max(entry.Generation, entry.DesiredGeneration) + 1
	operationID := "mop-" + uuid.NewString()
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version, OperationID: operationID, Binding: binding,
		MemoryID: id, Kind: protocol.MutationKindDelete, Generation: uint64(desired),
		ExpectedGeneration: uint64(entry.Generation), ExpectedBackendVersion: entry.BackendVersion, State: nil,
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid memory delete")
	}
	payload, _ := json.Marshal(envelope)
	if err := validateRemoteMutationLimits(authority, nil, payload); err != nil {
		return nil, err
	}
	admission, err := s.Governed.AdmitRemoteMemoryDelete(ctx, store.RemoteMemoryDeleteAdmission{
		Mutation:               s.mutationAdmission(mutationContext, authority, id, operationID, "", requestDigest, envelope, payload, now),
		ExpectedGeneration:     entry.Generation,
		ExpectedBackendVersion: entry.BackendVersion,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	memoryEnqueueTotal.WithLabelValues(string(store.MemoryOperationDelete)).Inc()
	// Try immediate dispatch only if fresh credentials are currently available;
	// the tombstone remains admitted even when this fails.
	fresh, freshErr := s.Resolver.Resolve(ctx, namespace)
	if freshErr == nil {
		authority = fresh
	}
	return s.materializeOrQueue(ctx, authority, admission, http.StatusNoContent)
}

// SetMemoryDisabled changes only the local governance overlay.
func (s *Service) SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool, actor, requestID string) error {
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return err
	}
	if !authority.Remote() {
		if s.Legacy == nil {
			return apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
		}
		if s.Governed == nil || strings.TrimSpace(authority.NamespaceUID) == "" {
			return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
				"legacy memory governance is unavailable")
		}
		auditActor := strings.TrimSpace(actor)
		if auditActor == "" {
			auditActor = "memory-operator"
		}
		governedLegacy, ok := s.Legacy.(legacyMemoryGovernanceStore)
		if !ok {
			return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
				"legacy memory governance does not support atomic disable changes")
		}
		return mapStoreError(governedLegacy.SetLegacyMemoryDisabledWithAudit(
			ctx, namespace, authority.NamespaceUID, id, disabled, auditActor,
			"local memory governance change", requestID, s.now(),
		))
	}
	entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, id)
	if err != nil {
		return mapStoreError(err)
	}
	if !entryMatchesAuthority(entry, authority.Binding) {
		return identityError()
	}
	if !disabled && (entry.Deleted || entry.MaterializationState == store.MemoryMaterializationDiverged ||
		entry.MaterializationState == store.MemoryMaterializationLost || entry.MaterializationState == store.MemoryMaterializationOrphaned) {
		return apierror.New(http.StatusConflict, ReasonDiverged, "memory cannot be enabled in its current state")
	}
	_, err = s.Governed.SetRemoteMemoryDisabled(ctx, store.RemoteMemoryDisabledChange{
		NamespaceUID: authority.NamespaceUID, ID: id, Disabled: disabled,
		ExpectedGovernanceRevision: entry.GovernanceRevision,
		Actor:                      actor, Reason: "local memory governance change", RequestID: requestID, Now: s.now(),
	})
	return mapStoreError(err)
}

// SetMemoryTrust changes only server-owned local trust after explicit authorization.
func (s *Service) SetMemoryTrust(
	ctx context.Context,
	namespace, id string,
	request TrustRequest,
	trustContext TrustContext,
) (*store.Memory, error) {
	if request.Trust != store.MemoryTrustUntrusted && request.Trust != store.MemoryTrustReviewed && request.Trust != store.MemoryTrustTrusted {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid memory trust value")
	}
	if strings.TrimSpace(request.Reason) == "" {
		return nil, apierror.New(http.StatusBadRequest, "", "reason is required")
	}
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		if s.Legacy == nil {
			return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
		}
		memory, getErr := s.Legacy.GetMemory(ctx, namespace, id)
		if getErr != nil {
			return nil, mapStoreError(getErr)
		}
		if overlayErr := s.applyLegacyGovernance(ctx, authority, memory); overlayErr != nil {
			return nil, overlayErr
		}
		if memory.Trust == request.Trust {
			return memory, nil
		}
		if s.Governed == nil || strings.TrimSpace(authority.NamespaceUID) == "" {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "legacy memory trust governance is unavailable")
		}
		actor := strings.TrimSpace(trustContext.Actor)
		if actor == "" {
			actor = "memory-operator"
		}
		governedLegacy, ok := s.Legacy.(legacyMemoryTrustGovernanceStore)
		if !ok {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
				"legacy memory governance does not support atomic trust changes")
		}
		updated, trustErr := governedLegacy.SetLegacyMemoryTrustWithAudit(
			ctx, memory, authority.NamespaceUID, request.Trust, actor, request.Reason, trustContext.RequestID, s.now(),
		)
		if trustErr != nil {
			return nil, mapStoreError(trustErr)
		}
		return updated, nil
	}
	if trustContext.AuthorizeRemote != nil {
		if err := trustContext.AuthorizeRemote(); err != nil {
			return nil, err
		}
	}
	entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if !entryMatchesAuthority(entry, authority.Binding) {
		return nil, identityError()
	}
	updated, err := s.Governed.SetRemoteMemoryTrust(ctx, store.RemoteMemoryTrustChange{
		NamespaceUID: authority.NamespaceUID, ID: id, Trust: request.Trust,
		ExpectedGovernanceRevision: entry.GovernanceRevision,
		Actor:                      trustContext.Actor, Reason: request.Reason, RequestID: trustContext.RequestID, Now: s.now(),
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	memory := remoteEntryToMemory(updated, "")
	return &memory, nil
}

// GetMemoryOperation returns an allowlisted operation summary.
func (s *Service) GetMemoryOperation(ctx context.Context, namespace, id string) (*store.MemoryOperation, error) {
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		return nil, store.ErrNotFound
	}
	operation, err := s.Governed.GetMemoryOperation(ctx, authority.NamespaceUID, id)
	return operation, mapStoreError(err)
}

// ListMemoryOperations returns bounded operation summaries for the active namespace incarnation.
func (s *Service) ListMemoryOperations(ctx context.Context, namespace string, filter store.MemoryOperationFilter) ([]store.MemoryOperation, error) {
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		return []store.MemoryOperation{}, nil
	}
	filter.NamespaceUID = authority.NamespaceUID
	operations, err := s.Governed.ListMemoryOperations(ctx, filter)
	return operations, mapStoreError(err)
}

func (s *Service) createLegacy(ctx context.Context, namespace string, request CreateRequest) (*MutationResult, error) {
	if s.Legacy == nil {
		return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
	}
	content, tags, err := s.normalizeContent(request.Content, request.Tags)
	if err != nil {
		return nil, err
	}
	metadata := memoryMetadata(request)
	memory := &store.Memory{
		ID: strings.TrimSpace(request.ID), Namespace: namespace, SessionName: metadata["sessionName"],
		AgentName: metadata["agentName"], TaskName: metadata["taskName"], ParentTask: metadata["parentTask"],
		Source: metadata["source"], Content: content, Tags: tags,
		Trust: store.MemoryTrustUntrusted,
	}
	if err := s.Legacy.CreateMemory(ctx, memory); err != nil {
		return nil, mapStoreError(err)
	}
	return &MutationResult{Memory: memory, StatusCode: http.StatusCreated}, nil
}

func (s *Service) updateLegacy(
	ctx context.Context,
	authority *ResolvedAuthority,
	namespace, id string,
	request UpdateRequest,
	mutationContext MutationContext,
) (*MutationResult, error) {
	if s.Legacy == nil {
		return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
	}
	memory, err := s.Legacy.GetMemory(ctx, namespace, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if overlayErr := s.applyLegacyGovernance(ctx, authority, memory); overlayErr != nil {
		return nil, overlayErr
	}
	if request.SessionName != nil {
		memory.SessionName = *request.SessionName
	}
	if request.AgentName != nil {
		memory.AgentName = *request.AgentName
	}
	if request.TaskName != nil {
		memory.TaskName = *request.TaskName
	}
	if request.ParentTask != nil {
		memory.ParentTask = *request.ParentTask
	}
	if request.Content != nil {
		memory.Content = *request.Content
	}
	if request.Source != nil {
		memory.Source = *request.Source
	}
	if request.Tags != nil {
		memory.Tags = append([]string(nil), (*request.Tags)...)
	}
	content, tags, err := s.normalizeContent(memory.Content, memory.Tags)
	if err != nil {
		return nil, err
	}
	memory.Content, memory.Tags = content, tags
	metadata := memoryMetadataFromStore(*memory)
	memory.SessionName = metadata["sessionName"]
	memory.AgentName = metadata["agentName"]
	memory.TaskName = metadata["taskName"]
	memory.ParentTask = metadata["parentTask"]
	memory.Source = metadata["source"]
	memory.SourceProposalID = metadata["sourceProposalId"]
	if authority != nil && strings.TrimSpace(authority.NamespaceUID) != "" {
		governedLegacy, ok := s.Legacy.(legacyMemoryUpdateGovernanceStore)
		if !ok {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
				"legacy memory governance does not support atomic trust demotion")
		}
		actor := mutationPrincipal(mutationContext)
		if actor == "" {
			actor = "authenticated-memory-writer"
		}
		reason := strings.TrimSpace(mutationContext.Reason)
		if reason == "" {
			reason = "legacy memory updated"
		}
		updated, err := governedLegacy.UpdateLegacyMemoryWithAudit(
			ctx, memory, authority.NamespaceUID, actor, reason, mutationContext.RequestID, s.now(),
		)
		if err != nil {
			return nil, mapStoreError(err)
		}
		return &MutationResult{Memory: updated, StatusCode: http.StatusOK}, nil
	}
	if err := s.Legacy.UpdateMemory(ctx, memory); err != nil {
		return nil, mapStoreError(err)
	}
	updated, err := s.Legacy.GetMemory(ctx, namespace, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	updated.Trust = memory.Trust
	updated.GovernanceRevision = memory.GovernanceRevision
	return &MutationResult{Memory: updated, StatusCode: http.StatusOK}, nil
}

func (s *Service) mutationAdmission(
	mutationContext MutationContext,
	authority *ResolvedAuthority,
	memoryID, operationID, proposalID, requestDigest string,
	envelope protocol.MutationEnvelope,
	payload []byte,
	now time.Time,
) store.MemoryMutationAdmission {
	principal := mutationPrincipal(mutationContext)
	locationBase := strings.TrimSpace(mutationContext.LocationBase)
	if locationBase == "" {
		locationBase = "/api/v1/memory-operations/"
	}
	trimmedLocationBase := strings.TrimRight(locationBase, "/")
	location := trimmedLocationBase + "/" + operationID
	if trimmedLocationBase == "/api/v1/memory-operations" {
		query := url.Values{}
		query.Set("namespace", authority.Namespace)
		location += "?" + query.Encode()
	}
	return store.MemoryMutationAdmission{
		Namespace: authority.Namespace, NamespaceUID: authority.NamespaceUID,
		ClusterID: authority.Binding.ClusterID, BackendUID: authority.Binding.BackendUID,
		AuthorityEpoch: authority.Binding.AuthorityEpoch, RoutingEpoch: authority.Binding.RoutingEpoch,
		MemoryID: memoryID, OperationID: operationID, ProposalID: proposalID,
		Principal: principal, Route: mutationContext.Route, IdempotencyKey: mutationContext.IdempotencyKey,
		RequestDigest:           requestDigest,
		OperationIdempotencyKey: operationID, MutationDigest: envelope.MutationDigest,
		ContentDigest: envelope.ContentDigest, Payload: payload, Actor: mutationContext.Actor,
		Reason: mutationContext.Reason, RequestID: mutationContext.RequestID,
		OriginalStatus: http.StatusAccepted, ResponseType: store.MemoryIdempotencyOperation,
		Location: location, RetryAfterSeconds: int(defaultMemoryOperationRetryAfter / time.Second),
		Now: now, MaxAgeAt: now.Add(defaultMemoryOperationMaxAge),
		IdempotencyExpiresAt: now.Add(defaultMemoryIdempotencyRetention),
	}
}

func (s *Service) materializeOrQueue(
	ctx context.Context,
	authority *ResolvedAuthority,
	admission *store.MemoryMutationAdmissionResult,
	immediateStatus int,
) (*MutationResult, error) {
	if admission == nil {
		return nil, apierror.New(http.StatusInternalServerError, ReasonBackendUnavailable, "memory admission returned no result")
	}
	operation := admission.Operation
	if admission.Replayed && admission.Idempotency.OriginalStatus == http.StatusAccepted {
		publicOperation := OperationFromStore(operation)
		return &MutationResult{
			Operation: &publicOperation, StatusCode: http.StatusAccepted,
			Location:   admission.Idempotency.Location,
			RetryAfter: time.Duration(admission.Idempotency.RetryAfterSeconds) * time.Second,
			Replayed:   true,
		}, nil
	}
	if authority != nil && authority.Adapter != nil && s.Dispatcher != nil {
		_, _ = s.Dispatcher.DispatchImmediate(ctx, authority.Namespace, operation.ID)
		if refreshed, err := s.Governed.GetMemoryOperation(ctx, authority.NamespaceUID, operation.ID); err == nil {
			operation = *refreshed
		}
	}
	if operation.State == store.MemoryOperationSucceeded {
		if immediateStatus == http.StatusNoContent {
			return &MutationResult{StatusCode: http.StatusNoContent, Replayed: admission.Replayed}, nil
		}
		entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, admission.Memory.ID)
		if err != nil {
			return nil, mapStoreError(err)
		}
		if !entryMatchesAuthority(entry, authority.Binding) {
			return nil, identityError()
		}
		memory, err := s.hydrate(ctx, authority, entry, true)
		if err != nil {
			return nil, err
		}
		return &MutationResult{Memory: memory, StatusCode: immediateStatus, Replayed: admission.Replayed}, nil
	}
	publicOperation := OperationFromStore(operation)
	return &MutationResult{
		Operation: &publicOperation, StatusCode: http.StatusAccepted,
		Location: admission.Idempotency.Location, RetryAfter: time.Duration(admission.Idempotency.RetryAfterSeconds) * time.Second,
		Replayed: admission.Replayed,
	}, nil
}

func (s *Service) hydrate(
	ctx context.Context,
	authority *ResolvedAuthority,
	entry *store.RemoteMemoryCatalogEntry,
	allowDisabled bool,
) (*store.Memory, error) {
	if entry == nil {
		return nil, store.ErrNotFound
	}
	if entry.Disabled && !allowDisabled {
		memory := remoteEntryToMemory(entry, "")
		return &memory, nil
	}
	if entry.Deleted || entry.MaterializationState == store.MemoryMaterializationDeleted ||
		entry.MaterializationState == store.MemoryMaterializationOrphaned {
		memory := remoteEntryToMemory(entry, "")
		return &memory, nil
	}
	if entry.MaterializationState == store.MemoryMaterializationDiverged || entry.MaterializationState == store.MemoryMaterializationLost {
		return nil, apierror.New(http.StatusConflict, ReasonDiverged, "remote memory materialization is not trusted")
	}
	if authority == nil || authority.Adapter == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend content is unavailable")
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	response, err := authority.Adapter.Get(ctx, protocol.GetRequest{
		ProtocolVersion: protocol.Version, Binding: binding,
		UpsertKey: protocol.CanonicalUpsertKey(binding, entry.ID),
	})
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend content is unavailable")
	}
	if response.Binding != binding {
		s.markMaterializationIssue(ctx, entry, store.MemoryMaterializationDiverged, "provider binding identity mismatch")
		return nil, apierror.New(http.StatusConflict, ReasonDiverged, "memory backend materialization has wrong identity")
	}
	if !response.Found || response.Record == nil {
		s.markMaterializationIssue(ctx, entry, store.MemoryMaterializationLost, "provider materialization missing")
		return nil, apierror.New(http.StatusConflict, ReasonDiverged, "memory backend materialization is missing")
	}
	record := response.Record
	if record.MemoryID != entry.ID || record.UpsertKey != protocol.CanonicalUpsertKey(binding, entry.ID) ||
		int64(record.Generation) != entry.Generation || record.BackendVersion != entry.BackendVersion ||
		record.BackendMemoryID != entry.BackendMemoryID || record.ContentDigest != entry.ContentDigest ||
		protocol.ContentDigest(record.Content) != entry.ContentDigest || record.State != protocol.RecordStateLive ||
		!remoteRecordCollectionsMatchCatalog(record, entry) {
		s.markMaterializationIssue(ctx, entry, store.MemoryMaterializationDiverged, "provider materialization verification failed")
		return nil, apierror.New(http.StatusConflict, ReasonDiverged, "memory backend materialization failed verification")
	}
	memory := remoteEntryToMemory(entry, record.Content)
	memory.ContentAvailable = true
	return &memory, nil
}

func (s *Service) markMaterializationIssue(
	ctx context.Context,
	entry *store.RemoteMemoryCatalogEntry,
	state store.MemoryMaterializationState,
	reason string,
) {
	if s == nil || s.Governed == nil || entry == nil {
		return
	}
	memoryDivergenceTotal.WithLabelValues(string(state)).Inc()
	_, _ = s.Governed.MarkRemoteMemoryMaterializationIssue(ctx, store.RemoteMemoryMaterializationIssue{
		NamespaceUID: entry.NamespaceUID, ID: entry.ID, BackendUID: entry.BackendUID,
		AuthorityEpoch: entry.AuthorityEpoch, RoutingEpoch: entry.RoutingEpoch,
		ExpectedGeneration: entry.Generation, ExpectedBackendVersion: entry.BackendVersion,
		State: state, Actor: "orka-memory-hydrator", Reason: reason, Now: s.now(),
	})
}

func (s *Service) remoteCreateGenerations(
	ctx context.Context,
	authority *ResolvedAuthority,
	memoryID string,
) (int64, int64, error) {
	if authority == nil || authority.Binding == nil {
		return 0, 0, identityError()
	}
	watermarks, ok := s.Governed.(remoteMemoryGenerationWatermarkStore)
	if !ok {
		return 0, 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory generation watermark store is unavailable")
	}
	expected, err := watermarks.GetRemoteMemoryGenerationWatermark(
		ctx, authority.NamespaceUID, memoryID, authority.Binding.BackendUID,
		authority.Binding.AuthorityEpoch, authority.Binding.StoreUUID,
	)
	if err != nil {
		return 0, 0, mapStoreError(err)
	}
	if expected < 0 || expected == math.MaxInt64 {
		return 0, 0, apierror.New(http.StatusConflict, ReasonDiverged,
			"memory generation watermark is exhausted")
	}
	return expected, expected + 1, nil
}

func (s *Service) normalizeContent(content string, tags []string) (string, []string, error) {
	content = redact.SensitiveText(content)
	if strings.TrimSpace(content) == "" {
		return "", nil, apierror.New(http.StatusBadRequest, "", "content is required")
	}
	limit := s.ContentLimit
	if limit <= 0 || limit > protocol.MaxContentBytes {
		limit = defaultMemoryContentBytes
	}
	if len([]byte(content)) > limit {
		return "", nil, apierror.New(http.StatusRequestEntityTooLarge, "", "memory content exceeds the configured limit")
	}
	safeTags := redact.SensitiveStringSlice(append([]string{}, nonNilStrings(tags)...))
	normalized, err := protocol.NormalizeTags(safeTags)
	if err != nil {
		return "", nil, apierror.New(http.StatusBadRequest, "", "invalid memory tags")
	}
	return content, normalized, nil
}

func validateRemoteMutationLimits(
	authority *ResolvedAuthority,
	state *protocol.MutationState,
	payload []byte,
) error {
	if authority == nil || authority.Backend == nil || authority.Backend.Status.ObservedCapabilities == nil {
		return nil
	}
	limits := authority.Backend.Status.ObservedCapabilities.Limits
	exceeds := limits.MaxRequestBytes > 0 && int64(len(payload)) > limits.MaxRequestBytes
	if state != nil {
		exceeds = exceeds || limits.MaxContentBytes > 0 && int64(len(state.Content)) > limits.MaxContentBytes ||
			limits.MaxTags > 0 && len(state.Tags) > int(limits.MaxTags) ||
			limits.MaxMetadataEntries > 0 && len(state.Metadata) > int(limits.MaxMetadataEntries)
		for _, tag := range state.Tags {
			exceeds = exceeds || limits.MaxTagBytes > 0 && len(tag) > int(limits.MaxTagBytes)
		}
		for key, value := range state.Metadata {
			exceeds = exceeds || limits.MaxMetadataKeyBytes > 0 && len(key) > int(limits.MaxMetadataKeyBytes) ||
				limits.MaxMetadataValueBytes > 0 && len(value) > int(limits.MaxMetadataValueBytes)
		}
	}
	if exceeds {
		return apierror.New(http.StatusRequestEntityTooLarge, "",
			"memory mutation exceeds the active backend's advertised limits")
	}
	return nil
}

func validateRemoteSearchQueryLimit(authority *ResolvedAuthority, query string) error {
	if authority == nil || authority.Backend == nil || authority.Backend.Status.ObservedCapabilities == nil {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search capabilities are unavailable")
	}
	maxQueryBytes := protocol.MaxQueryBytes
	if advertised := authority.Backend.Status.ObservedCapabilities.Limits.MaxQueryBytes; advertised > 0 {
		maxQueryBytes = min(maxQueryBytes, int(advertised))
	}
	if len(query) > maxQueryBytes {
		return apierror.New(http.StatusRequestEntityTooLarge, "",
			"memory search exceeds the active backend's advertised limits")
	}
	return nil
}

func validateRemoteSearchLimits(authority *ResolvedAuthority, request *protocol.SearchRequest) error {
	if err := protocol.ValidateSearchRequest(request); err != nil {
		return apierror.New(http.StatusBadRequest, "", "memory search query is invalid")
	}
	if err := validateRemoteSearchQueryLimit(authority, request.Query); err != nil {
		return err
	}
	maxRequestBytes := int64(protocol.MaxHTTPBodyBytes)
	if advertised := authority.Backend.Status.ObservedCapabilities.Limits.MaxRequestBytes; advertised > 0 {
		maxRequestBytes = min(maxRequestBytes, advertised)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return apierror.New(http.StatusBadRequest, "", "memory search request is invalid")
	}
	if int64(len(payload)) > maxRequestBytes {
		return apierror.New(http.StatusRequestEntityTooLarge, "",
			"memory search exceeds the active backend's advertised limits")
	}
	return nil
}

func (s *Service) resolve(ctx context.Context, namespace string, fresh bool) (*ResolvedAuthority, error) {
	if s.Resolver == nil || s.Governed == nil {
		return &ResolvedAuthority{Namespace: namespace}, nil
	}
	if fresh {
		return s.Resolver.Resolve(ctx, namespace)
	}
	return s.Resolver.ResolveLocal(ctx, namespace)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func requireRemoteRead(authority *ResolvedAuthority) error {
	if authority == nil || authority.Backend == nil || authority.Binding == nil || authority.Adapter == nil {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is unavailable")
	}
	switch authority.Backend.Status.EffectiveLifecycleState {
	case corev1alpha1.MemoryBackendEffectiveLifecycleActive, corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly:
		if authority.Backend.Status.Ready {
			return nil
		}
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is not ready")
	case corev1alpha1.MemoryBackendEffectiveLifecycleDisabled:
		return apierror.New(http.StatusConflict, ReasonBackendDisabled, "memory backend is disabled")
	case corev1alpha1.MemoryBackendEffectiveLifecycleRemoved, corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned:
		return apierror.New(http.StatusGone, ReasonBackendRemoved, "memory backend has been removed")
	default:
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend lifecycle is unavailable")
	}
}

func requireRemoteMutation(authority *ResolvedAuthority, deleteOnly bool) error {
	if authority == nil || authority.Backend == nil || authority.Binding == nil {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is unavailable")
	}
	if deleteOnly {
		return nil
	}
	switch authority.Backend.Status.EffectiveLifecycleState {
	case corev1alpha1.MemoryBackendEffectiveLifecycleActive:
		if authority.Backend.Status.Ready {
			return nil
		}
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is not ready")
	case corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly:
		return apierror.New(http.StatusConflict, ReasonBackendReadOnly, "memory backend is read-only")
	case corev1alpha1.MemoryBackendEffectiveLifecycleDisabled:
		return apierror.New(http.StatusConflict, ReasonBackendDisabled, "memory backend is disabled")
	case corev1alpha1.MemoryBackendEffectiveLifecycleRemoved, corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned:
		return apierror.New(http.StatusGone, ReasonBackendRemoved, "memory backend has been removed")
	default:
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend lifecycle is unavailable")
	}
}

func requireIdempotency(mutationContext MutationContext) error {
	if strings.TrimSpace(mutationContext.IdempotencyKey) == "" {
		return apierror.New(http.StatusPreconditionRequired, ReasonIdempotencyKeyRequired,
			"Idempotency-Key is required for remote memory mutations; upgrade the client and retry")
	}
	if len(mutationContext.IdempotencyKey) > 256 {
		return apierror.New(http.StatusBadRequest, "", "Idempotency-Key is too long")
	}
	return nil
}

func mutationPrincipal(mutationContext MutationContext) string {
	principal := strings.TrimSpace(mutationContext.Principal)
	if principal == "" {
		principal = strings.TrimSpace(mutationContext.Actor)
	}
	return principal
}

// lookupMutationReplay checks caller idempotency against local durable state before
// any fresh backend validation or provider hydration. Its bounded immutable snapshot
// reproduces the original status, headers, operation, and successful memory body.
func (s *Service) lookupMutationReplay(
	ctx context.Context,
	localAuthority *ResolvedAuthority,
	mutationContext MutationContext,
	requestDigest string,
) (*MutationResult, bool, error) {
	if localAuthority == nil || !localAuthority.Remote() {
		return nil, false, nil
	}
	if s.Governed == nil {
		return nil, true, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory idempotency store is unavailable")
	}
	record, err := s.Governed.GetMemoryIdempotency(ctx, localAuthority.NamespaceUID, mutationPrincipal(mutationContext),
		strings.TrimSpace(mutationContext.Route), strings.TrimSpace(mutationContext.IdempotencyKey))
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, mapStoreError(err)
	}
	if localAuthority.Binding == nil || record.AuthorityEpoch != localAuthority.Binding.AuthorityEpoch ||
		record.RoutingEpoch != localAuthority.Binding.RoutingEpoch {
		return nil, true, apierror.New(http.StatusConflict, ReasonIdempotencyKeyReuse,
			"Idempotency-Key belongs to a prior memory authority or routing binding")
	}
	if record.RequestDigest != requestDigest {
		return nil, true, apierror.New(http.StatusConflict, ReasonIdempotencyKeyReuse,
			"Idempotency-Key was reused with different input")
	}
	var snapshot struct {
		Memory    store.RemoteMemoryCatalogEntry `json:"memory"`
		Operation store.MemoryOperation          `json:"operation"`
		Content   []byte                         `json:"content"`
	}
	if len(record.ResponseSnapshot) > 0 {
		if err := json.Unmarshal(record.ResponseSnapshot, &snapshot); err != nil ||
			snapshot.Memory.ID != record.MemoryID || snapshot.Operation.ID != record.OperationID {
			return nil, true, apierror.New(http.StatusInternalServerError, ReasonBackendUnavailable,
				"memory idempotency snapshot is invalid")
		}
	}
	status := record.OriginalStatus
	if status == 0 {
		status = http.StatusAccepted
	}
	result := &MutationResult{
		StatusCode: status, Location: record.Location,
		RetryAfter: time.Duration(record.RetryAfterSeconds) * time.Second, Replayed: true,
	}
	switch record.ResponseType {
	case store.MemoryIdempotencyOperation:
		operation := &snapshot.Operation
		if operation.ID == "" {
			var getErr error
			operation, getErr = s.Governed.GetMemoryOperation(ctx, localAuthority.NamespaceUID, record.OperationID)
			if getErr != nil {
				return nil, true, mapStoreError(getErr)
			}
		}
		publicOperation := OperationFromStore(*operation)
		result.Operation = &publicOperation
		return result, true, nil
	case store.MemoryIdempotencyEmpty:
		return result, true, nil
	case store.MemoryIdempotencyMemory:
		entry := &snapshot.Memory
		if entry.ID == "" || len(record.ResponseSnapshot) == 0 {
			return nil, true, apierror.New(http.StatusInternalServerError, ReasonBackendUnavailable,
				"memory idempotency response snapshot is unavailable")
		}
		if entry.NamespaceUID != localAuthority.NamespaceUID {
			return nil, true, identityError()
		}
		memory := remoteEntryToMemory(entry, string(snapshot.Content))
		result.Memory = &memory
		return result, true, nil
	default:
		return nil, true, apierror.New(http.StatusInternalServerError, ReasonBackendUnavailable,
			"memory idempotency response is invalid")
	}
}

func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apierror.New(http.StatusNotFound, "", "memory resource not found")
	case errors.Is(err, store.ErrDuplicateMismatch):
		return apierror.New(http.StatusConflict, ReasonIdempotencyKeyReuse, "Idempotency-Key was reused with different input")
	case errors.Is(err, store.ErrConflict):
		return apierror.New(http.StatusConflict, ReasonOperationInProgress, err.Error())
	case errors.Is(err, store.ErrCapacity):
		return apierror.New(http.StatusTooManyRequests, ReasonBackendUnavailable, "memory operation capacity is full").WithRetryAfter(5 * time.Second)
	case errors.Is(err, store.ErrNotReady):
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is not ready")
	case errors.Is(err, store.ErrValidation):
		return apierror.New(http.StatusBadRequest, "", err.Error())
	default:
		return apierror.New(http.StatusInternalServerError, ReasonBackendUnavailable, "memory operation failed")
	}
}

func identityError() error {
	return apierror.New(http.StatusConflict, ReasonIdentityMismatch, "memory binding identity is invalid")
}

func remoteEntryToMemory(entry *store.RemoteMemoryCatalogEntry, content string) store.Memory {
	if entry == nil {
		return store.Memory{}
	}
	return store.Memory{
		ID: entry.ID, Namespace: entry.Namespace, SessionName: entry.SessionName, AgentName: entry.AgentName,
		TaskName: entry.TaskName, ParentTask: entry.ParentTask, Source: entry.Source,
		SourceProposalID: entry.SourceProposalID, Content: content, Tags: append([]string(nil), entry.Tags...),
		Disabled: entry.Disabled, Deleted: entry.Deleted, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt,
		LastRecalledAt: entry.LastRecalledAt, RecalledCount: entry.RecalledCount,
		Generation: entry.Generation, DesiredGeneration: entry.DesiredGeneration,
		GovernanceRevision: entry.GovernanceRevision, MaterializationState: entry.MaterializationState,
		PendingOperationID: entry.PendingOperationID, Trust: entry.Trust,
		ContentDigest: entry.ContentDigest, ContentAvailable: content != "" && entry.ContentAvailable,
	}
}

func validateDirectMemorySource(source string) error {
	if normalizeSource(source) == memorySourceProposal {
		return apierror.New(http.StatusBadRequest, "", "memory_proposal is reserved for accepted proposal application")
	}
	return nil
}

func replacementMemoryMetadata(
	entry *store.RemoteMemoryCatalogEntry,
	memory store.Memory,
	content string,
	tags []string,
) map[string]string {
	metadata := memoryMetadataFromStore(memory)
	if entry == nil {
		return metadata
	}
	changed := entry.ContentDigest != protocol.ContentDigest(content) || !slices.Equal(entry.Tags, tags) ||
		entry.SessionName != metadata["sessionName"] || entry.AgentName != metadata["agentName"] ||
		entry.TaskName != metadata["taskName"] || entry.ParentTask != metadata["parentTask"] ||
		normalizeSource(entry.Source) != metadata["source"] || entry.SourceProposalID != metadata["sourceProposalId"]
	if changed {
		delete(metadata, "sourceProposalId")
		if metadata["source"] == memorySourceProposal {
			metadata["source"] = memorySourceManual
		}
	}
	return metadata
}

func memoryMetadata(request CreateRequest) map[string]string {
	return compactMetadata(map[string]string{
		"sessionName": request.SessionName, "agentName": request.AgentName, "taskName": request.TaskName,
		"parentTask": request.ParentTask, "source": normalizeSource(request.Source),
	})
}

func memoryMetadataFromStore(memory store.Memory) map[string]string {
	return compactMetadata(map[string]string{
		"sessionName": memory.SessionName, "agentName": memory.AgentName, "taskName": memory.TaskName,
		"parentTask": memory.ParentTask, "source": normalizeSource(memory.Source),
		"sourceProposalId": memory.SourceProposalID,
	})
}

func remoteRecordCollectionsMatchCatalog(record *protocol.MemoryRecord, entry *store.RemoteMemoryCatalogEntry) bool {
	if record == nil || entry == nil {
		return false
	}
	expectedTags, err := protocol.NormalizeTags(nonNilStrings(entry.Tags))
	if err != nil {
		return false
	}
	actualTags, err := protocol.NormalizeTags(record.Tags)
	if err != nil || !slices.Equal(actualTags, record.Tags) || !slices.Equal(actualTags, expectedTags) {
		return false
	}
	expectedMetadata, err := protocol.NormalizeMetadata(memoryMetadataFromStore(remoteEntryToMemory(entry, "")))
	if err != nil {
		return false
	}
	actualMetadata, err := protocol.NormalizeMetadata(record.Metadata)
	return err == nil && maps.Equal(actualMetadata, record.Metadata) && maps.Equal(actualMetadata, expectedMetadata)
}

func compactMetadata(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		if value = strings.TrimSpace(redact.SensitiveText(value)); value != "" {
			result[key] = value
		}
	}
	return result
}

func normalizeSource(source string) string {
	source = strings.TrimSpace(redact.SensitiveText(source))
	if source == "" {
		return "api"
	}
	return source
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func digestJSON(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boundedMemoryLimit(limit int) int {
	if limit <= 0 {
		return defaultRemoteCatalogLimit
	}
	return min(limit, maxRemoteCatalogLimit)
}

func entryMatchesAuthority(entry *store.RemoteMemoryCatalogEntry, binding *store.MemoryBackendBinding) bool {
	return entry != nil && binding != nil && entry.Namespace == binding.Namespace && entry.NamespaceUID == binding.NamespaceUID &&
		entry.ClusterID == binding.ClusterID && entry.BackendUID == binding.BackendUID &&
		entry.AuthorityEpoch == binding.AuthorityEpoch && entry.TenantID == binding.TenantID && entry.StoreUUID == binding.StoreUUID
}

func ensureResolvedSearchCapability(backend *corev1alpha1.MemoryBackend, actualMode string) error {
	if backend == nil || backend.Status.ObservedCapabilities == nil {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search capabilities are unavailable")
	}
	capability := corev1alpha1.MemoryBackendCapabilityKeywordSearch
	switch actualMode {
	case protocol.SearchModeKeyword:
	case protocol.SearchModeSemantic:
		capability = corev1alpha1.MemoryBackendCapabilitySemanticSearch
	case protocol.SearchModeHybrid:
		capability = corev1alpha1.MemoryBackendCapabilityHybridSearch
	default:
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend selected an invalid mode")
	}
	if !slices.Contains(backend.Status.ObservedCapabilities.Effective, capability) {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend selected an unadvertised mode")
	}
	return nil
}

func sourceFilterValues(source string) []string {
	if source = strings.TrimSpace(source); source != "" {
		return []string{source}
	}
	return nil
}

func memoryEntryMatchesFilter(entry *store.RemoteMemoryCatalogEntry, filter store.MemoryFilter) bool {
	if entry == nil {
		return false
	}
	if entry.Deleted || entry.MaterializationState == store.MemoryMaterializationDeleted {
		if !filter.IncludeDeleted || !entry.Deleted || entry.MaterializationState != store.MemoryMaterializationDeleted {
			return false
		}
	} else if entry.MaterializationState != store.MemoryMaterializationActive || entry.Disabled && !filter.IncludeDisabled {
		return false
	}
	if filter.SessionName != "" && entry.SessionName != filter.SessionName ||
		filter.AgentName != "" && entry.AgentName != filter.AgentName ||
		filter.TaskName != "" && entry.TaskName != filter.TaskName ||
		filter.ParentTask != "" && entry.ParentTask != filter.ParentTask ||
		filter.Source != "" && entry.Source != filter.Source {
		return false
	}
	if len(filter.Trust) > 0 && !slices.Contains(filter.Trust, entry.Trust) {
		return false
	}
	for _, tag := range filter.Tags {
		if !slices.Contains(entry.Tags, strings.ToLower(strings.TrimSpace(tag))) {
			return false
		}
	}
	return true
}

// Search performs explicit bounded search and verifies every remote result against the active local catalog.
func (s *Service) Search(
	ctx context.Context,
	namespace string,
	request SearchRequest,
	searchContext SearchContext,
) (*SearchResponse, error) {
	return s.search(ctx, namespace, request, searchContext)
}

//nolint:gocyclo // Search enforces bounded paging, authorization, local joins, filtering, and verification in one flow.
func (s *Service) search(
	ctx context.Context,
	namespace string,
	request SearchRequest,
	searchContext SearchContext,
) (*SearchResponse, error) {
	if len(request.Trust) == 0 && !searchContext.PreserveEmptyTrust {
		request.Trust = []store.MemoryTrust{store.MemoryTrustReviewed, store.MemoryTrustTrusted}
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = protocol.SearchModeKeyword
	}
	if mode != protocol.SearchModeKeyword && mode != protocol.SearchModeSemantic &&
		mode != protocol.SearchModeHybrid && mode != protocol.SearchModeAuto {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid memory search mode")
	}
	request.Query = strings.TrimSpace(redact.SensitiveText(request.Query))
	if len(request.Query) > protocol.MaxQueryBytes {
		return nil, apierror.New(http.StatusRequestEntityTooLarge, "", "memory search exceeds the protocol query limit")
	}
	authority, err := s.resolve(ctx, namespace, true)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		return s.searchLegacy(ctx, authority, namespace, request, mode)
	}
	if err := requireRemoteRead(authority); err != nil {
		return nil, err
	}
	if err := authorizeRemoteSearch(searchContext); err != nil {
		return nil, err
	}
	if err := ensureSearchCapability(authority.Backend, mode); err != nil {
		return nil, err
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	if err := validateRemoteSearchQueryLimit(authority, request.Query); err != nil {
		return nil, err
	}
	queryDigest := memorySearchQueryDigest(request, mode)
	cursorState, replayPageToken, snapshotExpiresAt, tombstones, err := loadRemoteSearchContinuation(
		ctx, s.Governed, authority.Binding, queryDigest, request.Cursor, s.now(),
	)
	if errors.Is(err, errLegacyRemoteSearchCursor) {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
			"memory search cursor predates exact identity tracking; restart the search")
	}
	if err != nil || !request.IncludeDeleted && tombstones != nil {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid or expired memory search cursor")
	}
	if strings.TrimSpace(request.Cursor) == "" {
		cursorState.SeenRecordState = newRemoteSearchSeenRecordState()
	}
	if tombstones == nil {
		tombstones = &remoteSearchTombstoneCursor{Exhausted: !request.IncludeDeleted}
	}
	target := boundedMemoryLimit(request.Limit)
	providerPageSize, err := remoteSearchPageSize(authority)
	if err != nil {
		return nil, err
	}
	snapshotRecordLimit, err := remoteSearchSnapshotRecordLimit(authority)
	if err != nil {
		return nil, err
	}
	snapshotTTL, err := remoteSearchSnapshotTTL(authority)
	if err != nil {
		return nil, err
	}
	if remoteSearchSeenRecordStatePresent(cursorState.SeenRecordState) {
		if seenCount, ok := remoteSearchSeenRecordCount(cursorState.SeenRecordState); !ok || seenCount > snapshotRecordLimit {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
				"memory search cursor exceeds the active backend snapshot limit")
		}
	}
	if cursorState.PageSize == 0 {
		cursorState.PageSize = min(target, providerPageSize)
	} else if cursorState.PageSize > providerPageSize {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search page size exceeds the active backend capability")
	}
	var firstOutbound *protocol.SearchRequest
	if len(cursorState.Pending) > 0 {
		firstOutbound = &protocol.SearchRequest{
			ProtocolVersion: protocol.Version, Binding: binding, Mode: mode,
			Query: request.Query, PageSize: cursorState.PageSize, PageToken: replayPageToken,
		}
	} else if !cursorState.ProviderExhausted {
		firstOutbound = &protocol.SearchRequest{
			ProtocolVersion: protocol.Version, Binding: binding, Mode: mode,
			Query: request.Query, PageSize: cursorState.PageSize, PageToken: cursorState.ProviderToken,
		}
	}
	if firstOutbound != nil {
		if err := validateRemoteSearchLimits(authority, firstOutbound); err != nil {
			return nil, err
		}
	}
	actor := strings.TrimSpace(searchContext.Actor)
	if actor == "" {
		actor = "authenticated-memory-search"
	}
	if err := s.Governed.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: namespace, NamespaceUID: authority.NamespaceUID, Actor: actor,
		Action: "memory.search", AuthorityEpoch: authority.Binding.AuthorityEpoch,
		RoutingEpoch: authority.Binding.RoutingEpoch, RequestDigest: queryDigest,
		RequestID: searchContext.RequestID, CreatedAt: s.now(),
	}); err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search audit could not be committed")
	}
	items := make([]SearchHit, 0, target)
	seen := make(map[string]struct{}, target)
	actualMode := cursorState.ActualMode
	if actualMode == "" {
		actualMode = mode
	}
	candidates := 0
	pages := 0

	if len(cursorState.Pending) > 0 && len(items) < target && candidates < maxRemoteSearchCandidates {
		outbound := protocol.SearchRequest{
			ProtocolVersion: protocol.Version, Binding: binding, Mode: mode,
			Query: request.Query, PageSize: cursorState.PageSize, PageToken: replayPageToken,
		}
		if err := validateRemoteSearchLimits(authority, &outbound); err != nil {
			return nil, err
		}
		response, searchErr := authority.Adapter.Search(ctx, outbound)
		pages++
		if searchErr != nil {
			var adapterErr *AdapterError
			if errors.As(searchErr, &adapterErr) && adapterErr.Code == protocol.ErrorCodeSearchModeUnsupported {
				return nil, apierror.New(http.StatusUnprocessableEntity, ReasonSearchModeUnsupported, "memory search mode is unsupported")
			}
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory search backend is unavailable")
		}
		responseExpiry, responseErr := validateRemoteSearchPage(
			response, binding, mode, cursorState.PageSize, cursorState.ActualMode,
			snapshotExpiresAt, snapshotTTL, s.now(),
		)
		if responseErr != nil {
			return nil, responseErr
		}
		if err := ensureResolvedSearchCapability(authority.Backend, response.ActualMode); err != nil {
			return nil, err
		}
		if response.NextPageToken != cursorState.ProviderToken || response.Exhausted != cursorState.ProviderExhausted ||
			len(response.Records) != len(cursorState.ReplayPrefix)+len(cursorState.Pending) {
			return nil, apierror.New(http.StatusConflict, ReasonDiverged,
				"memory search continuation snapshot changed")
		}
		snapshotExpiresAt = responseExpiry
		actualMode = response.ActualMode
		for i := range cursorState.ReplayPrefix {
			if !searchCursorRecordMatches(cursorState.ReplayPrefix[i], &response.Records[i]) {
				return nil, apierror.New(http.StatusConflict, ReasonDiverged,
					"memory search continuation snapshot changed")
			}
		}
		pendingOffset := len(cursorState.ReplayPrefix)
		for i := range cursorState.Pending {
			if !searchCursorRecordMatches(cursorState.Pending[i], &response.Records[pendingOffset+i]) {
				return nil, apierror.New(http.StatusConflict, ReasonDiverged,
					"memory search continuation snapshot changed")
			}
		}
		if identityErr := validateRemoteSearchReplayPrefixIdentities(
			cursorState.SeenRecordState, response.Records[:pendingOffset],
		); identityErr != nil {
			return nil, identityErr
		}
		if identityErr := validateRemoteSearchRecordIdentities(
			cursorState.SeenRecordState, response.Records[pendingOffset:], snapshotRecordLimit,
		); identityErr != nil {
			return nil, identityErr
		}
		for len(cursorState.Pending) > 0 && len(items) < target && candidates < maxRemoteSearchCandidates {
			descriptor := cursorState.Pending[0]
			recordIndex := len(response.Records) - len(cursorState.Pending)
			record := &response.Records[recordIndex]
			cursorState.ReplayPrefix = append(cursorState.ReplayPrefix, descriptor)
			cursorState.Pending = cursorState.Pending[1:]
			candidates++
			if identityErr := trackRemoteSearchRecordIdentity(&cursorState.SeenRecordState, record.UpsertKey); identityErr != nil {
				return nil, identityErr
			}
			hit, eligible, hitErr := s.searchHit(ctx, authority, binding, record, descriptor.Score, request, actualMode)
			if hitErr != nil {
				return nil, hitErr
			}
			if eligible {
				appendUniqueSearchHit(&items, seen, hit)
			}
		}
		if len(cursorState.Pending) == 0 {
			cursorState.ReplayPrefix = nil
			replayPageToken = ""
		}
	}

	for pages < maxRemoteSearchPages && len(items) < target && candidates < maxRemoteSearchCandidates &&
		len(cursorState.Pending) == 0 && !cursorState.ProviderExhausted {
		requestPageToken := cursorState.ProviderToken
		outbound := protocol.SearchRequest{
			ProtocolVersion: protocol.Version, Binding: binding, Mode: mode,
			Query: request.Query, PageSize: cursorState.PageSize, PageToken: requestPageToken,
		}
		if err := validateRemoteSearchLimits(authority, &outbound); err != nil {
			return nil, err
		}
		response, searchErr := authority.Adapter.Search(ctx, outbound)
		pages++
		if searchErr != nil {
			var adapterErr *AdapterError
			if errors.As(searchErr, &adapterErr) && adapterErr.Code == protocol.ErrorCodeSearchModeUnsupported {
				return nil, apierror.New(http.StatusUnprocessableEntity, ReasonSearchModeUnsupported, "memory search mode is unsupported")
			}
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory search backend is unavailable")
		}
		responseExpiry, responseErr := validateRemoteSearchPage(
			response, binding, mode, cursorState.PageSize, cursorState.ActualMode,
			snapshotExpiresAt, snapshotTTL, s.now(),
		)
		if responseErr != nil {
			return nil, responseErr
		}
		if err := ensureResolvedSearchCapability(authority.Backend, response.ActualMode); err != nil {
			return nil, err
		}
		if !response.Exhausted && response.NextPageToken == requestPageToken {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete, "memory search pagination did not advance")
		}
		if identityErr := validateRemoteSearchRecordIdentities(
			cursorState.SeenRecordState, response.Records, snapshotRecordLimit,
		); identityErr != nil {
			return nil, identityErr
		}
		snapshotExpiresAt = responseExpiry
		actualMode = response.ActualMode
		cursorState.ActualMode = response.ActualMode
		cursorState.ProviderToken = response.NextPageToken
		cursorState.ProviderExhausted = response.Exhausted
		for i := range response.Records {
			if len(items) >= target || candidates >= maxRemoteSearchCandidates {
				if requestPageToken == "" {
					return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
						"memory search continuation could not preserve the provider snapshot")
				}
				cursorState.ReplayPrefix = appendSearchCursorRecords(nil, response.Records[:i])
				cursorState.Pending = appendSearchCursorRecords(cursorState.Pending, response.Records[i:])
				replayPageToken = requestPageToken
				break
			}
			candidates++
			record := &response.Records[i]
			if identityErr := trackRemoteSearchRecordIdentity(&cursorState.SeenRecordState, record.UpsertKey); identityErr != nil {
				return nil, identityErr
			}
			hit, eligible, hitErr := s.searchHit(ctx, authority, binding, record, record.Score, request, actualMode)
			if hitErr != nil {
				return nil, hitErr
			}
			if eligible {
				appendUniqueSearchHit(&items, seen, hit)
			}
		}
	}
	providerExhausted := cursorState.ProviderExhausted && len(cursorState.Pending) == 0
	// Preserve provider ranking first, then append completed local tombstones in
	// deterministic catalog order once the immutable provider snapshot is exhausted.
	if providerExhausted && !tombstones.Exhausted && len(items) < target &&
		candidates < maxRemoteSearchCandidates && pages < maxRemoteSearchPages {
		if err := s.appendRemoteSearchTombstones(
			ctx, authority, request, target, &items, seen, tombstones, &candidates, &pages,
		); err != nil {
			return nil, err
		}
	}
	exhausted := providerExhausted && tombstones.Exhausted
	pageComplete := exhausted || len(items) >= target
	cursor := ""
	if !exhausted {
		var persistedTombstones *remoteSearchTombstoneCursor
		if request.IncludeDeleted {
			persistedTombstones = tombstones
		}
		cursor, err = saveRemoteSearchContinuation(
			ctx, s.Governed, authority.Binding, queryDigest, cursorState, replayPageToken,
			snapshotExpiresAt, persistedTombstones, request.Cursor, target, s.now(),
		)
		if err != nil {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory search continuation could not be preserved")
		}
	} else if request.Cursor != "" {
		if err := s.Governed.RetireMemorySearchCursor(ctx, authority.NamespaceUID, request.Cursor, s.now()); err != nil {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory search cursor could not be retired")
		}
	}
	if !pageComplete && !request.AllowIncomplete {
		memoryIncompleteTotal.Inc()
		return nil, &IncompleteSearchError{
			Cause: apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"memory search scan budget was exhausted"),
			Cursor: cursor,
		}
	}
	if len(items) > 0 {
		ids := make([]string, 0, len(items))
		for _, item := range items {
			if !item.Memory.Deleted {
				ids = append(ids, item.Memory.ID)
			}
		}
		_ = s.Governed.MarkRemoteMemoriesRecalled(ctx, authority.NamespaceUID, ids, s.now())
	}
	return &SearchResponse{
		Items: items, ActualMode: actualMode, Cursor: cursor, Exhausted: exhausted, Complete: pageComplete,
	}, nil
}

func appendUniqueSearchHit(items *[]SearchHit, seen map[string]struct{}, hit *SearchHit) {
	if hit == nil || strings.TrimSpace(hit.Memory.ID) == "" {
		return
	}
	if _, exists := seen[hit.Memory.ID]; exists {
		return
	}
	seen[hit.Memory.ID] = struct{}{}
	*items = append(*items, *hit)
}

func (s *Service) appendRemoteSearchTombstones(
	ctx context.Context,
	authority *ResolvedAuthority,
	request SearchRequest,
	target int,
	items *[]SearchHit,
	seen map[string]struct{},
	cursor *remoteSearchTombstoneCursor,
	candidates, pages *int,
) error {
	if cursor == nil || cursor.Exhausted {
		return nil
	}
	var beforeUpdatedAt *time.Time
	beforeID := cursor.BeforeID
	if !cursor.BeforeUpdatedAt.IsZero() {
		value := cursor.BeforeUpdatedAt.UTC()
		beforeUpdatedAt = &value
	}
	for len(*items) < target && *candidates < maxRemoteSearchCandidates && *pages < maxRemoteSearchPages {
		pageSize := min(maxRemoteCatalogLimit, maxRemoteSearchCandidates-*candidates)
		entries, err := s.Governed.ListRemoteMemories(ctx, store.RemoteMemoryCatalogFilter{
			NamespaceUID: authority.NamespaceUID, IDs: request.IDs, Trust: request.Trust,
			IncludeDisabled: true, IncludeDeleted: true,
			States:          []store.MemoryMaterializationState{store.MemoryMaterializationDeleted},
			BeforeUpdatedAt: beforeUpdatedAt, BeforeID: beforeID, Limit: pageSize,
		})
		if err != nil {
			return mapStoreError(err)
		}
		if len(entries) == 0 {
			cursor.Exhausted = true
			return nil
		}
		*pages++
		processed := 0
		for i := range entries {
			entry := &entries[i]
			processed++
			*candidates++
			cursor.BeforeUpdatedAt = entry.UpdatedAt.UTC()
			cursor.BeforeID = entry.ID
			beforeUpdatedAt = &cursor.BeforeUpdatedAt
			beforeID = cursor.BeforeID
			// Deleted provider content is intentionally unavailable. Match only the
			// locally authoritative metadata and tags; never hydrate a tombstone.
			if !entryMatchesAuthority(entry, authority.Binding) || !searchEntryEligible(entry, request) ||
				!keywordCatalogEntryMatches(entry, request.Query) {
				continue
			}
			memory := remoteEntryToMemory(entry, "")
			appendUniqueSearchHit(items, seen, &SearchHit{Memory: memory})
			if len(*items) == target || *candidates == maxRemoteSearchCandidates {
				break
			}
		}
		if processed == len(entries) && len(entries) < pageSize {
			cursor.Exhausted = true
			return nil
		}
		if len(*items) == target || *candidates == maxRemoteSearchCandidates {
			return nil
		}
	}
	return nil
}

func (s *Service) searchLegacy(
	ctx context.Context,
	authority *ResolvedAuthority,
	namespace string,
	request SearchRequest,
	mode string,
) (*SearchResponse, error) {
	if mode == protocol.SearchModeSemantic || mode == protocol.SearchModeHybrid {
		return nil, apierror.New(http.StatusUnprocessableEntity, protocol.ErrorCodeSearchModeUnsupported,
			"memory search mode is unavailable for the legacy backend")
	}
	if s.Legacy == nil {
		return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory store is not configured")
	}
	request.Query = strings.TrimSpace(redact.SensitiveText(request.Query))
	queryDigest := memorySearchQueryDigest(request, protocol.SearchModeKeyword)
	cursorStore := s.legacySearchCursorStore()
	cursor, err := loadLegacySearchCursor(ctx, cursorStore, authority, queryDigest, request.Cursor, s.now())
	if err != nil {
		if errors.Is(err, errLegacySearchCursorInvalid) || errors.Is(err, store.ErrNotFound) {
			return nil, apierror.New(http.StatusBadRequest, "", "invalid or expired memory search cursor")
		}
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"legacy memory search cursor state is unavailable")
	}
	pageSize := boundedMemoryLimit(request.Limit)
	if cursor.PageSize > 0 {
		pageSize = min(pageSize, cursor.PageSize)
	}
	items := make([]SearchHit, 0, pageSize)
	if err := s.appendLegacyPendingSearchHits(ctx, authority, namespace, request, pageSize, &cursor, &items); err != nil {
		return nil, err
	}
	candidates, capped, err := s.listLegacySearchCandidates(ctx, namespace, request, cursor)
	if err != nil {
		return nil, err
	}
	scanned, lastScanned, err := s.appendLegacyCandidateSearchHits(
		ctx, authority, request, pageSize, candidates, &items,
	)
	if err != nil {
		return nil, err
	}
	hasMore := len(cursor.PendingIDs) > 0 || scanned < len(candidates) || capped
	complete := !hasMore || len(items) >= pageSize
	if !complete && !request.AllowIncomplete {
		pending := make([]string, 0, len(items)+len(cursor.PendingIDs))
		for _, item := range items {
			pending = append(pending, item.Memory.ID)
		}
		cursor.PendingIDs = append(pending, cursor.PendingIDs...)
	}
	next := ""
	if hasMore && (scanned > 0 || !cursor.BeforeUpdatedAt.IsZero()) {
		cursor.PageSize = max(cursor.PageSize, pageSize)
		if scanned > 0 {
			cursor.BeforeUpdatedAt = lastScanned.UpdatedAt.UTC()
			cursor.BeforeID = lastScanned.ID
		}
		next, err = saveLegacySearchCursor(ctx, cursorStore, authority, queryDigest, cursor, s.now())
		if err != nil {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"legacy memory search continuation could not be preserved")
		}
	} else if !hasMore && cursor.CursorID != "" {
		if err := cursorStore.RetireMemorySearchCursor(
			ctx, legacySearchCursorNamespace(authority), cursor.CursorID, s.now(),
		); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
				"legacy memory search cursor could not be retired")
		}
	}
	if !complete && !request.AllowIncomplete {
		return nil, legacySearchIncompleteError(next)
	}
	return &SearchResponse{
		Items: items, ActualMode: protocol.SearchModeKeyword, Cursor: next, Exhausted: !hasMore, Complete: complete,
	}, nil
}

func (s *Service) appendLegacyPendingSearchHits(
	ctx context.Context,
	authority *ResolvedAuthority,
	namespace string,
	request SearchRequest,
	pageSize int,
	cursor *legacySearchCursor,
	items *[]SearchHit,
) error {
	pendingOffset := 0
	for pendingOffset < len(cursor.PendingIDs) && len(*items) < pageSize {
		id := cursor.PendingIDs[pendingOffset]
		pendingOffset++
		memory, err := s.Legacy.GetMemory(ctx, namespace, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return mapStoreError(err)
		}
		if err := s.applyLegacyGovernance(ctx, authority, memory); err != nil {
			return err
		}
		if memoryMatchesSearchRequest(*memory, request) {
			*items = append(*items, SearchHit{Memory: *memory})
		}
	}
	cursor.PendingIDs = append([]string(nil), cursor.PendingIDs[pendingOffset:]...)
	return nil
}

func (s *Service) appendLegacyCandidateSearchHits(
	ctx context.Context,
	authority *ResolvedAuthority,
	request SearchRequest,
	pageSize int,
	candidates []store.Memory,
	items *[]SearchHit,
) (int, store.Memory, error) {
	scanned := 0
	var lastScanned store.Memory
	for i := 0; i < len(candidates) && len(*items) < pageSize; i++ {
		scanned++
		lastScanned = candidates[i]
		memory := candidates[i]
		if err := s.applyLegacyGovernance(ctx, authority, &memory); err != nil {
			return 0, store.Memory{}, err
		}
		if memoryMatchesSearchRequest(memory, request) {
			*items = append(*items, SearchHit{Memory: memory})
		}
	}
	return scanned, lastScanned, nil
}

func (s *Service) listLegacySearchCandidates(
	ctx context.Context,
	namespace string,
	request SearchRequest,
	cursor legacySearchCursor,
) ([]store.Memory, bool, error) {
	filter := store.MemoryFilter{
		Namespace: namespace, Query: request.Query, SessionName: request.SessionName,
		AgentName: request.AgentName, TaskName: request.TaskName, ParentTask: request.ParentTask,
		Tags: request.Tags, IDs: request.IDs,
		IncludeDisabled: request.IncludeDisabled, IncludeDeleted: request.IncludeDeleted,
		Limit: maxRemoteCatalogLimit,
	}
	if !cursor.BeforeUpdatedAt.IsZero() {
		before := cursor.BeforeUpdatedAt.UTC()
		filter.BeforeUpdatedAt = &before
		filter.BeforeID = cursor.BeforeID
	}
	var memories []store.Memory
	capped := false
	if len(request.Sources) <= 1 {
		if len(request.Sources) == 1 {
			filter.Source = request.Sources[0]
		}
		listed, err := s.Legacy.ListMemories(ctx, filter)
		if err != nil {
			return nil, false, mapStoreError(err)
		}
		return listed, len(listed) >= maxRemoteCatalogLimit, nil
	}
	seen := make(map[string]struct{})
	for _, source := range request.Sources {
		filter.Source = source
		batch, err := s.Legacy.ListMemories(ctx, filter)
		if err != nil {
			return nil, false, mapStoreError(err)
		}
		if len(batch) >= maxRemoteCatalogLimit {
			capped = true
		}
		for _, memory := range batch {
			if _, ok := seen[memory.ID]; ok {
				continue
			}
			seen[memory.ID] = struct{}{}
			memories = append(memories, memory)
		}
	}
	slices.SortFunc(memories, func(a, b store.Memory) int {
		if a.UpdatedAt.After(b.UpdatedAt) {
			return -1
		}
		if a.UpdatedAt.Before(b.UpdatedAt) {
			return 1
		}
		return strings.Compare(b.ID, a.ID)
	})
	if len(memories) > maxRemoteCatalogLimit {
		memories = memories[:maxRemoteCatalogLimit]
		capped = true
	}
	return memories, capped, nil
}

func legacySearchIncompleteError(cursor string) error {
	memoryIncompleteTotal.Inc()
	return &IncompleteSearchError{
		Cause: apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
			"legacy memory search reached its pre-filter scan cap and cannot prove completeness"),
		Cursor: cursor,
	}
}

func memorySearchQueryDigest(request SearchRequest, mode string) string {
	return digestJSON(struct {
		Query           string              `json:"query"`
		Tags            []string            `json:"tags"`
		IDs             []string            `json:"ids"`
		Sources         []string            `json:"sources"`
		Trust           []store.MemoryTrust `json:"trust"`
		SessionName     string              `json:"sessionName"`
		TaskName        string              `json:"taskName"`
		ParentTask      string              `json:"parentTask"`
		AgentName       string              `json:"agentName"`
		Mode            string              `json:"mode"`
		IncludeDisabled bool                `json:"includeDisabled"`
		IncludeDeleted  bool                `json:"includeDeleted"`
	}{
		Query: request.Query, Tags: request.Tags, IDs: request.IDs,
		Sources: request.Sources, Trust: request.Trust, SessionName: request.SessionName,
		TaskName: request.TaskName, ParentTask: request.ParentTask, AgentName: request.AgentName,
		Mode: mode, IncludeDisabled: request.IncludeDisabled, IncludeDeleted: request.IncludeDeleted,
	})
}

func (s *Service) applyLegacyGovernanceFilter(
	ctx context.Context,
	authority *ResolvedAuthority,
	memories []store.Memory,
	trust []store.MemoryTrust,
	limit int,
) ([]store.Memory, error) {
	result := make([]store.Memory, 0, min(len(memories), limit))
	for i := range memories {
		if err := s.applyLegacyGovernance(ctx, authority, &memories[i]); err != nil {
			return nil, err
		}
		if len(trust) > 0 && !slices.Contains(trust, memories[i].Trust) {
			continue
		}
		result = append(result, memories[i])
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (s *Service) applyLegacyGovernance(ctx context.Context, authority *ResolvedAuthority, memory *store.Memory) error {
	if memory == nil || s.Governed == nil || authority == nil || strings.TrimSpace(authority.NamespaceUID) == "" {
		return nil
	}
	var audits []store.MemoryAuditRecord
	var beforeCreatedAt *time.Time
	beforeID := ""
	for {
		records, err := s.Governed.ListMemoryAudit(ctx, store.MemoryAuditFilter{
			NamespaceUID: authority.NamespaceUID, MemoryID: memory.ID,
			BeforeCreatedAt: beforeCreatedAt, BeforeID: beforeID, Limit: maxRemoteCatalogLimit,
		})
		if err != nil {
			return mapStoreError(err)
		}
		audits = append(audits, records...)
		if len(records) < maxRemoteCatalogLimit {
			break
		}
		last := records[len(records)-1]
		createdAt := last.CreatedAt.UTC()
		beforeCreatedAt = &createdAt
		beforeID = last.ID
	}
	applyLegacyGovernanceRecords(memory, audits)
	return nil
}

func applyLegacyGovernanceRecords(memory *store.Memory, records []store.MemoryAuditRecord) {
	if memory == nil {
		return
	}
	effectiveTrust := memory.Trust
	governanceRevision := max(memory.GovernanceRevision, int64(1))
	trustResolved := false
	for _, record := range records {
		if record.AuthorityEpoch != 0 || record.RoutingEpoch != 0 {
			continue
		}
		switch record.Action {
		case legacyMemoryTrustAuditAction:
			previousTrust := store.MemoryTrust(record.PreviousState)
			newTrust := store.MemoryTrust(record.NewState)
			if !validMemoryTrust(previousTrust) || !validMemoryTrust(newTrust) || previousTrust == newTrust {
				continue
			}
			governanceRevision++
			if !trustResolved {
				effectiveTrust = newTrust
				trustResolved = true
			}
		case legacyMemoryDisableAuditAction:
			if (record.PreviousState == legacyMemoryDisabledState(false) && record.NewState == legacyMemoryDisabledState(true)) ||
				(record.PreviousState == legacyMemoryDisabledState(true) && record.NewState == legacyMemoryDisabledState(false)) {
				governanceRevision++
			}
		}
	}
	memory.Trust = effectiveTrust
	memory.GovernanceRevision = governanceRevision
}

func validMemoryTrust(trust store.MemoryTrust) bool {
	return trust == store.MemoryTrustUntrusted || trust == store.MemoryTrustReviewed || trust == store.MemoryTrustTrusted
}

func legacyMemoryDisabledState(disabled bool) string {
	if disabled {
		return "disabled=true"
	}
	return "disabled=false"
}

func authorizeRemoteSearch(searchContext SearchContext) error {
	if searchContext.RemoteAuthorized {
		return nil
	}
	if searchContext.AuthorizeRemote == nil {
		return apierror.New(http.StatusForbidden, ReasonSearchRemoteAuth, "remote memory search requires explicit authorization")
	}
	if err := searchContext.AuthorizeRemote(); err != nil {
		return apierror.New(http.StatusForbidden, ReasonSearchRemoteAuth, "remote memory search is not authorized")
	}
	return nil
}

func remoteSearchPageSize(authority *ResolvedAuthority) (int, error) {
	if authority == nil || authority.Backend == nil || authority.Backend.Status.ObservedCapabilities == nil {
		return 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search capabilities are unavailable")
	}
	pageSize := int(authority.Backend.Status.ObservedCapabilities.Limits.MaxPageSize)
	if pageSize <= 0 || pageSize > protocol.MaxPageSize {
		return 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search page size capability is invalid")
	}
	return pageSize, nil
}

func remoteSearchSnapshotRecordLimit(authority *ResolvedAuthority) (int, error) {
	if authority == nil || authority.Backend == nil || authority.Backend.Status.ObservedCapabilities == nil {
		return 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search capabilities are unavailable")
	}
	limit := int(authority.Backend.Status.ObservedCapabilities.Limits.MaxSnapshotRecords)
	if limit <= 0 || limit > protocol.MaxSnapshotRecords {
		return 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search snapshot record capability is invalid")
	}
	return limit, nil
}

func remoteSearchSnapshotTTL(authority *ResolvedAuthority) (time.Duration, error) {
	if authority == nil || authority.Backend == nil || authority.Backend.Status.ObservedCapabilities == nil {
		return 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search capabilities are unavailable")
	}
	seconds := authority.Backend.Status.ObservedCapabilities.Limits.SnapshotTTLSeconds
	if seconds <= 0 || seconds > protocol.MaxSnapshotTTLSeconds {
		return 0, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search snapshot TTL capability is invalid")
	}
	return time.Duration(seconds) * time.Second, nil
}

func ensureSearchCapability(backend *corev1alpha1.MemoryBackend, mode string) error {
	if backend == nil || backend.Status.ObservedCapabilities == nil {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory search capabilities are unavailable")
	}
	capabilities := backend.Status.ObservedCapabilities.Effective
	has := func(capability corev1alpha1.MemoryBackendCapability) bool {
		return slices.Contains(capabilities, capability)
	}
	switch mode {
	case protocol.SearchModeKeyword:
		if !has(corev1alpha1.MemoryBackendCapabilityKeywordSearch) {
			return apierror.New(http.StatusUnprocessableEntity, ReasonSearchModeUnsupported, "keyword memory search is unsupported")
		}
	case protocol.SearchModeSemantic:
		if !has(corev1alpha1.MemoryBackendCapabilitySemanticSearch) {
			return apierror.New(http.StatusUnprocessableEntity, ReasonSearchModeUnsupported, "semantic memory search is unsupported")
		}
	case protocol.SearchModeHybrid:
		if !has(corev1alpha1.MemoryBackendCapabilityHybridSearch) {
			return apierror.New(http.StatusUnprocessableEntity, ReasonSearchModeUnsupported, "hybrid memory search is unsupported")
		}
	case protocol.SearchModeAuto:
		if !has(corev1alpha1.MemoryBackendCapabilityKeywordSearch) {
			return apierror.New(http.StatusUnprocessableEntity, ReasonSearchModeUnsupported, "automatic memory search is unsupported")
		}
	}
	return nil
}

func searchEntryEligible(entry *store.RemoteMemoryCatalogEntry, request SearchRequest) bool {
	if entry == nil {
		return false
	}
	if entry.Deleted || entry.MaterializationState == store.MemoryMaterializationDeleted {
		if !request.IncludeDeleted || !entry.Deleted || entry.MaterializationState != store.MemoryMaterializationDeleted {
			return false
		}
	} else if entry.MaterializationState != store.MemoryMaterializationActive || entry.Disabled && !request.IncludeDisabled {
		return false
	}
	if len(request.IDs) > 0 && !slices.Contains(request.IDs, entry.ID) {
		return false
	}
	if len(request.Trust) > 0 && !slices.Contains(request.Trust, entry.Trust) {
		return false
	}
	if len(request.Sources) > 0 && !slices.Contains(request.Sources, entry.Source) {
		return false
	}
	if request.SessionName != "" && entry.SessionName != request.SessionName ||
		request.TaskName != "" && entry.TaskName != request.TaskName ||
		request.ParentTask != "" && entry.ParentTask != request.ParentTask ||
		request.AgentName != "" && entry.AgentName != request.AgentName {
		return false
	}
	for _, tag := range request.Tags {
		if !slices.Contains(entry.Tags, strings.ToLower(strings.TrimSpace(tag))) {
			return false
		}
	}
	return true
}

func memoryMatchesSearchRequest(memory store.Memory, request SearchRequest) bool {
	materializationState := store.MemoryMaterializationActive
	if memory.Deleted {
		materializationState = store.MemoryMaterializationDeleted
	}
	entry := store.RemoteMemoryCatalogEntry{
		ID: memory.ID, SessionName: memory.SessionName, AgentName: memory.AgentName,
		TaskName: memory.TaskName, ParentTask: memory.ParentTask, Source: memory.Source,
		Tags: memory.Tags, Disabled: memory.Disabled, Deleted: memory.Deleted, Trust: memory.Trust,
		MaterializationState: materializationState,
	}
	return searchEntryEligible(&entry, request)
}

func keywordCatalogEntryMatches(entry *store.RemoteMemoryCatalogEntry, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	if entry == nil {
		return false
	}
	contains := func(value string) bool {
		return strings.Contains(strings.ToLower(value), query)
	}
	if slices.ContainsFunc(entry.Tags, contains) {
		return true
	}
	localMetadata := compactMetadata(map[string]string{
		"sessionName": entry.SessionName, "agentName": entry.AgentName, "taskName": entry.TaskName,
		"parentTask": entry.ParentTask, "source": entry.Source, "sourceProposalId": entry.SourceProposalID,
	})
	for key, value := range localMetadata {
		if contains(key) || contains(value) {
			return true
		}
	}
	return false
}

func keywordRecordMatches(record *protocol.MemoryRecord, entry *store.RemoteMemoryCatalogEntry, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	if record != nil && strings.Contains(strings.ToLower(record.Content), query) {
		return true
	}
	return keywordCatalogEntryMatches(entry, query)
}

type remoteSearchTombstoneCursor struct {
	BeforeUpdatedAt time.Time `json:"t,omitempty"`
	BeforeID        string    `json:"i,omitempty"`
	Exhausted       bool      `json:"x,omitempty"`
}

func (c remoteSearchTombstoneCursor) valid() bool {
	return c.BeforeUpdatedAt.IsZero() == (strings.TrimSpace(c.BeforeID) == "")
}

type persistedRemoteSearchCursor struct {
	ProviderToken     string                       `json:"p"`
	ProviderExhausted bool                         `json:"x,omitempty"`
	PageSize          int                          `json:"z"`
	ActualMode        string                       `json:"m,omitempty"`
	Pending           []remoteSearchCursorRecord   `json:"n,omitempty"`
	ReplayPrefix      []remoteSearchCursorRecord   `json:"f,omitempty"`
	SnapshotExpiresAt time.Time                    `json:"e"`
	ReplayPageToken   string                       `json:"r,omitempty"`
	Tombstones        *remoteSearchTombstoneCursor `json:"d,omitempty"`
	SeenRecordState   []byte                       `json:"s,omitempty"`
}

func (p persistedRemoteSearchCursor) cursor() remoteSearchCursor {
	return remoteSearchCursor{
		ProviderToken: p.ProviderToken, ProviderExhausted: p.ProviderExhausted,
		PageSize: p.PageSize, ActualMode: p.ActualMode,
		Pending:         append([]remoteSearchCursorRecord(nil), p.Pending...),
		ReplayPrefix:    append([]remoteSearchCursorRecord(nil), p.ReplayPrefix...),
		SeenRecordState: cloneRemoteSearchSeenRecordState(p.SeenRecordState),
	}
}

//nolint:gocyclo // Cursor persistence validates provider and local-tombstone continuation invariants together.
func saveRemoteSearchContinuation(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	binding *store.MemoryBackendBinding,
	queryDigest string,
	state remoteSearchCursor,
	replayPageToken string,
	snapshotExpiresAt time.Time,
	tombstones *remoteSearchTombstoneCursor,
	consumedCursor string,
	normalizedPageLimit int,
	now time.Time,
) (string, error) {
	now = now.UTC()
	providerContinuation := state.ProviderToken != "" || len(state.Pending) > 0
	tombstoneContinuation := tombstones != nil && !tombstones.Exhausted
	if governed == nil || binding == nil || !providerContinuation && !tombstoneContinuation ||
		normalizedPageLimit <= 0 || normalizedPageLimit > maxRemoteCatalogLimit ||
		state.PageSize <= 0 || state.PageSize > protocol.MaxPageSize ||
		(state.ActualMode != protocol.SearchModeKeyword && state.ActualMode != protocol.SearchModeSemantic &&
			state.ActualMode != protocol.SearchModeHybrid) ||
		len(state.Pending) > protocol.MaxPageSize || len(state.ReplayPrefix)+len(state.Pending) > protocol.MaxPageSize ||
		(len(state.Pending) > 0) != (replayPageToken != "") || (len(state.Pending) == 0 && len(state.ReplayPrefix) > 0) ||
		state.ProviderExhausted != (state.ProviderToken == "") ||
		(replayPageToken != "" && !state.ProviderExhausted && replayPageToken == state.ProviderToken) ||
		len(state.ProviderToken) > protocol.MaxPageTokenBytes || len(replayPageToken) > protocol.MaxPageTokenBytes ||
		tombstones != nil && !tombstones.valid() || !validRemoteSearchSeenRecordState(state.SeenRecordState) ||
		providerContinuation && !remoteSearchSeenRecordStatePresent(state.SeenRecordState) {
		return "", errors.New("memory search cursor state is unavailable")
	}
	if providerContinuation {
		snapshotExpiresAt = snapshotExpiresAt.UTC()
		if !snapshotExpiresAt.After(now) {
			return "", errors.New("memory search cursor snapshot has expired")
		}
	} else {
		if !state.ProviderExhausted || replayPageToken != "" {
			return "", errors.New("memory search cursor state is unavailable")
		}
		snapshotExpiresAt = time.Time{}
	}
	identity, err := protocolBinding(binding)
	if err != nil {
		return "", err
	}
	var tombstoneState *remoteSearchTombstoneCursor
	if tombstones != nil {
		copy := *tombstones
		tombstoneState = &copy
	}
	seenRecordState := state.SeenRecordState
	if !providerContinuation {
		seenRecordState = nil
	}
	payload, err := json.Marshal(persistedRemoteSearchCursor{
		ProviderToken: state.ProviderToken, ProviderExhausted: state.ProviderExhausted,
		PageSize: state.PageSize, ActualMode: state.ActualMode,
		Pending: state.Pending, ReplayPrefix: state.ReplayPrefix,
		SnapshotExpiresAt: snapshotExpiresAt, ReplayPageToken: replayPageToken,
		Tombstones: tombstoneState, SeenRecordState: cloneRemoteSearchSeenRecordState(seenRecordState),
	})
	if err != nil || len(payload) == 0 || len(payload) > maxRemoteSearchCursorBytes {
		return "", errors.New("memory search cursor state is invalid")
	}
	expiresAt := now.Add(remoteSearchCursorTTL)
	if providerContinuation {
		continuationToken := state.ProviderToken
		if continuationToken == "" {
			continuationToken = replayPageToken
		}
		var ok bool
		expiresAt, ok = (protocol.SearchResponse{
			NextPageToken: continuationToken, SnapshotExpiresAt: snapshotExpiresAt,
		}).ContinuationExpiresAt(expiresAt)
		if !ok || !expiresAt.After(now) {
			return "", errors.New("memory search cursor snapshot has expired")
		}
	}
	id := remoteSearchCursorPrefix + uuid.NewString()
	if consumedCursor = strings.TrimSpace(consumedCursor); consumedCursor != "" {
		id = remoteSearchSuccessorID(consumedCursor, normalizedPageLimit)
		if err := governed.RetireMemorySearchCursor(ctx, binding.NamespaceUID, consumedCursor, now); err != nil {
			return "", err
		}
	}
	if err := governed.SaveMemorySearchCursor(ctx, store.MemorySearchCursorState{
		ID: id, NamespaceUID: binding.NamespaceUID,
		BindingDigest: protocol.BindingDigest(identity), QueryDigest: queryDigest,
		State: payload, CreatedAt: now, ExpiresAt: expiresAt,
	}); err != nil {
		return "", err
	}
	return id, nil
}

func remoteSearchSuccessorID(consumedCursor string, normalizedPageLimit int) string {
	shapeDigest := digestJSON(struct {
		ConsumedCursor string `json:"cursor"`
		PageLimit      int    `json:"limit"`
	}{ConsumedCursor: consumedCursor, PageLimit: normalizedPageLimit})
	return remoteSearchCursorPrefix + uuid.NewSHA1(uuid.NameSpaceOID, []byte(shapeDigest)).String()
}

//nolint:gocyclo // Cursor decoding validates every authority, bound, replay, and expiry invariant fail closed.
func loadRemoteSearchContinuation(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	binding *store.MemoryBackendBinding,
	queryDigest, encoded string,
	now time.Time,
) (remoteSearchCursor, string, time.Time, *remoteSearchTombstoneCursor, error) {
	if strings.TrimSpace(encoded) == "" {
		return remoteSearchCursor{}, "", time.Time{}, nil, nil
	}
	encoded = strings.TrimSpace(encoded)
	if strings.HasPrefix(encoded, legacyRemoteSearchCursorPrefix) && !strings.HasPrefix(encoded, remoteSearchCursorPrefix) {
		return remoteSearchCursor{}, "", time.Time{}, nil, errLegacyRemoteSearchCursor
	}
	if governed == nil || binding == nil || !strings.HasPrefix(encoded, remoteSearchCursorPrefix) || len(encoded) > 128 {
		return remoteSearchCursor{}, "", time.Time{}, nil, errors.New("invalid memory search cursor")
	}
	identity, err := protocolBinding(binding)
	if err != nil {
		return remoteSearchCursor{}, "", time.Time{}, nil, err
	}
	now = now.UTC()
	stored, err := governed.GetMemorySearchCursor(ctx, binding.NamespaceUID, encoded, now)
	if err != nil {
		return remoteSearchCursor{}, "", time.Time{}, nil, err
	}
	if stored.ID != encoded || stored.NamespaceUID != binding.NamespaceUID ||
		stored.BindingDigest != protocol.BindingDigest(identity) || stored.QueryDigest != queryDigest ||
		len(stored.State) == 0 || len(stored.State) > maxRemoteSearchCursorBytes {
		return remoteSearchCursor{}, "", time.Time{}, nil, errors.New("mismatched memory search cursor")
	}
	var persisted persistedRemoteSearchCursor
	if err := json.Unmarshal(stored.State, &persisted); err != nil {
		return remoteSearchCursor{}, "", time.Time{}, nil, errors.New("invalid memory search cursor state")
	}
	cursor := persisted.cursor()
	providerContinuation := cursor.ProviderToken != "" || len(cursor.Pending) > 0
	tombstoneContinuation := persisted.Tombstones != nil && !persisted.Tombstones.Exhausted
	snapshotExpiresAt := persisted.SnapshotExpiresAt.UTC()
	if cursor.PageSize <= 0 || cursor.PageSize > protocol.MaxPageSize ||
		(cursor.ActualMode != protocol.SearchModeKeyword && cursor.ActualMode != protocol.SearchModeSemantic &&
			cursor.ActualMode != protocol.SearchModeHybrid) ||
		!providerContinuation && !tombstoneContinuation || len(cursor.Pending) > protocol.MaxPageSize ||
		len(cursor.ReplayPrefix)+len(cursor.Pending) > protocol.MaxPageSize ||
		(len(cursor.Pending) > 0) != (persisted.ReplayPageToken != "") ||
		(len(cursor.Pending) == 0 && len(cursor.ReplayPrefix) > 0) ||
		cursor.ProviderExhausted != (cursor.ProviderToken == "") ||
		(persisted.ReplayPageToken != "" && !cursor.ProviderExhausted && persisted.ReplayPageToken == cursor.ProviderToken) ||
		len(cursor.ProviderToken) > protocol.MaxPageTokenBytes || len(persisted.ReplayPageToken) > protocol.MaxPageTokenBytes ||
		persisted.Tombstones != nil && !persisted.Tombstones.valid() ||
		!validRemoteSearchSeenRecordState(cursor.SeenRecordState) ||
		providerContinuation != remoteSearchSeenRecordStatePresent(cursor.SeenRecordState) ||
		stored.CreatedAt.After(now) || !stored.ExpiresAt.After(now) || !stored.ExpiresAt.After(stored.CreatedAt) ||
		stored.ExpiresAt.Sub(stored.CreatedAt) > remoteSearchCursorTTL {
		return remoteSearchCursor{}, "", time.Time{}, nil, errors.New("invalid memory search cursor state")
	}
	if providerContinuation {
		if !snapshotExpiresAt.After(now) || stored.ExpiresAt.After(snapshotExpiresAt) {
			return remoteSearchCursor{}, "", time.Time{}, nil, errors.New("invalid memory search cursor state")
		}
	} else if !cursor.ProviderExhausted || persisted.ReplayPageToken != "" || !persisted.SnapshotExpiresAt.IsZero() {
		return remoteSearchCursor{}, "", time.Time{}, nil, errors.New("invalid memory search cursor state")
	}
	var tombstones *remoteSearchTombstoneCursor
	if persisted.Tombstones != nil {
		copy := *persisted.Tombstones
		tombstones = &copy
	}
	return cursor, persisted.ReplayPageToken, snapshotExpiresAt, tombstones, nil
}

func validateRemoteSearchPage(
	response *protocol.SearchResponse,
	binding protocol.Binding,
	requestedMode string,
	pageSize int,
	expectedActualMode string,
	expectedSnapshotExpiresAt time.Time,
	maximumSnapshotTTL time.Duration,
	now time.Time,
) (time.Time, error) {
	if response == nil {
		return time.Time{}, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend returned no response")
	}
	if response.Binding != binding {
		return time.Time{}, identityError()
	}
	if response.RequestedMode != requestedMode || len(response.Records) > pageSize ||
		(response.ActualMode != protocol.SearchModeKeyword && response.ActualMode != protocol.SearchModeSemantic &&
			response.ActualMode != protocol.SearchModeHybrid) ||
		(requestedMode != protocol.SearchModeAuto && response.ActualMode != requestedMode) ||
		response.Exhausted != (response.NextPageToken == "") || len(response.NextPageToken) > protocol.MaxPageTokenBytes {
		return time.Time{}, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend returned an invalid page")
	}
	if expectedActualMode != "" && response.ActualMode != expectedActualMode {
		return time.Time{}, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend changed the resolved mode across pages")
	}
	snapshotExpiresAt := response.SnapshotExpiresAt.UTC()
	if !snapshotExpiresAt.IsZero() && (maximumSnapshotTTL <= 0 || snapshotExpiresAt.After(now.UTC().Add(maximumSnapshotTTL))) {
		return time.Time{}, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend exceeded its advertised snapshot TTL")
	}
	if (!response.Exhausted || !expectedSnapshotExpiresAt.IsZero()) && !snapshotExpiresAt.After(now.UTC()) {
		return time.Time{}, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend returned an expired continuation snapshot")
	}
	if !expectedSnapshotExpiresAt.IsZero() && !snapshotExpiresAt.Equal(expectedSnapshotExpiresAt.UTC()) {
		return time.Time{}, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend changed the snapshot expiry across pages")
	}
	return snapshotExpiresAt, nil
}

func validateRemoteSearchReplayPrefixIdentities(
	seenRecordState []byte,
	records []protocol.MemoryRecord,
) error {
	probe := cloneRemoteSearchSeenRecordState(seenRecordState)
	for i := range records {
		seen, err := rememberRemoteSearchRecordIdentity(&probe, records[i].UpsertKey)
		if err != nil || !seen {
			return apierror.New(http.StatusConflict, ReasonDiverged,
				"memory search continuation replay prefix changed")
		}
	}
	return nil
}

func validateRemoteSearchRecordIdentities(
	seenRecordState []byte,
	records []protocol.MemoryRecord,
	maximumRecords int,
) error {
	candidateState := cloneRemoteSearchSeenRecordState(seenRecordState)
	for i := range records {
		if err := trackRemoteSearchRecordIdentity(&candidateState, records[i].UpsertKey); err != nil {
			return err
		}
	}
	if count, ok := remoteSearchSeenRecordCount(candidateState); !ok || count > maximumRecords {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend exceeded its advertised snapshot record limit")
	}
	return nil
}

func trackRemoteSearchRecordIdentity(state *[]byte, upsertKey string) error {
	seen, err := rememberRemoteSearchRecordIdentity(state, upsertKey)
	switch {
	case errors.Is(err, errRemoteSearchIdentityCapacity):
		return apierror.New(http.StatusServiceUnavailable, ReasonResultSetIncomplete,
			"memory search identity tracking capacity was exhausted")
	case err != nil:
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory search backend returned an invalid record identity")
	case seen:
		return apierror.New(http.StatusConflict, ReasonDiverged,
			"memory search backend repeated a record across the continuation")
	default:
		return nil
	}
}

func appendSearchCursorRecords(
	pending []remoteSearchCursorRecord,
	records []protocol.MemoryRecord,
) []remoteSearchCursorRecord {
	for i := range records {
		record := &records[i]
		pending = append(pending, remoteSearchCursorRecord{
			MemoryID: record.MemoryID, UpsertKey: record.UpsertKey, Generation: record.Generation,
			BackendVersion: record.BackendVersion, BackendMemoryID: record.BackendMemoryID,
			ContentDigest: record.ContentDigest, Score: record.Score,
		})
	}
	return pending
}

func searchCursorRecordMatches(descriptor remoteSearchCursorRecord, record *protocol.MemoryRecord) bool {
	return record != nil && record.MemoryID == descriptor.MemoryID && record.UpsertKey == descriptor.UpsertKey &&
		record.Generation == descriptor.Generation &&
		record.BackendVersion == descriptor.BackendVersion && record.BackendMemoryID == descriptor.BackendMemoryID &&
		record.ContentDigest == descriptor.ContentDigest && record.Score == descriptor.Score
}

func (s *Service) searchHit(
	ctx context.Context,
	authority *ResolvedAuthority,
	binding protocol.Binding,
	record *protocol.MemoryRecord,
	score float64,
	request SearchRequest,
	actualMode string,
) (*SearchHit, bool, error) {
	if record == nil {
		return nil, false, apierror.New(http.StatusConflict, ReasonDiverged, "memory search result is missing")
	}
	entry, err := s.Governed.GetRemoteMemory(ctx, authority.NamespaceUID, record.MemoryID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mapStoreError(err)
	}
	if !entryMatchesAuthority(entry, authority.Binding) || entry.Deleted ||
		entry.MaterializationState != store.MemoryMaterializationActive || !searchEntryEligible(entry, request) {
		return nil, false, nil
	}
	if record.State != protocol.RecordStateLive || record.UpsertKey != protocol.CanonicalUpsertKey(binding, entry.ID) ||
		int64(record.Generation) != entry.Generation || record.BackendVersion != entry.BackendVersion ||
		record.BackendMemoryID != entry.BackendMemoryID || record.ContentDigest != entry.ContentDigest ||
		protocol.ContentDigest(record.Content) != entry.ContentDigest || !remoteRecordCollectionsMatchCatalog(record, entry) {
		return nil, false, apierror.New(http.StatusConflict, ReasonDiverged, "memory search result failed materialization verification")
	}
	if actualMode == protocol.SearchModeKeyword && !keywordRecordMatches(record, entry, request.Query) {
		return nil, false, nil
	}
	memory := remoteEntryToMemory(entry, record.Content)
	memory.ContentAvailable = true
	return &SearchHit{Memory: memory, Score: score}, true, nil
}

// ApplyMemoryProposal atomically links accepted proposal governance to remote admission.
func (s *Service) ApplyMemoryProposal(
	ctx context.Context,
	namespace, proposalID, appliedBy string,
	mutationContext MutationContext,
) (*MutationResult, error) {
	localAuthority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !localAuthority.Remote() {
		if s.Proposals == nil {
			return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory proposal store is not configured")
		}
		apply := store.MemoryProposalApply{Namespace: namespace, ID: proposalID, AppliedBy: appliedBy}
		var memory *store.Memory
		var applyErr error
		if s.Governed != nil && strings.TrimSpace(localAuthority.NamespaceUID) != "" {
			governedProposals, ok := s.Proposals.(legacyMemoryProposalGovernanceStore)
			if !ok {
				return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
					"legacy memory proposal governance does not support atomic audit snapshots")
			}
			var audits []store.MemoryAuditRecord
			memory, audits, applyErr = governedProposals.ApplyLegacyMemoryProposalWithAudit(
				ctx, apply, localAuthority.NamespaceUID,
			)
			if applyErr == nil {
				applyLegacyGovernanceRecords(memory, audits)
			}
		} else {
			memory, applyErr = s.Proposals.ApplyMemoryProposal(ctx, apply)
		}
		if applyErr != nil {
			return nil, mapStoreError(applyErr)
		}
		return &MutationResult{Memory: memory, StatusCode: http.StatusOK}, nil
	}
	if err := requireIdempotency(mutationContext); err != nil {
		return nil, err
	}
	requestDigest := digestJSON(struct {
		ProposalID string `json:"proposalId"`
		AppliedBy  string `json:"appliedBy"`
	}{proposalID, appliedBy})
	if replay, found, replayErr := s.lookupMutationReplay(ctx, localAuthority, mutationContext, requestDigest); found || replayErr != nil {
		return replay, replayErr
	}

	authority, err := s.resolve(ctx, namespace, true)
	if err != nil {
		return nil, err
	}
	if err := requireRemoteMutation(authority, false); err != nil {
		return nil, err
	}
	if s.Proposals == nil {
		return nil, apierror.New(http.StatusNotImplemented, ReasonBackendUnavailable, "memory proposal store is not configured")
	}
	proposal, err := s.Proposals.GetMemoryProposal(ctx, namespace, proposalID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if proposal.Type != "memory" {
		return nil, apierror.New(http.StatusBadRequest, "", "proposal cannot be applied as memory")
	}
	if proposal.Status == "applied" || proposal.Status == "applying" {
		return nil, apierror.New(http.StatusConflict, ReasonOperationInProgress,
			"proposal application already has a caller-idempotency outcome")
	}
	if proposal.Status != "accepted" {
		return nil, apierror.New(http.StatusConflict, ReasonOperationInProgress, "proposal must be accepted before application")
	}
	content, tags, err := s.normalizeContent(proposal.Content, tagsFromProposalDescription(proposal.Description))
	if err != nil {
		return nil, err
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	now := s.now()
	memoryID := "mem-" + uuid.NewString()
	expectedGeneration, desiredGeneration, err := s.remoteCreateGenerations(ctx, authority, memoryID)
	if err != nil {
		return nil, err
	}
	operationID := "mop-" + uuid.NewString()
	metadata := compactMetadata(map[string]string{
		"agentName": proposal.AgentName, "taskName": proposal.TaskName,
		"source": memorySourceProposal, "sourceProposalId": proposal.ID,
	})
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version, OperationID: operationID, Binding: binding,
		MemoryID: memoryID, Kind: protocol.MutationKindCreate,
		Generation: uint64(desiredGeneration), ExpectedGeneration: uint64(expectedGeneration),
		State: &protocol.MutationState{Content: content, Tags: tags, Metadata: metadata},
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		return nil, apierror.New(http.StatusBadRequest, "", "invalid proposal memory mutation")
	}
	payload, _ := json.Marshal(envelope)
	if err := validateRemoteMutationLimits(authority, envelope.State, payload); err != nil {
		return nil, err
	}
	catalog := store.RemoteMemoryCatalogEntry{
		ID: memoryID, Namespace: namespace, NamespaceUID: authority.NamespaceUID,
		ClusterID: authority.Binding.ClusterID, BackendUID: authority.Binding.BackendUID,
		AuthorityEpoch: authority.Binding.AuthorityEpoch, RoutingEpoch: authority.Binding.RoutingEpoch,
		TenantID: authority.Binding.TenantID, StoreUUID: authority.Binding.StoreUUID,
		Generation: expectedGeneration, DesiredGeneration: desiredGeneration, GovernanceRevision: 1,
		MaterializationState: store.MemoryMaterializationPending, Trust: store.MemoryTrustReviewed,
		AgentName: metadata["agentName"], TaskName: metadata["taskName"], Source: metadata["source"],
		SourceProposalID: metadata["sourceProposalId"], Tags: tags, ContentDigest: envelope.ContentDigest,
		PendingOperationID: operationID, CreatedAt: now, UpdatedAt: now,
	}
	admission, err := s.Governed.AdmitRemoteMemoryProposalApply(ctx, store.RemoteMemoryProposalApplyAdmission{
		Mutation: s.mutationAdmission(mutationContext, authority, memoryID, operationID, proposal.ID, requestDigest, envelope, payload, now),
		Memory:   catalog, AppliedBy: appliedBy,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	memoryEnqueueTotal.WithLabelValues(string(store.MemoryOperationCreate)).Inc()
	return s.materializeOrQueue(ctx, authority, admission, http.StatusOK)
}

// RetryMemoryOperation performs an audited manual retry of the same operation ID.
func (s *Service) RetryMemoryOperation(ctx context.Context, namespace, id, actor, reason, requestID string) (*store.MemoryOperation, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, apierror.New(http.StatusBadRequest, "", "reason is required")
	}
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !authority.Remote() {
		return nil, store.ErrNotFound
	}
	operation, err := s.Governed.GetMemoryOperation(ctx, authority.NamespaceUID, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	updated, err := s.Governed.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: authority.NamespaceUID, ID: id, BackendUID: operation.BackendUID,
		AuthorityEpoch: operation.AuthorityEpoch, RoutingEpoch: operation.RoutingEpoch,
		Manual: true, NextRetryAt: s.now(), Actor: actor, Reason: reason, RequestID: requestID, Now: s.now(),
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return updated, nil
}

// AbandonMemoryOperation derives provider non-application and durable fencing
// from authenticated OMS calls; callers supply only audited intent metadata.
func (s *Service) AbandonMemoryOperation(
	ctx context.Context,
	namespace, id, actor, reason, requestID string,
) (*store.MemoryOperation, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, apierror.New(http.StatusBadRequest, "", "reason is required")
	}
	localAuthority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return nil, err
	}
	if !localAuthority.Remote() {
		return nil, store.ErrNotFound
	}
	operation, err := s.Governed.GetMemoryOperation(ctx, localAuthority.NamespaceUID, id)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if operation.State != store.MemoryOperationDeadLettered {
		return nil, apierror.New(http.StatusConflict, ReasonOperationInProgress,
			"only a dead-lettered memory operation can be abandoned")
	}
	authority, err := s.resolve(ctx, namespace, true)
	if err != nil {
		return nil, err
	}
	if authority.Binding == nil || authority.Adapter == nil ||
		authority.Binding.BackendUID != operation.BackendUID ||
		authority.Binding.AuthorityEpoch != operation.AuthorityEpoch ||
		authority.Binding.RoutingEpoch < operation.RoutingEpoch {
		return nil, apierror.New(http.StatusConflict, ReasonIdentityMismatch,
			"memory operation no longer belongs to the active provider authority")
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil {
		return nil, identityError()
	}
	fence, err := authority.Adapter.AdvanceRoutingFence(ctx, protocol.RoutingFenceRequest{
		ProtocolVersion: protocol.Version, Binding: binding,
	})
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory provider fence could not be verified")
	}
	if fence == nil || fence.Binding != binding || fence.BindingDigest != protocol.BindingDigest(binding) ||
		fence.MaximumRoutingEpoch < binding.RoutingEpoch ||
		(fence.Result != protocol.ResultApplied && fence.Result != protocol.ResultPreconditionFailed) {
		return nil, apierror.New(http.StatusConflict, ReasonIdentityMismatch,
			"memory provider returned an invalid routing fence acknowledgement")
	}
	if operation.SendStartedAt != nil && fence.MaximumRoutingEpoch <= uint64(operation.RoutingEpoch) {
		return nil, apierror.New(http.StatusConflict, ReasonOperationInProgress,
			"sent memory operation cannot be abandoned until a newer routing epoch is durably fenced")
	}
	lookup, err := authority.Adapter.LookupOperation(ctx, protocol.OperationLookupRequest{
		ProtocolVersion: protocol.Version, Binding: binding, OperationID: operation.ID,
	})
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"memory provider operation outcome could not be verified")
	}
	providerNeverApplied, err := providerOperationNeverApplied(binding, operation, lookup)
	if err != nil {
		return nil, err
	}
	updated, err := s.Governed.AbandonMemoryOperation(ctx, store.MemoryOperationAbandonment{
		NamespaceUID: localAuthority.NamespaceUID, ID: id, BackendUID: operation.BackendUID,
		AuthorityEpoch: operation.AuthorityEpoch, RoutingEpoch: operation.RoutingEpoch,
		Actor: actor, Reason: reason, RequestID: requestID,
		ProviderNeverApplied: providerNeverApplied, Fenced: true, Now: s.now(),
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return updated, nil
}

func providerOperationNeverApplied(
	currentBinding protocol.Binding,
	operation *store.MemoryOperation,
	lookup *protocol.OperationLookupResponse,
) (bool, error) {
	if operation == nil || lookup == nil || lookup.Binding != currentBinding {
		return false, apierror.New(http.StatusConflict, ReasonIdentityMismatch,
			"memory provider operation lookup identity did not match")
	}
	if !lookup.Found {
		if lookup.Receipt != nil {
			return false, apierror.New(http.StatusConflict, ReasonIdentityMismatch,
				"memory provider returned an invalid operation lookup")
		}
		return true, nil
	}
	receipt := lookup.Receipt
	if receipt == nil || receipt.OperationID != operation.ID || receipt.MutationDigest != operation.MutationDigest ||
		protocol.AuthorityDigest(receipt.Binding) != protocol.AuthorityDigest(currentBinding) ||
		int64(receipt.Binding.RoutingEpoch) != operation.RoutingEpoch {
		return false, apierror.New(http.StatusConflict, ReasonIdentityMismatch,
			"memory provider operation receipt did not correlate to the operation")
	}
	switch receipt.Result {
	case protocol.ResultApplied, protocol.ResultNotFound:
		return false, apierror.New(http.StatusConflict, ReasonOperationInProgress,
			"memory provider reports that the operation was applied")
	case protocol.ResultPreconditionFailed, protocol.ResultIdempotencyConflict, protocol.ResultIdentityConflict,
		protocol.ResultRetryableError, protocol.ResultNonRetryableError:
		return true, nil
	default:
		return false, apierror.New(http.StatusConflict, ReasonIdentityMismatch,
			"memory provider returned an unsupported operation outcome")
	}
}

func tagsFromProposalDescription(description string) []string {
	for line := range strings.SplitSeq(description, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 5 || !strings.EqualFold(line[:5], "tags:") {
			continue
		}
		parts := strings.Split(line[5:], ",")
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				result = append(result, part)
			}
		}
		return result
	}
	return []string{}
}

// IsRemote reports whether durable remote authority has ever been activated for the current namespace incarnation.
func (s *Service) IsRemote(ctx context.Context, namespace string) (bool, error) {
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return false, err
	}
	return authority.Remote(), nil
}

// MarkMemoriesRecalled updates recall telemetry without changing content generation.
func (s *Service) MarkMemoriesRecalled(ctx context.Context, namespace string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	authority, err := s.resolve(ctx, namespace, false)
	if err != nil {
		return err
	}
	if !authority.Remote() {
		if s.Legacy == nil {
			return nil
		}
		return s.Legacy.MarkMemoriesRecalled(ctx, namespace, ids)
	}
	return s.Governed.MarkRemoteMemoriesRecalled(ctx, authority.NamespaceUID, ids, s.now())
}
