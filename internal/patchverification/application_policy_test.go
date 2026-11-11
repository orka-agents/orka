package patchverification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func applicationTestPolicy(t *testing.T) *ApplicationPolicy {
	t.Helper()
	policy := &ApplicationPolicy{
		Version: ApplicationProtocolVersion, MaxSyntheticSecrets: 2,
		Resources: []ApplicationResourceRule{
			{APIVersion: "testing.example/v1", Kind: "Widget", Resource: "widgets", Namespaced: true, Status: true,
				Verbs: []string{"get", "list", "watch", "create", "update", "patch"},
				CRD:   &ApplicationCRDIdentity{Name: "widgets.testing.example", UID: "approved-crd", SchemaDigest: Digest([]byte("complete-crd-spec"))}},
			{APIVersion: "v1", Kind: "Secret", Resource: "secrets", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
			{APIVersion: "v1", Kind: "Service", Resource: "services", Namespaced: true, Status: true,
				Verbs: []string{"get", "patch", "update"}},
		},
		SubjectPermissions: []ApplicationPermission{
			{APIGroup: "testing.example", Resources: []string{"widgets"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroup: "testing.example", Resources: []string{"widgets/status"}, Verbs: []string{"update", "patch"}},
			{Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch"}},
			{Resources: []string{"services/status"}, Verbs: []string{"patch"}},
		},
		Actors: []ApplicationActorPolicy{
			{ID: "author", Permissions: []ApplicationPermission{{APIGroup: "testing.example", Resources: []string{"widgets"}, Verbs: []string{"create", "update"}}}},
			{ID: "restricted"},
		},
	}
	require.NoError(t, ValidateApplicationPolicy(*policy))
	return policy
}

func TestApplicationOperatorPolicyDoesNotLetModelsSelfGrantAuthority(t *testing.T) {
	policy := applicationTestPolicy(t)
	raw := json.RawMessage(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"case"},"spec":{"secretRef":"synthetic"}}`)
	_, err := ValidateApplicationResource(raw)
	require.Error(t, err)
	_, err = ValidateApplicationResourceWithPolicy(raw, policy)
	require.NoError(t, err)
	for _, invalid := range []json.RawMessage{
		[]byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"case","namespace":"outside"},"spec":{}}`),
		[]byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"case","uid":"foreign"},"spec":{}}`),
		[]byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"case"},"spec":{},"status":{"passed":true}}`),
		[]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"synthetic"},"data":{"value":"untrusted"}}`),
	} {
		_, err = ValidateApplicationResourceWithPolicy(invalid, policy)
		require.Error(t, err)
	}
	manifest := applicationTestManifest(t)
	plan := *manifest.Application
	plan.Resources = []json.RawMessage{raw}
	plan.Permissions = []ApplicationPermission{{APIGroup: "testing.example", Resources: []string{"widgets/status"}, Verbs: []string{"patch"}}}
	plan.SyntheticSecrets = []ApplicationSyntheticSecret{{Name: "synthetic", Keys: []ApplicationSyntheticSecretKey{{Name: "value", MarkerID: "protected-value"}}}}
	require.NoError(t, ValidateApplicationPlanWithPolicy(plan, policy))
	plan.Permissions[0].Resources = []string{"widgets"}
	plan.Permissions[0].Verbs = []string{"create"}
	require.Error(t, ValidateApplicationPlanWithPolicy(plan, policy))
	policy.Resources[0].CRD = nil
	require.Error(t, ValidateApplicationPolicy(*policy))
}

func TestApplicationPolicyRejectsWildcardsClusterScopeAndCredentialMutation(t *testing.T) {
	for _, mutate := range []func(*ApplicationPolicy){
		func(p *ApplicationPolicy) { p.Resources[0].Namespaced = false },
		func(p *ApplicationPolicy) { p.Resources[0].Verbs = []string{"*"} },
		func(p *ApplicationPolicy) { p.Resources[0].Resource = "*" },
		func(p *ApplicationPolicy) { p.Resources[0].CRD.SchemaDigest = "" },
		func(p *ApplicationPolicy) { p.Resources[1].Verbs = []string{"create"} },
		func(p *ApplicationPolicy) { p.SubjectPermissions[0].Resources = []string{"pods/exec"} },
		func(p *ApplicationPolicy) { p.Actors[0].Permissions[0].Verbs = []string{"bind"} },
	} {
		policy := applicationTestPolicy(t)
		mutate(policy)
		require.Error(t, ValidateApplicationPolicy(*policy))
	}
}

type applicationTestActor struct {
	identity ApplicationActorIdentity
	status   int
}

func (actor applicationTestActor) Apply(_ context.Context, _ string, action ApplicationAction, target ApplicationActionTarget) (ApplicationActionObservation, error) {
	result := ApplicationActionObservation{Index: target.Index, Actor: actor.identity, APIVersion: target.APIVersion,
		Resource: target.Resource, Name: target.Name, StatusCode: actor.status, Target: target}
	if actor.status == 201 {
		result.UID, result.ResourceVersion = "created-object", "12"
	}
	if actor.status == 403 {
		result.AuthorizationDenied = true
	}
	return result, nil
}

func applicationActionFixture(t *testing.T) (ApplicationWorkerInput, Check) {
	t.Helper()
	manifest := applicationTestManifest(t)
	manifest.ApplicationPolicy = applicationTestPolicy(t)
	action := ApplicationAction{ActorID: "author", Verb: "create", ExpectedStatus: 201,
		Resource: []byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"case"},"spec":{"enabled":true}}`)}
	manifest.Checks[0].Application.Actions = []ApplicationAction{action}
	binding, evidence := applicationTestEvidence(t, manifest)
	input := ApplicationWorkerInput{
		Version: ApplicationProtocolVersion, Manifest: manifest, Binding: binding, Side: Original,
		TaskUID: "observer-task", CheckID: "unsafe", Target: evidence[0].Application.Target,
		Worker:   applicationTestIdentity("observer-original-unsafe", manifest.Environment.Dependencies[ApplicationObserverImageIDKey]),
		Artifact: &evidence[0].Application.Artifact, Subject: &evidence[0].Application.Subject,
		Actors: []ApplicationActorIdentity{{ID: "author", Namespace: "protected-run", NamespaceUID: "protected-uid",
			ServiceAccountName: "actor-author", ServiceAccountUID: "actor-author-uid"}},
		ActionTargets: []ApplicationActionTarget{{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets", Name: "case", MustBeAbsent: true, OwnerUID: "run-ledger-uid"}},
	}
	input.Worker.Namespace, input.Worker.NamespaceUID = "protected-run", "protected-uid"
	return input, manifest.Checks[0]
}

func TestApplicationActorsEmitEffectBeforeInterpretingExpectedOutcome(t *testing.T) {
	input, check := applicationActionFixture(t)
	client := applicationTestActor{identity: input.Actors[0], status: 201}
	var receipts []ApplicationActionObservation
	check.Application.Actions[0].ExpectedStatus = 403
	_, err := executeApplicationActions(t.Context(), input, check, map[string]ApplicationActorClient{"author": client},
		func(receipt ApplicationActionObservation) error { receipts = append(receipts, receipt); return nil })
	require.Error(t, err)
	require.Len(t, receipts, 1)
	require.Equal(t, "created-object", receipts[0].UID)
	input.ActionTargets[0].UID = "foreign"
	require.Error(t, ValidateApplicationActionTarget(check.Application.Actions[0], input.ActionTargets[0], 0, input.Manifest.ApplicationPolicy))
}

func TestApplicationReceiveAllowsOnlyCompleteWindowAbsenceWithPositiveNormal(t *testing.T) {
	input, check := applicationActionFixture(t)
	none, err := ApplicationReceiveExpectation(false, false)
	require.NoError(t, err)
	present, err := ApplicationReceiveExpectation(true, true)
	require.NoError(t, err)
	_, err = ApplicationReceiveExpectation(false, true)
	require.Error(t, err)
	check.Healthy, check.Failure = none, present
	check.Application.Kubernetes = nil
	check.Application.TCP = &ApplicationTCPCheck{Mode: "receive", Target: "receiver", Port: 8080, MarkerID: "protected-value",
		MaxBytes: 1024, WindowSeconds: 2, Terminator: []byte("\r\n\r\n")}
	require.NoError(t, ValidateApplicationCheckWithPolicy(check, input.Manifest.ApplicationPolicy))
	check.Application.Actions = nil
	require.Error(t, ValidateApplicationCheckWithPolicy(check, input.Manifest.ApplicationPolicy), "receive windows need a stimulus after readiness")
	for _, raw := range []string{"partial frame", strings.Repeat("x", 33)} {
		_, err = applicationReadFrame(strings.NewReader(raw), []byte("\r\n\r\n"), 32)
		require.Error(t, err)
	}
	frame, err := applicationReadFrame(strings.NewReader("complete\r\n\r\n"), []byte("\r\n\r\n"), 32)
	require.NoError(t, err)
	require.Equal(t, "complete\r\n\r\n", string(frame))
}

func TestApplicationTranscriptRetainsCleanupReceiptsButNeverIncompleteSuccess(t *testing.T) {
	input, check := applicationActionFixture(t)
	digest, err := ApplicationWorkerInputDigest(input)
	require.NoError(t, err)
	receipt, err := (applicationTestActor{identity: input.Actors[0], status: 201}).Apply(t.Context(), input.Target.Namespace, check.Application.Actions[0], input.ActionTargets[0])
	require.NoError(t, err)
	frame := ApplicationStreamFrame{Version: ApplicationProtocolVersion, Kind: "action",
		TaskUID: input.TaskUID, PodUID: input.Worker.PodUID, InputDigest: digest, Action: &receipt}
	require.NoError(t, ValidateApplicationStreamFrame(input, frame))
	content, err := MarshalApplicationReport(frame)
	require.NoError(t, err)
	frames, err := DecodeApplicationTranscript(bytes.NewReader(content))
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))
	require.Len(t, frames, 1)
	frames, err = DecodeApplicationTranscript(strings.NewReader(string(content) + `{"version":1`))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Len(t, frames, 1)
	frame.InputDigest = Digest([]byte("different input"))
	require.Error(t, ValidateApplicationStreamFrame(input, frame))
}
