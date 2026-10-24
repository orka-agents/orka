/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package conformance probes the closed orka.memory.v1 contract. Prepare and
// VerifyAfterRestart are deliberately separate so release automation can prove
// durable receipts, fences, records, tombstones, ownership, and snapshots.
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	memoryv1 "github.com/orka-agents/orka/pkg/memory/v1"
)

const (
	checkpointVersion                 = "orka.memory.v1.conformance.checkpoint.v3"
	conformanceFailpointHeader        = "X-Orka-Memory-Conformance-Failpoint"
	conformanceProviderCommitGapValue = "provider-commit-before-receipt-v1"
	conformanceTag                    = "conformance"

	fixtureRoleAuthentication     = "a"
	fixtureRoleStrictCodec        = "b"
	fixtureRoleCASBase            = "c"
	fixtureRoleCASLeft            = "d"
	fixtureRoleCASRight           = "e"
	fixtureRoleMain               = "f"
	fixtureRoleDuplicateCreate    = "g"
	fixtureRoleWrongCAS           = "h"
	fixtureRoleReplace            = "i"
	fixtureRoleSearch             = "j"
	fixtureRolePostSnapshot       = "k"
	fixtureRoleDeleteSnapshot     = "l"
	fixtureRoleAbsentDelete       = "m"
	fixtureRoleStaleRouting       = "n"
	fixtureRoleProviderCommitGap  = "o"
	fixtureRoleStaleResurrection  = "p"
	fixtureRoleSecondStaleRouting = "r"
)

// Checkpoint contains no credentials. It is safe to persist between prepare
// and post-restart verification phases.
type Checkpoint struct {
	CheckpointVersion                   string                     `json:"checkpointVersion"`
	ProtocolVersion                     string                     `json:"protocolVersion"`
	RunID                               string                     `json:"runId"`
	StoreName                           string                     `json:"storeName"`
	Binding                             memoryv1.Binding           `json:"binding"`
	CapabilitiesRevision                string                     `json:"capabilitiesRevision"`
	Capabilities                        memoryv1.Capabilities      `json:"capabilities"`
	CapabilityLimits                    memoryv1.CapabilityLimits  `json:"capabilityLimits"`
	ReplayMutation                      memoryv1.MutationEnvelope  `json:"replayMutation"`
	ReplayReceipt                       memoryv1.MutationReceipt   `json:"replayReceipt"`
	LiveRecord                          memoryv1.MemoryRecord      `json:"liveRecord"`
	TombstoneRecord                     memoryv1.MemoryRecord      `json:"tombstoneRecord"`
	PaginationRequest                   memoryv1.SearchRequest     `json:"paginationRequest"`
	PaginationExpectedKeys              []string                   `json:"paginationExpectedKeys"`
	PaginationExpectedDigests           []string                   `json:"paginationExpectedDigests"`
	PaginationActualMode                string                     `json:"paginationActualMode"`
	PaginationRecordsBeforeContinuation int                        `json:"paginationRecordsBeforeContinuation"`
	PaginationSnapshotExpiry            time.Time                  `json:"paginationSnapshotExpiry"`
	SnapshotExcludedKey                 string                     `json:"snapshotExcludedKey"`
	StaleMutation                       memoryv1.MutationEnvelope  `json:"staleMutation"`
	StaleReceipt                        memoryv1.MutationReceipt   `json:"staleReceipt"`
	ProviderCommitGapMutation           *memoryv1.MutationEnvelope `json:"providerCommitGapMutation,omitempty"`
}

// CheckResult is the sanitized outcome of one conformance phase.
type CheckResult struct {
	Passed       bool                           `json:"passed"`
	Phase        string                         `json:"phase"`
	Message      string                         `json:"message"`
	Capabilities *memoryv1.CapabilitiesResponse `json:"capabilities,omitempty"`
}

type fixturePlan struct {
	limits    memoryv1.CapabilityLimits
	namespace string
	query     string
}

func newFixturePlan(runID string, limits memoryv1.CapabilityLimits) (fixturePlan, error) {
	if limits.MaxSnapshotRecords < 2 {
		return fixturePlan{}, fmt.Errorf(
			"adapter advertises maxSnapshotRecords=%d; "+
				"memory protocol conformance requires at least 2 records to prove stable multi-page pagination",
			limits.MaxSnapshotRecords,
		)
	}
	queryBytes := min(limits.MaxContentBytes, limits.MaxQueryBytes)
	sum := sha256.Sum256([]byte(runID))
	namespace := hex.EncodeToString(sum[:16])
	querySeed := "q" + base64.RawURLEncoding.EncodeToString(sum[:12])
	if queryBytes < len(querySeed) {
		return fixturePlan{}, fmt.Errorf(
			"adapter advertises maxContentBytes=%d and maxQueryBytes=%d; "+
				"memory protocol conformance requires at least %d shared keyword bytes for run isolation",
			limits.MaxContentBytes,
			limits.MaxQueryBytes,
			len(querySeed),
		)
	}
	query := querySeed
	plan := fixturePlan{limits: limits, namespace: namespace, query: query}
	for name, state := range map[string]*memoryv1.MutationState{
		"minimal": plan.state("a"),
		"search":  plan.state(plan.query),
	} {
		if err := validateFixtureState(limits, state); err != nil {
			return fixturePlan{}, fmt.Errorf("adapter limits cannot encode the %s conformance state: %w", name, err)
		}
	}
	request, err := plan.searchRequest(DefaultBinding(), memoryv1.SearchModeKeyword)
	if err != nil {
		return fixturePlan{}, err
	}
	if request.PageSize != 1 {
		return fixturePlan{}, errors.New("conformance pagination fixture must use one record per page")
	}
	return plan, nil
}

func (p fixturePlan) operationID(role string) string {
	return "mop-" + p.namespace + "-" + role
}

func (p fixturePlan) memoryID(role string) string {
	return "mem-" + p.namespace + "-" + role
}

func (p fixturePlan) conflictBackendUID(role string) string {
	return "conflict-" + p.namespace + "-" + role
}

func (p fixturePlan) state(content string, markers ...string) *memoryv1.MutationState {
	marker := "t"
	if len(markers) > 0 && markers[0] != "" {
		marker = markers[0]
	}
	return &memoryv1.MutationState{
		Content: content,
		Tags:    []string{marker},
		Metadata: map[string]string{
			marker: marker,
		},
	}
}

func (p fixturePlan) mutation(
	binding memoryv1.Binding,
	operationRole string,
	memoryID string,
	kind string,
	generation uint64,
	expectedGeneration uint64,
	expectedVersion string,
	state *memoryv1.MutationState,
) (memoryv1.MutationEnvelope, error) {
	request, err := makeMutation(
		binding,
		p.operationID(operationRole),
		memoryID,
		kind,
		generation,
		expectedGeneration,
		expectedVersion,
		state,
	)
	if err != nil {
		return memoryv1.MutationEnvelope{}, err
	}
	if err := validateFixtureMutation(p.limits, &request); err != nil {
		return memoryv1.MutationEnvelope{}, err
	}
	return request, nil
}

func (p fixturePlan) searchRequest(
	binding memoryv1.Binding,
	mode string,
) (memoryv1.SearchRequest, error) {
	request := memoryv1.SearchRequest{
		ProtocolVersion: memoryv1.Version,
		Binding:         binding,
		Mode:            mode,
		Query:           p.query,
		PageSize:        1,
		PageToken:       "",
	}
	if err := validateFixtureSearch(p.limits, &request); err != nil {
		return memoryv1.SearchRequest{}, err
	}
	return request, nil
}

func validateFixtureMutation(limits memoryv1.CapabilityLimits, request *memoryv1.MutationEnvelope) error {
	if request == nil || request.State == nil {
		return nil
	}
	if err := validateFixtureState(limits, request.State); err != nil {
		return fmt.Errorf("mutation fixture exceeds advertised limits: %w", err)
	}
	return nil
}

func validateFixtureState(limits memoryv1.CapabilityLimits, state *memoryv1.MutationState) error {
	if state == nil {
		return nil
	}
	if len(state.Content) > limits.MaxContentBytes {
		return fmt.Errorf("content uses %d bytes, maxContentBytes=%d", len(state.Content), limits.MaxContentBytes)
	}
	if len(state.Tags) > limits.MaxTags {
		return fmt.Errorf("tags uses %d entries, maxTags=%d", len(state.Tags), limits.MaxTags)
	}
	for _, tag := range state.Tags {
		if len(tag) > limits.MaxTagBytes {
			return fmt.Errorf("tag uses %d bytes, maxTagBytes=%d", len(tag), limits.MaxTagBytes)
		}
	}
	if len(state.Metadata) > limits.MaxMetadataEntries {
		return fmt.Errorf(
			"metadata uses %d entries, maxMetadataEntries=%d",
			len(state.Metadata),
			limits.MaxMetadataEntries,
		)
	}
	for key, value := range state.Metadata {
		if len(key) > limits.MaxMetadataKeyBytes {
			return fmt.Errorf("metadata key uses %d bytes, maxMetadataKeyBytes=%d", len(key), limits.MaxMetadataKeyBytes)
		}
		if len(value) > limits.MaxMetadataValueBytes {
			return fmt.Errorf(
				"metadata value uses %d bytes, maxMetadataValueBytes=%d",
				len(value),
				limits.MaxMetadataValueBytes,
			)
		}
	}
	return nil
}

