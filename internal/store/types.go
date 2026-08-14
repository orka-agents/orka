package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// SessionTypeGateway identifies canonical Sessions exclusively owned by gateway admission/projection.
const SessionTypeGateway = "gateway"

// SessionRecord represents a full session.
type SessionRecord struct {
	Namespace     string
	Name          string
	SessionType   string // "task", "chat", or "gateway"
	ActiveTask    string
	ActiveTaskUID string
	MessageCount  int
	InputTokens   int
	OutputTokens  int
	Cancelled     bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Messages      []SessionMessage
}

// SessionMetadata is the lightweight listing representation.
type SessionMetadata struct {
	Name          string
	SessionType   string
	MessageCount  int
	InputTokens   int
	OutputTokens  int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ActiveTask    string
	ActiveTaskUID string
}

// SessionMessage is a single transcript entry.
type SessionMessage struct {
	ID         string            `json:"id"`
	Order      int64             `json:"order,omitempty"`
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	Input      map[string]any    `json:"input,omitempty"`
	ToolCalls  any               `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallID,omitempty"`
	SourceType string            `json:"sourceType,omitempty"`
	SourceRef  string            `json:"sourceRef,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timestamp  time.Time         `json:"ts"`
}

// ArtifactMetadata describes a stored artifact file.
type ArtifactMetadata struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"createdAt"`
}

