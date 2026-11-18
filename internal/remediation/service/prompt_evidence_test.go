package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

func readableEvidenceRecord(t *testing.T) *pv.Record {
	t.Helper()
	proposal := proposedChecks(t)
	record := &pv.Record{Manifest: pv.Manifest{Checks: proposal.Checks}}
	for _, side := range []string{pv.Original, pv.Patched} {
		for _, check := range record.Manifest.Checks {
			exit := 0
			record.Evidence = append(record.Evidence, pv.EvidenceRecord{Observation: pv.Observation{
				Side: side, CheckID: check.ID, ContainerID: "container-" + side,
				StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(2, 0),
				Executed: true, HTTPCompleted: true, ExitCode: &exit,
				StdoutDigest: pv.Digest([]byte(check.Healthy.Stdout)), StdoutBytes: len(check.Healthy.Stdout),
				StderrDigest: pv.Digest(nil),
			}})
		}
	}
	return record
}

func TestModelEvidenceOmitsUnavailableOutputWithoutChangingRecord(t *testing.T) {
	for name, mutate := range map[string]func(*pv.EvidenceRecord){
		"rejected": func(evidence *pv.EvidenceRecord) { evidence.Rejection = "rejected" },
		"unknown check": func(evidence *pv.EvidenceRecord) {
			evidence.Observation.CheckID = "unknown"
		},
		"unmatched": func(evidence *pv.EvidenceRecord) {
			evidence.Observation.StdoutDigest = pv.Digest([]byte("SYNTHETIC_UNEXPECTED_OUTPUT"))
		},
		"length":    func(evidence *pv.EvidenceRecord) { evidence.Observation.StdoutBytes++ },
		"truncated": func(evidence *pv.EvidenceRecord) { evidence.Observation.OutputTruncated = true },
		"incomplete": func(evidence *pv.EvidenceRecord) {
			evidence.Observation.HTTPCompleted = false
		},
		"setup": func(evidence *pv.EvidenceRecord) {
			evidence.Observation.SetupError = "SYNTHETIC_PRIVATE_DIAGNOSTIC"
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := readableEvidenceRecord(t)
			mutate(&record.Evidence[0])
			before := marshalTest(t, record)
			projected := modelExecution(record)
			require.Empty(t, projected.Evidence[0].ObservedStdout)
			require.NotContains(t, marshalTest(t, projected.Evidence[0]), `"observedStdout"`)
			require.NotContains(t, marshalTest(t, projected), "SYNTHETIC_PRIVATE_DIAGNOSTIC")
			require.NotContains(t, marshalTest(t, projected), "SYNTHETIC_UNEXPECTED_OUTPUT")
			require.NotEmpty(t, projected.Evidence[1].ObservedStdout)
			require.Equal(t, before, marshalTest(t, record))
		})
	}
}

func TestModelEvidenceBudgetIncludesEscapedOutputForBothSides(t *testing.T) {
	record := readableEvidenceRecord(t)
	for i := range record.Manifest.Checks {
		expectation, err := pv.HTTPExpectation(200, strings.Repeat("\"\\\n", 300))
		require.NoError(t, err)
		record.Manifest.Checks[i].Healthy = expectation
	}
	for i := range record.Evidence {
		observation := &record.Evidence[i].Observation
		for _, check := range record.Manifest.Checks {
			if observation.CheckID == check.ID {
				observation.StdoutDigest = pv.Digest([]byte(check.Healthy.Stdout))
				observation.StdoutBytes = len(check.Healthy.Stdout)
			}
		}
	}
	projected := modelExecution(record)
	withOutput, err := json.Marshal(projected)
	require.NoError(t, err)
	for i := range projected.Evidence {
		require.NotEmpty(t, projected.Evidence[i].ObservedStdout)
		projected.Evidence[i].ObservedStdout = ""
	}
	withoutOutput, err := json.Marshal(projected)
	require.NoError(t, err)
	metadata := modelEvidenceMetadataBudget + modelEvidenceCheckBudget*len(record.Manifest.Checks)
	require.Equal(t, len(withOutput)-len(withoutOutput), modelEvidenceBudget(record.Manifest.Checks)-metadata)
	require.True(t, modelEvidenceWithinBudget(record))
}

