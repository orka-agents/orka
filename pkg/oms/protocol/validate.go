/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	identityPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	operationIDPattern = regexp.MustCompile(`^mop-[A-Za-z0-9][A-Za-z0-9._-]*$`)
	memoryIDPattern    = regexp.MustCompile(`^mem-[A-Za-z0-9][A-Za-z0-9._-]*$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	metadataKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)
	pageTokenPattern   = regexp.MustCompile(`^oms-page-v1\.[a-f0-9]{32}\.[0-9]+$`)
	storeNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	storeUUIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// ValidateBinding enforces safe identity syntax, positive bounded epochs, and
// the deterministic tenant derivation.
func ValidateBinding(binding Binding) error {
	for name, value := range map[string]string{
		"clusterId": binding.ClusterID, "namespaceUid": binding.NamespaceUID,
		"backendUid": binding.BackendUID, "storeUuid": binding.StoreUUID,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if !storeUUIDPattern.MatchString(binding.StoreUUID) {
		return errors.New("storeUuid is not a canonical UUID")
	}
	if binding.AuthorityEpoch == 0 || binding.AuthorityEpoch > math.MaxInt64 {
		return errors.New("authorityEpoch must be a positive signed 64-bit value")
	}
	if binding.RoutingEpoch == 0 || binding.RoutingEpoch > math.MaxInt64 {
		return errors.New("routingEpoch must be a positive signed 64-bit value")
	}
	if binding.TenantID != DeriveTenantID(binding.ClusterID, binding.NamespaceUID) {
		return errors.New("tenantId does not match the canonical cluster/namespace derivation")
	}
	return nil
}

// ValidateStoreResolutionBinding validates the pre-authority identity used
// before a stable store UUID and authority epochs exist.
func ValidateStoreResolutionBinding(binding StoreResolutionBinding) error {
	for name, value := range map[string]string{
		"clusterId": binding.ClusterID, "namespaceUid": binding.NamespaceUID, "backendUid": binding.BackendUID,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if binding.TenantID != DeriveTenantID(binding.ClusterID, binding.NamespaceUID) {
		return errors.New("tenantId does not match the canonical cluster/namespace derivation")
	}
	return nil
}

// StoreResolutionBindingEqual compares the exact pre-authority identity echo.
func StoreResolutionBindingEqual(left, right StoreResolutionBinding) bool {
	return left == right
}

func ValidateStoreResolveRequest(request *StoreResolveRequest) error {
	if request == nil {
		return errors.New("store resolve request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	if err := ValidateStoreResolutionBinding(request.Binding); err != nil {
		return err
	}
	if !storeNamePattern.MatchString(request.StoreName) {
		return errors.New("storeName is invalid")
	}
	return nil
}

func ValidateStoreResolveResponse(response *StoreResolveResponse) error {
	if response == nil {
		return errors.New("store resolve response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateStoreResolutionBinding(response.Binding); err != nil {
		return err
	}
	if !storeNamePattern.MatchString(response.StoreName) {
		return errors.New("storeName is invalid")
	}
	if !storeUUIDPattern.MatchString(response.StoreUUID) {
		return errors.New("storeUuid is not a canonical UUID")
	}
	return nil
}

// BindingEqual compares the complete echoed binding.
func BindingEqual(left, right Binding) bool {
	return left == right
}

// AuthorityEqual compares a binding while ignoring its routing epoch.
func AuthorityEqual(left, right Binding) bool {
	left.RoutingEpoch = 0
	right.RoutingEpoch = 0
	return left == right
}

func ValidateCapabilitiesRequest(request *CapabilitiesRequest) error {
	if request == nil {
		return errors.New("capabilities request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	return ValidateBinding(request.Binding)
}

func ValidateCapabilitiesResponse(response *CapabilitiesResponse, now time.Time) error {
	if response == nil {
		return errors.New("capabilities response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateBinding(response.Binding); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"adapterName": response.AdapterName, "adapterVersion": response.AdapterVersion,
		"revision": response.Revision,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if response.ExpiresAt.IsZero() || (!now.IsZero() && !response.ExpiresAt.After(now)) {
		return errors.New("expiresAt must be in the future")
	}
	if err := validateRequiredCapabilities(response.Capabilities); err != nil {
		return err
	}
	return validateCapabilityLimits(response.Limits)
}

func validateRequiredCapabilities(capabilities Capabilities) error {
	required := map[string]bool{
		"durableIdempotency":         capabilities.DurableIdempotency,
		"idempotencyDigestConflicts": capabilities.IdempotencyDigestConflicts,
		"createIfAbsent":             capabilities.CreateIfAbsent,
		"conditionalMutation":        capabilities.ConditionalMutation,
		"monotonicGenerations":       capabilities.MonotonicGenerations,
		"deleteHighWatermark":        capabilities.DeleteHighWatermark,
		"durableRoutingFence":        capabilities.DurableRoutingFence,
		"operationLookup":            capabilities.OperationLookup,
		"exactGet":                   capabilities.ExactGet,
		"stablePagination":           capabilities.StablePagination,
		"exclusiveOwnership":         capabilities.ExclusiveOwnership,
		"keywordSearch":              capabilities.KeywordSearch,
		"auditVersionVisibility":     capabilities.AuditVersionVisibility,
	}
	for name, enabled := range required {
		if !enabled {
			return fmt.Errorf("required capability %s is false", name)
		}
	}
	return nil
}

func validateCapabilityLimits(limits CapabilityLimits) error {
	values := map[string]int{
		"maxRequestBytes":       limits.MaxRequestBytes,
		"maxResponseBytes":      limits.MaxResponseBytes,
		"maxContentBytes":       limits.MaxContentBytes,
		"maxTags":               limits.MaxTags,
		"maxTagBytes":           limits.MaxTagBytes,
		"maxMetadataEntries":    limits.MaxMetadataEntries,
		"maxMetadataKeyBytes":   limits.MaxMetadataKeyBytes,
		"maxMetadataValueBytes": limits.MaxMetadataValueBytes,
		"maxQueryBytes":         limits.MaxQueryBytes,
		"maxPageSize":           limits.MaxPageSize,
		"maxSnapshotRecords":    limits.MaxSnapshotRecords,
		"snapshotTtlSeconds":    limits.SnapshotTTLSeconds,
	}
	for name, value := range values {
		if value <= 0 {
			return fmt.Errorf("capability limit %s must be positive", name)
		}
	}
	if limits.MaxRequestBytes > MaxHTTPBodyBytes || limits.MaxResponseBytes > MaxAdapterResponseBytes ||
		limits.MaxContentBytes > MaxContentBytes || limits.MaxTags > MaxTags || limits.MaxTagBytes > MaxTagBytes ||
		limits.MaxMetadataEntries > MaxMetadataEntries || limits.MaxMetadataKeyBytes > MaxMetadataKeyBytes ||
		limits.MaxMetadataValueBytes > MaxMetadataValueBytes || limits.MaxQueryBytes > MaxQueryBytes ||
		limits.MaxPageSize > MaxPageSize || limits.MaxSnapshotRecords > MaxSnapshotRecords ||
		limits.SnapshotTTLSeconds > MaxSnapshotTTLSeconds {
		return errors.New("capability limits exceed the profile hard limits")
	}
	return nil
}

func ValidateOwnershipClaimRequest(request *OwnershipClaimRequest) error {
	if request == nil {
		return errors.New("ownership claim request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	return ValidateBinding(request.Binding)
}

func ValidateOwnershipClaimResponse(response *OwnershipClaimResponse) error {
	if response == nil {
		return errors.New("ownership claim response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateBinding(response.Binding); err != nil {
		return err
	}
	if response.Result != ResultApplied && response.Result != ResultIdentityConflict {
		return fmt.Errorf("unsupported ownership result %q", response.Result)
	}
	if response.BindingDigest != BindingDigest(response.Binding) {
		return errors.New("bindingDigest does not match binding")
	}
	if response.Result == ResultApplied && response.ClaimIdentity != AuthorityDigest(response.Binding) {
		return errors.New("claimIdentity does not match the claimed authority")
	}
	if response.ClaimIdentity != "" && !digestPattern.MatchString(response.ClaimIdentity) {
		return errors.New("claimIdentity is invalid")
	}
	if response.MaximumRoutingEpoch > math.MaxInt64 ||
		(response.Result == ResultApplied && response.MaximumRoutingEpoch == 0) {
		return errors.New("maximumRoutingEpoch is invalid")
	}
	if response.ClaimedAt.IsZero() {
		return errors.New("claimedAt is required")
	}
	return nil
}

func ValidateRoutingFenceRequest(request *RoutingFenceRequest) error {
	if request == nil {
		return errors.New("routing fence request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	return ValidateBinding(request.Binding)
}

func ValidateRoutingFenceResponse(response *RoutingFenceResponse) error {
	if response == nil {
		return errors.New("routing fence response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateBinding(response.Binding); err != nil {
		return err
	}
	if response.Result != ResultApplied &&
		response.Result != ResultIdentityConflict &&
		response.Result != ResultPreconditionFailed {
		return fmt.Errorf("unsupported routing fence result %q", response.Result)
	}
	if response.BindingDigest != BindingDigest(response.Binding) {
		return errors.New("bindingDigest does not match binding")
	}
	if response.MaximumRoutingEpoch > math.MaxInt64 ||
		(response.Result != ResultIdentityConflict && response.MaximumRoutingEpoch == 0) {
		return errors.New("maximumRoutingEpoch is invalid")
	}
	switch response.Result {
	case ResultApplied:
		if response.MaximumRoutingEpoch < response.Binding.RoutingEpoch {
			return errors.New("applied routing fence maximum is below the requested routingEpoch")
		}
	case ResultPreconditionFailed:
		if response.MaximumRoutingEpoch <= response.Binding.RoutingEpoch {
			return errors.New("precondition routing fence maximum must be newer than the requested routingEpoch")
		}
	}
	if response.CompletedAt.IsZero() {
		return errors.New("completedAt is required")
	}
	return nil
}

// ValidateMutationEnvelope verifies canonical identity, state, CAS, and both
// digests without mutating the request.
func ValidateMutationEnvelope(envelope *MutationEnvelope) error {
	if envelope == nil {
		return errors.New("mutation envelope is required")
	}
	if envelope.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", envelope.ProtocolVersion)
	}
	if err := ValidateBinding(envelope.Binding); err != nil {
		return err
	}
	if err := validateOperationID(envelope.OperationID); err != nil {
		return err
	}
	if err := validateMemoryID(envelope.MemoryID); err != nil {
		return err
	}
	if envelope.UpsertKey != CanonicalUpsertKey(envelope.Binding, envelope.MemoryID) {
		return errors.New("upsertKey is not canonical for the binding and memoryId")
	}
	if envelope.Generation == 0 || envelope.Generation > math.MaxInt64 {
		return errors.New("generation must be a positive signed 64-bit value")
	}
	if envelope.ExpectedGeneration > math.MaxInt64 {
		return errors.New("expectedGeneration exceeds signed 64-bit range")
	}
	if envelope.Generation <= envelope.ExpectedGeneration {
		return errors.New("generation must be greater than expectedGeneration")
	}
	if err := validateOptionalSafeString("expectedBackendVersion", envelope.ExpectedBackendVersion); err != nil {
		return err
	}
	switch envelope.Kind {
	case MutationKindCreate:
		if envelope.ExpectedBackendVersion != "" {
			return errors.New("create expectedBackendVersion must be empty")
		}
		if err := validateLiveMutationState(envelope.State, envelope.ContentDigest); err != nil {
			return err
		}
	case MutationKindReplace:
		if envelope.ExpectedGeneration == 0 {
			return errors.New("replace expectedGeneration must be positive")
		}
		if err := validateLiveMutationState(envelope.State, envelope.ContentDigest); err != nil {
			return err
		}
	case MutationKindDelete:
		if envelope.State != nil {
			return errors.New("delete state must be null")
		}
		if envelope.ContentDigest != EmptyContentDigest() {
			return errors.New("delete contentDigest must be the empty-content digest")
		}
	default:
		return fmt.Errorf("unsupported mutation kind %q", envelope.Kind)
	}
	if !digestPattern.MatchString(envelope.ContentDigest) {
		return errors.New("contentDigest is invalid")
	}
	if !digestPattern.MatchString(envelope.MutationDigest) {
		return errors.New("mutationDigest is invalid")
	}
	expectedDigest, err := MutationDigest(envelope)
	if err != nil {
		return err
	}
	if !constantTimeEqual(envelope.MutationDigest, expectedDigest) {
		return errors.New("mutationDigest does not match the canonical envelope")
	}
	return nil
}

func validateLiveMutationState(state *MutationState, contentDigest string) error {
	if state == nil {
		return errors.New("create/replace state is required")
	}
	if err := validateContent(state.Content); err != nil {
		return err
	}
	if contentDigest != ContentDigest(state.Content) {
		return errors.New("contentDigest does not match exact content bytes")
	}
	normalizedTags, err := NormalizeTags(state.Tags)
	if err != nil {
		return err
	}
	if !slices.Equal(normalizedTags, state.Tags) {
		return errors.New("tags are not in canonical normalized order")
	}
	normalizedMetadata, err := NormalizeMetadata(state.Metadata)
	if err != nil {
		return err
	}
	if !equalStringMap(normalizedMetadata, state.Metadata) {
		return errors.New("metadata is not canonical")
	}
	return nil
}

func ValidateMutationReceipt(receipt *MutationReceipt) error {
	if receipt == nil {
		return errors.New("mutation receipt is required")
	}
	if receipt.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", receipt.ProtocolVersion)
	}
	if err := ValidateBinding(receipt.Binding); err != nil {
		return err
	}
	if !isTerminalResult(receipt.Result) {
		return fmt.Errorf("unsupported mutation result %q", receipt.Result)
	}
	if err := validateOperationID(receipt.OperationID); err != nil {
		return err
	}
	if receipt.BindingDigest != BindingDigest(receipt.Binding) {
		return errors.New("bindingDigest does not match binding")
	}
	if !digestPattern.MatchString(receipt.ContentDigest) || !digestPattern.MatchString(receipt.MutationDigest) {
		return errors.New("receipt digest is invalid")
	}
	if receipt.AppliedGeneration > math.MaxInt64 {
		return errors.New("appliedGeneration exceeds signed 64-bit range")
	}
	if err := validateOptionalSafeString("backendVersion", receipt.BackendVersion); err != nil {
		return err
	}
	if err := validateOptionalSafeString("backendMemoryId", receipt.BackendMemoryID); err != nil {
		return err
	}
	switch receipt.Result {
	case ResultApplied, ResultNotFound:
		if receipt.AppliedGeneration == 0 || receipt.BackendVersion == "" || receipt.BackendMemoryID == "" {
			return errors.New("durable receipt is missing applied generation or backend identity")
		}
		if receipt.Result == ResultNotFound && receipt.ContentDigest != EmptyContentDigest() {
			return errors.New("notFound receipt contentDigest must be the empty-content digest")
		}
	case ResultPreconditionFailed, ResultIdempotencyConflict, ResultIdentityConflict,
		ResultRetryableError, ResultNonRetryableError:
		if receipt.AppliedGeneration != 0 || receipt.BackendVersion != "" || receipt.BackendMemoryID != "" {
			return errors.New("non-applied receipt must not contain durable generation or backend identity")
		}
	}
	if receipt.CompletedAt.IsZero() {
		return errors.New("completedAt is required")
	}
	return nil
}

func isTerminalResult(result string) bool {
	switch result {
	case ResultApplied, ResultNotFound, ResultPreconditionFailed, ResultIdempotencyConflict,
		ResultIdentityConflict, ResultRetryableError, ResultNonRetryableError:
		return true
	default:
		return false
	}
}

func ValidateGetRequest(request *GetRequest) error {
	if request == nil {
		return errors.New("get request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	if err := ValidateBinding(request.Binding); err != nil {
		return err
	}
	return validateUpsertKey(request.UpsertKey, request.Binding)
}

func ValidateGetResponse(response *GetResponse) error {
	if response == nil {
		return errors.New("get response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateBinding(response.Binding); err != nil {
		return err
	}
	if response.Found != (response.Record != nil) {
		return errors.New("found and record presence disagree")
	}
	if response.Record != nil {
		return ValidateMemoryRecord(response.Record, response.Binding)
	}
	return nil
}

func ValidateOperationLookupRequest(request *OperationLookupRequest) error {
	if request == nil {
		return errors.New("operation lookup request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	if err := ValidateBinding(request.Binding); err != nil {
		return err
	}
	return validateOperationID(request.OperationID)
}

func ValidateOperationLookupResponse(response *OperationLookupResponse) error {
	if response == nil {
		return errors.New("operation lookup response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateBinding(response.Binding); err != nil {
		return err
	}
	if response.Found != (response.Receipt != nil) {
		return errors.New("found and receipt presence disagree")
	}
	if response.Receipt != nil {
		if !AuthorityEqual(response.Binding, response.Receipt.Binding) {
			return errors.New("operation receipt authority does not match response binding")
		}
		return ValidateMutationReceipt(response.Receipt)
	}
	return nil
}

func ValidateMemoryRecord(record *MemoryRecord, binding Binding) error {
	if record == nil {
		return errors.New("memory record is required")
	}
	if err := validateMemoryID(record.MemoryID); err != nil {
		return err
	}
	if record.UpsertKey != CanonicalUpsertKey(binding, record.MemoryID) {
		return errors.New("record upsertKey is not canonical")
	}
	if record.Generation == 0 || record.Generation > math.MaxInt64 {
		return errors.New("record generation is invalid")
	}
	if err := validateOptionalSafeString(
		"backendVersion", record.BackendVersion,
	); err != nil || record.BackendVersion == "" {
		return errors.New("record backendVersion is required and must be safe")
	}
	if err := validateOptionalSafeString(
		"backendMemoryId", record.BackendMemoryID,
	); err != nil || record.BackendMemoryID == "" {
		return errors.New("record backendMemoryId is required and must be safe")
	}
	if record.UpdatedAt.IsZero() {
		return errors.New("record updatedAt is required")
	}
	if math.IsNaN(record.Score) || math.IsInf(record.Score, 0) || record.Score < 0 {
		return errors.New("record score is invalid")
	}
	switch record.State {
	case RecordStateLive:
		state := &MutationState{Content: record.Content, Tags: record.Tags, Metadata: record.Metadata}
		return validateLiveMutationState(state, record.ContentDigest)
	case RecordStateTombstone:
		if record.Content != "" ||
			len(record.Tags) != 0 ||
			len(record.Metadata) != 0 ||
			record.Tags == nil ||
			record.Metadata == nil {
			return errors.New("tombstone content, tags, and metadata must be explicit empty values")
		}
		if record.ContentDigest != EmptyContentDigest() {
			return errors.New("tombstone contentDigest is invalid")
		}
		return nil
	default:
		return fmt.Errorf("unsupported record state %q", record.State)
	}
}

func ValidateSearchRequest(request *SearchRequest) error {
	if request == nil {
		return errors.New("search request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	if err := ValidateBinding(request.Binding); err != nil {
		return err
	}
	if !isSearchMode(request.Mode) {
		return fmt.Errorf("unsupported search mode %q", request.Mode)
	}
	if len(request.Query) > MaxQueryBytes ||
		!utf8.ValidString(request.Query) ||
		containsUnsafeControl(request.Query, true) {
		return errors.New("query is invalid")
	}
	if request.PageSize <= 0 || request.PageSize > MaxPageSize {
		return fmt.Errorf("pageSize must be between 1 and %d", MaxPageSize)
	}
	if len(request.PageToken) > MaxPageTokenBytes ||
		(request.PageToken != "" && !pageTokenPattern.MatchString(request.PageToken)) {
		return errors.New("pageToken is invalid")
	}
	return nil
}

func ValidateSearchResponse(response *SearchResponse) error {
	return validateSearchResponseAt(response, time.Now())
}

func validateSearchResponseAt(response *SearchResponse, now time.Time) error {
	if response == nil {
		return errors.New("search response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := ValidateBinding(response.Binding); err != nil {
		return err
	}
	if !isSearchMode(response.RequestedMode) ||
		(response.ActualMode != SearchModeKeyword &&
			response.ActualMode != SearchModeSemantic &&
			response.ActualMode != SearchModeHybrid) {
		return errors.New("search response mode is invalid")
	}
	if response.RequestedMode != SearchModeAuto && response.RequestedMode != response.ActualMode {
		return errors.New("explicit search mode was downgraded")
	}
	if response.Records == nil || len(response.Records) > MaxPageSize {
		return errors.New("search records are missing or exceed the page bound")
	}
	for i := range response.Records {
		if response.Records[i].State != RecordStateLive {
			return errors.New("search results must not contain tombstones")
		}
		if err := ValidateMemoryRecord(&response.Records[i], response.Binding); err != nil {
			return err
		}
		if response.ActualMode == SearchModeKeyword && response.Records[i].Score != 0 {
			return errors.New("keyword search results must have score 0")
		}
	}
	if response.Exhausted != (response.NextPageToken == "") {
		return errors.New("exhausted and nextPageToken disagree")
	}
	if len(response.NextPageToken) > MaxPageTokenBytes ||
		(response.NextPageToken != "" && !pageTokenPattern.MatchString(response.NextPageToken)) {
		return errors.New("nextPageToken is invalid")
	}
	if response.SnapshotExpiresAt.IsZero() {
		return errors.New("snapshotExpiresAt is required")
	}
	if !response.Exhausted && !response.SnapshotExpiresAt.After(now) {
		return errors.New("snapshotExpiresAt must be in the future for a continuation")
	}
	return nil
}

func isSearchMode(mode string) bool {
	switch mode {
	case SearchModeKeyword, SearchModeSemantic, SearchModeHybrid, SearchModeAuto:
		return true
	default:
		return false
	}
}

func ValidateErrorResponse(response *ErrorResponse) error {
	if response == nil {
		return errors.New("error response is required")
	}
	if response.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if response.Binding != nil {
		if err := ValidateBinding(*response.Binding); err != nil {
			return err
		}
	}
	if err := validateIdentity("code", response.Code); err != nil {
		return err
	}
	if !isErrorCode(response.Code) {
		return errors.New("error code is not part of the closed OMS profile")
	}
	if response.Message == "" ||
		len(response.Message) > MaxErrorMessageBytes ||
		!utf8.ValidString(response.Message) ||
		containsUnsafeControl(response.Message, true) {
		return errors.New("error message is invalid")
	}
	if response.RetryAfterSeconds < 0 || (!response.Retryable && response.RetryAfterSeconds != 0) {
		return errors.New("retryAfterSeconds is invalid")
	}
	return nil
}

func isErrorCode(code string) bool {
	switch code {
	case ErrorCodeUnauthorized,
		ErrorCodeInvalidRequest,
		ErrorCodeMethodNotAllowed,
		ErrorCodeNotFound,
		ErrorCodeInternal,
		ErrorCodeResponseTooLarge,
		ErrorCodeSearchModeUnsupported,
		ErrorCodeIdentityConflict,
		ErrorCodeRoutingFenced,
		ErrorCodePageTokenInvalid,
		ErrorCodePageTokenExpired,
		ErrorCodeSnapshotCapacity:
		return true
	default:
		return false
	}
}

// ConstantTimeBearerEqual compares bearer tokens without prefix timing leaks.
func ConstantTimeBearerEqual(got, want string) bool {
	got = strings.TrimSpace(got)
	want = strings.TrimSpace(want)
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// BearerToken extracts a bearer token from an Authorization header.
func BearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// SanitizeMessage removes unsupported controls and bounds adapter-owned text.
func SanitizeMessage(message string, limit int) string {
	message = strings.TrimSpace(message)
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, message)
	if limit > 0 && len(message) > limit {
		message = message[:limit]
		for !utf8.ValidString(message) && len(message) > 0 {
			message = message[:len(message)-1]
		}
	}
	return message
}

func validateContent(content string) error {
	if !utf8.ValidString(content) {
		return errors.New("content must be valid UTF-8")
	}
	if len(content) > MaxContentBytes {
		return fmt.Errorf("content exceeds %d bytes", MaxContentBytes)
	}
	if containsUnsafeControl(content, true) {
		return errors.New("content contains unsafe control characters")
	}
	return nil
}

func validateTag(tag string) error {
	if tag == "" {
		return errors.New("tag must not be empty")
	}
	if len(tag) > MaxTagBytes || !utf8.ValidString(tag) || containsUnsafeControl(tag, false) {
		return fmt.Errorf("tag %q is invalid", truncateForError(tag))
	}
	return nil
}

func validateMetadataEntry(key, value string) error {
	if key == "" || len(key) > MaxMetadataKeyBytes || !metadataKeyPattern.MatchString(key) {
		return fmt.Errorf("metadata key %q is invalid", truncateForError(key))
	}
	if len(value) > MaxMetadataValueBytes || !utf8.ValidString(value) || containsUnsafeControl(value, false) {
		return fmt.Errorf("metadata value for %q is invalid", truncateForError(key))
	}
	return nil
}

func validateIdentity(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > MaxIdentityBytes || !identityPattern.MatchString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateOptionalSafeString(name, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxIdentityBytes ||
		!utf8.ValidString(value) ||
		containsUnsafeControl(value, false) ||
		strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateOperationID(value string) error {
	if value == "" || len(value) > MaxOperationIDBytes || !operationIDPattern.MatchString(value) {
		return errors.New("operationId is invalid")
	}
	return nil
}

func validateMemoryID(value string) error {
	if value == "" || len(value) > MaxIdentityBytes || !memoryIDPattern.MatchString(value) {
		return errors.New("memoryId is invalid")
	}
	return nil
}

func validateUpsertKey(value string, binding Binding) error {
	if len(value) == 0 ||
		len(value) > 4*MaxIdentityBytes ||
		!utf8.ValidString(value) ||
		containsUnsafeControl(value, false) {
		return errors.New("upsertKey is invalid")
	}
	prefix := "orka:" + binding.ClusterID + ":" + binding.NamespaceUID + ":" + fmt.Sprintf("%d:", binding.AuthorityEpoch)
	if !strings.HasPrefix(value, prefix) {
		return errors.New("upsertKey does not match binding")
	}
	return validateMemoryID(strings.TrimPrefix(value, prefix))
}

func containsUnsafeControl(value string, allowTextWhitespace bool) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		if !unicode.IsControl(r) {
			return false
		}
		if allowTextWhitespace && (r == '\n' || r == '\r' || r == '\t') {
			return false
		}
		return true
	})
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func equalStringMap(left, right map[string]string) bool {
	if left == nil || right == nil || len(left) != len(right) {
		return left == nil && right == nil
	}
	for key, value := range left {
		rightValue, ok := right[key]
		if !ok || rightValue != value {
			return false
		}
	}
	return true
}

func truncateForError(value string) string {
	value = SanitizeMessage(value, 32)
	if value == "" {
		return "<empty>"
	}
	return value
}
