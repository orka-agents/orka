package patchverification

import (
	"encoding/json"

	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
)

type RunState = verificationv1.RunState

const (
	RunRunning     RunState = verificationv1.RunRunning
	RunFinalized   RunState = verificationv1.RunFinalized
	RunCancelled   RunState = verificationv1.RunCancelled
	RunInterrupted RunState = verificationv1.RunInterrupted
	RunInvalid     RunState = verificationv1.RunInvalid

	MaxManifestBytes    = verificationv1.MaxManifestBytes
	MaxObservationBytes = verificationv1.MaxObservationBytes
	MaxOutputBytes      = verificationv1.MaxOutputBytes
	MaxProvenanceBytes  = verificationv1.MaxProvenanceBytes
	MaxRunBlobBytes     = verificationv1.MaxRunBlobBytes
	MaxFrozenFiles      = verificationv1.MaxFrozenFiles
	MaxServices         = verificationv1.MaxServices
	MaxIncidents        = verificationv1.MaxIncidents
)

var (
	ErrRunNotFound = verificationv1.ErrRunNotFound
	ErrBinding     = verificationv1.ErrBinding
	ErrConflict    = verificationv1.ErrConflict
	ErrClosed      = verificationv1.ErrClosed
	ErrIntegrity   = verificationv1.ErrIntegrity
	ErrLimit       = verificationv1.ErrLimit
	ErrEvidence    = verificationv1.ErrEvidence
)

type BlobReference = verificationv1.BlobReference

type EvidenceRecord = verificationv1.EvidenceRecord

type Incident = verificationv1.Incident

type Seal = verificationv1.Seal

type Record = verificationv1.Record

func NewRunBinding(manifest Manifest, attemptID, originalTaskID, patchedTaskID string) (Binding, error) {
	if err := ValidateManifest(manifest); err != nil {
		return Binding{}, err
	}
	digest, err := ManifestDigest(manifest)
	if err != nil {
		return Binding{}, err
	}
	binding := Binding{AttemptID: attemptID, OriginalTaskID: originalTaskID, PatchedTaskID: patchedTaskID, ManifestDigest: digest}
	binding.RunID = deterministicRunID(binding)
	return binding, ValidateRunBinding(manifest, binding)
}

func ValidateRunBinding(manifest Manifest, binding Binding) error {
	digest, err := ManifestDigest(manifest)
	if err != nil {
		return err
	}
	if ValidateBinding(manifest.Action, binding) != nil || !identifierPattern.MatchString(binding.OriginalTaskID) ||
		(binding.PatchedTaskID != "" && !identifierPattern.MatchString(binding.PatchedTaskID)) ||
		binding.ManifestDigest != digest || binding.RunID != deterministicRunID(binding) {
		return ErrBinding
	}
	return nil
}

func deterministicRunID(binding Binding) string {
	binding.RunID = ""
	content, _ := json.Marshal(binding)
	return "pv-" + Digest(content)[len("sha256:"):]
}
