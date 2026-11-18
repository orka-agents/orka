package kube

import (
	"context"
	"strings"

	"github.com/google/uuid"
	pv "github.com/orka-agents/orka/internal/patchverification"
)

// Legacy-shaped records are fixtures for the shared evidence engine, not a
// production admission path. Production submissions require worker preparation.
func (s *Service) Submit(ctx context.Context, namespace, submittedBy string, manifest pv.Manifest, provenance map[string][]byte) (*pv.KubernetesSubmission, error) {
	requestID := "vr-" + uuid.NewString()
	suffix := strings.ReplaceAll(requestID[3:], "-", "")[:16]
	submission := pv.KubernetesSubmission{Namespace: namespace, RequestID: requestID,
		SubmittedBy: submittedBy, AttemptID: uuid.NewString(), OriginalTaskName: "pv-" + suffix + "-original",
		Manifest: manifest, Provenance: provenance, State: pv.SubmissionPreparing}
	if manifest.Action == pv.VerifyPatch {
		submission.PatchedTaskName = "pv-" + suffix + "-patched"
	}
	if err := s.store.CreateKubernetesValidationSubmission(ctx, submission); err != nil {
		return nil, err
	}
	return s.GetSubmission(ctx, namespace, requestID)
}
