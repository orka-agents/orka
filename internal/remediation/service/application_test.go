package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

type applicationPlanningFixture struct {
	contract.ExecutionBackend
	scope    contract.EvidenceScope
	defaults pv.ApplicationPlan
	policy   *pv.ApplicationPolicy
}

func (backend *applicationPlanningFixture) EvidenceScope() contract.EvidenceScope {
	return backend.scope
}
func (backend *applicationPlanningFixture) ApplicationPlanDefaults() pv.ApplicationPlan {
	return backend.defaults
}
func (backend *applicationPlanningFixture) ApplicationPolicy() *pv.ApplicationPolicy {
	return backend.policy
}

func proposedApplicationChecks(t *testing.T) remediation.CheckProposal {
	t.Helper()
	normalValue := json.RawMessage(`[{"hostname":"ready.invalid"}]`)
	healthy, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{
		Exists: true, Found: true, Value: normalValue,
	})
	require.NoError(t, err)
	failure, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{})
	require.NoError(t, err)
	proposal := remediation.CheckProposal{
		Scope: []string{"complete synthetic report"},
		Application: &pv.ApplicationPlan{
			Version: 1, Build: pv.ApplicationBuild{Profile: pv.ApplicationBuildGoPackage, Package: "./cmd/example", GoVersion: "go1.27.2"},
			Runtime: pv.ApplicationRuntime{Image: "example.invalid/runtime@sha256:" + strings.Repeat("a", 64),
				ImageID: "sha256:" + strings.Repeat("b", 64)},
			Resources: []json.RawMessage{json.RawMessage(
				`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"synthetic"},` +
					`"spec":{"defaultBackend":{"service":{"name":"backend","port":{"number":8080}}}}}`,
			)},
			Permissions: []pv.ApplicationPermission{{
				APIGroup: "networking.k8s.io", Resources: []string{"ingresses/status"},
				Verbs: []string{"update", "patch"}, ResourceNames: []string{"synthetic"},
			}},
		},
	}
	for _, kind := range []string{pv.Reproduction, pv.Normal} {
		proposal.Checks = append(proposal.Checks, pv.Check{
			ID: kind, Kind: kind, TimeoutSeconds: 30, Healthy: healthy, Failure: failure,
			Application: &pv.ApplicationCheck{Version: 1, Kubernetes: &pv.ApplicationKubernetesCheck{
				APIVersion: "networking.k8s.io/v1", Resource: "ingresses",
				Name: "synthetic", Pointer: "/status/loadBalancer/ingress",
			}},
		})
		if kind == pv.Normal {
			proposal.Checks[len(proposal.Checks)-1].Application.Kubernetes.NormalValue = normalValue
		}
	}
	return proposal
}

func TestApplicationPlanCannotBeRelabelledAsSourceHarness(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	err = f.service.freezeChecks(t.Context(), r, proposedApplicationChecks(t))
	var failure *stageFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, contract.ReasonEvidenceScopeUnsupported, failure.reason)
	require.True(t, r.state.Checks.empty())
	require.Zero(t, f.backend.startCalls.Load())
	r.state.Authority.EvidenceScope = contract.ApplicationRuntime
	err = f.service.freezeChecks(t.Context(), r, proposedChecks(t))
	require.ErrorAs(t, err, &failure)
	require.Equal(t, contract.ReasonEvidenceScopeUnsupported, failure.reason)
}

func TestApplicationCheckRevisionsKeepFrozenSubjectAndCoverage(t *testing.T) {
	for _, change := range []string{"probe", "scope", "gap", "normal", "package", "args", "permissions"} {
		t.Run(change, func(t *testing.T) {
			f := newServiceFixture(t)
			request := f.request(contract.Validate)
			_, err := f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			r, err := f.service.load(t.Context(), request.RequestID)
			require.NoError(t, err)
			r.state.Authority.EvidenceScope = contract.ApplicationRuntime
			original := proposedApplicationChecks(t)
			f.service.config.Backend = &applicationPlanningFixture{
				ExecutionBackend: f.backend, scope: contract.ApplicationRuntime, defaults: *original.Application,
			}
			require.NoError(t, f.service.freezeChecks(t.Context(), r, original))
			before := r.state.Checks
			revised := proposedApplicationChecks(t)
			switch change {
			case "probe":
				for index := range revised.Checks {
					revised.Checks[index].Application.Kubernetes.Pointer = "/status/conditions"
				}
			case "scope":
				revised.Scope = []string{"silently narrower"}
			case "gap":
				revised.Gaps = []string{"unsupported required runtime behavior"}
			case "normal":
				revised.Checks = revised.Checks[:1]
			case "package":
				revised.Application.Build.Package = "./cmd/different"
			case "args":
				revised.Application.Runtime.Args = []string{"--disable-feature"}
			case "permissions":
				revised.Application.Permissions = []pv.ApplicationPermission{{
					Resources: []string{"services"}, Verbs: []string{"get"},
				}}
			}
			err = f.service.freezeChecks(t.Context(), r, revised)
			if change == "probe" {
				require.NoError(t, err)
				require.NotEqual(t, before.Digest, r.state.Checks.Digest)
				var frozen contract.Checks
				require.NoError(t, f.service.readJSON(t.Context(), r, r.state.Checks, &frozen))
				require.Equal(t, original.Application, frozen.Application)
			} else {
				require.Error(t, err)
				require.Equal(t, before, r.state.Checks)
			}
		})
	}
}

