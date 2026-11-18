package applicationexec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func installPolicyFixture(t *testing.T, f *fixture) {
	t.Helper()
	spec := map[string]any{
		"group": "fixtures.orka.ai", "scope": "Namespaced",
		"names":      map[string]any{"kind": "Probe", "plural": "probes", "singular": "probe", "listKind": "ProbeList"},
		"conversion": map[string]any{"strategy": "None"},
		"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true,
			"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": map[string]any{
				"spec":   map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
				"status": map[string]any{"type": "object", "properties": map[string]any{"ready": map[string]any{"type": "boolean"}}},
			}}},
			"subresources": map[string]any{"status": map[string]any{}},
		}},
	}
	content, err := json.Marshal(spec)
	require.NoError(t, err)
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "probes.fixtures.orka.ai", "uid": "profile-crd"},
		"spec":     spec, "status": map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
	}}
	require.NoError(t, f.target.Create(t.Context(), crd))
	policy := &pv.ApplicationPolicy{Version: pv.ApplicationProtocolVersion, MaxSyntheticSecrets: 2,
		Resources: []pv.ApplicationResourceRule{
			{APIVersion: "fixtures.orka.ai/v1", Kind: "Probe", Resource: "probes", Namespaced: true, Status: true,
				Verbs: []string{"get", "list", "watch", "create", "update", "patch"},
				CRD:   &pv.ApplicationCRDIdentity{Name: crd.GetName(), UID: string(crd.GetUID()), SchemaDigest: pv.Digest(content)}},
			{APIVersion: "v1", Kind: "Secret", Resource: "secrets", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
		},
		SubjectPermissions: []pv.ApplicationPermission{
			{APIGroup: "fixtures.orka.ai", Resources: []string{"probes"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroup: "fixtures.orka.ai", Resources: []string{"probes/status"}, Verbs: []string{"patch", "update"}},
			{APIGroup: "", Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch"}},
		},
		Actors: []pv.ApplicationActorPolicy{
			{ID: "writer", Permissions: []pv.ApplicationPermission{{APIGroup: "fixtures.orka.ai", Resources: []string{"probes"}, Verbs: []string{"create", "update"}}}},
			{ID: "denied", Permissions: nil},
		}}
	require.NoError(t, pv.ValidateApplicationPolicy(*policy))
	f.backend.config.ResourcePolicy = policy
	f.request.ApplicationPolicy = f.backend.ApplicationPolicy()
	plan := f.backend.ApplicationPlanDefaults()
	plan.Build.Package = "."
	plan.SyntheticSecrets = []pv.ApplicationSyntheticSecret{{Name: "synthetic-credential",
		Keys: []pv.ApplicationSyntheticSecretKey{{Name: "value", MarkerID: "sensitive"}}}}
	f.request.Checks.Application = &plan
	healthy, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{Exists: true, Found: true, Value: json.RawMessage(`true`)})
	require.NoError(t, err)
	failure, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{Exists: true, Found: true, Value: json.RawMessage(`false`)})
	require.NoError(t, err)
	for i := range f.request.Checks.Checks {
		check := &f.request.Checks.Checks[i]
		check.Healthy, check.Failure = healthy, failure
		check.Application.Kubernetes = &pv.ApplicationKubernetesCheck{APIVersion: "fixtures.orka.ai/v1", Resource: "probes", Name: check.ID, Pointer: "/status/ready"}
		if check.Kind == pv.Normal {
			check.Application.Kubernetes.NormalValue = json.RawMessage(`true`)
		}
		check.Application.Actions = []pv.ApplicationAction{{ActorID: "writer", Verb: "create", ExpectedStatus: 201,
			Resource: json.RawMessage(`{"apiVersion":"fixtures.orka.ai/v1","kind":"Probe","metadata":{"name":"` + check.ID + `"},"spec":{"value":"fixture"}}`)}}
	}
}

