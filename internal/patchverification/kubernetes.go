package patchverification

import verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"

type SubmissionState = verificationv1.SubmissionState

const (
	SubmissionPreparing  SubmissionState = verificationv1.SubmissionPreparing
	SubmissionRunning    SubmissionState = verificationv1.SubmissionRunning
	SubmissionTerminal   SubmissionState = verificationv1.SubmissionTerminal
	SubmissionCancelling SubmissionState = verificationv1.SubmissionCancelling
)

type KubernetesSubmission = verificationv1.KubernetesSubmission

type DispatchState = verificationv1.DispatchState

const (
	DispatchPlanned  DispatchState = verificationv1.DispatchPlanned
	DispatchCreated  DispatchState = verificationv1.DispatchCreated
	DispatchObserved DispatchState = verificationv1.DispatchObserved
	DispatchRecorded DispatchState = verificationv1.DispatchRecorded
	DispatchFailed   DispatchState = verificationv1.DispatchFailed
)

type KubernetesDispatch = verificationv1.KubernetesDispatch