func TestApplicationExecutionUsesFullSourceWithoutExpandingModelPacket(t *testing.T) {
	f := newServiceFixture(t)
	f.service.config.Backend = &applicationPlanningFixture{
		ExecutionBackend: f.backend, scope: contract.ApplicationRuntime, defaults: *proposedApplicationChecks(t).Application,
	}
	f.service.config.BoundedExecution = true
	request := f.request(contract.Validate)
	request.RequiredEvidenceScope = contract.ApplicationRuntime
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	target := f.service.config.Source.(fixtureSource).target
	r.state.Source = &target
	r.state.Parent.Name, r.state.Parent.UID = "parent", "parent-uid"
	packet := source.Packet{Version: 1, Target: target, Files: []source.PacketFile{{Path: "README", Content: "bounded source\n"}}}
	r.state.Packet, err = f.service.putJSON(t.Context(), r, packet)
	require.NoError(t, err)
	inventory := []source.Entry{
		{Path: "README", Mode: "100644", Size: 15},
		{Path: "vendor", Mode: "040000"},
	}
	r.state.Inventory, err = f.service.putJSON(t.Context(), r, inventory)
	require.NoError(t, err)
	require.NoError(t, f.service.freezeChecks(t.Context(), r, proposedApplicationChecks(t)))
	var dispatched *contract.ExecutionRequest
	f.backend.validateRequest = func(execution contract.ExecutionRequest) error {
		dispatched = &execution
		return contract.ErrInvalid
	}
	require.Error(t, f.service.runChecks(t.Context(), r, intake.Report{Title: "synthetic report"}, false))
	require.NotNil(t, dispatched)
	require.Empty(t, dispatched.Source.Paths, "bounded model files must never restrict the immutable application build context")
	require.NotNil(t, dispatched.Checks.Application)
	require.Empty(t, dispatched.Checks.Files)
	require.Equal(t, contract.ApplicationRuntime, dispatched.EvidenceScope)
	require.NotNil(t, dispatched.Deadline)
	require.Equal(t, r.state.Deadline, *dispatched.Deadline, "mechanical recovery cannot reset the original run deadline")
	require.Zero(t, f.backend.startCalls.Load(), "this test stops at preflight, without pretending to execute an application")
}

func TestApplicationPlanIsBoundIntoIndependentEvidenceComparison(t *testing.T) {
	proposal := proposedApplicationChecks(t)
	request := contract.ExecutionRequest{
		Action: pv.ValidateReport, Problem: "synthetic", EvidenceScope: contract.ApplicationRuntime,
		Source: contract.Source{Repository: "https://github.com/example/project", Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)},
		Checks: contract.Checks{Scope: proposal.Scope, Gaps: proposal.Gaps, Checks: proposal.Checks, Application: proposal.Application},
	}

	manifest := &pv.Manifest{
		Action: request.Action, Problem: request.Problem, Scope: request.Checks.Scope, Gaps: request.Checks.Gaps,
		Sources: pv.Sources{Repository: request.Source.Repository, Original: pv.SourceIdentity{Commit: request.Source.Commit, Tree: request.Source.Tree}},
		Checks:  request.Checks.Checks, Application: request.Checks.Application,
		Environment: pv.Environment{Dependencies: map[string]string{contract.EvidenceScopeKey: string(contract.ApplicationRuntime)}},
	}
	require.True(t, manifestMatchesRequest(manifest, request))
	manifest.ApplicationPolicy = &pv.ApplicationPolicy{Version: 1}
	require.False(t, manifestMatchesRequest(manifest, request), "an execution cannot self-grant an operator policy")
	manifest.ApplicationPolicy = nil
	changed := *manifest.Application
	changed.Build.Package = "./cmd/different"
	manifest.Application = &changed
	require.False(t, manifestMatchesRequest(manifest, request), "even a self-consistent execution cannot replace the frozen application")
	require.Error(t, checkRecord(request, &pv.Record{Manifest: *manifest, State: pv.RunFinalized}),
		"model/runtime labels without a seal and independently bound observations never prove runtime behavior")
}

