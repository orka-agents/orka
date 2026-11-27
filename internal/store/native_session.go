package store

import (
	"context"
	"encoding/json"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

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

type NativeSessionStore interface {
	SaveNativeSession(ctx context.Context, record NativeSessionRecord) error
	GetNativeSession(ctx context.Context, namespace, sessionName, sessionUID string) (*NativeSessionRecord, error)
}

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

func NativeSessionImportDigest(namespace, name, dataDigest string) string {
	body, _ := json.Marshal(struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Digest    string `json:"digest"`
	}{namespace, name, dataDigest})
	return CanonicalBytesDigest(body)
}

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

type NativeSessionFinalizationSeal struct {
	Native           NativeSessionRecord
	Carried          bool
	Stored           *storagev1.SealedNativeSession
	MessageCount     int
	ThroughMessageID string
}

type NativeSessionFinalizationSealer interface {
	SealNativeSessionFinalization(ctx context.Context, request NativeSessionFinalizationSeal) (*storagev1.SealedNativeSessionFinalization, error)
}
