/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package protocol defines the closed orka.oms.v0alpha1 HTTP wire contract.
package protocol

import "time"

const (
	// Version is the frozen OMS profile version.
	Version = "orka.oms.v0alpha1"

	PathHealth         = "/v1/health"
	PathStoreResolve   = "/v1/stores/resolve"
	PathCapabilities   = "/v1/capabilities"
	PathOwnershipClaim = "/v1/ownership/claim"
	PathRoutingFence   = "/v1/routing-fences/advance"
	PathMutations      = "/v1/mutations"
	PathRecordsGet     = "/v1/records/get"
	PathOperationsGet  = "/v1/operations/get"
	PathSearch         = "/v1/search"

	MutationKindCreate  = "create"
	MutationKindReplace = "replace"
	MutationKindDelete  = "delete"

	ResultApplied             = "applied"
	ResultNotFound            = "notFound"
	ResultPreconditionFailed  = "preconditionFailed"
	ResultIdempotencyConflict = "idempotencyConflict"
	ResultIdentityConflict    = "identityConflict"
	ResultRetryableError      = "retryableError"
	ResultNonRetryableError   = "nonRetryableError"

	RecordStateLive      = "live"
	RecordStateTombstone = "tombstone"

	SearchModeKeyword  = "keyword"
	SearchModeSemantic = "semantic"
	SearchModeHybrid   = "hybrid"
	SearchModeAuto     = "auto"

	ErrorCodeUnauthorized          = "OMS_UNAUTHORIZED"
	ErrorCodeInvalidRequest        = "OMS_INVALID_REQUEST"
	ErrorCodeMethodNotAllowed      = "OMS_METHOD_NOT_ALLOWED"
	ErrorCodeNotFound              = "OMS_ENDPOINT_NOT_FOUND"
	ErrorCodeInternal              = "OMS_INTERNAL_ERROR"
	ErrorCodeResponseTooLarge      = "OMS_RESPONSE_TOO_LARGE"
	ErrorCodeSearchModeUnsupported = "MEMORY_SEARCH_MODE_UNSUPPORTED"
	ErrorCodeIdentityConflict      = "OMS_IDENTITY_CONFLICT"
	ErrorCodeRoutingFenced         = "OMS_ROUTING_EPOCH_FENCED"
	ErrorCodePageTokenInvalid      = "OMS_PAGE_TOKEN_INVALID"
	ErrorCodePageTokenExpired      = "OMS_PAGE_TOKEN_EXPIRED"
	ErrorCodeSnapshotCapacity      = "OMS_SNAPSHOT_CAPACITY_EXCEEDED"

	// MaxHTTPBodyBytes is the exact normative request-body hard maximum. It
	// accommodates the worst-case encoding/json representation of
	// MaxContentBytes plus the bounded envelope metadata: encoding/json may
	// expand each '<', '>', or '&' byte to a six-byte Unicode escape.
	MaxHTTPBodyBytes        = 2 << 20
	MaxAdapterResponseBytes = 4 << 20
	MaxContentBytes         = 256 << 10
	MaxIdentityBytes        = 256
	MaxOperationIDBytes     = 128
	MaxBackendVersionBytes  = 256
	MaxTags                 = 64
	MaxTagBytes             = 128
	MaxMetadataEntries      = 32
	MaxMetadataKeyBytes     = 64
	MaxMetadataValueBytes   = 1024
	MaxQueryBytes           = 1024
	MaxPageTokenBytes       = 256
	MaxPageSize             = 8
	MaxSnapshotRecords      = 1024
	MaxSnapshotTTLSeconds   = 24 * 60 * 60
	MaxErrorMessageBytes    = 512
)

// Binding is the complete identity and routing fence carried by every OMS
// profile operation. TenantID is deterministically derived from ClusterID and
// NamespaceUID.
type Binding struct {
	ClusterID      string `json:"clusterId"`
	NamespaceUID   string `json:"namespaceUid"`
	BackendUID     string `json:"backendUid"`
	AuthorityEpoch uint64 `json:"authorityEpoch"`
	RoutingEpoch   uint64 `json:"routingEpoch"`
	TenantID       string `json:"tenantId"`
	StoreUUID      string `json:"storeUuid"`
}

// HealthResponse is the authenticated transport-liveness response. Health is
// intentionally not an authority or content operation.
type HealthResponse struct {
	ProtocolVersion string `json:"protocolVersion"`
	Status          string `json:"status"`
}

// StoreResolutionBinding is the pre-authority identity used to resolve an
// operator-selected store name. Authority/routing epochs and store UUID do not
// exist at this stage and are intentionally absent from the wire object.
type StoreResolutionBinding struct {
	ClusterID    string `json:"clusterId"`
	NamespaceUID string `json:"namespaceUid"`
	BackendUID   string `json:"backendUid"`
	TenantID     string `json:"tenantId"`
}

// StoreResolveRequest resolves one immutable operator-selected store name.
type StoreResolveRequest struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Binding         StoreResolutionBinding `json:"binding"`
	StoreName       string                 `json:"storeName"`
}

