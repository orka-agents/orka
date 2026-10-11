package store

import (
	"context"
	"encoding/json"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
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
// not-yet-bound import to the authorized migration export API.
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

type NativeSessionImportReceipt struct {
	Namespace         string    `json:"namespace"`
	SessionName       string    `json:"sessionName"`
	OperationID       string    `json:"operationID"`
	RequestDigest     string    `json:"requestDigest"`
	DataDigest        string    `json:"dataDigest"`
	ProviderSessionID string    `json:"providerSessionID"`
	CreatedAt         time.Time `json:"createdAt"`
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

// NativeSessionCaptureDigest binds immutable finalization input. SQLite derives
// the canonical transcript boundary after appending the terminal messages.
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