func validateFixtureSearch(limits memoryv1.CapabilityLimits, request *memoryv1.SearchRequest) error {
	if request == nil {
		return errors.New("search fixture is required")
	}
	if len(request.Query) > limits.MaxQueryBytes {
		return fmt.Errorf("search query uses %d bytes, maxQueryBytes=%d", len(request.Query), limits.MaxQueryBytes)
	}
	if request.PageSize > limits.MaxPageSize {
		return fmt.Errorf("search pageSize=%d exceeds maxPageSize=%d", request.PageSize, limits.MaxPageSize)
	}
	return nil
}

// Check runs prepare plus verification in one process. Release gates that need
// a restart proof must call Prepare, restart the adapter, and then call
// VerifyAfterRestart with the returned checkpoint.
func Check(ctx context.Context, target Target) (result CheckResult) {
	checkpoint, prepared := Prepare(ctx, target)
	if !prepared.Passed {
		return prepared
	}
	verified := VerifyAfterRestart(ctx, target, checkpoint)
	if !verified.Passed {
		return verified
	}
	verified.Phase = "check"
	verified.Message = "adapter conforms to orka.memory.v1 (restart durability not independently exercised)"
	return SanitizeCheckResult(verified, target.AuthorizationValue)
}

// Prepare performs the mutating contract phase and returns durable state that
// must remain valid after the adapter is restarted.
//
//nolint:gocyclo // Conformance intentionally exercises the complete closed profile in one ordered phase.
func Prepare(ctx context.Context, target Target) (checkpoint Checkpoint, result CheckResult) {
	result.Phase = "prepare"
	defer func() { result = SanitizeCheckResult(result, target.AuthorizationValue) }()

	client, err := newContractClient(target)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	binding := target.Binding
	if binding == (memoryv1.Binding{}) {
		binding = DefaultBinding()
	}
	storeName := strings.TrimSpace(target.StoreName)
	if storeName == "" {
		storeName = "conformance-store"
	}
	storeBinding := memoryv1.StoreResolutionBinding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, TenantID: binding.TenantID,
	}
	resolved, err := resolveStore(ctx, client, storeBinding, storeName)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	replayedResolution, err := resolveStore(ctx, client, storeBinding, storeName)
	if err != nil || replayedResolution.StoreUUID != resolved.StoreUUID {
		result.Message = "store resolution was not stable and idempotent"
		return checkpoint, result
	}
	binding.StoreUUID = resolved.StoreUUID
	if err := memoryv1.ValidateBinding(binding); err != nil {
		result.Message = safeError("resolved conformance binding is invalid", err)
		return checkpoint, result
	}
	runID := strings.TrimSpace(target.RunID)
	if runID == "" {
		runID, err = randomRunID()
		if err != nil {
			result.Message = "could not generate conformance run ID"
			return checkpoint, result
		}
	}
	if !isSafeRunID(runID) {
		result.Message = "conformance run ID must contain only lowercase ASCII letters, digits, and hyphens"
		return checkpoint, result
	}

	caps, err := probe(ctx, client, binding)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	result.Capabilities = caps
	fixtures, err := newFixturePlan(runID, caps.Limits)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if err := verifyAuthentication(ctx, client, storeBinding, storeName, binding, fixtures); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if err := verifyStrictStoreCodec(ctx, client, storeBinding, storeName); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if err := verifyStrictCapabilityCodec(ctx, client, binding); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	claim, err := claimOwnership(ctx, client, binding)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if claim.Result != memoryv1.ResultApplied {
		result.Message = "ownership claim was not applied"
		return checkpoint, result
	}
	if claim.MaximumRoutingEpoch > binding.RoutingEpoch {
		binding.RoutingEpoch = claim.MaximumRoutingEpoch
	}
	if err := verifyExclusiveOwnership(ctx, client, binding, fixtures.conflictBackendUID("a")); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if err := verifyStrictMutationCodec(ctx, client, binding, fixtures); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if err := verifyConcurrentCAS(ctx, client, binding, fixtures); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	mainMutation, err := fixtures.mutation(
		binding,
		fixtureRoleMain,
		fixtures.memoryID(fixtureRoleMain),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state("a"),
	)
	if err != nil {
		result.Message = safeError("could not construct create mutation", err)
		return checkpoint, result
	}
	createReceipt, createBody, err := mutation(ctx, client, mainMutation, memoryv1.ResultApplied)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	replayReceipt, replayBody, err := mutation(ctx, client, mainMutation, memoryv1.ResultApplied)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if !reflect.DeepEqual(createReceipt, replayReceipt) || !bytes.Equal(createBody, replayBody) {
		result.Message = "exact operation replay did not return the original byte-equivalent receipt"
		return checkpoint, result
	}

	conflicting := mainMutation
	conflicting.State = fixtures.state("b")
	conflicting.MutationDigest = ""
	if err := memoryv1.PrepareMutation(&conflicting); err != nil {
		result.Message = safeError("could not construct idempotency conflict", err)
		return checkpoint, result
	}
	if err := validateFixtureMutation(caps.Limits, &conflicting); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, conflicting, memoryv1.ResultIdempotencyConflict); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	duplicateCreate, err := fixtures.mutation(
		binding,
		fixtureRoleDuplicateCreate,
		mainMutation.MemoryID,
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		cloneState(mainMutation.State),
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, duplicateCreate, memoryv1.ResultPreconditionFailed); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	got, err := getRecord(ctx, client, binding, mainMutation.UpsertKey)
	if err != nil || got == nil || got.Generation != 1 || got.Content != mainMutation.State.Content {
		result.Message = "exact get did not return the created materialization"
		return checkpoint, result
	}
	lookedUp, err := lookupOperation(ctx, client, binding, mainMutation.OperationID)
	if err != nil || lookedUp == nil || !reflect.DeepEqual(*lookedUp, createReceipt) {
		result.Message = "operation lookup did not return the original receipt"
		return checkpoint, result
	}

	wrongReplace, err := fixtures.mutation(
		binding,
		fixtureRoleWrongCAS,
		mainMutation.MemoryID,
		memoryv1.MutationKindReplace,
		3,
		2,
		"",
		fixtures.state("c"),
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, wrongReplace, memoryv1.ResultPreconditionFailed); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	replaceMutation, err := fixtures.mutation(
		binding,
		fixtureRoleReplace,
		mainMutation.MemoryID,
		memoryv1.MutationKindReplace,
		2,
		1,
		createReceipt.BackendVersion,
		fixtures.state(fixtures.query, "r"),
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, replaceMutation, memoryv1.ResultApplied); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	liveRecord, err := getRecord(ctx, client, binding, mainMutation.UpsertKey)
	if err != nil ||
		liveRecord == nil ||
		liveRecord.Generation != 2 ||
		liveRecord.Content != replaceMutation.State.Content ||
		!slices.Equal(liveRecord.Tags, replaceMutation.State.Tags) ||
		!maps.Equal(liveRecord.Metadata, replaceMutation.State.Metadata) {
		result.Message = "conditional replace did not materialize the complete generation 2 state"
		return checkpoint, result
	}
	expectedSnapshotRecords := make([]memoryv1.MemoryRecord, 0, 2)
	expectedSnapshotRecords = append(expectedSnapshotRecords, *liveRecord)

	searchFixture, err := fixtures.mutation(
		binding,
		fixtureRoleSearch,
		fixtures.memoryID(fixtureRoleSearch),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state(fixtures.query),
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, searchFixture, memoryv1.ResultApplied); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	searchRecord, err := getRecord(ctx, client, binding, searchFixture.UpsertKey)
	if err != nil || searchRecord == nil {
		result.Message = "search fixture was not readable through exact get"
		return checkpoint, result
	}
	expectedSnapshotRecords = append(expectedSnapshotRecords, *searchRecord)
	expectedSnapshotDigests, err := recordDigestByKey(binding, expectedSnapshotRecords)
	if err != nil {
		result.Message = safeError("could not digest exact search fixtures", err)
		return checkpoint, result
	}
	if err := verifySearchModeCapabilities(
		ctx, client, binding, caps.Capabilities, fixtures,
	); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	searchRequest, err := fixtures.searchRequest(binding, memoryv1.SearchModeKeyword)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	firstPage, err := search(ctx, client, searchRequest)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if firstPage.ActualMode != memoryv1.SearchModeKeyword || firstPage.Exhausted || firstPage.NextPageToken == "" {
		result.Message = "explicit keyword search did not produce the required pagination fixture"
		return checkpoint, result
	}
	allSnapshotRecords, err := collectSnapshot(ctx, client, searchRequest, firstPage)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if err := verifySnapshotFixture(binding, allSnapshotRecords, expectedSnapshotDigests); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	checkpointFirstPage, err := search(ctx, client, searchRequest)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if checkpointFirstPage.ActualMode != memoryv1.SearchModeKeyword ||
		checkpointFirstPage.Exhausted || checkpointFirstPage.NextPageToken == "" ||
		!reflect.DeepEqual(checkpointFirstPage.Records, firstPage.Records) {
		result.Message = "durable pagination checkpoint did not reproduce the verified first page"
		return checkpoint, result
	}
	firstPage = checkpointFirstPage
	remainingRecords := allSnapshotRecords[len(firstPage.Records):]
	remainingKeys, remainingDigests, err := recordProofs(binding, remainingRecords)
	if err != nil {
		result.Message = safeError("could not record pagination proof", err)
		return checkpoint, result
	}
	if len(remainingKeys) == 0 {
		result.Message = "pagination fixture did not produce a continuation page"
		return checkpoint, result
	}

	postSnapshot, err := fixtures.mutation(
		binding,
		fixtureRolePostSnapshot,
		fixtures.memoryID(fixtureRolePostSnapshot),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state(fixtures.query),
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, postSnapshot, memoryv1.ResultApplied); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	snapshotTombstoneKey := remainingKeys[len(remainingKeys)-1]
	toDelete, err := getRecord(ctx, client, binding, snapshotTombstoneKey)
	if err != nil || toDelete == nil {
		result.Message = "could not load snapshot record selected for deletion"
		return checkpoint, result
	}
	deleteSnapshotRecord, err := fixtures.mutation(
		binding,
		fixtureRoleDeleteSnapshot,
		toDelete.MemoryID,
		memoryv1.MutationKindDelete,
		toDelete.Generation+1,
		toDelete.Generation,
		toDelete.BackendVersion,
		nil,
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, deleteSnapshotRecord, memoryv1.ResultApplied); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}

	absentDelete, err := fixtures.mutation(
		binding,
		fixtureRoleAbsentDelete,
		fixtures.memoryID(fixtureRoleAbsentDelete),
		memoryv1.MutationKindDelete,
		1,
		0,
		"",
		nil,
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if _, _, err := mutation(ctx, client, absentDelete, memoryv1.ResultNotFound); err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	tombstoneRecord, err := getRecord(ctx, client, binding, absentDelete.UpsertKey)
	if err != nil ||
		tombstoneRecord == nil ||
		tombstoneRecord.State != memoryv1.RecordStateTombstone ||
		tombstoneRecord.Generation != 1 {
		result.Message = "delete-if-absent did not persist a generation high-watermark tombstone"
		return checkpoint, result
	}
	if binding.RoutingEpoch == math.MaxInt64 {
		result.Message = "routing epoch cannot be advanced for conformance"
		return checkpoint, result
	}
	staleBinding := binding
	binding.RoutingEpoch++
	fence, err := advanceFence(ctx, client, binding)
	if err != nil || fence.Result != memoryv1.ResultApplied || fence.MaximumRoutingEpoch != binding.RoutingEpoch {
		result.Message = "routing fence did not advance durably"
		return checkpoint, result
	}
	staleMutation, err := fixtures.mutation(
		staleBinding,
		fixtureRoleStaleRouting,
		fixtures.memoryID(fixtureRoleStaleRouting),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state("h"),
	)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	staleReceipt, _, err := mutation(ctx, client, staleMutation, memoryv1.ResultIdentityConflict)
	if err != nil {
		result.Message = err.Error()
		return checkpoint, result
	}
	if staleRecord, getErr := getRecord(
		ctx, client, binding, staleMutation.UpsertKey,
	); getErr != nil || staleRecord != nil {
		result.Message = "stale routing mutation changed provider content"
		return checkpoint, result
	}

	currentLive, err := getRecord(ctx, client, binding, liveRecord.UpsertKey)
	if err != nil || currentLive == nil {
		result.Message = "live record was not readable after routing-fence advancement"
		return checkpoint, result
	}
	searchRequest.Binding = binding
	searchRequest.PageToken = firstPage.NextPageToken

	var providerCommitGapMutation *memoryv1.MutationEnvelope
	if target.ProviderCommitGapProof {
		gapMutation, gapErr := fixtures.mutation(
			binding,
			fixtureRoleProviderCommitGap,
			fixtures.memoryID(fixtureRoleProviderCommitGap),
			memoryv1.MutationKindCreate,
			1,
			0,
			"",
			fixtures.state("i"),
		)
		if gapErr != nil {
			result.Message = gapErr.Error()
			return checkpoint, result
		}
		if gapErr = induceProviderCommitGap(ctx, client, gapMutation); gapErr != nil {
			result.Message = gapErr.Error()
			return checkpoint, result
		}
		providerCommitGapMutation = &gapMutation
	}

	checkpoint = Checkpoint{
		CheckpointVersion: checkpointVersion, ProtocolVersion: memoryv1.Version, RunID: runID,
		StoreName: storeName, Binding: binding, CapabilitiesRevision: caps.Revision,
		Capabilities: caps.Capabilities, CapabilityLimits: caps.Limits,
		ReplayMutation: mainMutation, ReplayReceipt: createReceipt, LiveRecord: *currentLive,
		TombstoneRecord: *tombstoneRecord, PaginationRequest: searchRequest,
		PaginationExpectedKeys: remainingKeys, PaginationExpectedDigests: remainingDigests,
		PaginationActualMode:                firstPage.ActualMode,
		PaginationRecordsBeforeContinuation: len(firstPage.Records),
		PaginationSnapshotExpiry:            firstPage.SnapshotExpiresAt,
		SnapshotExcludedKey:                 postSnapshot.UpsertKey,
		StaleMutation:                       staleMutation, StaleReceipt: staleReceipt,
		ProviderCommitGapMutation: providerCommitGapMutation,
	}
	if err := ValidateCheckpoint(checkpoint); err != nil {
		result.Message = safeError("generated checkpoint is invalid", err)
		return Checkpoint{}, result
	}
	result.Passed = true
	result.Message = "memory protocol conformance prepare phase passed; restart the adapter before verify"
	return checkpoint, result
}