// StoreResolveResponse returns a stable canonical UUID for the store name and
// echoes the exact pre-authority identity.
type StoreResolveResponse struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Binding         StoreResolutionBinding `json:"binding"`
	StoreName       string                 `json:"storeName"`
	StoreUUID       string                 `json:"storeUuid"`
}

// CapabilitiesRequest asks the adapter to evaluate capabilities for one exact
// binding identity.
type CapabilitiesRequest struct {
	ProtocolVersion string  `json:"protocolVersion"`
	Binding         Binding `json:"binding"`
}

// CapabilitiesResponse advertises one stable capability revision with numeric
// limits and a finite validation expiry.
type CapabilitiesResponse struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Binding         Binding          `json:"binding"`
	AdapterName     string           `json:"adapterName"`
	AdapterVersion  string           `json:"adapterVersion"`
	Revision        string           `json:"revision"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	Capabilities    Capabilities     `json:"capabilities"`
	Limits          CapabilityLimits `json:"limits"`
}

// Capabilities are the provider-neutral semantics required by the OMS profile.
type Capabilities struct {
	DurableIdempotency         bool `json:"durableIdempotency"`
	IdempotencyDigestConflicts bool `json:"idempotencyDigestConflicts"`
	CreateIfAbsent             bool `json:"createIfAbsent"`
	ConditionalMutation        bool `json:"conditionalMutation"`
	MonotonicGenerations       bool `json:"monotonicGenerations"`
	DeleteHighWatermark        bool `json:"deleteHighWatermark"`
	DurableRoutingFence        bool `json:"durableRoutingFence"`
	OperationLookup            bool `json:"operationLookup"`
	ExactGet                   bool `json:"exactGet"`
	StablePagination           bool `json:"stablePagination"`
	ExclusiveOwnership         bool `json:"exclusiveOwnership"`
	KeywordSearch              bool `json:"keywordSearch"`
	AuditVersionVisibility     bool `json:"auditVersionVisibility"`
	SemanticSearch             bool `json:"semanticSearch"`
	HybridSearch               bool `json:"hybridSearch"`
}

// CapabilityLimits are provider-advertised limits bounded by the exact profile
// hard maxima above. A value of zero is invalid.
type CapabilityLimits struct {
	MaxRequestBytes       int `json:"maxRequestBytes"`
	MaxResponseBytes      int `json:"maxResponseBytes"`
	MaxContentBytes       int `json:"maxContentBytes"`
	MaxTags               int `json:"maxTags"`
	MaxTagBytes           int `json:"maxTagBytes"`
	MaxMetadataEntries    int `json:"maxMetadataEntries"`
	MaxMetadataKeyBytes   int `json:"maxMetadataKeyBytes"`
	MaxMetadataValueBytes int `json:"maxMetadataValueBytes"`
	MaxQueryBytes         int `json:"maxQueryBytes"`
	MaxPageSize           int `json:"maxPageSize"`
	MaxSnapshotRecords    int `json:"maxSnapshotRecords"`
	SnapshotTTLSeconds    int `json:"snapshotTtlSeconds"`
}

// OwnershipClaimRequest durably claims the writer slot for the binding's
// tenant/store/authority scope.
type OwnershipClaimRequest struct {
	ProtocolVersion string  `json:"protocolVersion"`
	Binding         Binding `json:"binding"`
}

// OwnershipClaimResponse returns the durable owner decision and current fence.
type OwnershipClaimResponse struct {
	ProtocolVersion     string    `json:"protocolVersion"`
	Binding             Binding   `json:"binding"`
	Result              string    `json:"result"`
	BindingDigest       string    `json:"bindingDigest"`
	ClaimIdentity       string    `json:"claimIdentity"`
	MaximumRoutingEpoch uint64    `json:"maximumRoutingEpoch"`
	ClaimedAt           time.Time `json:"claimedAt"`
}

// RoutingFenceRequest advances the maximum accepted routing epoch to at least
// Binding.RoutingEpoch.
type RoutingFenceRequest struct {
	ProtocolVersion string  `json:"protocolVersion"`
	Binding         Binding `json:"binding"`
}

// RoutingFenceResponse is the durable fence result.
type RoutingFenceResponse struct {
	ProtocolVersion     string    `json:"protocolVersion"`
	Binding             Binding   `json:"binding"`
	Result              string    `json:"result"`
	BindingDigest       string    `json:"bindingDigest"`
	MaximumRoutingEpoch uint64    `json:"maximumRoutingEpoch"`
	CompletedAt         time.Time `json:"completedAt"`
}

// MutationState is the canonical provider content state for create/replace.
// Tags and Metadata are required non-nil collections on the wire. Delete uses
// an explicit JSON null state.
type MutationState struct {
	Content  string            `json:"content"`
	Tags     []string          `json:"tags"`
	Metadata map[string]string `json:"metadata"`
}

// MutationEnvelope is the canonical create, replace, or delete request.
type MutationEnvelope struct {
	ProtocolVersion        string         `json:"protocolVersion"`
	OperationID            string         `json:"operationId"`
	Binding                Binding        `json:"binding"`
	MemoryID               string         `json:"memoryId"`
	UpsertKey              string         `json:"upsertKey"`
	Kind                   string         `json:"kind"`
	Generation             uint64         `json:"generation"`
	ExpectedGeneration     uint64         `json:"expectedGeneration"`
	ExpectedBackendVersion string         `json:"expectedBackendVersion"`
	ContentDigest          string         `json:"contentDigest"`
	MutationDigest         string         `json:"mutationDigest"`
	State                  *MutationState `json:"state"`
}

// MutationReceipt is a closed, bounded terminal mutation result. Exact replay
// returns the byte-equivalent receipt representation.
type MutationReceipt struct {
	ProtocolVersion   string    `json:"protocolVersion"`
	Binding           Binding   `json:"binding"`
	Result            string    `json:"result"`
	OperationID       string    `json:"operationId"`
	BindingDigest     string    `json:"bindingDigest"`
	AppliedGeneration uint64    `json:"appliedGeneration"`
	BackendVersion    string    `json:"backendVersion"`
	BackendMemoryID   string    `json:"backendMemoryId"`
	ContentDigest     string    `json:"contentDigest"`
	MutationDigest    string    `json:"mutationDigest"`
	CompletedAt       time.Time `json:"completedAt"`
}

// MemoryRecord is the provider-side materialized record or retained tombstone.
type MemoryRecord struct {
	MemoryID        string            `json:"memoryId"`
	UpsertKey       string            `json:"upsertKey"`
	State           string            `json:"state"`
	Generation      uint64            `json:"generation"`
	BackendVersion  string            `json:"backendVersion"`
	BackendMemoryID string            `json:"backendMemoryId"`
	ContentDigest   string            `json:"contentDigest"`
	Content         string            `json:"content"`
	Tags            []string          `json:"tags"`
	Metadata        map[string]string `json:"metadata"`
	UpdatedAt       time.Time         `json:"updatedAt"`
	Score           float64           `json:"score,omitempty"`
}

// GetRequest performs exact lookup by canonical upsert key.
type GetRequest struct {
	ProtocolVersion string  `json:"protocolVersion"`
	Binding         Binding `json:"binding"`
	UpsertKey       string  `json:"upsertKey"`
}

// GetResponse returns either one exact record/tombstone or found=false.
type GetResponse struct {
	ProtocolVersion string        `json:"protocolVersion"`
	Binding         Binding       `json:"binding"`
	Found           bool          `json:"found"`
	Record          *MemoryRecord `json:"record"`
}

// OperationLookupRequest performs exact durable lookup by operation ID.
type OperationLookupRequest struct {
	ProtocolVersion string  `json:"protocolVersion"`
	Binding         Binding `json:"binding"`
	OperationID     string  `json:"operationId"`
}

// OperationLookupResponse returns the original terminal receipt when found.
type OperationLookupResponse struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Binding         Binding          `json:"binding"`
	Found           bool             `json:"found"`
	Receipt         *MutationReceipt `json:"receipt"`
}

// SearchRequest performs stable bounded search. PageToken is required on the
// wire and is the empty string for the first page.
type SearchRequest struct {
	ProtocolVersion string  `json:"protocolVersion"`
	Binding         Binding `json:"binding"`
	Mode            string  `json:"mode"`
	Query           string  `json:"query"`
	PageSize        int     `json:"pageSize"`
	PageToken       string  `json:"pageToken"`
}

// SearchResponse identifies requested and actual modes. Exhausted=true and an
// empty nextPageToken are the only terminal pagination representation.
type SearchResponse struct {
	ProtocolVersion   string         `json:"protocolVersion"`
	Binding           Binding        `json:"binding"`
	RequestedMode     string         `json:"requestedMode"`
	ActualMode        string         `json:"actualMode"`
	Records           []MemoryRecord `json:"records"`
	NextPageToken     string         `json:"nextPageToken"`
	Exhausted         bool           `json:"exhausted"`
	SnapshotExpiresAt time.Time      `json:"snapshotExpiresAt"`
}

// ContinuationExpiresAt caps a caller-selected local continuation deadline at
// the provider snapshot expiry. The boolean is false when this response does
// not carry a nonterminal continuation. Callers should use this only after the
// response has passed protocol validation.
func (response SearchResponse) ContinuationExpiresAt(localExpiresAt time.Time) (time.Time, bool) {
	if response.Exhausted || response.NextPageToken == "" || response.SnapshotExpiresAt.IsZero() {
		return time.Time{}, false
	}
	if localExpiresAt.IsZero() || !localExpiresAt.Before(response.SnapshotExpiresAt) {
		return response.SnapshotExpiresAt, true
	}
	return localExpiresAt, true
}

// ErrorResponse is used only for transport/codec/profile errors. Semantic
// mutation outcomes use MutationReceipt variants.
type ErrorResponse struct {
	ProtocolVersion   string   `json:"protocolVersion"`
	Binding           *Binding `json:"binding"`
	Code              string   `json:"code"`
	Message           string   `json:"message"`
	Retryable         bool     `json:"retryable"`
	RetryAfterSeconds int      `json:"retryAfterSeconds"`
}
