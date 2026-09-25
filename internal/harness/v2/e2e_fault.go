package v2

import (
	"fmt"
	"strings"
	"time"
)

const E2EPromptWriteAmbiguityRecordPath = "/internal/v2/acp/e2e/prompt-write-ambiguity-records"

// E2EPromptWriteAmbiguityRecordRequest asks the controller to atomically
// consume the test-only prompt-write fault for one supervisor operation.
type E2EPromptWriteAmbiguityRecordRequest struct {
	Namespace         string           `json:"namespace"`
	Metadata          MutationMetadata `json:"metadata"`
	PromptOperationID OperationID      `json:"promptOperationID"`
}

func (r E2EPromptWriteAmbiguityRecordRequest) ValidateAt(now time.Time) error {
	if strings.TrimSpace(r.Namespace) == "" || strings.TrimSpace(r.Namespace) != r.Namespace {
		return fmt.Errorf("namespace is required without surrounding whitespace")
	}
	if r.Metadata.TaskUID == "" || r.Metadata.TaskAttempt == 0 || r.Metadata.PromptID == "" {
		return fmt.Errorf("task and prompt identity are required")
	}
	if err := r.Metadata.ValidateAt(now); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("prompt operation ID", string(r.PromptOperationID)); err != nil {
		return err
	}
	return nil
}

type E2EPromptWriteAmbiguityRecordResponse struct {
	Inject bool `json:"inject"`
}