// VerifyAfterRestart proves that the state created by Prepare remained durable.
//
//nolint:gocyclo // Restart verification checks every persisted memory protocol safety property.
func VerifyAfterRestart(ctx context.Context, target Target, checkpoint Checkpoint) (result CheckResult) {
	result.Phase = "verify"
	defer func() { result = SanitizeCheckResult(result, target.AuthorizationValue) }()
	if err := ValidateCheckpoint(checkpoint); err != nil {
		result.Message = safeError("checkpoint is invalid", err)
		return result
	}
	if err := ensureRestartContinuationUnexpired(checkpoint.PaginationSnapshotExpiry, time.Now().UTC()); err != nil {
		result.Message = err.Error()
		return result
	}
	client, err := newContractClient(target)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	caps, err := probe(ctx, client, checkpoint.Binding)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	result.Capabilities = caps
	if err := verifyRestartCapabilities(caps, checkpoint); err != nil {
		result.Message = err.Error()
		return result
	}
	fixtures, err := newFixturePlan(checkpoint.RunID, caps.Limits)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	continuation, err := collectContinuation(
		ctx, client, checkpoint.PaginationRequest, checkpoint.PaginationSnapshotExpiry,
		checkpoint.PaginationRecordsBeforeContinuation,
	)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	if err := verifyContinuationProof(checkpoint.Binding, continuation, checkpoint); err != nil {
		result.Message = err.Error()
		return result
	}
	if slices.Contains(checkpoint.PaginationExpectedKeys, checkpoint.SnapshotExcludedKey) {
		result.Message = "stable snapshot included a record created after the snapshot"
		return result
	}

	storeBinding := memoryv1.StoreResolutionBinding{
		ClusterID: checkpoint.Binding.ClusterID, NamespaceUID: checkpoint.Binding.NamespaceUID,
		BackendUID: checkpoint.Binding.BackendUID, TenantID: checkpoint.Binding.TenantID,
	}
	resolved, err := resolveStore(ctx, client, storeBinding, checkpoint.StoreName)
	if err != nil || resolved.StoreUUID != checkpoint.Binding.StoreUUID {
		result.Message = "stable store resolution did not survive restart"
		return result
	}

	claim, err := claimOwnership(ctx, client, checkpoint.Binding)
	if err != nil ||
		claim.Result != memoryv1.ResultApplied ||
		claim.MaximumRoutingEpoch < checkpoint.Binding.RoutingEpoch {
		result.Message = "exclusive ownership claim or routing fence did not survive restart"
		return result
	}
	if err := verifyExclusiveOwnership(ctx, client, checkpoint.Binding, fixtures.conflictBackendUID("b")); err != nil {
		result.Message = err.Error()
		return result
	}
	if checkpoint.ProviderCommitGapMutation != nil {
		gapReceipt, _, gapErr := mutation(ctx, client, *checkpoint.ProviderCommitGapMutation, memoryv1.ResultApplied)
		if gapErr != nil {
			result.Message = "provider-commit gap did not recover through operation lookup after restart"
			return result
		}
		gapLookup, lookupErr := lookupOperation(
			ctx,
			client,
			checkpoint.Binding,
			checkpoint.ProviderCommitGapMutation.OperationID,
		)
		if lookupErr != nil || gapLookup == nil || !reflect.DeepEqual(*gapLookup, gapReceipt) {
			result.Message = "recovered provider-commit gap receipt was not durable after restart"
			return result
		}
		gapRecord, getErr := getRecord(ctx, client, checkpoint.Binding, checkpoint.ProviderCommitGapMutation.UpsertKey)
		if getErr != nil ||
			gapRecord == nil ||
			gapRecord.Generation != checkpoint.ProviderCommitGapMutation.Generation ||
			checkpoint.ProviderCommitGapMutation.State == nil ||
			gapRecord.Content != checkpoint.ProviderCommitGapMutation.State.Content {
			result.Message = "recovered provider-commit gap record is missing or divergent"
			return result
		}
	}
	lookedUp, err := lookupOperation(ctx, client, checkpoint.Binding, checkpoint.ReplayMutation.OperationID)
	if err != nil || lookedUp == nil || !reflect.DeepEqual(*lookedUp, checkpoint.ReplayReceipt) {
		result.Message = "durable operation lookup failed after restart"
		return result
	}
	replayed, _, err := mutation(ctx, client, checkpoint.ReplayMutation, checkpoint.ReplayReceipt.Result)
	if err != nil || !reflect.DeepEqual(replayed, checkpoint.ReplayReceipt) {
		result.Message = "exact stale-routing replay did not return the original receipt after restart"
		return result
	}
	live, err := getRecord(ctx, client, checkpoint.Binding, checkpoint.LiveRecord.UpsertKey)
	if err != nil || live == nil || !reflect.DeepEqual(*live, checkpoint.LiveRecord) {
		result.Message = "live generation did not survive restart"
		return result
	}
	tombstone, err := getRecord(ctx, client, checkpoint.Binding, checkpoint.TombstoneRecord.UpsertKey)
	if err != nil || tombstone == nil || !reflect.DeepEqual(*tombstone, checkpoint.TombstoneRecord) {
		result.Message = "delete high-watermark tombstone did not survive restart"
		return result
	}

	staleCreate, err := fixtures.mutation(
		checkpoint.Binding,
		fixtureRoleStaleResurrection,
		checkpoint.TombstoneRecord.MemoryID,
		memoryv1.MutationKindCreate,
		checkpoint.TombstoneRecord.Generation,
		0,
		"",
		fixtures.state("j"),
	)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	if _, _, err := mutation(ctx, client, staleCreate, memoryv1.ResultPreconditionFailed); err != nil {
		result.Message = "delete high-watermark did not reject stale resurrection after restart"
		return result
	}

	staleReplay, _, err := mutation(ctx, client, checkpoint.StaleMutation, memoryv1.ResultIdentityConflict)
	if err != nil || !reflect.DeepEqual(staleReplay, checkpoint.StaleReceipt) {
		result.Message = "stale routing-fence receipt did not survive restart"
		return result
	}
	secondStale, err := fixtures.mutation(
		checkpoint.StaleMutation.Binding,
		fixtureRoleSecondStaleRouting,
		fixtures.memoryID(fixtureRoleSecondStaleRouting),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state("k"),
	)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	if _, _, err := mutation(ctx, client, secondStale, memoryv1.ResultIdentityConflict); err != nil {
		result.Message = "durable routing fence accepted a new stale mutation after restart"
		return result
	}
	if record, getErr := getRecord(
		ctx, client, checkpoint.Binding, secondStale.UpsertKey,
	); getErr != nil || record != nil {
		result.Message = "new stale routing mutation changed content after restart"
		return result
	}

	result.Passed = true
	result.Message = "adapter conforms to orka.memory.v1 including restart durability"
	return result
}