func TestSourceHarnessRecordCannotBecomeRuntimeProofByRelabelling(t *testing.T) {
	proposal := proposedChecks(t)
	files, err := remediation.FreezeProposalFiles(proposal)
	require.NoError(t, err)
	request := contract.ExecutionRequest{
		ID: "synthetic-runtime-claim", Action: pv.ValidateReport, Problem: "synthetic",
		EvidenceScope: contract.ApplicationRuntime,
		Source: contract.Source{Repository: "https://github.com/example/project",
			Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)},
		Checks: contract.Checks{Scope: proposal.Scope, Files: files, Checks: proposal.Checks},
	}

	record := executionRecord(t, request, "", "")
	require.NoError(t, pv.ValidateRunBinding(record.Manifest, record.Binding))
	require.Equal(t, "application-runtime", record.Manifest.Environment.Dependencies[contract.EvidenceScopeKey])
	require.Error(t, checkRecord(request, record),
		"an otherwise sealed HTTP harness record with a runtime label still lacks actual application proof")
}

func TestApplicationQualificationPrecheckDoesNotGrantAdmission(t *testing.T) {
	f := newServiceFixture(t)
	backend := &applicationPlanningFixture{ExecutionBackend: f.backend, defaults: *proposedApplicationChecks(t).Application}
	config := f.service.config
	config.Backend, config.BoundedExecution = backend, true
	coordinator, err := New(config)
	require.NoError(t, err, "an unqualified backend may be inspected for cleanup/configuration compatibility")
	require.NoError(t, CheckConfiguredAuthority(t.Context(), config, contract.ApplicationRuntime))
	require.ErrorIs(t, coordinator.CheckReady(t.Context()), contract.ErrEvidenceScopeUnsupported)
	request := f.request(contract.Validate)
	request.RequiredEvidenceScope = contract.ApplicationRuntime
	_, err = coordinator.Submit(t.Context(), "private", "subject", request)
	require.ErrorIs(t, err, contract.ErrEvidenceScopeUnsupported)
	require.Zero(t, f.backend.startCalls.Load())
	backend.scope = contract.ApplicationRuntime
	require.NoError(t, coordinator.CheckReady(t.Context()))
	_, err = coordinator.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	backend.scope = ""
	require.NoError(t, CheckConfiguredAuthority(t.Context(), config, contract.ApplicationRuntime),
		"same retained authority can be compared before backend readiness without changing its capability")
	agent := new(corev1alpha1.Agent)
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Namespace: "private", Name: config.AgentName}, agent))
	agent.Spec.Model.Name = "changed-native-model"
	agent.Generation++
	require.NoError(t, f.client.Update(t.Context(), agent))
	require.ErrorIs(t, CheckConfiguredAuthority(t.Context(), config, contract.ApplicationRuntime), contract.ErrConflict,
		"native authority drift must block before backend startup launches or resumes any experiment")
	require.Zero(t, f.backend.startCalls.Load())
}

func TestApplicationReviewCannotChangeActorOrResourceAuthority(t *testing.T) {
	initial := proposedApplicationChecks(t)
	initial.Checks[0].Application.Actions = []pv.ApplicationAction{{
		ActorID: "reporter", Verb: "create", ExpectedStatus: 201,
		Resource: []byte(`{"apiVersion":"applications.example.test/v1","kind":"Widget","metadata":{"name":"input"}}`),
	}}
	previous := contract.Checks{Scope: initial.Scope, Checks: initial.Checks, Application: initial.Application}
	for _, change := range []string{"actor", "expected status", "resource", "new mutation", "new observer"} {
		t.Run(change, func(t *testing.T) {
			var revised remediation.CheckProposal
			require.NoError(t, json.Unmarshal([]byte(marshalTest(t, initial)), &revised))
			switch change {
			case "actor":
				revised.Checks[0].Application.Actions[0].ActorID = "privileged-actor"
			case "expected status":
				revised.Checks[0].Application.Actions[0].ExpectedStatus = 403
			case "resource":
				revised.Checks[0].Application.Actions[0].Resource = []byte(`{"metadata":{"name":"different"}}`)
			case "new mutation":
				added := revised.Checks[0]
				added.ID = "additional-mutation"
				revised.Checks = append(revised.Checks, added)
			case "new observer":
				added := revised.Checks[1]
				added.ID = "additional-observer"
				revised.Checks = append(revised.Checks, added)
			}
			require.Equal(t, change == "new observer", preservesCheckCoverage(previous, revised))
		})
	}
}