func TestReadableEvidencePreflightUsesExactPromptByteLimit(t *testing.T) {
	proposal := proposedChecks(t)
	for i := range proposal.Checks {
		expectation, err := pv.HTTPExpectation(200, strings.Repeat("\"\\\n", 300))
		require.NoError(t, err)
		proposal.Checks[i].Healthy = expectation
	}
	request := contract.ExecutionRequest{
		Action: pv.VerifyPatch,
		Checks: contract.Checks{Scope: proposal.Scope, Checks: proposal.Checks, Requirements: proposal.Requirements},
	}
	prefix := "diff --git a/example b/example\n--- /dev/null\n+++ b/example\n@@ -0,0 +1 @@\n+"
	request.Patch = prefix + "\n"
	record := &pv.Record{Manifest: pv.Manifest{
		Scope: request.Checks.Scope, Checks: request.Checks.Checks,
		Environment: pv.Environment{Requirements: request.Checks.Requirements},
	}}
	reserve := modelEvidenceBudget(request.Checks.Checks)
	padding := maxPromptBytes - reserve - len(verificationPrompt(intake.Report{}, source.Packet{}, request.Patch, record, false))
	require.Positive(t, padding)
	for _, delta := range []int{-1, 0, 1} {
		request.Patch = prefix + strings.Repeat("x", padding+delta) + "\n"
		encodedBytes := len(verificationPrompt(intake.Report{}, source.Packet{}, request.Patch, record, false)) + reserve
		require.Equal(t, maxPromptBytes+delta, encodedBytes)
		err := preflightPromptEnvelopes(intake.Report{}, source.Packet{}, request, contract.Generate, false)
		if delta <= 0 {
			require.NoError(t, err)
		} else {
			var failure *stageFailure
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "source-too-large", failure.reason)
		}
	}

}

func TestApplicationPromptDistinguishesAuthorizationFromOther403s(t *testing.T) {
	record := &pv.Record{Evidence: []pv.EvidenceRecord{{Observation: pv.Observation{
		Application: &pv.ApplicationObservation{Actions: []pv.ApplicationActionObservation{{
			Actor:      pv.ApplicationActorIdentity{ID: "reporter", ServiceAccountUID: "actor-uid"},
			StatusCode: 403,
		}}},
	}}}}
	projected := modelExecution(record)
	require.False(t, projected.Evidence[0].Application.Actions[0].AuthorizationDenied)
	require.Contains(t, marshalTest(t, projected), `"authorizationDenied":false`)
	record.Evidence[0].Observation.Application.Actions[0].AuthorizationDenied = true
	projected = modelExecution(record)
	require.True(t, projected.Evidence[0].Application.Actions[0].AuthorizationDenied)
	require.Equal(t, "actor-uid", projected.Evidence[0].Application.Actions[0].ActorUID)
}

func TestApplicationPromptKeepsReplicaEvidenceSeparateFromStatus(t *testing.T) {
	record := &pv.Record{Evidence: []pv.EvidenceRecord{{Observation: pv.Observation{
		Application: &pv.ApplicationObservation{Replica: &pv.ApplicationReplicaObservation{}},
	}}}}
	projected := modelExecution(record)
	require.NotNil(t, projected.Evidence[0].Application.Replica)
	require.Contains(t, marshalTest(t, projected), `"replica":`)
	require.Equal(t, record.Evidence[0].Observation.Application.Replica, projected.Evidence[0].Application.Replica)
	withoutReplica := []pv.Check{{Application: &pv.ApplicationCheck{
		Kubernetes: &pv.ApplicationKubernetesCheck{APIVersion: "v1", Resource: "services"},
	}}}
	withReplica := []pv.Check{{Application: &pv.ApplicationCheck{
		Kubernetes: &pv.ApplicationKubernetesCheck{APIVersion: "apps/v1", Resource: "deployments"},
	}}}
	require.Equal(t, 8<<10, modelEvidenceBudget(withReplica)-modelEvidenceBudget(withoutReplica))
}