// ValidateCheckpoint rejects malformed or cross-profile checkpoint files.
func ValidateCheckpoint(checkpoint Checkpoint) error {
	if checkpoint.CheckpointVersion != checkpointVersion || checkpoint.ProtocolVersion != memoryv1.Version {
		return errors.New("checkpoint version is unsupported")
	}
	if !isSafeRunID(checkpoint.RunID) {
		return errors.New("checkpoint runId is invalid")
	}
	if err := memoryv1.ValidateBinding(checkpoint.Binding); err != nil {
		return err
	}
	storeBinding := memoryv1.StoreResolutionBinding{
		ClusterID: checkpoint.Binding.ClusterID, NamespaceUID: checkpoint.Binding.NamespaceUID,
		BackendUID: checkpoint.Binding.BackendUID, TenantID: checkpoint.Binding.TenantID,
	}
	if err := memoryv1.ValidateStoreResolveRequest(&memoryv1.StoreResolveRequest{
		ProtocolVersion: memoryv1.Version, Binding: storeBinding, StoreName: checkpoint.StoreName,
	}); err != nil {
		return err
	}
	if checkpoint.CapabilitiesRevision == "" {
		return errors.New("capabilitiesRevision is required")
	}
	if err := validateCheckpointCapabilities(checkpoint); err != nil {
		return err
	}
	if err := memoryv1.ValidateMutationEnvelope(&checkpoint.ReplayMutation); err != nil {
		return err
	}
	if err := memoryv1.ValidateMutationReceipt(&checkpoint.ReplayReceipt); err != nil {
		return err
	}
	if !memoryv1.AuthorityEqual(checkpoint.Binding, checkpoint.ReplayMutation.Binding) ||
		!memoryv1.AuthorityEqual(checkpoint.Binding, checkpoint.ReplayReceipt.Binding) {
		return errors.New("replay authority does not match checkpoint binding")
	}
	if err := memoryv1.ValidateMemoryRecord(&checkpoint.LiveRecord, checkpoint.Binding); err != nil {
		return err
	}
	if checkpoint.LiveRecord.State != memoryv1.RecordStateLive {
		return errors.New("liveRecord is not live")
	}
	if err := memoryv1.ValidateMemoryRecord(&checkpoint.TombstoneRecord, checkpoint.Binding); err != nil {
		return err
	}
	if checkpoint.TombstoneRecord.State != memoryv1.RecordStateTombstone {
		return errors.New("tombstoneRecord is not a tombstone")
	}
	if err := validateCheckpointPagination(checkpoint); err != nil {
		return err
	}
	if checkpoint.SnapshotExcludedKey == "" {
		return errors.New("snapshotExcludedKey is required")
	}
	if err := memoryv1.ValidateMutationEnvelope(&checkpoint.StaleMutation); err != nil {
		return err
	}
	if err := memoryv1.ValidateMutationReceipt(&checkpoint.StaleReceipt); err != nil {
		return err
	}
	if checkpoint.StaleMutation.Binding.RoutingEpoch >= checkpoint.Binding.RoutingEpoch {
		return errors.New("stale mutation is not below the checkpoint routing fence")
	}
	if checkpoint.ProviderCommitGapMutation != nil {
		if err := memoryv1.ValidateMutationEnvelope(checkpoint.ProviderCommitGapMutation); err != nil {
			return err
		}
		if !memoryv1.BindingEqual(checkpoint.ProviderCommitGapMutation.Binding, checkpoint.Binding) {
			return errors.New("provider-commit gap mutation binding does not match checkpoint binding")
		}
	}
	return nil
}

func validateCheckpointCapabilities(checkpoint Checkpoint) error {
	capabilities := memoryv1.CapabilitiesResponse{
		ProtocolVersion: memoryv1.Version,
		Binding:         checkpoint.Binding,
		AdapterName:     "conformance-checkpoint",
		AdapterVersion:  "conformance-checkpoint",
		Revision:        checkpoint.CapabilitiesRevision,
		ExpiresAt:       time.Unix(1, 0).UTC(),
		Capabilities:    checkpoint.Capabilities,
		Limits:          checkpoint.CapabilityLimits,
	}
	if err := memoryv1.ValidateCapabilitiesResponse(&capabilities, time.Time{}); err != nil {
		return fmt.Errorf("checkpoint capabilities are invalid: %w", err)
	}
	return nil
}

func verifyRestartCapabilities(caps *memoryv1.CapabilitiesResponse, checkpoint Checkpoint) error {
	if caps == nil {
		return errors.New("capability probe returned no response")
	}
	if caps.Revision != checkpoint.CapabilitiesRevision {
		return errors.New("capability revision changed between prepare and verify")
	}
	if caps.Capabilities != checkpoint.Capabilities {
		return errors.New("capability flags changed between prepare and verify")
	}
	if caps.Limits != checkpoint.CapabilityLimits {
		return errors.New("capability limits changed between prepare and verify")
	}
	return nil
}

func validateCheckpointPagination(checkpoint Checkpoint) error {
	if err := memoryv1.ValidateSearchRequest(&checkpoint.PaginationRequest); err != nil {
		return err
	}
	if checkpoint.PaginationRequest.PageToken == "" ||
		!memoryv1.BindingEqual(checkpoint.PaginationRequest.Binding, checkpoint.Binding) {
		return errors.New("pagination continuation is not bound to the checkpoint")
	}
	if checkpoint.PaginationRequest.Mode != memoryv1.SearchModeKeyword ||
		checkpoint.PaginationActualMode != memoryv1.SearchModeKeyword {
		return errors.New("pagination proof must use explicit keyword mode")
	}
	if checkpoint.PaginationSnapshotExpiry.IsZero() {
		return errors.New("paginationSnapshotExpiry is required")
	}
	if checkpoint.PaginationRecordsBeforeContinuation < 0 ||
		checkpoint.PaginationRecordsBeforeContinuation > checkpoint.PaginationRequest.PageSize {
		return errors.New("pagination records before continuation are invalid")
	}
	if len(checkpoint.PaginationExpectedKeys) == 0 ||
		len(checkpoint.PaginationExpectedKeys) != len(checkpoint.PaginationExpectedDigests) ||
		checkpoint.PaginationRecordsBeforeContinuation+len(checkpoint.PaginationExpectedKeys) >
			checkpoint.CapabilityLimits.MaxSnapshotRecords {
		return errors.New("pagination record proofs are missing or inconsistent")
	}
	seenKeys := make(map[string]struct{}, len(checkpoint.PaginationExpectedKeys))
	for index, key := range checkpoint.PaginationExpectedKeys {
		request := memoryv1.GetRequest{ProtocolVersion: memoryv1.Version, Binding: checkpoint.Binding, UpsertKey: key}
		if err := memoryv1.ValidateGetRequest(&request); err != nil {
			return err
		}
		if _, duplicate := seenKeys[key]; duplicate {
			return errors.New("paginationExpectedKeys contains a duplicate")
		}
		seenKeys[key] = struct{}{}
		if !isCanonicalDigest(checkpoint.PaginationExpectedDigests[index]) {
			return errors.New("paginationExpectedDigests contains an invalid digest")
		}
	}
	return nil
}

