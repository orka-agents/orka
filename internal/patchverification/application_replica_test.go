package patchverification

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func applicationReplicaPolicy(t *testing.T) *ApplicationPolicy {
	t.Helper()
	policy := applicationTestPolicy(t)
	policy.Fixtures = []ApplicationFixtureProfile{{
		ID: "replica-profile", Image: "example/fixture@" + Digest([]byte("fixture-image")), ImageID: Digest([]byte("fixture-config")),
		Args:          []string{"/usr/bin/sleep", "3600"},
		ReplicaTarget: &ApplicationReplicaTargetProfile{Name: "replica-target", InitialReplicas: 0, MaxReplicas: 4},
	}}
	policy.Resources = append(policy.Resources,
		ApplicationResourceRule{APIVersion: "apps/v1", Kind: "Deployment", Resource: "deployments", Namespaced: true,
			Verbs: []string{"get", "list", "watch"}, Status: true, Scale: true},
		ApplicationResourceRule{APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler", Resource: "horizontalpodautoscalers",
			Namespaced: true, Verbs: []string{"get", "list", "watch", "create", "update"}, Status: true},
	)
	policy.SubjectPermissions = append(policy.SubjectPermissions,
		ApplicationPermission{APIGroup: "apps", Resources: []string{"deployments"}, Verbs: []string{"get", "list", "watch"}},
		ApplicationPermission{APIGroup: "apps", Resources: []string{"deployments/scale"}, Verbs: []string{"get", "patch", "update"}, ResourceNames: []string{"replica-target"}},
		ApplicationPermission{APIGroup: "autoscaling", Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get", "list", "watch", "create", "update"}},
	)
	require.NoError(t, ValidateApplicationPolicy(*policy))
	return policy
}

func TestApplicationReplicaPrimitivePinsNameAndOperatorTemplate(t *testing.T) {
	policy := applicationReplicaPolicy(t)
	plan := *applicationTestManifest(t).Application
	plan.Fixtures = []ApplicationFixture{{ID: "replica-target", Kind: "replica", Profile: "replica-profile"}}
	plan.Permissions = []ApplicationPermission{{APIGroup: "apps", Resources: []string{"deployments/scale"},
		Verbs: []string{"patch"}, ResourceNames: []string{"replica-target"}}}
	require.NoError(t, ValidateApplicationPlanWithPolicy(plan, policy))
	plan.Fixtures[0].ID = "another-target"
	require.Error(t, ValidateApplicationPlanWithPolicy(plan, policy))
	plan.Fixtures[0].ID = "replica-target"
	plan.Permissions[0].ResourceNames = nil
	require.Error(t, ValidateApplicationPlanWithPolicy(plan, policy))
	plan.Permissions[0].ResourceNames = []string{"replica-target"}
	plan.Fixtures[0].Config = map[string]string{"script": "untrusted"}
	require.Error(t, ValidateApplicationPlanWithPolicy(plan, policy))
	manifest := applicationTestManifest(t)
	manifest.ApplicationPolicy = policy
	manifest.Application.Fixtures = []ApplicationFixture{{ID: "replica-target", Kind: "replica", Profile: "replica-profile"}}
	require.Error(t, ValidateManifest(manifest), "selecting a replica cannot silently omit actual scaling normal control")
}

func TestApplicationScalingNeverAdmitsRawDeploymentHPAOrActorPrivilege(t *testing.T) {
	policy := applicationReplicaPolicy(t)
	for _, raw := range []json.RawMessage{
		[]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"replica-target"},"spec":{"replicas":1}}`),
		[]byte(`{"apiVersion":"autoscaling/v2","kind":"HorizontalPodAutoscaler","metadata":{"name":"fake-normal"},"spec":{}}`),
	} {
		_, err := ValidateApplicationResourceWithPolicy(raw, policy)
		require.Error(t, err)
	}
	policy.Actors[0].Permissions = append(policy.Actors[0].Permissions, ApplicationPermission{
		APIGroup: "autoscaling", Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"create"},
	})
	require.Error(t, ValidateApplicationPolicy(*policy))
	for _, mutate := range []func(*ApplicationPolicy){
		func(p *ApplicationPolicy) { p.Fixtures[0].ReplicaTarget.MaxReplicas = 5 },
		func(p *ApplicationPolicy) { p.Fixtures[0].ReplicaTarget.InitialReplicas = -1 },
		func(p *ApplicationPolicy) { p.Fixtures[0].ConfigKeys = []string{"template"} },
		func(p *ApplicationPolicy) { p.Resources[len(p.Resources)-2].Verbs = []string{"update"} },
		func(p *ApplicationPolicy) { p.Resources[len(p.Resources)-1].Verbs = []string{"delete"} },
		func(p *ApplicationPolicy) { p.Resources[len(p.Resources)-2].Namespaced = false },
	} {
		p := applicationReplicaPolicy(t)
		mutate(p)
		require.Error(t, ValidateApplicationPolicy(*p))
	}
}

func TestApplicationReplicaNormalControlMustShowActualNonInitialReconciliation(t *testing.T) {
	policy := applicationReplicaPolicy(t)
	check := applicationTestManifest(t).Checks[1]
	check.Application.Kubernetes = &ApplicationKubernetesCheck{APIVersion: "apps/v1", Resource: "deployments",
		Name: "replica-target", Pointer: "/status/readyReplicas"}
	check.Healthy, _ = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte("1")})
	check.Failure, _ = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: false})
	require.Error(t, ValidateApplicationCheckWithPolicy(check, policy))
	check.Application.Actions = []ApplicationAction{{ActorID: "author", Verb: "create", ExpectedStatus: 201,
		Resource: []byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"normal-scaling"},"spec":{}}`)}}
	require.NoError(t, ValidateApplicationCheckWithPolicy(check, policy))
	policy.Fixtures[0].ReplicaTarget.InitialReplicas = 1
	require.Error(t, ValidateApplicationCheckWithPolicy(check, policy), "fixture's initial ready status is not controller reconciliation")
}

