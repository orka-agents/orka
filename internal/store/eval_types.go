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

	EvalAgentIdentity           = storagev1.EvalAgentIdentity
	EvalDecisionBinding         = storagev1.EvalDecisionBinding
	EvalExperimentTrialOutcome  = storagev1.EvalExperimentTrialOutcome
	EvalExperimentGrade         = storagev1.EvalExperimentGrade
	EvalExperimentArmOutcome    = storagev1.EvalExperimentArmOutcome
	EvalExperimentTargetOutcome = storagev1.EvalExperimentTargetOutcome
	EvalExperimentOutcome       = storagev1.EvalExperimentOutcome
	EvalDecisionActor           = storagev1.EvalDecisionActor
	EvalDecisionRecord          = storagev1.EvalDecisionRecord
	EvalExperimentEntry         = storagev1.EvalExperimentEntry
	EvalExperimentListOptions   = storagev1.EvalExperimentListOptions
	EvalExperimentPage          = storagev1.EvalExperimentPage
)

const (
	EvalTrialSchemaVersion     = storagev1.EvalTrialSchemaVersion
	EvalTrialOutcomePass       = storagev1.EvalTrialOutcomePass
	EvalTrialOutcomeFail       = storagev1.EvalTrialOutcomeFail
	EvalTrialOutcomeInfraError = storagev1.EvalTrialOutcomeInfraError
	MaxEvalTrialRecordBytes    = storagev1.MaxEvalTrialRecordBytes

	EvalExperimentSchemaVersion = storagev1.EvalExperimentSchemaVersion

	EvalExperimentOutcomeSchemaVersion = storagev1.EvalExperimentOutcomeSchemaVersion
	EvalDecisionSchemaVersion          = storagev1.EvalDecisionSchemaVersion
	EvalExperimentResultPassed         = storagev1.EvalExperimentResultPassed
	EvalExperimentResultFailed         = storagev1.EvalExperimentResultFailed
	EvalExperimentResultIncomparable   = storagev1.EvalExperimentResultIncomparable
	EvalDecisionApproved               = storagev1.EvalDecisionApproved
	EvalDecisionRejected               = storagev1.EvalDecisionRejected
	MaxEvalExperimentOutcomeBytes      = storagev1.MaxEvalExperimentOutcomeBytes
	MaxEvalDecisionReasonBytes         = storagev1.MaxEvalDecisionReasonBytes
	DefaultEvalExperimentListLimit     = storagev1.DefaultEvalExperimentListLimit
	MaxEvalExperimentListLimit         = storagev1.MaxEvalExperimentListLimit
)