func probe(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
) (*memoryv1.CapabilitiesResponse, error) {
	healthBody, status, err := client.do(ctx, http.MethodGet, memoryv1.PathHealth, client.authorizationValue, nil)
	if err != nil {
		return nil, fmt.Errorf("health probe failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("health probe returned HTTP %d", status)
	}
	if containsCredential(healthBody, client.authorizationValue) {
		return nil, errors.New("health response contained the bearer token")
	}
	if _, err := memoryv1.DecodeHealthResponse(healthBody); err != nil {
		return nil, err
	}
	body, status, err := client.postJSON(
		ctx,
		memoryv1.PathCapabilities,
		memoryv1.CapabilitiesRequest{ProtocolVersion: memoryv1.Version, Binding: binding},
	)
	if err != nil {
		return nil, fmt.Errorf("capability probe failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("capability probe returned HTTP %d", status)
	}
	if containsCredential(body, client.authorizationValue) {
		return nil, errors.New("capability response contained the bearer token")
	}
	caps, err := memoryv1.DecodeCapabilitiesResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.BindingEqual(caps.Binding, binding) {
		return nil, errors.New("capability response did not echo the exact binding")
	}
	client.configureLimits(caps.Limits)
	if err := client.validateResponseBytes(memoryv1.PathCapabilities, body); err != nil {
		return nil, err
	}
	if err := client.validateResponseBytes(memoryv1.PathHealth, healthBody); err != nil {
		return nil, err
	}
	return caps, nil
}

type errorExpectation struct {
	status            int
	code              string
	binding           *memoryv1.Binding
	retryable         bool
	retryAfterSeconds int
}

func validateErrorVariant(response contractResponse, expected errorExpectation) error {
	if response.StatusCode != expected.status {
		return fmt.Errorf("HTTP status = %d, want %d", response.StatusCode, expected.status)
	}
	decoded, err := memoryv1.DecodeErrorResponse(response.Body)
	if err != nil {
		return fmt.Errorf("response is not a closed bounded error: %w", err)
	}
	if decoded.Code != expected.code || decoded.Retryable != expected.retryable ||
		decoded.RetryAfterSeconds != expected.retryAfterSeconds {
		return errors.New("response returned the wrong error code or retry semantics")
	}
	if expected.binding == nil {
		if decoded.Binding != nil {
			return errors.New("response unexpectedly included a binding")
		}
	} else if decoded.Binding == nil || !memoryv1.BindingEqual(*decoded.Binding, *expected.binding) {
		return errors.New("response did not echo the expected binding")
	}
	if _, present := response.Header[http.CanonicalHeaderKey("Retry-After")]; !expected.retryable && present {
		return errors.New("non-retryable response included Retry-After")
	}
	return nil
}

func verifyAuthentication(
	ctx context.Context,
	client *contractClient,
	storeBinding memoryv1.StoreResolutionBinding,
	storeName string,
	binding memoryv1.Binding,
	fixtures fixturePlan,
) error {
	mutationRequest, err := fixtures.mutation(
		binding,
		fixtureRoleAuthentication,
		fixtures.memoryID(fixtureRoleAuthentication),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state("k"),
	)
	if err != nil {
		return err
	}
	searchRequest, err := fixtures.searchRequest(binding, memoryv1.SearchModeKeyword)
	if err != nil {
		return err
	}
	probes := []struct {
		name, method, path string
		value              any
		body               []byte
	}{
		{name: "health", method: http.MethodGet, path: memoryv1.PathHealth},
		{
			name: "store resolution", method: http.MethodPost, path: memoryv1.PathStoreResolve,
			value: memoryv1.StoreResolveRequest{
				ProtocolVersion: memoryv1.Version,
				Binding:         storeBinding,
				StoreName:       storeName,
			},
		},
		{
			name: "capabilities", method: http.MethodPost, path: memoryv1.PathCapabilities,
			value: memoryv1.CapabilitiesRequest{
				ProtocolVersion: memoryv1.Version,
				Binding:         binding,
			},
		},
		{
			name: "ownership claim", method: http.MethodPost, path: memoryv1.PathOwnershipClaim,
			value: memoryv1.OwnershipClaimRequest{
				ProtocolVersion: memoryv1.Version,
				Binding:         binding,
			},
		},
		{
			name: "routing fence", method: http.MethodPost, path: memoryv1.PathRoutingFence,
			value: memoryv1.RoutingFenceRequest{
				ProtocolVersion: memoryv1.Version,
				Binding:         binding,
			},
		},
		{name: "mutation", method: http.MethodPost, path: memoryv1.PathMutations, value: mutationRequest},
		{
			name: "exact get", method: http.MethodPost, path: memoryv1.PathRecordsGet,
			value: memoryv1.GetRequest{
				ProtocolVersion: memoryv1.Version,
				Binding:         binding,
				UpsertKey:       mutationRequest.UpsertKey,
			},
		},
		{
			name: "operation lookup", method: http.MethodPost, path: memoryv1.PathOperationsGet,
			value: memoryv1.OperationLookupRequest{
				ProtocolVersion: memoryv1.Version,
				Binding:         binding,
				OperationID:     mutationRequest.OperationID,
			},
		},
		{name: "search", method: http.MethodPost, path: memoryv1.PathSearch, value: searchRequest},
	}
	for index := range probes {
		if probes[index].value == nil {
			continue
		}
		body, marshalErr := client.marshalJSONRequest("authentication "+probes[index].name, probes[index].value)
		if marshalErr != nil {
			return marshalErr
		}
		probes[index].body = body
	}
	for _, probe := range probes {
		for _, token := range []string{"", client.authorizationValue + "-invalid"} {
			response, requestErr := client.doResponse(ctx, probe.method, probe.path, token, probe.body, nil)
			if requestErr != nil {
				return fmt.Errorf("%s authentication probe failed: %w", probe.name, requestErr)
			}
			if containsCredential(response.Body, client.authorizationValue) {
				return fmt.Errorf("%s authentication response contained the bearer token", probe.name)
			}
			if err := validateErrorVariant(response, errorExpectation{
				status: http.StatusUnauthorized, code: memoryv1.ErrorCodeUnauthorized,
				retryable: false, retryAfterSeconds: 0,
			}); err != nil {
				return fmt.Errorf("%s authentication error response is not the exact unauthorized variant: %w", probe.name, err)
			}
		}
	}
	return nil
}

func verifyStrictCapabilityCodec(ctx context.Context, client *contractClient, binding memoryv1.Binding) error {
	valid, err := client.marshalJSONRequest(
		"capabilities codec baseline",
		memoryv1.CapabilitiesRequest{ProtocolVersion: memoryv1.Version, Binding: binding},
	)
	if err != nil {
		return err
	}
	unknown := append([]byte(`{"x":0,`), valid[1:]...)
	duplicate := append([]byte(`{"protocolVersion":"orka.memory.v1",`), valid[1:]...)
	trailing := append(append([]byte(nil), valid...), []byte(` 0`)...)
	unsafeRequest := memoryv1.CapabilitiesRequest{ProtocolVersion: memoryv1.Version, Binding: binding}
	unsafeRequest.Binding.ClusterID = "unsafe\ncluster"
	unsafeBody, err := client.marshalJSONRequest("unsafe capabilities codec", unsafeRequest)
	if err != nil {
		return err
	}
	oversized := append(
		append([]byte(nil), valid...),
		bytes.Repeat([]byte(" "), memoryv1.MaxHTTPBodyBytes+1-len(valid))...,
	)
	probes := []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "unknown field", status: http.StatusBadRequest, body: unknown},
		{name: "duplicate field", status: http.StatusBadRequest, body: duplicate},
		{name: "trailing JSON", status: http.StatusBadRequest, body: trailing},
		{name: "unsafe identity", status: http.StatusBadRequest, body: unsafeBody},
		{name: "oversized body", status: http.StatusRequestEntityTooLarge, body: oversized},
	}
	for _, probe := range probes {
		if probe.name != "oversized body" {
			if err := client.validateRequestBytes("capabilities "+probe.name, probe.body); err != nil {
				return err
			}
		}
		if err := expectCodecRejection(
			ctx, client, memoryv1.PathCapabilities, probe.name, probe.status, probe.body,
		); err != nil {
			return err
		}
	}
	return nil
}

