package service

import (
	"encoding/json"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

const (
	modelEvidenceMetadataBudget = 4 << 10
	modelEvidenceCheckBudget    = 4 << 10
)

type promptCheckResult struct {
	CheckID         string `json:"checkID"`
	Outcome         string `json:"outcome"`
	OriginalOutcome string `json:"originalOutcome,omitempty"`
	PatchedOutcome  string `json:"patchedOutcome,omitempty"`
}

type promptObservation struct {
	Side            string                        `json:"side"`
	CheckID         string                        `json:"checkID"`
	TaskID          string                        `json:"taskID"`
	JobUID          string                        `json:"jobUID,omitempty"`
	PodUID          string                        `json:"podUID,omitempty"`
	Executed        bool                          `json:"executed"`
	HTTPCompleted   bool                          `json:"httpCompleted"`
	ExitCode        *int                          `json:"exitCode"`
	StdoutDigest    string                        `json:"stdoutDigest"`
	StdoutBytes     int                           `json:"stdoutBytes"`
	ObservedStdout  string                        `json:"observedStdout,omitempty"`
	StderrDigest    string                        `json:"stderrDigest"`
	StderrBytes     int                           `json:"stderrBytes"`
	TimedOut        bool                          `json:"timedOut"`
	Skipped         bool                          `json:"skipped"`
	OutputTruncated bool                          `json:"outputTruncated"`
	SetupFailed     bool                          `json:"setupFailed"`
	Application     *promptApplicationObservation `json:"application,omitempty"`
}

type promptApplicationObservation struct {
	Completed    bool                              `json:"completed"`
	PlanDigest   string                            `json:"planDigest"`
	Target       pv.ApplicationTarget              `json:"target"`
	Source       pv.SourceIdentity                 `json:"source"`
	BinaryDigest string                            `json:"binaryDigest"`
	Build        pv.ApplicationContainerIdentity   `json:"build"`
	Subject      pv.ApplicationContainerIdentity   `json:"subject"`
	Kubernetes   *pv.ApplicationAPIIdentity        `json:"kubernetes,omitempty"`
	APIBaseline  *pv.ApplicationAPIIdentity        `json:"apiBaseline,omitempty"`
	TCP          *pv.ApplicationTCPIdentity        `json:"tcp,omitempty"`
	Actions      []promptApplicationAction         `json:"actions,omitempty"`
	Replica      *pv.ApplicationReplicaObservation `json:"replica,omitempty"`
}

type promptApplicationAction struct {
	Index               int                        `json:"index"`
	ActorID             string                     `json:"actorID"`
	ActorUID            string                     `json:"actorUID"`
	TargetDigest        string                     `json:"targetDigest"`
	APIVersion          string                     `json:"apiVersion"`
	Resource            string                     `json:"resource"`
	Name                string                     `json:"name"`
	StatusCode          int                        `json:"statusCode"`
	AuthorizationDenied bool                       `json:"authorizationDenied"`
	ObjectUID           string                     `json:"objectUID,omitempty"`
	ResourceVersion     string                     `json:"resourceVersion,omitempty"`
	InitialAPI          *pv.ApplicationAPIIdentity `json:"initialAPI,omitempty"`
}

type promptExecution struct {
	Binding           pv.Binding                `json:"binding"`
	Sources           pv.Sources                `json:"sources"`
	EnvironmentDigest string                    `json:"environmentDigest"`
	SealDigest        string                    `json:"sealDigest,omitempty"`
	Checks            remediation.CheckProposal `json:"checks"`
	Conclusion        pv.Conclusion             `json:"conclusion"`
	Results           []promptCheckResult       `json:"results"`
	Evidence          []promptObservation       `json:"evidence"`
	ApplicationPolicy *pv.ApplicationPolicy     `json:"applicationPolicy,omitempty"`
}

// The model sees decision-relevant observations and one copy of the checks,
// not duplicate manifests, encoded seals, or diagnostic output. The complete
// authoritative record remains intact in the run's private immutable blobs.
func modelExecution(record *pv.Record) *promptExecution {
	if record == nil {
		return nil
	}
	environment, _ := json.Marshal(record.Manifest.Environment)
	model := &promptExecution{
		Binding: record.Binding, Sources: record.Manifest.Sources, EnvironmentDigest: pv.Digest(environment),
		Conclusion:        record.Assessment.Conclusion,
		ApplicationPolicy: record.Manifest.ApplicationPolicy,
		Checks: remediation.CheckProposal{
			Scope: record.Manifest.Scope, Gaps: record.Manifest.Gaps, Requirements: record.Manifest.Environment.Requirements,
			Checks: record.Manifest.Checks, Services: record.Manifest.Environment.Services,
			Application: record.Manifest.Application,
		},
	}
	if record.Seal != nil {
		model.SealDigest = record.Seal.Digest
	}
	for _, file := range record.Manifest.Files {
		model.Checks.Files = append(model.Checks.Files, remediation.ProposedFile{
			Path: file.Path, Content: string(file.Content), Executable: file.Executable,
		})
	}
	for _, result := range record.Assessment.Checks {
		model.Results = append(model.Results, promptCheckResult{
			CheckID: result.CheckID, Outcome: result.Outcome, OriginalOutcome: result.OriginalOutcome, PatchedOutcome: result.PatchedOutcome,
		})
	}
	for _, item := range record.Evidence {
		observation := item.Observation
		var observedStdout string
		var application *promptApplicationObservation
		if item.Rejection == "" {
			for _, check := range record.Manifest.Checks {
				if output, matched := pv.MatchedHTTPOutput(check, observation); matched {
					observedStdout = output
					break
				}
				if output, matched := pv.MatchedApplicationOutput(check, observation); matched {
					observedStdout = output
					break
				}
			}
			if runtime := observation.Application; runtime != nil {
				application = &promptApplicationObservation{
					Completed: runtime.Completed, PlanDigest: runtime.PlanDigest, Target: runtime.Target,
					Source: runtime.Artifact.Source, BinaryDigest: runtime.Artifact.BinaryDigest,
					Build: runtime.Artifact.Build, Subject: runtime.Subject,
					Kubernetes: runtime.Kubernetes, TCP: runtime.TCP,
					APIBaseline: runtime.APIBaseline,
					Replica:     runtime.Replica,
				}
				for _, action := range runtime.Actions {
					target, _ := json.Marshal(action.Target)
					application.Actions = append(application.Actions, promptApplicationAction{
						Index: action.Index, ActorID: action.Actor.ID, ActorUID: action.Actor.ServiceAccountUID,
						TargetDigest: pv.Digest(target), APIVersion: action.APIVersion, Resource: action.Resource,
						Name: action.Name, StatusCode: action.StatusCode, ObjectUID: action.UID, ResourceVersion: action.ResourceVersion,
						AuthorizationDenied: action.AuthorizationDenied,
						InitialAPI:          action.InitialAPI,
					})
				}
			}
		}
		model.Evidence = append(model.Evidence, promptObservation{
			Side: observation.Side, CheckID: observation.CheckID, TaskID: observation.TaskID,
			JobUID: observation.JobUID, PodUID: observation.PodUID,
			Executed: observation.Executed, HTTPCompleted: observation.HTTPCompleted, ExitCode: observation.ExitCode,
			StdoutDigest: observation.StdoutDigest, StdoutBytes: observation.StdoutBytes,
			ObservedStdout: observedStdout,
			StderrDigest:   observation.StderrDigest, StderrBytes: observation.StderrBytes,
			TimedOut: observation.TimedOut, Skipped: observation.Skipped, OutputTruncated: observation.OutputTruncated,
			SetupFailed: observation.SetupError != "",
			Application: application,
		})
	}
	return model
}

func modelEvidenceBudget(checks []pv.Check) int {
	budget := modelEvidenceMetadataBudget + modelEvidenceCheckBudget*len(checks)
	for _, check := range checks {
		if check.Application != nil {
			budget += modelEvidenceCheckBudget
			budget += (2 << 10) * len(check.Application.Actions)
			if check.Application.Kubernetes != nil &&
				check.Application.Kubernetes.APIVersion == "apps/v1" &&
				check.Application.Kubernetes.Resource == "deployments" {
				budget += 8 << 10
			}
		}
		outputBytes := 0
		for _, expectation := range []pv.Expectation{check.Healthy, check.Failure} {
			encoded, _ := json.Marshal(expectation.Stdout)
			outputBytes = max(outputBytes, len(encoded))
		}
		budget += 2 * (len(`,"observedStdout":`) + outputBytes)
	}
	return budget
}

func modelEvidenceWithinBudget(record *pv.Record) bool {
	model := modelExecution(record)
	model.Checks = remediation.CheckProposal{}
	model.ApplicationPolicy = nil
	encoded, err := json.Marshal(model)
	return err == nil && len(encoded) <= modelEvidenceBudget(record.Manifest.Checks)
}

func preflightPromptEnvelopes(report intake.Report, packet source.Packet, request contract.ExecutionRequest, mode contract.Mode, revised bool) error {
	// This budget-only envelope has no observations and is never dispatched.
	// Known report/source/check/patch bytes are encoded exactly; the reserve
	// covers bounded binding metadata and both sides' compact observations.
	record := &pv.Record{Manifest: pv.Manifest{
		Scope: request.Checks.Scope, Gaps: request.Checks.Gaps, Files: request.Checks.Files, Checks: request.Checks.Checks,
		Environment:       pv.Environment{Requirements: request.Checks.Requirements, Services: request.Checks.Services},
		Application:       request.Checks.Application,
		ApplicationPolicy: request.ApplicationPolicy,
	}}
	reserve := modelEvidenceBudget(request.Checks.Checks)
	var envelopes []string
	if request.Action == pv.VerifyPatch {
		envelopes = []string{verificationPrompt(report, packet, request.Patch, record, revised)}
	} else {
		envelopes = []string{reproductionPrompt(report, packet, record)}
		if mode == contract.Generate {
			envelopes = append(envelopes, generationPrompt(report, packet, record))
		}
	}
	for _, envelope := range envelopes {
		if len(envelope)+reserve > maxPromptBytes {
			return fail("source-too-large")
		}
	}
	return nil
}
