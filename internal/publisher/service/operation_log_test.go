package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/orka-agents/orka/internal/publisher"
)

func TestFailedOperationLogsOnlyPublicErrorFields(t *testing.T) {
	var logs bytes.Buffer
	fixture := newServiceFixtureWithOptions(t, nil, func(config *Config) {
		config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})
	_, err := fixture.client.ReclaimPublication(context.Background(), PublicationReclaimRequest{
		Metadata: OperationMetadata{Namespace: testNamespace, OperationID: "reclaim-logged", PublicationID: "publication-other"},
		Request:  publisher.ReclaimRequest{PublicationID: "publication-logged", PublicationGeneration: 1},
	})
	assertClientCode(t, err, "invalid_request")

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("failed operation log = %q: %v", logs.String(), err)
	}
	if entry["level"] != "WARN" || entry["operation"] != string(OperationPublicationReclaim) ||
		entry["operationId"] != "reclaim-logged" || entry["code"] != "invalid_request" || entry["status"] != float64(400) {
		t.Fatalf("failed operation log = %v", entry)
	}
	for _, secret := range [][]byte{fixture.bearer, fixture.operationSecret, fixture.artifactSecret} {
		if bytes.Contains(logs.Bytes(), secret) {
			t.Fatalf("failed operation log leaked secret material: %q", logs.String())
		}
	}
}