func verifyStrictMutationCodec(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
	fixtures fixturePlan,
) error {
	request, err := fixtures.mutation(
		binding,
		fixtureRoleStrictCodec,
		fixtures.memoryID(fixtureRoleStrictCodec),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state("g"),
	)
	if err != nil {
		return err
	}
	valid, err := client.marshalJSONRequest("mutation codec baseline", request)
	if err != nil {
		return err
	}
	unknown := append([]byte(`{"x":0,`), valid[1:]...)
	trailing := append(append([]byte(nil), valid...), []byte(` 0`)...)
	for name, body := range map[string][]byte{"mutation unknown field": unknown, "mutation trailing JSON": trailing} {
		if err := client.validateRequestBytes(name, body); err != nil {
			return err
		}
		if err := expectCodecRejection(ctx, client, memoryv1.PathMutations, name, http.StatusBadRequest, body); err != nil {
			return err
		}
	}
	if record, err := getRecord(ctx, client, binding, request.UpsertKey); err != nil || record != nil {
		return errors.New("malformed mutation changed provider content")
	}
	return nil
}

func expectCodecRejection(
	ctx context.Context,
	client *contractClient,
	path string,
	name string,
	expectedStatus int,
	body []byte,
) error {
	response, err := client.doResponse(ctx, http.MethodPost, path, client.authorizationValue, body, nil)
	if err != nil {
		return fmt.Errorf("%s probe failed: %w", name, err)
	}
	if containsCredential(response.Body, client.authorizationValue) {
		return fmt.Errorf("%s response contained the bearer token", name)
	}
	if err := validateErrorVariant(response, errorExpectation{
		status: expectedStatus, code: memoryv1.ErrorCodeInvalidRequest,
		retryable: false, retryAfterSeconds: 0,
	}); err != nil {
		return fmt.Errorf("%s response is not the exact invalid-request variant: %w", name, err)
	}
	return nil
}

func resolveStore(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.StoreResolutionBinding,
	storeName string,
) (*memoryv1.StoreResolveResponse, error) {
	body, status, err := client.postJSON(ctx, memoryv1.PathStoreResolve, memoryv1.StoreResolveRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding, StoreName: storeName,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store resolution returned HTTP %d", status)
	}
	response, err := memoryv1.DecodeStoreResolveResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.StoreResolutionBindingEqual(response.Binding, binding) || response.StoreName != storeName {
		return nil, errors.New("store resolution did not echo the exact pre-authority identity and store name")
	}
	return response, nil
}

func verifyStrictStoreCodec(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.StoreResolutionBinding,
	storeName string,
) error {
	valid, err := client.marshalJSONRequest("store codec baseline", memoryv1.StoreResolveRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding, StoreName: storeName,
	})
	if err != nil {
		return err
	}
	unknown := append([]byte(`{"x":0,`), valid[1:]...)
	trailing := append(append([]byte(nil), valid...), []byte(` 0`)...)
	for name, body := range map[string][]byte{"store unknown field": unknown, "store trailing JSON": trailing} {
		if err := client.validateRequestBytes(name, body); err != nil {
			return err
		}
		if err := expectCodecRejection(
			ctx, client, memoryv1.PathStoreResolve, name, http.StatusBadRequest, body,
		); err != nil {
			return err
		}
	}
	return nil
}

