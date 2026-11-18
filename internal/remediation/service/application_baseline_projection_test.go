package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

func TestApplicationPromptRetainsAuthoritativeNormalBaselines(t *testing.T) {
	fixture := &pv.ApplicationAPIIdentity{
		APIVersion: "networking.k8s.io/v1", Resource: "ingresses", Name: "synthetic",
		UID: "target-uid", ResourceVersion: "create-revision", Pointer: "/status/loadBalancer/ingress",
	}
	actor := *fixture
	actor.ResourceVersion = "actor-write-revision"
	record := &pv.Record{Evidence: []pv.EvidenceRecord{{Observation: pv.Observation{
		Application: &pv.ApplicationObservation{
			APIBaseline: fixture,
			Actions: []pv.ApplicationActionObservation{{
				Actor:      pv.ApplicationActorIdentity{ID: "reporter", ServiceAccountUID: "actor-uid"},
				StatusCode: 200, InitialAPI: &actor,
			}},
		},
	}}}}
	before := marshalTest(t, record)
	projected := modelExecution(record)
	require.Equal(t, fixture, projected.Evidence[0].Application.APIBaseline)
	require.Equal(t, &actor, projected.Evidence[0].Application.Actions[0].InitialAPI)
	require.Contains(t, marshalTest(t, projected), "create-revision")
	require.Contains(t, marshalTest(t, projected), "actor-write-revision")
	require.Equal(t, before, marshalTest(t, record), "projection cannot replace or invent a baseline")
}

func TestApplicationPlanningAndReviewDescribeRealNormalTransition(t *testing.T) {
	plan := proposedApplicationChecks(t)
	planning := applicationChecksPrompt(intake.Report{}, source.Packet{}, *plan.Application, nil)
	require.Contains(t, planning, `normalValue:"<actual public healthy value for a non-replica normal check>"`)
	require.Contains(t, planning, "Explicit false")
	require.Contains(t, planning, "Do not return apiBaseline")
	for _, review := range []string{
		reproductionPrompt(intake.Report{}, source.Packet{}, nil),
		verificationPrompt(intake.Report{}, source.Packet{}, "", nil, false),
	} {
		require.Contains(t, review, "normalValue is the declared public healthy value, not observed status")
		require.Contains(t, review, "apiBaseline")
		require.Contains(t, review, "initialAPI")
		require.Contains(t, review, "exact pointer/UID")
		require.Contains(t, review, "resourceVersion and reduced-status digest change")
	}
}
