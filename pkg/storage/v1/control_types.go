package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxControlIdentifierBytes = 1024
)

type ControllerEpochFence struct {
	Name     string `json:"name"`
	Epoch    int64  `json:"epoch"`
	HolderID string `json:"holderId"`
}

type ControllerEpoch struct {
	Name          string    `json:"name"`
	Epoch         int64     `json:"epoch"`
	HolderID      string    `json:"holderId"`
	RequestDigest string    `json:"requestDigest"`
	Version       int64     `json:"version"`
	AcquiredAt    time.Time `json:"acquiredAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type VerifiedBranchBaseline struct {
	RepositoryID string `json:"repositoryId"`
	Ref          string `json:"ref"`
	SHA          string `json:"sha"`
}

type SessionTurnState string

const (
	SessionTurnOpen      SessionTurnState = "Open"
	SessionTurnFinalized SessionTurnState = "Finalized"
)

type SessionTurnTerminalKind string

const (
	SessionTurnAssistantResult SessionTurnTerminalKind = "AssistantResult"
	SessionTurnOutcomeMarker   SessionTurnTerminalKind = "OutcomeMarker"
)

type SessionTurnKey struct {
	SessionUID      string `json:"sessionUid"`
	LeaseGeneration int64  `json:"leaseGeneration"`
	TaskUID         string `json:"taskUid"`
	Attempt         int64  `json:"attempt"`
	PromptID        string `json:"promptId"`
}

func (k SessionTurnKey) CanonicalID() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	return CanonicalControlID("session-turn", k.SessionUID, strconv.FormatInt(k.LeaseGeneration, 10), k.TaskUID, strconv.FormatInt(k.Attempt, 10), k.PromptID), nil
}

func (k SessionTurnKey) Validate() error {
	if err := validateControlIdentifier("session UID", k.SessionUID); err != nil {
		return err
	}
	if k.LeaseGeneration < 1 {
		return ValidationErrorf("session lease generation must be at least 1")
	}
	if err := validateControlIdentifier("task UID", k.TaskUID); err != nil {
		return err
	}
	if k.Attempt < 1 {
		return ValidationErrorf("session turn attempt must be at least 1")
	}
	return validateControlIdentifier("prompt ID", k.PromptID)
}

type SessionTurn struct {
	ID                    string                  `json:"id"`
	Key                   SessionTurnKey          `json:"key"`
	PromptAttemptID       string                  `json:"promptAttemptId"`
	RequestDigest         string                  `json:"requestDigest"`
	UserPrompt            string                  `json:"userPrompt"`
	State                 SessionTurnState        `json:"state"`
	TerminalKind          SessionTurnTerminalKind `json:"terminalKind,omitempty"`
	TerminalContent       string                  `json:"terminalContent,omitempty"`
	FinalizationDigest    string                  `json:"finalizationDigest,omitempty"`
	NativeSessionDigest   string                  `json:"nativeSessionDigest,omitempty"`
	PublicationID         string                  `json:"publicationId,omitempty"`
	PublicationReceipt    *PublicationReceipt     `json:"publicationReceipt,omitempty"`
	ProjectionID          string                  `json:"projectionId,omitempty"`
	ProjectionKind        string                  `json:"projectionKind,omitempty"`
	ProjectionDigest      string                  `json:"projectionDigest,omitempty"`
	ProjectionAvailableAt time.Time               `json:"projectionAvailableAt,omitempty"`
	ControllerEpochName   string                  `json:"controllerEpochName"`
	ControllerEpoch       int64                   `json:"controllerEpoch"`
	Version               int64                   `json:"version"`
	CreatedAt             time.Time               `json:"createdAt"`
	FinalizedAt           *time.Time              `json:"finalizedAt,omitempty"`
	UpdatedAt             time.Time               `json:"updatedAt"`
}

type BranchClaimAvailability string

const (
	BranchClaimAvailable             BranchClaimAvailability = "Available"
	BranchClaimReconciliationBlocked BranchClaimAvailability = "ReconciliationBlocked"
)

type RemoteRefState struct {
	Absent bool   `json:"absent"`
	SHA    string `json:"sha,omitempty"`
}

func (r RemoteRefState) Equal(other RemoteRefState) bool {
	return r.Absent == other.Absent && r.SHA == other.SHA
}

func (r RemoteRefState) Validate(field string) error {
	if r.Absent == (r.SHA != "") {
		return ValidationErrorf("%s must set exactly one of absent or SHA", field)
	}
	if r.SHA != "" {
		return ValidateGitObjectID(field+" SHA", r.SHA)
	}
	return nil
}

type PublicationState string

const (
	PublicationPreparing              PublicationState = "Preparing"
	PublicationPrepared               PublicationState = "Prepared"
	PublicationPublishing             PublicationState = "Publishing"
	PublicationVerifying              PublicationState = "Verifying"
	PublicationVerifiedExact          PublicationState = "VerifiedExact"
	PublicationDeliveredSuperseded    PublicationState = "DeliveredSuperseded"
	PublicationCancelledBeforePublish PublicationState = "CancelledBeforePublish"
	PublicationDeliveryConflict       PublicationState = "DeliveryConflict"
	PublicationCredentialBlocked      PublicationState = "CredentialBlocked"
	PublicationPreparationFailed      PublicationState = "PreparationFailed"
	PublicationOutcomeUnknown         PublicationState = "PublicationOutcomeUnknown"
)

func IsTerminalPublicationState(state PublicationState) bool {
	switch state {
	case PublicationVerifiedExact, PublicationDeliveredSuperseded,
		PublicationCancelledBeforePublish, PublicationDeliveryConflict,
		PublicationCredentialBlocked, PublicationPreparationFailed, PublicationOutcomeUnknown:
		return true
	default:
		return false
	}
}

type PullRequestIntent struct {
	BaseRepositoryID      string `json:"baseRepositoryId"`
	BaseRef               string `json:"baseRef"`
	HeadRepositoryID      string `json:"headRepositoryId"`
	HeadRef               string `json:"headRef"`
	PublicationGeneration int64  `json:"publicationGeneration"`
	ExpectedHeadSHA       string `json:"expectedHeadSha"`
}

type PreparedPublicationReceipt struct {
	OperationID      string    `json:"operationId"`
	RequestDigest    string    `json:"requestDigest"`
	TreeSHA          string    `json:"treeSha"`
	CommitSHA        string    `json:"commitSha"`
	ManifestDigest   string    `json:"manifestDigest"`
	RelativeRoot     string    `json:"relativeRoot,omitempty"`
	BundleArtifactID string    `json:"bundleArtifactId"`
	BundleDigest     string    `json:"bundleDigest"`
	BundleSizeBytes  int64     `json:"bundleSizeBytes"`
	BundleMediaType  string    `json:"bundleMediaType"`
	BundleRef        string    `json:"bundleRef"`
	PreparedAt       time.Time `json:"preparedAt"`
}

type PublishOperationReceipt struct {
	OperationID            string         `json:"operationId"`
	RequestDigest          string         `json:"requestDigest"`
	TargetRepositoryID     string         `json:"targetRepositoryId"`
	TargetRef              string         `json:"targetRef"`
	RemoteBefore           RemoteRefState `json:"remoteBefore"`
	ExpectedCommitSHA      string         `json:"expectedCommitSha"`
	AcknowledgementUnknown bool           `json:"acknowledgementUnknown"`
	PublishedAt            time.Time      `json:"publishedAt"`
}

type PublicationVerificationReceipt struct {
	OperationID           string           `json:"operationId"`
	RequestDigest         string           `json:"requestDigest"`
	Outcome               PublicationState `json:"outcome"`
	ExpectedCommitSHA     string           `json:"expectedCommitSha"`
	ObservedRemote        RemoteRefState   `json:"observedRemote"`
	DescendantProofDigest string           `json:"descendantProofDigest,omitempty"`
	VerifiedAt            time.Time        `json:"verifiedAt"`
}

type PublicationReceipt struct {
	PublicationID string                          `json:"publicationId"`
	Generation    int64                           `json:"generation"`
	State         PublicationState                `json:"state"`
	Prepared      *PreparedPublicationReceipt     `json:"prepared,omitempty"`
	Publish       *PublishOperationReceipt        `json:"publish,omitempty"`
	Verification  *PublicationVerificationReceipt `json:"verification,omitempty"`
	PullRequest   *PullRequestOperationReceipt    `json:"pullRequest,omitempty"`
}

type PullRequestOperationReceipt struct {
	OperationID   string    `json:"operationId"`
	RequestDigest string    `json:"requestDigest"`
	IntentKey     string    `json:"intentKey"`
	ForgeID       string    `json:"forgeId"`
	URL           string    `json:"url"`
	State         string    `json:"state"`
	HeadSHA       string    `json:"headSha"`
	ReconciledAt  time.Time `json:"reconciledAt"`
}

type OutboxProjectionState string

const (
	OutboxProjectionPending    OutboxProjectionState = "Pending"
	OutboxProjectionDelivering OutboxProjectionState = "Delivering"
	OutboxProjectionDelivered  OutboxProjectionState = "Delivered"
	OutboxProjectionDeadLetter OutboxProjectionState = "DeadLetter"
)

type OutboxProjection struct {
	ID                  string                `json:"id"`
	AggregateKind       string                `json:"aggregateKind"`
	AggregateID         string                `json:"aggregateId"`
	ProjectionKind      string                `json:"projectionKind"`
	PayloadDigest       string                `json:"payloadDigest"`
	Payload             json.RawMessage       `json:"payload"`
	State               OutboxProjectionState `json:"state"`
	Attempts            int64                 `json:"attempts"`
	InitialAvailableAt  time.Time             `json:"initialAvailableAt"`
	AvailableAt         time.Time             `json:"availableAt"`
	LeaseOwner          string                `json:"leaseOwner,omitempty"`
	LeaseExpiresAt      *time.Time            `json:"leaseExpiresAt,omitempty"`
	DeliveryDigest      string                `json:"deliveryDigest,omitempty"`
	LastError           string                `json:"lastError,omitempty"`
	LastOperationID     string                `json:"lastOperationId,omitempty"`
	LastOperationDigest string                `json:"lastOperationDigest,omitempty"`
	ControllerEpochName string                `json:"controllerEpochName"`
	ControllerEpoch     int64                 `json:"controllerEpoch"`
	Version             int64                 `json:"version"`
	CreatedAt           time.Time             `json:"createdAt"`
	UpdatedAt           time.Time             `json:"updatedAt"`
	DeliveredAt         *time.Time            `json:"deliveredAt,omitempty"`
}

type ClaimOutboxProjectionsRequest struct {
	Fence         ControllerEpochFence `json:"fence"`
	WorkerID      string               `json:"workerId"`
	Limit         int                  `json:"limit"`
	LeaseDuration time.Duration        `json:"leaseDuration"`
	Now           time.Time            `json:"now"`
}

type CompleteOutboxProjectionRequest struct {
	ID              string                `json:"id"`
	Fence           ControllerEpochFence  `json:"fence"`
	ExpectedVersion int64                 `json:"expectedVersion"`
	LeaseOwner      string                `json:"leaseOwner"`
	OperationID     string                `json:"operationId"`
	OperationDigest string                `json:"operationDigest"`
	NewState        OutboxProjectionState `json:"newState"`
	DeliveryDigest  string                `json:"deliveryDigest,omitempty"`
	LastError       string                `json:"lastError,omitempty"`
	AvailableAt     time.Time             `json:"availableAt,omitempty"`
	UpdatedAt       time.Time             `json:"updatedAt"`
}

func CanonicalControlID(kind string, components ...string) string {
	h := sha256.New()
	writeCanonicalComponent(h, kind)
	for _, component := range components {
		writeCanonicalComponent(h, component)
	}
	return kind + ":sha256:" + hex.EncodeToString(h.Sum(nil))
}

func writeCanonicalComponent(w hash.Hash, value string) {
	_, _ = fmt.Fprintf(w, "%d:", len(value))
	_, _ = w.Write([]byte(value))
}

func ValidateCanonicalDigest(field, value string) error {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return ValidationErrorf("%s must be a canonical sha256 digest", field)
	}
	encoded := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(encoded) != encoded {
		return ValidationErrorf("%s must use lowercase hexadecimal", field)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return ValidationErrorf("%s must be a canonical sha256 digest", field)
	}
	return nil
}

func ValidateGitObjectID(field, value string) error {
	if len(value) != 40 && len(value) != 64 {
		return ValidationErrorf("%s must be a 40- or 64-character Git object ID", field)
	}
	if strings.ToLower(value) != value {
		return ValidationErrorf("%s must use lowercase hexadecimal", field)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ValidationErrorf("%s must be a hexadecimal Git object ID", field)
	}
	return nil
}

func ValidateControlIdentifier(field, value string) error {
	return validateControlIdentifier(field, value)
}

func validateControlIdentifier(field, value string) error {
	if value == "" {
		return ValidationErrorf("%s is required", field)
	}
	if strings.TrimSpace(value) != value {
		return ValidationErrorf("%s must not contain surrounding whitespace", field)
	}
	if len(value) > MaxControlIdentifierBytes {
		return ValidationErrorf("%s exceeds %d bytes", field, MaxControlIdentifierBytes)
	}
	if !utf8.ValidString(value) {
		return ValidationErrorf("%s must be valid UTF-8", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ValidationErrorf("%s contains a control character", field)
		}
	}
	return nil
}

const (
	DefaultControllerEpochName = "orka-controller"
	maxControlReasonBytes      = 16 * 1024
	maxControlPayloadBytes     = 4 * 1024 * 1024
)

func ValidateFullBranchRef(ref string) error {
	if err := validateControlIdentifier("branch ref", ref); err != nil {
		return err
	}
	if !strings.HasPrefix(ref, "refs/heads/") || len(ref) == len("refs/heads/") {
		return ValidationErrorf("branch ref must be a full refs/heads/<branch> ref")
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return ValidationErrorf("branch ref %q is not canonical", ref)
	}
	return nil
}

func ValidateControlReason(field, value string) error {
	return validateControlReason(field, value)
}

func ValidateControlPayload(field string, payload []byte) error {
	return validateControlPayload(field, payload)
}

func validateControlReason(field, value string) error {
	if len(value) > maxControlReasonBytes {
		return ValidationErrorf("%s exceeds %d bytes", field, maxControlReasonBytes)
	}
	if !utf8.ValidString(value) {
		return ValidationErrorf("%s must be valid UTF-8", field)
	}
	return nil
}

func validateControlPayload(field string, payload []byte) error {
	if len(payload) == 0 {
		return ValidationErrorf("%s is required", field)
	}
	if len(payload) > maxControlPayloadBytes {
		return ValidationErrorf("%s exceeds %d bytes", field, maxControlPayloadBytes)
	}
	if !json.Valid(payload) {
		return ValidationErrorf("%s must be valid JSON", field)
	}
	return nil
}