func claimOwnership(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
) (*memoryv1.OwnershipClaimResponse, error) {
	body, status, err := client.postJSON(
		ctx,
		memoryv1.PathOwnershipClaim,
		memoryv1.OwnershipClaimRequest{ProtocolVersion: memoryv1.Version, Binding: binding},
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusConflict {
		return nil, fmt.Errorf("ownership claim returned HTTP %d", status)
	}
	response, err := memoryv1.DecodeOwnershipClaimResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.BindingEqual(response.Binding, binding) {
		return nil, errors.New("ownership response did not echo the exact binding")
	}
	return response, nil
}

func verifyExclusiveOwnership(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
	conflictBackendUID string,
) error {
	conflict := binding
	conflict.BackendUID = conflictBackendUID
	if conflict.BackendUID == binding.BackendUID {
		conflict.BackendUID = "conflict-owner"
	}
	response, err := claimOwnership(ctx, client, conflict)
	if err != nil {
		return err
	}
	if response.Result != memoryv1.ResultIdentityConflict {
		return errors.New("adapter allowed a second writer claim for the same authority scope")
	}
	return nil
}

func induceProviderCommitGap(ctx context.Context, client *contractClient, request memoryv1.MutationEnvelope) error {
	if client.limitsConfigured {
		if err := validateFixtureMutation(client.limits, &request); err != nil {
			return err
		}
	}
	body, err := client.marshalJSONRequest("provider-commit gap mutation", request)
	if err != nil {
		return err
	}
	response, err := client.doResponse(
		ctx, http.MethodPost, memoryv1.PathMutations, client.authorizationValue, body,
		http.Header{conformanceFailpointHeader: []string{conformanceProviderCommitGapValue}},
	)
	if err != nil {
		return fmt.Errorf("provider-commit gap failpoint request failed: %w", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable ||
		response.Header.Get(conformanceFailpointHeader) != conformanceProviderCommitGapValue {
		return errors.New("adapter did not acknowledge the opt-in provider-commit gap failpoint")
	}
	failure, err := memoryv1.DecodeErrorResponse(response.Body)
	if err != nil || failure.Code != memoryv1.ErrorCodeInternal || !failure.Retryable || failure.Binding == nil ||
		!memoryv1.BindingEqual(*failure.Binding, request.Binding) {
		return errors.New("provider-commit gap failpoint did not return the closed retryable error variant")
	}
	record, err := getRecord(ctx, client, request.Binding, request.UpsertKey)
	if err != nil {
		return err
	}
	if record != nil {
		return errors.New("provider-commit gap was materialized locally before restart recovery")
	}
	return nil
}

func verifyConcurrentCAS(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
	fixtures fixturePlan,
) error {
	base, err := fixtures.mutation(
		binding,
		fixtureRoleCASBase,
		fixtures.memoryID(fixtureRoleCASBase),
		memoryv1.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state("d"),
	)
	if err != nil {
		return err
	}
	baseReceipt, _, err := mutation(ctx, client, base, memoryv1.ResultApplied)
	if err != nil {
		return err
	}
	replacements := make([]memoryv1.MutationEnvelope, 2)
	for index, replacement := range []struct {
		role    string
		content string
	}{
		{role: fixtureRoleCASLeft, content: "e"},
		{role: fixtureRoleCASRight, content: "f"},
	} {
		replacements[index], err = fixtures.mutation(
			binding,
			replacement.role,
			base.MemoryID,
			memoryv1.MutationKindReplace,
			2,
			1,
			baseReceipt.BackendVersion,
			fixtures.state(replacement.content),
		)
		if err != nil {
			return err
		}
	}
	type response struct {
		receipt memoryv1.MutationReceipt
		err     error
	}
	start := make(chan struct{})
	results := make(chan response, len(replacements))
	for _, replacement := range replacements {
		go func() {
			<-start
			body, status, requestErr := client.postJSON(ctx, memoryv1.PathMutations, replacement)
			if requestErr != nil {
				results <- response{err: requestErr}
				return
			}
			receipt, decodeErr := memoryv1.DecodeMutationReceipt(body)
			if decodeErr != nil {
				results <- response{err: decodeErr}
				return
			}
			if !memoryv1.BindingEqual(receipt.Binding, replacement.Binding) || receipt.OperationID != replacement.OperationID ||
				receipt.MutationDigest != replacement.MutationDigest || status != resultHTTPStatus(receipt.Result) {
				results <- response{err: errors.New("concurrent CAS receipt did not match its request")}
				return
			}
			results <- response{receipt: *receipt}
		}()
	}
	close(start)
	counts := map[string]int{}
	for range replacements {
		result := <-results
		if result.err != nil {
			return fmt.Errorf("concurrent CAS request failed: %w", result.err)
		}
		counts[result.receipt.Result]++
	}
	if counts[memoryv1.ResultApplied] != 1 || counts[memoryv1.ResultPreconditionFailed] != 1 || len(counts) != 2 {
		return fmt.Errorf(
			"concurrent same-generation replacements produced results %#v, want one applied and one preconditionFailed",
			counts,
		)
	}
	record, err := getRecord(ctx, client, binding, base.UpsertKey)
	if err != nil || record == nil || record.Generation != 2 ||
		(record.Content != replacements[0].State.Content && record.Content != replacements[1].State.Content) {
		return errors.New("concurrent CAS did not materialize exactly one generation-2 replacement")
	}
	return nil
}

func advanceFence(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
) (*memoryv1.RoutingFenceResponse, error) {
	body, status, err := client.postJSON(
		ctx,
		memoryv1.PathRoutingFence,
		memoryv1.RoutingFenceRequest{ProtocolVersion: memoryv1.Version, Binding: binding},
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusConflict {
		return nil, fmt.Errorf("routing fence returned HTTP %d", status)
	}
	response, err := memoryv1.DecodeRoutingFenceResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.BindingEqual(response.Binding, binding) {
		return nil, errors.New("routing fence response did not echo the exact binding")
	}
	return response, nil
}

func mutation(
	ctx context.Context,
	client *contractClient,
	request memoryv1.MutationEnvelope,
	expectedResult string,
) (memoryv1.MutationReceipt, []byte, error) {
	if client.limitsConfigured {
		if err := validateFixtureMutation(client.limits, &request); err != nil {
			return memoryv1.MutationReceipt{}, nil, err
		}
	}
	body, status, err := client.postJSON(ctx, memoryv1.PathMutations, request)
	if err != nil {
		return memoryv1.MutationReceipt{}, nil, err
	}
	if containsCredential(body, client.authorizationValue) {
		return memoryv1.MutationReceipt{}, nil, errors.New("mutation response contained the bearer token")
	}
	receipt, err := memoryv1.DecodeMutationReceipt(body)
	if err != nil {
		return memoryv1.MutationReceipt{}, nil, err
	}
	if receipt.Result != expectedResult {
		return memoryv1.MutationReceipt{}, nil, fmt.Errorf(
			"mutation %s result = %s, want %s",
			request.OperationID,
			receipt.Result,
			expectedResult,
		)
	}
	if !memoryv1.BindingEqual(receipt.Binding, request.Binding) ||
		receipt.OperationID != request.OperationID ||
		receipt.MutationDigest != request.MutationDigest {
		return memoryv1.MutationReceipt{}, nil, errors.New("mutation receipt did not echo the request identity")
	}
	wantStatus := resultHTTPStatus(expectedResult)
	if status != wantStatus {
		return memoryv1.MutationReceipt{}, nil, fmt.Errorf(
			"mutation result %s returned HTTP %d, want %d",
			expectedResult,
			status,
			wantStatus,
		)
	}
	return *receipt, body, nil
}

func getRecord(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
	upsertKey string,
) (*memoryv1.MemoryRecord, error) {
	body, status, err := client.postJSON(
		ctx,
		memoryv1.PathRecordsGet,
		memoryv1.GetRequest{
			ProtocolVersion: memoryv1.Version,
			Binding:         binding,
			UpsertKey:       upsertKey,
		},
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("exact get returned HTTP %d", status)
	}
	response, err := memoryv1.DecodeGetResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.BindingEqual(response.Binding, binding) {
		return nil, errors.New("exact get did not echo the binding")
	}
	return response.Record, nil
}

func lookupOperation(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
	operationID string,
) (*memoryv1.MutationReceipt, error) {
	body, status, err := client.postJSON(ctx, memoryv1.PathOperationsGet, memoryv1.OperationLookupRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding, OperationID: operationID,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("operation lookup returned HTTP %d", status)
	}
	response, err := memoryv1.DecodeOperationLookupResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.BindingEqual(response.Binding, binding) {
		return nil, errors.New("operation lookup did not echo the binding")
	}
	return response.Receipt, nil
}

func search(
	ctx context.Context,
	client *contractClient,
	request memoryv1.SearchRequest,
) (*memoryv1.SearchResponse, error) {
	if client.limitsConfigured {
		if err := validateFixtureSearch(client.limits, &request); err != nil {
			return nil, fmt.Errorf("search fixture exceeds advertised limits: %w", err)
		}
	}
	body, status, err := client.postJSON(ctx, memoryv1.PathSearch, request)
	receivedAt := time.Now().UTC()
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("search returned HTTP %d", status)
	}
	response, err := memoryv1.DecodeSearchResponse(body)
	if err != nil {
		return nil, err
	}
	if !memoryv1.BindingEqual(response.Binding, request.Binding) {
		return nil, errors.New("search did not echo the binding")
	}
	if response.RequestedMode != request.Mode {
		return nil, errors.New("search did not echo the requested mode")
	}
	if request.Mode != memoryv1.SearchModeAuto && response.ActualMode != request.Mode {
		return nil, errors.New("search changed an explicit requested mode")
	}
	if client.limitsConfigured && request.PageToken == "" {
		maximumExpiry := receivedAt.Add(time.Duration(client.limits.SnapshotTTLSeconds) * time.Second)
		if response.SnapshotExpiresAt.After(maximumExpiry) {
			return nil, errors.New("search snapshot expiry exceeds the advertised TTL")
		}
	}
	if len(response.Records) > request.PageSize {
		return nil, fmt.Errorf(
			"search returned %d records for requested pageSize %d",
			len(response.Records),
			request.PageSize,
		)
	}
	if request.PageToken != "" && !response.Exhausted && !nextPageTokenFollows(request.PageToken, response.NextPageToken) {
		return nil, errors.New("search continuation token did not advance within the same snapshot")
	}
	return response, nil
}

func nextPageTokenFollows(current, next string) bool {
	currentParts := strings.Split(current, ".")
	nextParts := strings.Split(next, ".")
	if len(currentParts) != 3 ||
		len(nextParts) != 3 ||
		currentParts[0] != nextParts[0] ||
		currentParts[1] != nextParts[1] {
		return false
	}
	currentOffset, currentErr := strconv.Atoi(currentParts[2])
	nextOffset, nextErr := strconv.Atoi(nextParts[2])
	return currentErr == nil && nextErr == nil && nextOffset > currentOffset
}

func verifySearchModeCapabilities(
	ctx context.Context,
	client *contractClient,
	binding memoryv1.Binding,
	capabilities memoryv1.Capabilities,
	fixtures fixturePlan,
) error {
	autoRequest, err := fixtures.searchRequest(binding, memoryv1.SearchModeAuto)
	if err != nil {
		return err
	}
	auto, err := consumeSearchProbe(ctx, client, autoRequest)
	if err != nil {
		return fmt.Errorf("auto search failed: %w", err)
	}
	if !searchModeAdvertised(capabilities, auto.ActualMode) {
		return fmt.Errorf("auto search selected unadvertised mode %q", auto.ActualMode)
	}

	for _, probe := range []struct {
		mode       string
		advertised bool
	}{
		{mode: memoryv1.SearchModeSemantic, advertised: capabilities.SemanticSearch},
		{mode: memoryv1.SearchModeHybrid, advertised: capabilities.HybridSearch},
	} {
		request, requestErr := fixtures.searchRequest(binding, probe.mode)
		if requestErr != nil {
			return requestErr
		}
		if probe.advertised {
			if _, err := consumeSearchProbe(ctx, client, request); err != nil {
				return fmt.Errorf("advertised explicit %s search failed: %w", probe.mode, err)
			}
			continue
		}

		body, err := client.marshalJSONRequest("explicit "+probe.mode+" search", request)
		if err != nil {
			return err
		}
		response, err := client.doResponse(ctx, http.MethodPost, memoryv1.PathSearch, client.authorizationValue, body, nil)
		if err != nil {
			return err
		}
		if err := validateErrorVariant(response, errorExpectation{
			status: http.StatusUnprocessableEntity, code: memoryv1.ErrorCodeSearchModeUnsupported,
			binding: &binding, retryable: false, retryAfterSeconds: 0,
		}); err != nil {
			return fmt.Errorf("unadvertised explicit %s search returned the wrong error variant: %w", probe.mode, err)
		}
	}
	return nil
}

func consumeSearchProbe(
	ctx context.Context,
	client *contractClient,
	request memoryv1.SearchRequest,
) (*memoryv1.SearchResponse, error) {
	first, err := search(ctx, client, request)
	if err != nil {
		return nil, err
	}
	if _, err := collectSnapshot(ctx, client, request, first); err != nil {
		return nil, err
	}
	return first, nil
}

func searchModeAdvertised(capabilities memoryv1.Capabilities, mode string) bool {
	switch mode {
	case memoryv1.SearchModeKeyword:
		return capabilities.KeywordSearch
	case memoryv1.SearchModeSemantic:
		return capabilities.SemanticSearch
	case memoryv1.SearchModeHybrid:
		return capabilities.HybridSearch
	default:
		return false
	}
}

func snapshotRecordLimit(client *contractClient) int {
	if client != nil && client.limitsConfigured {
		return client.limits.MaxSnapshotRecords
	}
	return memoryv1.MaxSnapshotRecords
}

func ensureRestartContinuationUnexpired(expiresAt, now time.Time) error {
	if expiresAt.IsZero() || !now.Before(expiresAt) {
		return errors.New(
			"restart pagination checkpoint expired before continuation verification; " +
				"rerun prepare and verify within the adapter's advertised snapshot TTL",
		)
	}
	return nil
}

func collectSnapshot(
	ctx context.Context,
	client *contractClient,
	request memoryv1.SearchRequest,
	first *memoryv1.SearchResponse,
) ([]memoryv1.MemoryRecord, error) {
	result := make([]memoryv1.MemoryRecord, 0, len(first.Records))
	seenRecords := make(map[string]struct{}, len(first.Records))
	maxRecords := snapshotRecordLimit(client)
	if err := appendUniqueSnapshotRecords(&result, seenRecords, first.Records, 0, maxRecords); err != nil {
		return nil, err
	}
	response := first
	seenTokens := map[string]struct{}{}
	for pages := 0; !response.Exhausted; pages++ {
		if pages > memoryv1.MaxSnapshotRecords {
			return nil, errors.New("pagination did not exhaust within the profile bound")
		}
		if _, duplicate := seenTokens[response.NextPageToken]; duplicate {
			return nil, errors.New("pagination repeated a continuation token")
		}
		seenTokens[response.NextPageToken] = struct{}{}
		request.PageToken = response.NextPageToken
		next, err := search(ctx, client, request)
		if err != nil {
			return nil, err
		}
		if next.ActualMode != first.ActualMode || !next.SnapshotExpiresAt.Equal(first.SnapshotExpiresAt) {
			return nil, errors.New("pagination continuation changed snapshot mode or expiry")
		}
		if err := appendUniqueSnapshotRecords(&result, seenRecords, next.Records, 0, maxRecords); err != nil {
			return nil, err
		}
		response = next
	}
	return result, nil
}

type snapshotContinuation struct {
	Records           []memoryv1.MemoryRecord
	ActualMode        string
	SnapshotExpiresAt time.Time
}

func collectContinuation(
	ctx context.Context,
	client *contractClient,
	request memoryv1.SearchRequest,
	expiresAt time.Time,
	recordsBeforeContinuation int,
) (snapshotContinuation, error) {
	result := make([]memoryv1.MemoryRecord, 0)
	seenTokens := map[string]struct{}{request.PageToken: {}}
	seenRecords := make(map[string]struct{})
	maxRecords := snapshotRecordLimit(client)
	if recordsBeforeContinuation < 0 || recordsBeforeContinuation > maxRecords {
		return snapshotContinuation{}, errors.New("pagination continuation record offset exceeds maxSnapshotRecords")
	}
	var snapshotExpiry time.Time
	var actualMode string
	continuationCtx, cancel := context.WithDeadline(ctx, expiresAt)
	defer cancel()
	for pages := 0; ; pages++ {
		if pages > memoryv1.MaxSnapshotRecords {
			return snapshotContinuation{}, errors.New("pagination continuation did not exhaust")
		}
		if err := ensureRestartContinuationUnexpired(expiresAt, time.Now().UTC()); err != nil {
			return snapshotContinuation{}, err
		}
		response, err := search(continuationCtx, client, request)
		if err != nil {
			if expiryErr := ensureRestartContinuationUnexpired(expiresAt, time.Now().UTC()); expiryErr != nil {
				return snapshotContinuation{}, expiryErr
			}
			return snapshotContinuation{}, err
		}
		if err := ensureRestartContinuationUnexpired(expiresAt, time.Now().UTC()); err != nil {
			return snapshotContinuation{}, err
		}
		if snapshotExpiry.IsZero() {
			snapshotExpiry, actualMode = response.SnapshotExpiresAt, response.ActualMode
		} else if !response.SnapshotExpiresAt.Equal(snapshotExpiry) || response.ActualMode != actualMode {
			return snapshotContinuation{}, errors.New("pagination continuation changed snapshot mode or expiry")
		}
		if err := appendUniqueSnapshotRecords(
			&result, seenRecords, response.Records, recordsBeforeContinuation, maxRecords,
		); err != nil {
			return snapshotContinuation{}, err
		}
		if response.Exhausted {
			if response.NextPageToken != "" {
				return snapshotContinuation{}, errors.New("exhausted response included a next page token")
			}
			return snapshotContinuation{
				Records: result, ActualMode: actualMode, SnapshotExpiresAt: snapshotExpiry,
			}, nil
		}
		if _, duplicate := seenTokens[response.NextPageToken]; duplicate {
			return snapshotContinuation{}, errors.New("pagination repeated a continuation token")
		}
		seenTokens[response.NextPageToken] = struct{}{}
		request.PageToken = response.NextPageToken
	}
}

func appendUniqueSnapshotRecords(
	destination *[]memoryv1.MemoryRecord,
	seen map[string]struct{},
	records []memoryv1.MemoryRecord,
	recordsBefore int,
	maxRecords int,
) error {
	for _, record := range records {
		if recordsBefore+len(*destination) >= maxRecords {
			return fmt.Errorf("search snapshot exceeded advertised maxSnapshotRecords=%d", maxRecords)
		}
		if _, duplicate := seen[record.UpsertKey]; duplicate {
			return errors.New("pagination repeated a memory record")
		}
		seen[record.UpsertKey] = struct{}{}
		*destination = append(*destination, record)
	}
	return nil
}

func makeMutation(
	binding memoryv1.Binding,
	operationID string,
	memoryID string,
	kind string,
	generation uint64,
	expectedGeneration uint64,
	expectedVersion string,
	state *memoryv1.MutationState,
) (memoryv1.MutationEnvelope, error) {
	request := memoryv1.MutationEnvelope{
		ProtocolVersion: memoryv1.Version, OperationID: operationID, Binding: binding,
		MemoryID: memoryID, Kind: kind, Generation: generation, ExpectedGeneration: expectedGeneration,
		ExpectedBackendVersion: expectedVersion, State: state,
	}
	if err := memoryv1.PrepareMutation(&request); err != nil {
		return memoryv1.MutationEnvelope{}, err
	}
	return request, nil
}

func resultHTTPStatus(result string) int {
	switch result {
	case memoryv1.ResultApplied, memoryv1.ResultNotFound:
		return http.StatusOK
	case memoryv1.ResultPreconditionFailed, memoryv1.ResultIdempotencyConflict, memoryv1.ResultIdentityConflict:
		return http.StatusConflict
	case memoryv1.ResultRetryableError:
		return http.StatusServiceUnavailable
	case memoryv1.ResultNonRetryableError:
		return http.StatusUnprocessableEntity
	default:
		return 0
	}
}

func recordProofs(binding memoryv1.Binding, records []memoryv1.MemoryRecord) ([]string, []string, error) {
	keys := make([]string, 0, len(records))
	digests := make([]string, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for index := range records {
		record := &records[index]
		if _, duplicate := seen[record.UpsertKey]; duplicate {
			return nil, nil, errors.New("record proof contains a duplicate upsert key")
		}
		digest, err := memoryv1.MemoryRecordDigest(record, binding)
		if err != nil {
			return nil, nil, err
		}
		seen[record.UpsertKey] = struct{}{}
		keys = append(keys, record.UpsertKey)
		digests = append(digests, digest)
	}
	return keys, digests, nil
}

func recordDigestByKey(binding memoryv1.Binding, records []memoryv1.MemoryRecord) (map[string]string, error) {
	keys, digests, err := recordProofs(binding, records)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(keys))
	for index := range keys {
		result[keys[index]] = digests[index]
	}
	return result, nil
}

func verifySnapshotFixture(
	binding memoryv1.Binding,
	records []memoryv1.MemoryRecord,
	expectedDigests map[string]string,
) error {
	actualDigests, err := recordDigestByKey(binding, records)
	if err != nil {
		return err
	}
	if len(actualDigests) != len(expectedDigests) {
		return fmt.Errorf("keyword snapshot returned %d unique records, want %d", len(actualDigests), len(expectedDigests))
	}
	for key, expected := range expectedDigests {
		actual, exists := actualDigests[key]
		if !exists {
			return errors.New("keyword snapshot omitted an exact-get fixture")
		}
		if actual != expected {
			return errors.New("keyword snapshot record diverged from exact get")
		}
	}
	return nil
}

func verifyContinuationProof(
	binding memoryv1.Binding,
	continuation snapshotContinuation,
	checkpoint Checkpoint,
) error {
	if continuation.ActualMode != checkpoint.PaginationActualMode ||
		!continuation.SnapshotExpiresAt.Equal(checkpoint.PaginationSnapshotExpiry) {
		return errors.New("durable snapshot mode or expiry changed across restart")
	}
	keys, digests, err := recordProofs(binding, continuation.Records)
	if err != nil {
		return fmt.Errorf("durable snapshot continuation is invalid: %w", err)
	}
	if !slices.Equal(keys, checkpoint.PaginationExpectedKeys) {
		return fmt.Errorf("durable snapshot continuation keys = %v, want %v", keys, checkpoint.PaginationExpectedKeys)
	}
	if !slices.Equal(digests, checkpoint.PaginationExpectedDigests) {
		return errors.New("durable snapshot continuation records changed across restart")
	}
	return nil
}
func isCanonicalDigest(value string) bool {
	const prefix = "sha256:"
	encoded, ok := strings.CutPrefix(value, prefix)
	if !ok || len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == 32
}

func cloneState(state *memoryv1.MutationState) *memoryv1.MutationState {
	if state == nil {
		return nil
	}
	return &memoryv1.MutationState{
		Content:  state.Content,
		Tags:     append([]string(nil), state.Tags...),
		Metadata: cloneMap(state.Metadata),
	}
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	maps.Copy(result, input)
	return result
}

func randomRunID() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func isSafeRunID(value string) bool {
	if value == "" || len(value) > 48 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// SanitizeCheckResult returns an output-safe result copy.
func SanitizeCheckResult(result CheckResult, authorizationValue string) CheckResult {
	result.Message = sanitizeOutputText(result.Message, authorizationValue, conformanceMessageLimit)
	if result.Capabilities != nil {
		copy := *result.Capabilities
		copy.AdapterName = sanitizeOutputText(copy.AdapterName, authorizationValue, memoryv1.MaxIdentityBytes)
		copy.AdapterVersion = sanitizeOutputText(copy.AdapterVersion, authorizationValue, memoryv1.MaxIdentityBytes)
		copy.Revision = sanitizeOutputText(copy.Revision, authorizationValue, memoryv1.MaxIdentityBytes)
		result.Capabilities = &copy
	}
	return result
}