// PlanState represents the autonomous plan state for a coordinator task.
type PlanState struct {
	TaskName     string
	Namespace    string
	Iteration    int
	Summary      string // Human-readable progress summary
	ProgressPct  int    // 0-100 progress estimate
	GoalComplete bool   // LLM determined goal is met
	PlanDocument string // Freeform markdown plan managed by LLM
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Memory represents durable namespace-scoped memory captured from tasks,
// sessions, or explicit worker calls.
type Memory struct {
	ID                   string                     `json:"id"`
	Namespace            string                     `json:"namespace"`
	SessionName          string                     `json:"sessionName,omitempty"`
	AgentName            string                     `json:"agentName,omitempty"`
	TaskName             string                     `json:"taskName,omitempty"`
	ParentTask           string                     `json:"parentTask,omitempty"`
	Source               string                     `json:"source"`
	SourceProposalID     string                     `json:"sourceProposalId,omitempty"`
	Content              string                     `json:"content"`
	Tags                 []string                   `json:"tags,omitempty"`
	Disabled             bool                       `json:"disabled"`
	Deleted              bool                       `json:"deleted"`
	CreatedAt            time.Time                  `json:"createdAt"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
	LastRecalledAt       *time.Time                 `json:"lastRecalledAt,omitempty"`
	RecalledCount        int                        `json:"recalledCount"`
	Generation           int64                      `json:"generation"`
	DesiredGeneration    int64                      `json:"desiredGeneration"`
	GovernanceRevision   int64                      `json:"governanceRevision"`
	MaterializationState MemoryMaterializationState `json:"materializationState"`
	PendingOperationID   string                     `json:"pendingOperationId,omitempty"`
	Trust                MemoryTrust                `json:"trust"`
	ContentDigest        string                     `json:"contentDigest,omitempty"`
	ContentAvailable     bool                       `json:"contentAvailable"`
}

// MemoryFilter constrains memory list and recall queries.
type MemoryFilter struct {
	Namespace       string
	Query           string
	SessionName     string
	AgentName       string
	TaskName        string
	ParentTask      string
	Source          string
	Tags            []string
	IDs             []string
	Trust           []MemoryTrust
	IncludeDisabled bool
	IncludeDeleted  bool
	Limit           int
	Cursor          string
	BeforeUpdatedAt *time.Time
	BeforeID        string
}

// TranscriptSearchFilter constrains transcript-backed recall/search.
type TranscriptSearchFilter struct {
	Namespace          string
	Query              string
	SessionName        string
	ExcludeSessionName string
	Roles              []string
	Limit              int
	MaxSnippetLength   int
}

// TranscriptSearchResult is a compact prior transcript hit.
type TranscriptSearchResult struct {
	SessionName     string    `json:"sessionName"`
	MessageID       int64     `json:"messageId"`
	StableMessageID string    `json:"stableMessageId,omitempty"`
	Role            string    `json:"role"`
	Name            string    `json:"name,omitempty"`
	Snippet         string    `json:"snippet"`
	CreatedAt       time.Time `json:"createdAt"`
}

// MemoryProposal represents a proposed memory-adjacent change such as a reusable skill.
type MemoryProposal struct {
	ID                         string     `json:"id"`
	Namespace                  string     `json:"namespace"`
	TaskName                   string     `json:"taskName,omitempty"`
	AgentName                  string     `json:"agentName,omitempty"`
	Type                       string     `json:"type"`
	SkillName                  string     `json:"skillName,omitempty"`
	Title                      string     `json:"title"`
	Description                string     `json:"description,omitempty"`
	Content                    string     `json:"content,omitempty"`
	Patch                      string     `json:"patch,omitempty"`
	Status                     string     `json:"status"`
	Reviewer                   string     `json:"reviewer,omitempty"`
	ReviewNote                 string     `json:"reviewNote,omitempty"`
	AppliedMemoryID            string     `json:"appliedMemoryId,omitempty"`
	ApplyOperationID           string     `json:"applyOperationId,omitempty"`
	AppliedBy                  string     `json:"appliedBy,omitempty"`
	ApplicationAbandonedBy     string     `json:"applicationAbandonedBy,omitempty"`
	ApplicationAbandonedReason string     `json:"applicationAbandonedReason,omitempty"`
	CreatedAt                  time.Time  `json:"createdAt"`
	UpdatedAt                  time.Time  `json:"updatedAt"`
	ReviewedAt                 *time.Time `json:"reviewedAt,omitempty"`
	AppliedAt                  *time.Time `json:"appliedAt,omitempty"`
	ApplicationAbandonedAt     *time.Time `json:"applicationAbandonedAt,omitempty"`
}

// MemoryProposalFilter constrains proposal list queries.
type MemoryProposalFilter struct {
	Namespace string
	TaskName  string
	AgentName string
	Type      string
	Status    string
	Query     string
	Limit     int
}

// MemoryProposalReview records governance review decisions for proposals.
type MemoryProposalReview struct {
	Namespace  string
	ID         string
	Status     string
	Reviewer   string
	ReviewNote string
}

// MemoryProposalApply records an explicit proposal application request.
type MemoryProposalApply struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	AppliedBy string `json:"appliedBy"`
}

// MemoryTrust is the server-owned trust classification for a memory.
type MemoryTrust string

const (
	MemoryTrustUntrusted MemoryTrust = "untrusted"
	MemoryTrustReviewed  MemoryTrust = "reviewed"
	MemoryTrustTrusted   MemoryTrust = "trusted"
)

// MemoryMaterializationState is Orka's durable view of remote materialization.
type MemoryMaterializationState string

const (
	MemoryMaterializationPending  MemoryMaterializationState = "pending"
	MemoryMaterializationActive   MemoryMaterializationState = "active"
	MemoryMaterializationDeleted  MemoryMaterializationState = "deleted"
	MemoryMaterializationDiverged MemoryMaterializationState = "diverged"
	MemoryMaterializationLost     MemoryMaterializationState = "lost"
	MemoryMaterializationOrphaned MemoryMaterializationState = "orphaned"
)

// MemoryBackendMode selects the durable authority surface.
type MemoryBackendMode string

const (
	MemoryBackendModeLegacy MemoryBackendMode = "legacy"
	MemoryBackendModeRemote MemoryBackendMode = "remote"
)

// MemoryBackendBindingState is the durable namespace routing state.
type MemoryBackendBindingState string

const (
	MemoryBackendBindingLegacy         MemoryBackendBindingState = "legacy"
	MemoryBackendBindingValidating     MemoryBackendBindingState = "validating"
	MemoryBackendBindingAccepting      MemoryBackendBindingState = "accepting"
	MemoryBackendBindingDraining       MemoryBackendBindingState = "draining"
	MemoryBackendBindingRecovering     MemoryBackendBindingState = "recovering"
	MemoryBackendBindingDecommissioned MemoryBackendBindingState = "decommissioned"
	MemoryBackendBindingRemoved        MemoryBackendBindingState = "removed"
)

// MemoryOperationKind is a full-content remote mutation kind.
type MemoryOperationKind string

const (
	MemoryOperationCreate  MemoryOperationKind = "create"
	MemoryOperationReplace MemoryOperationKind = "replace"
	MemoryOperationDelete  MemoryOperationKind = "delete"
)

// MaxMemoryOperationPayloadBytes is the immutable hard cap for the canonical
// OMS request retained in the durable operation ledger. The wire request may
// be larger before canonical JSON normalization (for example, due to HTML
// escaping), but the stored canonical representation may not exceed 512 KiB.
const MaxMemoryOperationPayloadBytes = 512 << 10

// MaxMemoryIdempotencySnapshotBytes bounds the immutable replay snapshot kept
// with one caller idempotency record.
const MaxMemoryIdempotencySnapshotBytes = 512 << 10

// MemoryOperationState is the durable dispatcher state.
type MemoryOperationState string

const (
	MemoryOperationQueued       MemoryOperationState = "queued"
	MemoryOperationLeased       MemoryOperationState = "leased"
	MemoryOperationDispatching  MemoryOperationState = "dispatching"
	MemoryOperationAmbiguous    MemoryOperationState = "ambiguous"
	MemoryOperationDeadLettered MemoryOperationState = "dead_lettered"
	MemoryOperationSucceeded    MemoryOperationState = "succeeded"
	MemoryOperationAbandoned    MemoryOperationState = "abandoned"
	MemoryOperationSuperseded   MemoryOperationState = "superseded"
	MemoryOperationOrphaned     MemoryOperationState = "orphaned"
)

// MemoryIdempotencyResponseType identifies the stable shape reconstructed on replay.
type MemoryIdempotencyResponseType string

const (
	MemoryIdempotencyMemory    MemoryIdempotencyResponseType = "memory"
	MemoryIdempotencyOperation MemoryIdempotencyResponseType = "operation"
	MemoryIdempotencyEmpty     MemoryIdempotencyResponseType = "empty"
)

// ControllerFeatureHeartbeat advertises a live controller instance's memory feature epoch.
type ControllerFeatureHeartbeat struct {
	InstanceID      string    `json:"instanceId"`
	Role            string    `json:"role"`
	FeatureEpoch    int64     `json:"featureEpoch"`
	LastHeartbeatAt time.Time `json:"lastHeartbeatAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// MemoryBackendBinding is the durable authority and routing identity for a namespace incarnation.
type MemoryBackendBinding struct {
	Namespace               string                    `json:"namespace"`
	NamespaceUID            string                    `json:"namespaceUid"`
	ClusterID               string                    `json:"clusterId"`
	Mode                    MemoryBackendMode         `json:"mode"`
	BackendUID              string                    `json:"backendUid"`
	BackendGeneration       int64                     `json:"backendGeneration"`
	AuthorityEpoch          int64                     `json:"authorityEpoch"`
	RoutingEpoch            int64                     `json:"routingEpoch"`
	SpecDigest              string                    `json:"specDigest"`
	EndpointDigest          string                    `json:"endpointDigest"`
	ResolvedAddressDigest   string                    `json:"resolvedAddressDigest"`
	ServerCertificateDigest string                    `json:"serverCertificateDigest"`
	SecretName              string                    `json:"secretName"`
	SecretKey               string                    `json:"secretKey"`
	SecretUID               string                    `json:"secretUid"`
	SecretResourceVersion   string                    `json:"secretResourceVersion"`
	TenantID                string                    `json:"tenantId"`
	StoreName               string                    `json:"storeName"`
	StoreUUID               string                    `json:"storeUuid"`
	OwnershipClaim          string                    `json:"ownershipClaim"`
	CapabilityRevision      string                    `json:"capabilityRevision"`
	Protocol                string                    `json:"protocol"`
	State                   MemoryBackendBindingState `json:"state"`
	ActivationEpoch         int64                     `json:"activationEpoch"`
	MinimumFeatureEpoch     int64                     `json:"minimumFeatureEpoch"`
	ValidationExpiresAt     time.Time                 `json:"validationExpiresAt"`
	ActivatedAt             time.Time                 `json:"activatedAt"`
	UpdatedAt               time.Time                 `json:"updatedAt"`
	DecommissionedAt        *time.Time                `json:"decommissionedAt,omitempty"`
}

// MemoryBackendBindingFilter constrains safe binding discovery for dispatch/recovery.
type MemoryBackendBindingFilter struct {
	Modes              []MemoryBackendMode
	States             []MemoryBackendBindingState
	BeforeNamespaceUID string
	Limit              int
}

// MemoryBackendBindingVisitor receives each binding from a complete, ordered scan.
type MemoryBackendBindingVisitor func(MemoryBackendBinding) error

// MemoryBackendActivation atomically installs remote authority, archives legacy rows, and fences legacy writes.
type MemoryBackendActivation struct {
	Binding              MemoryBackendBinding `json:"binding"`
	RequiredFeatureEpoch int64                `json:"requiredFeatureEpoch"`
	Actor                string               `json:"actor"`
	Reason               string               `json:"reason"`
	RequestID            string               `json:"requestId,omitempty"`
	Now                  time.Time            `json:"now"`
}

// MemoryBackendActivationResult reports the installed binding and archived legacy row count.
type MemoryBackendActivationResult struct {
	Binding          MemoryBackendBinding `json:"binding"`
	ArchivedMemories int                  `json:"archivedMemories"`
	AlreadyActive    bool                 `json:"alreadyActive"`
}

// MemoryBackendTransition changes lifecycle/routing state with an epoch CAS. Accepting-to-Draining may preserve the current epoch to install a local admission barrier before old-route work drains.
type MemoryBackendTransition struct {
	NamespaceUID         string                    `json:"namespaceUid"`
	BackendUID           string                    `json:"backendUid"`
	ExpectedState        MemoryBackendBindingState `json:"expectedState"`
	State                MemoryBackendBindingState `json:"state"`
	ExpectedRoutingEpoch int64                     `json:"expectedRoutingEpoch"`
	RoutingEpoch         int64                     `json:"routingEpoch"`
	Actor                string                    `json:"actor"`
	Reason               string                    `json:"reason"`
	RequestID            string                    `json:"requestId,omitempty"`
	Now                  time.Time                 `json:"now"`
}

// MemoryBackendBindingRefresh updates fresh non-secret validation metadata under an authority/routing CAS.
type MemoryBackendBindingRefresh struct {
	Binding              MemoryBackendBinding `json:"binding"`
	ExpectedRoutingEpoch int64                `json:"expectedRoutingEpoch"`
	Actor                string               `json:"actor"`
	Reason               string               `json:"reason"`
	RequestID            string               `json:"requestId,omitempty"`
	Now                  time.Time            `json:"now"`
}

// LegacyMemoryRestorePreview is a non-mutating restore safety assessment.
type LegacyMemoryRestorePreview struct {
	Namespace             string     `json:"namespace"`
	NamespaceUID          string     `json:"namespaceUid"`
	BackendUID            string     `json:"backendUid"`
	AuthorityEpoch        int64      `json:"authorityEpoch"`
	RoutingEpoch          int64      `json:"routingEpoch"`
	TenantID              string     `json:"tenantId"`
	StoreName             string     `json:"storeName"`
	StoreUUID             string     `json:"storeUuid"`
	PreviewDigest         string     `json:"previewDigest,omitempty"`
	PreviewExpiresAt      *time.Time `json:"previewExpiresAt,omitempty"`
	ArchivedMemories      int        `json:"archivedMemories"`
	OldestCreatedAt       *time.Time `json:"oldestCreatedAt,omitempty"`
	NewestCreatedAt       *time.Time `json:"newestCreatedAt,omitempty"`
	ConflictingMemories   int        `json:"conflictingMemories"`
	RemoteDeletedMemories int        `json:"remoteDeletedMemories"`
	UnresolvedOperations  int        `json:"unresolvedOperations"`
	BlockingCatalogRows   int        `json:"blockingCatalogRows"`
	Restorable            bool       `json:"restorable"`
	Reason                string     `json:"reason,omitempty"`
}

// LegacyMemoryRestore requests an audited, clean return to legacy authority.
type LegacyMemoryRestore struct {
	NamespaceUID           string    `json:"namespaceUid"`
	BackendUID             string    `json:"backendUid"`
	ExpectedAuthorityEpoch int64     `json:"expectedAuthorityEpoch,omitempty"`
	ExpectedRoutingEpoch   int64     `json:"expectedRoutingEpoch,omitempty"`
	ExpectedTenantID       string    `json:"expectedTenantId,omitempty"`
	ExpectedStoreName      string    `json:"expectedStoreName,omitempty"`
	ExpectedStoreUUID      string    `json:"expectedStoreUuid,omitempty"`
	PreviewDigest          string    `json:"previewDigest,omitempty"`
	Actor                  string    `json:"actor"`
	Reason                 string    `json:"reason"`
	RequestID              string    `json:"requestId,omitempty"`
	Now                    time.Time `json:"now"`
}

// LegacyMemoryRestoreResult reports the restored row count and resulting binding.
type LegacyMemoryRestoreResult struct {
	Binding          MemoryBackendBinding `json:"binding"`
	RestoredMemories int                  `json:"restoredMemories"`
}

// RemoteMemoryCatalogEntry is retention-safe metadata for a remotely authoritative memory.
// It intentionally contains no raw memory content.
type RemoteMemoryCatalogEntry struct {
	ID                   string                     `json:"id"`
	Namespace            string                     `json:"namespace"`
	NamespaceUID         string                     `json:"namespaceUid"`
	ClusterID            string                     `json:"clusterId"`
	BackendUID           string                     `json:"backendUid"`
	AuthorityEpoch       int64                      `json:"authorityEpoch"`
	RoutingEpoch         int64                      `json:"routingEpoch"`
	TenantID             string                     `json:"tenantId"`
	StoreUUID            string                     `json:"storeUuid"`
	BackendMemoryID      string                     `json:"backendMemoryId,omitempty"`
	BackendVersion       string                     `json:"backendVersion,omitempty"`
	Generation           int64                      `json:"generation"`
	DesiredGeneration    int64                      `json:"desiredGeneration"`
	GovernanceRevision   int64                      `json:"governanceRevision"`
	MaterializationState MemoryMaterializationState `json:"materializationState"`
	Disabled             bool                       `json:"disabled"`
	Deleted              bool                       `json:"deleted"`
	Trust                MemoryTrust                `json:"trust"`
	SessionName          string                     `json:"sessionName,omitempty"`
	AgentName            string                     `json:"agentName,omitempty"`
	TaskName             string                     `json:"taskName,omitempty"`
	ParentTask           string                     `json:"parentTask,omitempty"`
	Source               string                     `json:"source"`
	SourceProposalID     string                     `json:"sourceProposalId,omitempty"`
	Tags                 []string                   `json:"tags,omitempty"`
	ContentDigest        string                     `json:"contentDigest,omitempty"`
	ContentAvailable     bool                       `json:"contentAvailable"`
	PendingOperationID   string                     `json:"pendingOperationId,omitempty"`
	CreatedAt            time.Time                  `json:"createdAt"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
	LastRecalledAt       *time.Time                 `json:"lastRecalledAt,omitempty"`
	RecalledCount        int                        `json:"recalledCount"`
}

// RemoteMemoryCatalogFilter constrains deterministic catalog listing.
type RemoteMemoryCatalogFilter struct {
	NamespaceUID    string
	IDs             []string
	States          []MemoryMaterializationState
	Trust           []MemoryTrust
	IncludeDisabled bool
	IncludeDeleted  bool
	BeforeUpdatedAt *time.Time
	BeforeID        string
	Limit           int
}

// MemoryMutationAdmission contains common caller, binding, payload, and retention data.
// MemoryID and OperationID must be allocated before the canonical payload and digests are computed.
type MemoryMutationAdmission struct {
	Namespace      string `json:"namespace"`
	NamespaceUID   string `json:"namespaceUid"`
	ClusterID      string `json:"clusterId"`
	BackendUID     string `json:"backendUid"`
	AuthorityEpoch int64  `json:"authorityEpoch"`
	RoutingEpoch   int64  `json:"routingEpoch"`
	MemoryID       string `json:"memoryId"`
	OperationID    string `json:"operationId"`
	// ProposalID is reserved for AdmitRemoteMemoryProposalApply; ordinary create, replace, and delete admissions clear it.
	ProposalID              string                        `json:"proposalId,omitempty"`
	Principal               string                        `json:"principal"`
	Route                   string                        `json:"route"`
	IdempotencyKey          string                        `json:"idempotencyKey"`
	RequestDigest           string                        `json:"requestDigest"`
	OperationIdempotencyKey string                        `json:"operationIdempotencyKey,omitempty"`
	MutationDigest          string                        `json:"mutationDigest"`
	ContentDigest           string                        `json:"contentDigest,omitempty"`
	Payload                 []byte                        `json:"-"`
	Actor                   string                        `json:"actor"`
	Reason                  string                        `json:"reason"`
	RequestID               string                        `json:"requestId,omitempty"`
	OriginalStatus          int                           `json:"originalStatus"`
	ResponseType            MemoryIdempotencyResponseType `json:"responseType"`
	Location                string                        `json:"location,omitempty"`
	RetryAfterSeconds       int                           `json:"retryAfterSeconds,omitempty"`
	Now                     time.Time                     `json:"now"`
	MaxAgeAt                time.Time                     `json:"maxAgeAt"`
	IdempotencyExpiresAt    time.Time                     `json:"idempotencyExpiresAt"`
}

// RemoteMemoryCreateAdmission atomically creates a pending catalog row and generation-one operation.
type RemoteMemoryCreateAdmission struct {
	Mutation MemoryMutationAdmission  `json:"mutation"`
	Memory   RemoteMemoryCatalogEntry `json:"memory"`
}

// RemoteMemoryReplaceAdmission atomically admits a full replacement using a materialized-generation CAS.
// Proposal identity is server-owned and is not attached to ordinary replacements.
type RemoteMemoryReplaceAdmission struct {
	Mutation               MemoryMutationAdmission  `json:"mutation"`
	Memory                 RemoteMemoryCatalogEntry `json:"memory"`
	ExpectedGeneration     int64                    `json:"expectedGeneration"`
	ExpectedBackendVersion string                   `json:"expectedBackendVersion,omitempty"`
}

// RemoteMemoryDeleteAdmission atomically installs a higher-generation local tombstone.
// Proposal identity is server-owned and is not attached to ordinary deletes.
type RemoteMemoryDeleteAdmission struct {
	Mutation               MemoryMutationAdmission `json:"mutation"`
	ExpectedGeneration     int64                   `json:"expectedGeneration"`
	ExpectedBackendVersion string                  `json:"expectedBackendVersion,omitempty"`
}

// RemoteMemoryProposalApplyAdmission atomically links accepted proposal application to a create operation.
type RemoteMemoryProposalApplyAdmission struct {
	Mutation  MemoryMutationAdmission  `json:"mutation"`
	Memory    RemoteMemoryCatalogEntry `json:"memory"`
	AppliedBy string                   `json:"appliedBy"`
}

// MemoryIdempotencyRecord is a compact replay record that never stores raw content.
type MemoryIdempotencyRecord struct {
	NamespaceUID          string                        `json:"namespaceUid"`
	Principal             string                        `json:"principal"`
	Route                 string                        `json:"route"`
	CallerKey             string                        `json:"callerKey"`
	RequestDigest         string                        `json:"requestDigest"`
	AuthorityEpoch        int64                         `json:"authorityEpoch"`
	RoutingEpoch          int64                         `json:"routingEpoch"`
	OriginalStatus        int                           `json:"originalStatus"`
	ResponseType          MemoryIdempotencyResponseType `json:"responseType"`
	MemoryID              string                        `json:"memoryId,omitempty"`
	OperationID           string                        `json:"operationId,omitempty"`
	Location              string                        `json:"location,omitempty"`
	RetryAfterSeconds     int                           `json:"retryAfterSeconds,omitempty"`
	ResponseDigest        string                        `json:"responseDigest,omitempty"`
	ResponseSnapshot      []byte                        `json:"-"`
	ExpiresAt             time.Time                     `json:"expiresAt"`
	TerminalBindingDigest string                        `json:"terminalBindingDigest,omitempty"`
	CreatedAt             time.Time                     `json:"createdAt"`
	UpdatedAt             time.Time                     `json:"updatedAt"`
}

// MemoryMutationAdmissionResult is returned for both first admission and exact caller replay.
type MemoryMutationAdmissionResult struct {
	Memory      RemoteMemoryCatalogEntry `json:"memory"`
	Operation   MemoryOperation          `json:"operation"`
	Idempotency MemoryIdempotencyRecord  `json:"idempotency"`
	Replayed    bool                     `json:"replayed"`
}

// MemoryOperation is a retention-safe operation summary. Payload is only exposed by MemoryOperationDispatch.
type MemoryOperation struct {
	Sequence                       int64                `json:"sequence"`
	ID                             string               `json:"id"`
	Namespace                      string               `json:"namespace"`
	NamespaceUID                   string               `json:"namespaceUid"`
	ClusterID                      string               `json:"clusterId"`
	BackendUID                     string               `json:"backendUid"`
	AuthorityEpoch                 int64                `json:"authorityEpoch"`
	RoutingEpoch                   int64                `json:"routingEpoch"`
	MemoryID                       string               `json:"memoryId"`
	ProposalID                     string               `json:"proposalId,omitempty"`
	Kind                           MemoryOperationKind  `json:"kind"`
	DesiredGeneration              int64                `json:"desiredGeneration"`
	ExpectedMaterializedGeneration int64                `json:"expectedMaterializedGeneration"`
	ExpectedBackendVersion         string               `json:"expectedBackendVersion,omitempty"`
	OperationIdempotencyKey        string               `json:"operationIdempotencyKey"`
	MutationIdempotencyKey         string               `json:"mutationIdempotencyKey"`
	RequestDigest                  string               `json:"requestDigest"`
	MutationDigest                 string               `json:"mutationDigest"`
	ContentDigest                  string               `json:"contentDigest,omitempty"`
	PayloadBytes                   int                  `json:"payloadBytes"`
	State                          MemoryOperationState `json:"state"`
	LeaseOwner                     string               `json:"-"`
	LeaseEpoch                     int64                `json:"leaseEpoch"`
	LeaseOriginState               MemoryOperationState `json:"-"`
	LeaseExpiresAt                 *time.Time           `json:"-"`
	SendStartedAt                  *time.Time           `json:"sendStartedAt,omitempty"` // Earliest durable send boundary; once set, provider application remains possible until disproved.
	RequestDeadline                *time.Time           `json:"requestDeadline,omitempty"`
	Dispatches                     int                  `json:"dispatches"`
	Attempts                       int                  `json:"attempts"`
	NextRetryAt                    time.Time            `json:"nextRetryAt"`
	MaxAgeAt                       time.Time            `json:"maxAgeAt"`
	PayloadRetainUntil             time.Time            `json:"payloadRetainUntil"`
	ErrorCode                      string               `json:"errorCode,omitempty"`
	ErrorMessage                   string               `json:"errorMessage,omitempty"`
	ReceiptBindingDigest           string               `json:"receiptBindingDigest,omitempty"`
	ReceiptAppliedGeneration       int64                `json:"receiptAppliedGeneration,omitempty"`
	ReceiptBackendVersion          string               `json:"receiptBackendVersion,omitempty"`
	ReceiptBackendMemoryID         string               `json:"receiptBackendMemoryId,omitempty"`
	ReceiptContentDigest           string               `json:"receiptContentDigest,omitempty"`
	ReceiptMutationDigest          string               `json:"receiptMutationDigest,omitempty"`
	ReceiptCompletedAt             *time.Time           `json:"receiptCompletedAt,omitempty"`
	Actor                          string               `json:"actor"`
	Reason                         string               `json:"reason,omitempty"`
	CreatedAt                      time.Time            `json:"createdAt"`
	UpdatedAt                      time.Time            `json:"updatedAt"`
	CompletedAt                    *time.Time           `json:"completedAt,omitempty"`
}

// MemoryOperationDispatch is the only store shape that exposes the bounded canonical payload.
type MemoryOperationDispatch struct {
	Operation MemoryOperation `json:"operation"`
	Payload   []byte          `json:"-"`
}

// MemoryOperationFilter constrains operation queries without exposing payloads.
type MemoryOperationFilter struct {
	NamespaceUID    string
	MemoryID        string
	ProposalID      string
	Kinds           []MemoryOperationKind
	States          []MemoryOperationState
	BeforeCreatedAt *time.Time
	BeforeSequence  int64
	Limit           int
}

// MemoryOperationClaim claims one due operation under the current binding identity.
type MemoryOperationClaim struct {
	NamespaceUID   string
	BackendUID     string
	AuthorityEpoch int64
	RoutingEpoch   int64
	LeaseOwner     string
	Now            time.Time
	LeaseDuration  time.Duration
	// AllowExpiredValidationMaintenance permits lease recovery and maximum-age
	// expiry under an otherwise exact binding after validation freshness lapses.
	// It never permits a new dispatch lease to be created.
	AllowExpiredValidationMaintenance bool
}

// MemoryOperationSend marks the durable send boundary before network I/O.
type MemoryOperationSend struct {
	NamespaceUID    string
	ID              string
	BackendUID      string
	AuthorityEpoch  int64
	RoutingEpoch    int64
	LeaseOwner      string
	LeaseEpoch      int64
	Now             time.Time
	RequestDeadline time.Time
}

// MemoryOperationReceipt is the allowlisted terminal adapter receipt.
type MemoryOperationReceipt struct {
	BindingIdentityDigest string    `json:"bindingIdentityDigest"`
	AppliedGeneration     int64     `json:"appliedGeneration"`
	BackendVersion        string    `json:"backendVersion,omitempty"`
	BackendMemoryID       string    `json:"backendMemoryId,omitempty"`
	ContentDigest         string    `json:"contentDigest,omitempty"`
	MutationDigest        string    `json:"mutationDigest"`
	CompletedAt           time.Time `json:"completedAt"`
}

// MemoryIdempotencyOutcome is a compact immediate/deferred response contract.
type MemoryIdempotencyOutcome struct {
	Status            int                           `json:"status"`
	ResponseType      MemoryIdempotencyResponseType `json:"responseType"`
	Location          string                        `json:"location,omitempty"`
	RetryAfterSeconds int                           `json:"retryAfterSeconds,omitempty"`
	ResponseDigest    string                        `json:"responseDigest,omitempty"`
}

// MemoryOperationCompletion completes an exact leased dispatch attempt.
type MemoryOperationCompletion struct {
	NamespaceUID               string
	ID                         string
	BackendUID                 string
	AuthorityEpoch             int64
	RoutingEpoch               int64
	LeaseOwner                 string
	LeaseEpoch                 int64
	Receipt                    MemoryOperationReceipt
	FinalizeIdempotencyOutcome bool
	IdempotencyOutcome         MemoryIdempotencyOutcome
	Now                        time.Time
	Actor                      string
	Reason                     string
	RequestID                  string
}

// MemoryOperationRetry reschedules, marks ambiguous, dead-letters, or manually retries the same operation.
// Manual dead-letter retries require the canonical payload to remain durably retained.
type MemoryOperationRetry struct {
	NamespaceUID   string
	ID             string
	BackendUID     string
	AuthorityEpoch int64
	RoutingEpoch   int64
	LeaseOwner     string
	LeaseEpoch     int64
	Ambiguous      bool
	DeadLetter     bool
	Manual         bool
	UnsentRelease  bool
	// DependencyFailure identifies an endpoint-, DNS-, TLS-, authentication-,
	// rate-limit-, or provider-wide outage. It advances dispatch telemetry but
	// does not consume the operation-specific semantic attempt budget.
	DependencyFailure bool
	ErrorCode         string
	ErrorMessage      string
	NextRetryAt       time.Time
	MaxAgeAt          time.Time
	Actor             string
	Reason            string
	RequestID         string
	Now               time.Time
}

// MemoryVerifiedCheckpoint is the durable, identity-bound proof that a remote
// adapter/content-store checkpoint covers operation sequences through the
// supplied watermark.
type MemoryVerifiedCheckpoint struct {
	ID                       string    `json:"id"`
	Namespace                string    `json:"namespace"`
	NamespaceUID             string    `json:"namespaceUid"`
	BackendUID               string    `json:"backendUid"`
	AuthorityEpoch           int64     `json:"authorityEpoch"`
	RoutingEpoch             int64     `json:"routingEpoch"`
	TenantID                 string    `json:"tenantId"`
	StoreUUID                string    `json:"storeUuid"`
	MaximumOperationSequence int64     `json:"maximumOperationSequence"`
	CheckpointDigest         string    `json:"checkpointDigest"`
	Actor                    string    `json:"actor"`
	Reason                   string    `json:"reason"`
	RequestID                string    `json:"requestId,omitempty"`
	VerifiedAt               time.Time `json:"verifiedAt"`
}

// MemoryRecoveryRouteIdentity contains the exact non-secret validated route
// identity that a matched pre-activation recovery set must cover. Lifecycle
// state and Kubernetes generation are intentionally excluded so a receipt
// recorded while Staged remains valid for the immediately following explicit
// Active request, while any endpoint, credential, store, capability, or
// protocol change invalidates it.
type MemoryRecoveryRouteIdentity struct {
	NamespaceUID            string
	BackendUID              string
	ClusterIdentityDigest   string
	EndpointDigest          string
	ResolvedAddressDigest   string
	ServerCertificateDigest string
	SecretName              string
	SecretKey               string
	SecretUID               string
	SecretResourceVersion   string
	StoreName               string
	StoreUUID               string
	CapabilityRevision      string
	Protocol                string
}

// Digest returns the canonical digest used to bind an activation recovery
// receipt to one exact validated route without exposing route metadata.
func (identity MemoryRecoveryRouteIdentity) Digest() string {
	values := []string{
		identity.NamespaceUID,
		identity.BackendUID,
		identity.ClusterIdentityDigest,
		identity.EndpointDigest,
		identity.ResolvedAddressDigest,
		identity.ServerCertificateDigest,
		identity.SecretName,
		identity.SecretKey,
		identity.SecretUID,
		identity.SecretResourceVersion,
		identity.StoreName,
		identity.StoreUUID,
		identity.CapabilityRevision,
		identity.Protocol,
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RecoveryRouteIdentity returns the exact non-secret route identity represented
// by a durable binding.
func (binding MemoryBackendBinding) RecoveryRouteIdentity() MemoryRecoveryRouteIdentity {
	clusterDigest := sha256.Sum256([]byte(binding.ClusterID))
	return MemoryRecoveryRouteIdentity{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ClusterIdentityDigest: "sha256:" + hex.EncodeToString(clusterDigest[:]),
		EndpointDigest:        binding.EndpointDigest, ResolvedAddressDigest: binding.ResolvedAddressDigest,
		ServerCertificateDigest: binding.ServerCertificateDigest,
		SecretName:              binding.SecretName, SecretKey: binding.SecretKey,
		SecretUID: binding.SecretUID, SecretResourceVersion: binding.SecretResourceVersion,
		StoreName: binding.StoreName, StoreUUID: binding.StoreUUID,
		CapabilityRevision: binding.CapabilityRevision, Protocol: binding.Protocol,
	}
}

// MemoryActivationRecoveryReceipt is the durable proof that an operator
// captured and verified a matched pre-activation recovery set for the exact
// validated route. Activation consumes only a fresh, exact match.
type MemoryActivationRecoveryReceipt struct {
	ID             string    `json:"id"`
	Namespace      string    `json:"namespace"`
	NamespaceUID   string    `json:"namespaceUid"`
	BackendUID     string    `json:"backendUid"`
	RouteDigest    string    `json:"routeDigest"`
	StoreUUID      string    `json:"storeUuid"`
	ManifestDigest string    `json:"manifestDigest"`
	Actor          string    `json:"actor"`
	Reason         string    `json:"reason"`
	RequestID      string    `json:"requestId,omitempty"`
	VerifiedAt     time.Time `json:"verifiedAt"`
}

// MemoryGovernancePurge performs one audited, identity-bound retention purge.
// Payload and receipt purge is limited to a verified checkpoint watermark.
type MemoryGovernancePurge struct {
	NamespaceUID             string
	BackendUID               string
	AuthorityEpoch           int64
	RoutingEpoch             int64
	StoreUUID                string
	CheckpointID             string
	MaximumOperationSequence int64
	Before                   time.Time
	PurgePayloads            bool
	PurgeReceipts            bool
	PurgeExpiredIdempotency  bool
	PurgeTombstones          bool
	PurgeAudit               bool
	Actor                    string
	Reason                   string
	RequestID                string
	Now                      time.Time
}

// MemoryGovernancePurgeResult reports only bounded aggregate effects.
type MemoryGovernancePurgeResult struct {
	PayloadsPurged    int64  `json:"payloadsPurged"`
	ReceiptsPurged    int64  `json:"receiptsPurged"`
	IdempotencyPurged int64  `json:"idempotencyPurged"`
	TombstonesPurged  int64  `json:"tombstonesPurged"`
	AuditRowsPurged   int64  `json:"auditRowsPurged"`
	PurgeDigest       string `json:"purgeDigest"`
}

// MemoryOperationAbandonment terminally abandons a dead letter after a durable fence; any mutation that crossed a send boundary also requires ProviderNeverApplied proof.
type MemoryOperationAbandonment struct {
	NamespaceUID         string
	ID                   string
	BackendUID           string
	AuthorityEpoch       int64
	RoutingEpoch         int64
	Actor                string
	Reason               string
	RequestID            string
	ProviderNeverApplied bool
	Fenced               bool
	Now                  time.Time
}

// MemoryOperationOrphaning force-orphans unresolved operations only after the
// binding has entered a state that no longer permits delete dispatch.
type MemoryOperationOrphaning struct {
	NamespaceUID   string
	BackendUID     string
	AuthorityEpoch int64
	RoutingEpoch   int64
	Actor          string
	Reason         string
	RequestID      string
	Now            time.Time
}

// MemoryDecommissionResolution terminally resolves all remaining operations
// after the exact remote routing fence has been acknowledged. Provably unsent
// operations become abandoned; operations that may have crossed the send
// boundary become orphaned without asserting a provider outcome.
type MemoryDecommissionResolution struct {
	NamespaceUID   string
	BackendUID     string
	AuthorityEpoch int64
	RoutingEpoch   int64
	Actor          string
	Reason         string
	RequestID      string
	Now            time.Time
}

// RemoteMemoryMaterializationIssue fail-closes a verified hydration mismatch or missing materialization.
type RemoteMemoryMaterializationIssue struct {
	NamespaceUID           string
	ID                     string
	BackendUID             string
	AuthorityEpoch         int64
	RoutingEpoch           int64
	ExpectedGeneration     int64
	ExpectedBackendVersion string
	State                  MemoryMaterializationState
	Actor                  string
	Reason                 string
	RequestID              string
	Now                    time.Time
}

// RemoteMemoryDisabledChange changes only the local governance overlay.
type RemoteMemoryDisabledChange struct {
	NamespaceUID               string
	ID                         string
	Disabled                   bool
	ExpectedGovernanceRevision int64
	Actor                      string
	Reason                     string
	RequestID                  string
	Now                        time.Time
}

// RemoteMemoryTrustChange changes only the server-owned trust classification.
type RemoteMemoryTrustChange struct {
	NamespaceUID               string
	ID                         string
	Trust                      MemoryTrust
	ExpectedGovernanceRevision int64
	Actor                      string
	Reason                     string
	RequestID                  string
	Now                        time.Time
}

// MaxMemorySearchCursorStateBytes is the immutable hard cap for opaque,
// short-lived server-side pagination state. Namespace and global byte quotas
// remain the primary aggregate admission bounds.
const MaxMemorySearchCursorStateBytes = 128 << 10

// MemorySearchCursorState is opaque, short-lived server-side pagination state.
type MemorySearchCursorState struct {
	ID            string
	NamespaceUID  string
	BindingDigest string
	QueryDigest   string
	State         []byte
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

// MemoryAuditRecord is immutable, compact, and excludes content, queries, credentials, endpoints, and provider bodies.
type MemoryAuditRecord struct {
	ID                   string    `json:"id"`
	Namespace            string    `json:"namespace"`
	NamespaceUID         string    `json:"namespaceUid"`
	Actor                string    `json:"actor"`
	Action               string    `json:"action"`
	Reason               string    `json:"reason,omitempty"`
	PreviousState        string    `json:"previousState,omitempty"`
	NewState             string    `json:"newState,omitempty"`
	AuthorityEpoch       int64     `json:"authorityEpoch"`
	PreviousRoutingEpoch int64     `json:"previousRoutingEpoch"`
	RoutingEpoch         int64     `json:"routingEpoch"`
	MemoryID             string    `json:"memoryId,omitempty"`
	OperationID          string    `json:"operationId,omitempty"`
	ProposalID           string    `json:"proposalId,omitempty"`
	RequestDigest        string    `json:"requestDigest,omitempty"`
	MutationDigest       string    `json:"mutationDigest,omitempty"`
	ContentDigest        string    `json:"contentDigest,omitempty"`
	RequestID            string    `json:"requestId,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
}

// MemoryAuditFilter constrains immutable audit listing.
type MemoryAuditFilter struct {
	NamespaceUID    string
	MemoryID        string
	OperationID     string
	ProposalID      string
	BeforeCreatedAt *time.Time
	BeforeID        string
	Limit           int
}
