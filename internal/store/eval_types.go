package store

import storagev1 "github.com/orka-agents/orka/pkg/storage/v1"

// Eval trial records are defined by the public storage contract in
// pkg/storage/v1.
type (
	EvalTrialRecord   = storagev1.EvalTrialRecord
	EvalTrialGrade    = storagev1.EvalTrialGrade
	EvalTrialSnapshot = storagev1.EvalTrialSnapshot
	EvalTrialStore    = storagev1.EvalTrialStore

	EvalExperimentRecord      = storagev1.EvalExperimentRecord
	EvalExperimentLimits      = storagev1.EvalExperimentLimits
	EvalExperimentReservation = storagev1.EvalExperimentReservation
	EvalExperimentStore       = storagev1.EvalExperimentStore
)

const (
	EvalTrialSchemaVersion     = storagev1.EvalTrialSchemaVersion
	EvalTrialOutcomePass       = storagev1.EvalTrialOutcomePass
	EvalTrialOutcomeFail       = storagev1.EvalTrialOutcomeFail
	EvalTrialOutcomeInfraError = storagev1.EvalTrialOutcomeInfraError
	MaxEvalTrialRecordBytes    = storagev1.MaxEvalTrialRecordBytes

	EvalExperimentSchemaVersion = storagev1.EvalExperimentSchemaVersion
)