func applicationReplicaTestEvidence(t *testing.T, initial, healthy, failure int32) (Manifest, Binding, []Observation) {
	t.Helper()
	manifest := applicationTestManifest(t)
	manifest.ApplicationPolicy = applicationReplicaPolicy(t)
	manifest.ApplicationPolicy.Fixtures[0].ReplicaTarget.InitialReplicas = initial
	manifest.Application.Fixtures = []ApplicationFixture{{ID: "replica-target", Kind: "replica", Profile: "replica-profile"}}
	check := &manifest.Checks[1]
	check.Application.Kubernetes = &ApplicationKubernetesCheck{APIVersion: "apps/v1", Resource: "deployments",
		Name: "replica-target", Pointer: "/status/readyReplicas"}
	check.Application.Actions = []ApplicationAction{{ActorID: "author", Verb: "create", ExpectedStatus: 201,
		Resource: []byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"normal-scaling"},"spec":{}}`)}}
	raw, err := json.Marshal(healthy)
	require.NoError(t, err)
	check.Healthy, err = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: raw})
	require.NoError(t, err)
	raw, err = json.Marshal(failure)
	require.NoError(t, err)
	check.Failure, err = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: raw})
	require.NoError(t, err)
	// The separate reproduction still requires its own normal control.
	control := applicationTestManifest(t).Checks[1]
	control.ID = "api-normal"
	manifest.Checks = append(manifest.Checks, control)
	binding, evidence := applicationTestEvidence(t, manifest)
	actor := ApplicationActorIdentity{ID: "author", Namespace: "protected-run", NamespaceUID: "protected-uid",
		ServiceAccountName: "author", ServiceAccountUID: "author-uid"}
	target := ApplicationActionTarget{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets",
		Name: "normal-scaling", MustBeAbsent: true, OwnerUID: "ledger-uid"}
	for index := range evidence {
		observation := &evidence[index]
		if observation.CheckID != manifest.Checks[1].ID {
			continue
		}
		receipt, err := (applicationTestActor{identity: actor, status: 201}).Apply(t.Context(), observation.Application.Target.Namespace,
			manifest.Checks[1].Application.Actions[0], target)
		require.NoError(t, err)
		observation.Application.Actions = []ApplicationActionObservation{receipt}
		setApplicationReplicaTestCount(t, manifest, observation, healthy, healthy)
	}
	return manifest, binding, evidence
}

func setApplicationReplicaTestCount(t *testing.T, manifest Manifest, observation *Observation, desired, ready int32) {
	t.Helper()
	profile := ApplicationReplicaTargetFor(manifest.ApplicationPolicy)
	pods := make([]ApplicationContainerIdentity, 0, ready)
	for index := range ready {
		pod := applicationTestIdentity("replica-"+observation.Side+"-"+strconv.Itoa(int(index)), profile.ImageID)
		pod.JobUID, pod.Namespace, pod.NamespaceUID = "", observation.Application.Target.Namespace, observation.Application.Target.NamespaceUID
		pods = append(pods, pod)
	}
	observation.Application.Replica = &ApplicationReplicaObservation{DeploymentUID: observation.Application.Kubernetes.UID,
		ReplicaSetUIDs: []string{"owned-rs-" + observation.Side}, Pods: pods,
		DesiredReplicas: desired, ReadyReplicas: ready, InitialReplicas: profile.ReplicaTarget.InitialReplicas,
		MaxReplicas: profile.ReplicaTarget.MaxReplicas}
	raw, err := json.Marshal(ready)
	require.NoError(t, err)
	expected, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: raw})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(expected.Stdout), &observation.Application.Kubernetes.Result))
	observation.StdoutDigest, observation.StdoutBytes = Digest([]byte(expected.Stdout)), len(expected.Stdout)
}

func TestApplicationReplicaTransportIsNotAcceptedWithoutReadyDescendantEvidence(t *testing.T) {
	manifest, binding, evidence := applicationReplicaTestEvidence(t, 0, 1, 0)
	observation := evidence[1]
	proof := observation.Application.Replica
	observation.Application.Replica = nil
	require.NoError(t, ValidateApplicationWorkerObservation(manifest, binding, observation))
	require.Error(t, ValidateObservation(manifest, binding, observation), "worker status alone must not become final replica evidence")
	observation.Application.Replica = proof
	require.NoError(t, ValidateObservation(manifest, binding, observation))
	observation.Application.Replica.Pods = nil
	require.Error(t, ValidateObservation(manifest, binding, observation))
}

func TestApplicationReplicaFailedNormalEvidenceRecordsRegression(t *testing.T) {
	for _, test := range []struct {
		name                    string
		initial, desired, ready int32
	}{
		{name: "unchanged initial count", initial: 1, desired: 1, ready: 1},
		{name: "zero ready replicas", initial: 0, desired: 0, ready: 0},
		{name: "partially ready replicas", initial: 0, desired: 2, ready: 1},
		{name: "changed but failed count", initial: 0, desired: 1, ready: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest, binding, evidence := applicationReplicaTestEvidence(t, test.initial, 2, test.ready)
			require.Equal(t, Verified, Evaluate(manifest, binding, evidence).Conclusion)
			patched := &evidence[len(manifest.Checks)+1]
			setApplicationReplicaTestCount(t, manifest, patched, test.desired, test.ready)
			require.NoError(t, ValidateObservation(manifest, binding, *patched))
			assessment := Evaluate(manifest, binding, evidence)
			require.Equal(t, Regression, assessment.Conclusion)
			require.Equal(t, OutcomeRegression, assessment.Checks[1].Outcome)
			require.Equal(t, OutcomePassed, assessment.Checks[1].OriginalOutcome)
			require.Equal(t, OutcomeRegression, assessment.Checks[1].PatchedOutcome)

			setApplicationReplicaTestCount(t, manifest, &evidence[1], test.desired, test.ready)
			require.NoError(t, ValidateObservation(manifest, binding, evidence[1]))
			require.False(t, OriginalReady(manifest, binding, evidence))
			require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion)
		})
	}
}

func TestApplicationReplicaFailedNormalEvidenceRetainsIntegrityChecks(t *testing.T) {
	for name, mutate := range map[string]func(*Observation){
		"missing replica evidence": func(o *Observation) { o.Application.Replica = nil },
		"wrong deployment UID":     func(o *Observation) { o.Application.Replica.DeploymentUID = "another-deployment" },
		"wrong initial count":      func(o *Observation) { o.Application.Replica.InitialReplicas = 0 },
		"desired exceeds maximum":  func(o *Observation) { o.Application.Replica.DesiredReplicas = 5 },
		"ready exceeds desired":    func(o *Observation) { o.Application.Replica.DesiredReplicas = 0 },
		"missing ready pod":        func(o *Observation) { o.Application.Replica.Pods = nil },
		"missing replica set":      func(o *Observation) { o.Application.Replica.ReplicaSetUIDs = nil },
		"duplicate replica set": func(o *Observation) {
			o.Application.Replica.ReplicaSetUIDs = append(o.Application.Replica.ReplicaSetUIDs, o.Application.Replica.ReplicaSetUIDs[0])
		},
		"wrong pod image":     func(o *Observation) { o.Application.Replica.Pods[0].ImageID = Digest([]byte("other-image")) },
		"wrong namespace":     func(o *Observation) { o.Application.Replica.Pods[0].NamespaceUID = "another-namespace" },
		"observer as replica": func(o *Observation) { o.Application.Replica.Pods[0].PodUID = o.PodUID },
		"wrong output digest": func(o *Observation) { o.StdoutDigest = Digest([]byte("unmeasured")) },
		"wrong output size":   func(o *Observation) { o.StdoutBytes++ },
		"wrong measured count": func(o *Observation) {
			o.Application.Replica.ReadyReplicas = 0
			o.Application.Replica.Pods = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, binding, evidence := applicationReplicaTestEvidence(t, 1, 2, 1)
			patched := &evidence[len(manifest.Checks)+1]
			setApplicationReplicaTestCount(t, manifest, patched, 1, 1)
			require.NoError(t, ValidateObservation(manifest, binding, *patched))
			mutate(patched)
			require.Error(t, ValidateObservation(manifest, binding, *patched))
		})
	}
}

func TestApplicationReplicaHealthyNormalStillRequiresReadyTransition(t *testing.T) {
	manifest, binding, evidence := applicationReplicaTestEvidence(t, 0, 1, 0)
	evidence[1].Application.Replica.DesiredReplicas = 2
	require.Error(t, ValidateObservation(manifest, binding, evidence[1]))
}

func TestApplicationReplicaUnmatchedNormalCountRemainsUnverified(t *testing.T) {
	manifest, binding, evidence := applicationReplicaTestEvidence(t, 1, 2, 1)
	patched := &evidence[len(manifest.Checks)+1]
	setApplicationReplicaTestCount(t, manifest, patched, 3, 3)
	require.NoError(t, ValidateObservation(manifest, binding, *patched))
	require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion)
}
