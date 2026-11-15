package store

import (
	"context"
	"encoding/json"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// NativeSessionRecord is private provider continuity state. SessionUID is the
// canonical Orka SessionControl identity, distinct from provider/runtime IDs.
type NativeSessionRecord struct {
	Namespace                string                          `json:"namespace"`
	SessionName              string                          `json:"sessionName"`
	SessionUID               string                          `json:"sessionUID"`
	Snapshot                 harnessv2.NativeSessionSnapshot `json:"snapshot"`
	ThroughMessageID         string                          `json:"throughMessageID,omitempty"`
	MessageCount             int                             `json:"messageCount"`
	RuntimeSessionGeneration int64                           `json:"runtimeSessionGeneration"`
	SourceOperationID        string                          `json:"sourceOperationID"`
	CreatedAt                time.Time                       `json:"createdAt"`
}

// NativeSessionStore persists encrypted native state independently of public
// Task artifacts and Session projections. Reads require the exact canonical
// Session UID and transcript boundary. Empty UID reads expose only a staged,
// not-yet-bound import to the authorized migration export API. The storage
// client implements it and seals records before a provider sees them.
type NativeSessionStore interface {
	SaveNativeSession(ctx context.Context, record NativeSessionRecord) error
	GetNativeSession(ctx context.Context, namespace, sessionName, sessionUID string) (*NativeSessionRecord, error)
}

// NativeSessionImport reserves a fresh Session name and stages verified state
// for its first real Task; it does not allocate a runtime or mutation lease.
type NativeSessionImport struct {
	Namespace     string
	SessionName   string
	OperationID   string
	RequestDigest string
	Snapshot      harnessv2.NativeSessionSnapshot
}

type NativeSessionImportStore interface {
	NativeSessionStore
	StageNativeSessionImport(ctx context.Context, request NativeSessionImport) (*NativeSessionImportReceipt, error)
}

// NativeSessionImportDigest binds retries to one target and exact bundle.
func NativeSessionImportDigest(namespace, name, dataDigest string) string {
	body, _ := json.Marshal(struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Digest    string `json:"digest"`
	}{namespace, name, dataDigest})
	return CanonicalBytesDigest(body)
}

// NativeSessionCaptureDigest binds immutable finalization input. It excludes
// the transcript boundary, which the finalization itself produces.
func NativeSessionCaptureDigest(record *NativeSessionRecord) string {
	if record == nil {
		return ""
	}
	canonical := *record
	canonical.CreatedAt = time.Time{}
	canonical.MessageCount = 0
	canonical.ThroughMessageID = ""
	body, _ := json.Marshal(canonical)
	return CanonicalBytesDigest(body)
}

// NativeSessionFinalizationSeal asks the storage client to seal a turn
// finalization's native state against the transcript boundary the
// finalization produces.
type NativeSessionFinalizationSeal struct {
	// Native is the plaintext capture, or the checkpoint carried across a
	// non-success outcome marker.
	Native  NativeSessionRecord
	Carried bool
	// Stored is the Session's stored native record. A carried checkpoint
	// requires it and must match it.
	Stored *storagev1.SealedNativeSession
	// MessageCount and ThroughMessageID are the transcript boundary after the
	// finalization appends its messages.
	MessageCount     int
	ThroughMessageID string
}

// NativeSessionFinalizationSealer seals the native state of a turn
// finalization. The storage client implements it and uses it for every
// production finalization. SQLite's duplicate SQL control implementation,
// which tests use in place of the Kubernetes control store, calls it inside
// its own transaction because it finalizes turns itself. The result leaves
// the expected transcript boundary for the caller to set.
type NativeSessionFinalizationSealer interface {
	SealNativeSessionFinalization(ctx context.Context, request NativeSessionFinalizationSeal) (*storagev1.SealedNativeSessionFinalization, error)
}