func TestOperatorPolicyCRDIdentityAndSchemaAreFrozen(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	r := f.newRun()
	require.NoError(t, f.backend.verifyProfile(t.Context()))
	original, err := f.backend.RequestDigest(f.request)
	require.NoError(t, err)
	request := f.request
	content, err := json.Marshal(f.request.ApplicationPolicy)
	require.NoError(t, err)
	var changed pv.ApplicationPolicy
	require.NoError(t, json.Unmarshal(content, &changed))
	changed.Resources[0].Verbs = append(changed.Resources[0].Verbs, "delete")
	request.ApplicationPolicy = &changed
	_, err = f.backend.RequestDigest(request)
	require.ErrorIs(t, err, contract.ErrInvalid)
	require.Equal(t, original, r.state.InputDigest)
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Name: "probes.fixtures.orka.ai"}, crd))
	require.NoError(t, unstructured.SetNestedField(crd.Object, "Cluster", "spec", "scope"))
	require.NoError(t, f.target.Update(t.Context(), crd))
	require.ErrorIs(t, f.backend.verifyProfile(t.Context()), contract.ErrConflict)
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
}

func TestSyntheticSecretsAndActorCredentialsNeverShareCandidateNamespace(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	f.autoPods = false
	r := f.newRun()
	require.NoError(t, f.backend.ensureTask(t.Context(), r, pv.Original))
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.NoError(t, f.backend.ensureProtected(t.Context(), r, pv.Original))
	candidateNamespace, protectedNamespace := namespaceName(r, pv.Original), namespaceName(r, protectedSide(pv.Original))
	candidate := &corev1.SecretList{}
	require.NoError(t, f.target.List(t.Context(), candidate, client.InNamespace(candidateNamespace)))
	require.Len(t, candidate.Items, 1)
	require.Equal(t, "synthetic-credential", candidate.Items[0].Name)
	value := candidate.Items[0].Data["value"]
	require.Len(t, value, 64)
	for _, byteValue := range value {
		require.True(t, byteValue >= '0' && byteValue <= '9' || byteValue >= 'a' && byteValue <= 'f')
	}
	ref, err := f.backend.markerReference(t.Context(), r, pv.Original, "sensitive")
	require.NoError(t, err)
	require.Equal(t, protectedNamespace, ref.Namespace)
	require.Equal(t, pv.Digest(value), ref.Digest)
	require.NotEqual(t, candidateNamespace, ref.Namespace)
	subjectRole := &rbacv1.Role{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: candidateNamespace, Name: "orka-subject"}, subjectRole))
	require.NotEmpty(t, subjectRole.Rules)

	check := f.request.Checks.Checks[0]
	actors, err := f.backend.actorIdentities(r, pv.Original, check)
	require.NoError(t, err)
	require.Len(t, actors, 1)
	require.Equal(t, protectedNamespace, actors[0].Namespace)
	role := "observe-0"
	_, err = f.backend.reserveActions(t.Context(), r, pv.Original, role, check, actors)
	require.NoError(t, err)
	job, err := f.backend.ensureJob(t.Context(), r, protectedSide(pv.Original), role, struct{}{}, jobOptions{
		role: "observer", mode: "observe", image: f.config.WorkerImage, taskUID: string(r.state.ArmTasks[pv.Original].UID),
		serviceAccount: "observer", token: true, actors: actors, actorSecret: actorSecretName(role), deadline: 30})
	require.NoError(t, err)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending-observer", Namespace: protectedNamespace, UID: "pending-observer",
		Labels:          map[string]string{runLabel: string(r.object.UID), "job-name": job.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}},
		Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodPending}}
	pod.Spec.NodeName = r.state.Target.NodeName
	require.NoError(t, f.target.Create(t.Context(), pod))
	observed, _, ready, err := f.backend.observeJob(t.Context(), r, protectedSide(pv.Original), role, job, f.config.WorkerImageID, false, true)
	require.NoError(t, err)
	require.False(t, ready)
	require.NotNil(t, observed)
	kube := kubernetesfake.NewClientset()
	calls := 0
	kube.PrependReactor("create", "serviceaccounts", func(action kubetesting.Action) (bool, runtime.Object, error) {
		require.Equal(t, "token", action.GetSubresource())
		require.Equal(t, protectedNamespace, action.GetNamespace())
		tokenRequest := action.(kubetesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
		require.Equal(t, pod.UID, tokenRequest.Spec.BoundObjectRef.UID)
		require.Equal(t, pod.Name, tokenRequest.Spec.BoundObjectRef.Name)
		calls++
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token: "non-credential-unit-fixture", ExpirationTimestamp: metav1.NewTime(time.Now().Add(10 * time.Minute))}}, nil
	})
	f.backend.targetKube = kube
	require.NoError(t, f.backend.ensureActorCredentials(t.Context(), r, pv.Original, role, observed, actors))
	require.Equal(t, 1, calls)
	require.NoError(t, f.backend.ensureActorCredentials(t.Context(), r, pv.Original, role, observed, actors))
	require.Equal(t, 1, calls, "a resumed credential receipt cannot mint fresh authority")
	secret := &corev1.Secret{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: protectedNamespace, Name: actorSecretName(role)}, secret))
	require.Equal(t, pod.UID, secret.OwnerReferences[0].UID)
	require.True(t, ptr.Deref(secret.Immutable, false))
	require.Len(t, secret.Data, 1)
	stateBytes, err := json.Marshal(r.state)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(stateBytes), "non-credential-unit-fixture"), "actor token must not enter the ledger")
	require.False(t, strings.Contains(string(stateBytes), string(value)), "synthetic values must not enter the ledger")
	require.NoError(t, f.target.List(t.Context(), candidate, client.InNamespace(candidateNamespace)))
	require.Len(t, candidate.Items, 1)
}

func TestActorReceiptSurvivesUnexpectedSuccessAndNeverReplaysCreate(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	f.request.Checks.Checks[0].Application.Actions[0].ExpectedStatus = 403
	r := f.newRun()
	require.NoError(t, f.backend.ensureTask(t.Context(), r, pv.Original))
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.NoError(t, f.backend.ensureProtected(t.Context(), r, pv.Original))
	check := f.request.Checks.Checks[0]
	actors, err := f.backend.actorIdentities(r, pv.Original, check)
	require.NoError(t, err)
	targets, err := f.backend.reserveActions(t.Context(), r, pv.Original, "observe-0", check, actors)
	require.NoError(t, err)
	require.True(t, targets[0].MustBeAbsent)
	object, err := pv.ValidateApplicationResourceWithPolicy(check.Application.Actions[0].Resource, f.request.ApplicationPolicy)
	require.NoError(t, err)
	object.SetNamespace(namespaceName(r, pv.Original))
	object.SetLabels(map[string]string{pv.ApplicationRunLabel: string(r.object.UID)})
	object.SetUID(types.UID("actor-created-object"))
	require.NoError(t, f.target.Create(t.Context(), object))
	input := pv.ApplicationWorkerInput{CheckID: check.ID, Actors: actors, ActionTargets: targets,
		Manifest: pv.Manifest{ApplicationPolicy: f.request.ApplicationPolicy, Checks: []pv.Check{check}}}
	result := pv.ApplicationActionObservation{Index: 0, Actor: actors[0], APIVersion: targets[0].APIVersion,
		Resource: targets[0].Resource, Name: targets[0].Name, Target: targets[0], StatusCode: 201,
		UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion()}
	require.NoError(t, f.backend.recordAction(t.Context(), r, pv.Original, "observe-0", input, result))
	receipt := r.state.Actions[actionSlot(pv.Original, "observe-0", 0)]
	require.NotNil(t, receipt.Outcome)
	require.Equal(t, object.GetUID(), r.effect(receipt.EffectKey).UID)
	require.NoError(t, f.backend.recordAction(t.Context(), r, pv.Original, "observe-0", input, result))
	result.UID = "replacement"
	require.ErrorIs(t, f.backend.recordAction(t.Context(), r, pv.Original, "observe-0", input, result), contract.ErrConflict)
}

func TestPolicyDoesNotPermitArbitraryAuthority(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	f.backend.qualified.Store(true)
	for _, resource := range []string{"pods", "secrets", "roles", "clusterroles", "serviceaccounts/token", "*"} {
		t.Run(resource, func(t *testing.T) {
			request := f.request
			content, err := json.Marshal(f.request.Checks)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(content, &request.Checks))
			request.Checks.Application.Permissions = append(request.Checks.Application.Permissions,
				pv.ApplicationPermission{APIGroup: "", Resources: []string{resource}, Verbs: []string{"create"}})
			_, err = f.backend.RequestDigest(request)
			require.ErrorIs(t, err, contract.ErrInvalid)
		})
	}
}
