package patchverification

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestApplicationNormalControlsRejectVacuousDeclarationsBeforeExecution(t *testing.T) {
	for _, value := range []string{"", "null", `""`, `" \n\t"`, "[]", "{}", `[null,""]`, `{"ready":null}`, `{"nested":{"value":[]}}`} {
		t.Run(value, func(t *testing.T) {
			manifest := applicationTestManifest(t)
			check := &manifest.Checks[1]
			check.Application.Kubernetes.NormalValue = []byte(value)
			if value != "" {
				check.Healthy, _ = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte(value)})
			}
			require.Error(t, ValidateManifest(manifest))
		})
	}
	for name, modify := range map[string]func(*Manifest){
		"never-created target": func(m *Manifest) { m.Checks[1].Application.Resources = nil },
		"absent healthy result": func(m *Manifest) {
			m.Checks[1].Healthy, _ = ApplicationAPIExpectation(ApplicationAPIValue{})
		},
		"missing status healthy result": func(m *Manifest) {
			m.Checks[1].Healthy, _ = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true})
		},
		"normal value differs from frozen healthy result": func(m *Manifest) {
			m.Checks[1].Application.Kubernetes.NormalValue = []byte("false")
		},
		"subject cannot write status":  func(m *Manifest) { m.Application.Permissions = nil },
		"subject can only read status": func(m *Manifest) { m.Application.Permissions[0].Verbs = []string{"get"} },
		"subject writes another target": func(m *Manifest) {
			m.Application.Permissions[0].ResourceNames = []string{"other-target"}
		},
		"reproduction cannot masquerade as a normal declaration": func(m *Manifest) {
			m.Checks[0].Application.Kubernetes.NormalValue = []byte("true")
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := applicationTestManifest(t)
			modify(&manifest)
			require.Error(t, ValidateManifest(manifest))
		})
	}
}

func TestApplicationNormalControlDoesNotInventPositiveTruthiness(t *testing.T) {
	for _, value := range []string{"false", "0", "9007199254740993", `"accepted"`, `{"errors":0}`, `[false]`} {
		t.Run(value, func(t *testing.T) {
			manifest := applicationTestManifest(t)
			check := &manifest.Checks[1]
			check.Application.Kubernetes.NormalValue = []byte(value)
			var err error
			check.Healthy, err = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte(value)})
			require.NoError(t, err)
			check.Failure, err = ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte(`"unexpected"`)})
			require.NoError(t, err)
			require.NoError(t, ValidateManifest(manifest), "a real transition may establish an explicitly declared false or zero")
			binding, evidence := applicationTestEvidence(t, manifest)
			require.NoError(t, ValidateObservation(manifest, binding, evidence[1]))
			require.Equal(t, Verified, Evaluate(manifest, binding, evidence).Conclusion)
		})
	}
}

func TestApplicationNormalEvidenceRejectsNoOpAndAdmissionDefaultOutcomes(t *testing.T) {
	manifest := applicationTestManifest(t)
	binding, evidence := applicationTestEvidence(t, manifest)
	require.Equal(t, Verified, Evaluate(manifest, binding, evidence).Conclusion)
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-op candidate", true: "default status mistaken for reconciliation"}[changed], func(t *testing.T) {
			binding, evidence := applicationTestEvidence(t, manifest)
			normal := &evidence[3]
			if changed {
				normal.Application.APIBaseline.Result = normal.Application.Kubernetes.Result
			} else {
				normal.Application.Kubernetes.Result = normal.Application.APIBaseline.Result
				normal.Application.Kubernetes.ResourceVersion = normal.Application.APIBaseline.ResourceVersion
				content, err := json.Marshal(normal.Application.Kubernetes.Result)
				require.NoError(t, err)
				normal.StdoutDigest, normal.StdoutBytes = Digest(content), len(content)
				require.NoError(t, ValidateObservation(manifest, binding, *normal), "a complete failed normal remains truthful evidence")
			}
			assessment := Evaluate(manifest, binding, evidence)
			require.Equal(t, UnableToVerify, assessment.Conclusion)
			require.NotEqual(t, NotReproduced, assessment.Conclusion)
		})
	}
	for name, modify := range map[string]func(*ApplicationObservation){
		"missing initial evidence": func(app *ApplicationObservation) { app.APIBaseline = nil },
		"initial target absent": func(app *ApplicationObservation) {
			app.APIBaseline.Result = ApplicationAPIResult{}
		},
		"initial UID changed":     func(app *ApplicationObservation) { app.APIBaseline.UID = "other-uid" },
		"initial version missing": func(app *ApplicationObservation) { app.APIBaseline.ResourceVersion = "" },
		"same initial version": func(app *ApplicationObservation) {
			app.APIBaseline.ResourceVersion = app.Kubernetes.ResourceVersion
		},
		"initial pointer swapped": func(app *ApplicationObservation) { app.APIBaseline.Pointer = "/status/unrelated" },
		"final pointer swapped":   func(app *ApplicationObservation) { app.Kubernetes.Pointer = "/status/unrelated" },
		"initial digest invalid":  func(app *ApplicationObservation) { app.APIBaseline.Result.ValueDigest = "unbound" },
	} {
		t.Run(name, func(t *testing.T) {
			binding, evidence := applicationTestEvidence(t, manifest)
			modify(evidence[1].Application)
			require.Error(t, ValidateObservation(manifest, binding, evidence[1]))
		})
	}
}

func TestApplicationAPIBaselineRetainsActualCreateRevisionWithoutRawValues(t *testing.T) {
	check := applicationTestManifest(t).Checks[1]
	value := "observed-only-" + rand.Text()
	created := applicationStatusObject("run-original", "case-normal")
	created.Object["status"] = map[string]any{"accepted": value}
	initial, err := ApplicationAPIIdentityFromObject(created, "run-original", *check.Application.Kubernetes, nil)
	require.NoError(t, err)
	require.Equal(t, "object-uid", initial.UID)
	require.Equal(t, "17", initial.ResourceVersion)
	require.Equal(t, "/status/accepted", initial.Pointer)
	require.NoError(t, ValidateApplicationAPIBaseline(check, *initial))
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.Equal(t, Digest(raw), initial.Result.ValueDigest)
	created.SetResourceVersion("18")
	created.Object["status"] = map[string]any{"accepted": true}
	later, err := ApplicationAPIIdentityFromObject(created, "run-original", *check.Application.Kubernetes, nil)
	require.NoError(t, err)
	require.NotEqual(t, initial.Result, later.Result)
	require.Equal(t, "17", initial.ResourceVersion, "a later GET or source object mutation cannot rewrite the captured initial revision")
	encoded, err := json.Marshal(initial)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), value)
	for name, modify := range map[string]func(*unstructured.Unstructured){
		"wrong namespace": func(object *unstructured.Unstructured) { object.SetNamespace("other") },
		"wrong name":      func(object *unstructured.Unstructured) { object.SetName("other") },
		"missing UID":     func(object *unstructured.Unstructured) { object.SetUID(types.UID("")) },
		"missing RV":      func(object *unstructured.Unstructured) { object.SetResourceVersion("") },
		"deleting":        func(object *unstructured.Unstructured) { object.SetDeletionTimestamp(new(metav1.Now())) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := created.DeepCopy()
			modify(bad)
			_, err := ApplicationAPIIdentityFromObject(bad, "run-original", *check.Application.Kubernetes, nil)
			require.Error(t, err)
		})
	}
}

func applicationNormalActorEvidence(t *testing.T) (Manifest, Binding, []Observation) {
	t.Helper()
	manifest := applicationTestManifest(t)
	manifest.ApplicationPolicy = applicationTestPolicy(t)
	manifest.Application.Permissions = []ApplicationPermission{{APIGroup: "testing.example", Resources: []string{"widgets/status"},
		Verbs: []string{"patch"}}}
	for index := range manifest.Checks {
		manifest.Checks[index].Application.Kubernetes.APIVersion = "testing.example/v1"
		manifest.Checks[index].Application.Kubernetes.Resource = "widgets"
	}
	normal := &manifest.Checks[1]
	normal.Application.Resources = nil
	normal.Application.Actions = []ApplicationAction{{ActorID: "author", Verb: "create", ExpectedStatus: 201,
		Resource: []byte(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"case-normal"},"spec":{"enabled":true}}`)}}
	require.NoError(t, ValidateManifest(manifest))
	binding, evidence := applicationTestEvidence(t, manifest)
	for index := range evidence {
		observation := &evidence[index]
		if observation.CheckID != normal.ID {
			continue
		}
		app := observation.Application
		actor := ApplicationActorIdentity{ID: "author", Namespace: "protected-" + observation.Side, NamespaceUID: "protected-" + observation.Side,
			ServiceAccountName: "author", ServiceAccountUID: "author-" + observation.Side}
		target := ApplicationActionTarget{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets", Name: "case-normal",
			MustBeAbsent: true, OwnerUID: "run-ledger"}
		app.Actions = []ApplicationActionObservation{{Index: 0, Actor: actor, APIVersion: target.APIVersion,
			Resource: target.Resource, Name: target.Name, StatusCode: 201, UID: app.APIBaseline.UID,
			ResourceVersion: app.APIBaseline.ResourceVersion, Target: target, InitialAPI: app.APIBaseline}}
	}
	return manifest, binding, evidence
}

func TestApplicationNormalActorResponseIsNotTheSubjectsProof(t *testing.T) {
	manifest, binding, evidence := applicationNormalActorEvidence(t)
	require.Equal(t, Verified, Evaluate(manifest, binding, evidence).Conclusion)
	for name, modify := range map[string]func(*ApplicationObservation){
		"actor-only default": func(app *ApplicationObservation) {
			app.APIBaseline.Result = app.Kubernetes.Result
		},
		"missing actor initial receipt": func(app *ApplicationObservation) { app.Actions[0].InitialAPI = nil },
		"forged actor UID": func(app *ApplicationObservation) {
			initial := *app.Actions[0].InitialAPI
			initial.UID = "unrelated-target"
			app.Actions[0].InitialAPI = &initial
		},
		"forged actor pointer": func(app *ApplicationObservation) {
			initial := *app.Actions[0].InitialAPI
			initial.Pointer = "/status/other"
			app.Actions[0].InitialAPI = &initial
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, binding, evidence := applicationNormalActorEvidence(t)
			modify(evidence[1].Application)
			require.Error(t, ValidateObservation(manifest, binding, evidence[1]))
			require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion)
		})
	}
}

func TestApplicationNormalFinalFrameCannotReplaceBackendBaseline(t *testing.T) {
	manifest := applicationTestManifest(t)
	binding, evidence := applicationTestEvidence(t, manifest)
	observation := evidence[1]
	worker := applicationTestIdentity("observer-original-normal", manifest.Environment.Dependencies[ApplicationObserverImageIDKey])
	input := ApplicationWorkerInput{Version: ApplicationProtocolVersion, Manifest: manifest, Binding: binding,
		Side: Original, TaskUID: "observer-task", CheckID: "normal", Target: observation.Application.Target,
		Worker: worker, Subject: &observation.Application.Subject, Artifact: &observation.Application.Artifact,
		APIBaseline: observation.Application.APIBaseline}
	require.NoError(t, ValidateApplicationWorkerInput(input))
	digest, err := ApplicationWorkerInputDigest(input)
	require.NoError(t, err)
	frame := ApplicationStreamFrame{Version: ApplicationProtocolVersion, Kind: "final", TaskUID: input.TaskUID, PodUID: worker.PodUID,
		InputDigest: digest, Report: &ApplicationReport{Version: ApplicationProtocolVersion, TaskUID: input.TaskUID, PodUID: worker.PodUID,
			Evidence: ExecutionEvidence{Observation: observation}}}
	require.NoError(t, ValidateApplicationStreamFrame(input, frame))
	forged := *observation.Application.APIBaseline
	forged.ResourceVersion = "100"
	frame.Report.Evidence.Observation.Application.APIBaseline = &forged
	require.Error(t, ValidateApplicationStreamFrame(input, frame))
	input.APIBaseline = nil
	require.Error(t, ValidateApplicationWorkerInput(input), "a no-actor normal requires its owned creation baseline before execution")
	encoded, err := json.Marshal(manifest.Checks[1].Application.Kubernetes)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"normalValue":true`)
	require.NotContains(t, strings.ToLower(string(encoded)), "baseline")
}

func TestApplicationNormalObserverUsesOwnedFixtureInitialResponseWithoutActors(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no reconciliation", true: "actual status transition"}[changed], func(t *testing.T) {
			manifest := applicationTestManifest(t)
			binding, evidence := applicationTestEvidence(t, manifest)
			check := manifest.Checks[1]
			proof := evidence[1]
			worker := applicationTestIdentity("observer-original-normal", manifest.Environment.Dependencies[ApplicationObserverImageIDKey])
			input := ApplicationWorkerInput{Version: ApplicationProtocolVersion, Manifest: manifest, Binding: binding,
				Side: Original, TaskUID: "observer-task", CheckID: check.ID, Target: proof.Application.Target,
				Worker: worker, Subject: &proof.Application.Subject, Artifact: &proof.Application.Artifact}
			initialObject := applicationStatusObject(input.Target.Namespace, check.Application.Kubernetes.Name)
			initialObject.Object["status"] = map[string]any{"accepted": false}
			initial, err := ApplicationAPIIdentityFromObject(initialObject, input.Target.Namespace, *check.Application.Kubernetes, nil)
			require.NoError(t, err)
			input.APIBaseline = initial
			subject := applicationLiveTestPod(input.Target.Namespace, *input.Subject, manifest.Application.Runtime.Image, true)
			observer := applicationLiveTestPod(input.Target.Namespace, worker, manifest.Environment.Dependencies[ApplicationObserverImageKey], false)
			objects := make([]runtime.Object, 0, 4)
			objects = append(objects, subject, observer)
			for _, identity := range []ApplicationContainerIdentity{*input.Subject, worker} {
				objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
					Name: identity.ServiceAccountName, Namespace: input.Target.Namespace, UID: types.UID(identity.ServiceAccountUID),
				}})
			}
			finalObject := initialObject.DeepCopy()
			if changed {
				finalObject.SetResourceVersion("18")
				finalObject.Object["status"] = map[string]any{"accepted": true}
			}
			observed, err := ObserveApplication(t.Context(), input, kubefake.NewSimpleClientset(objects...),
				dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), finalObject), nil)
			require.NoError(t, err)
			require.NoError(t, ValidateObservation(manifest, binding, observed.Observation))
			require.Equal(t, initial, observed.Observation.Application.APIBaseline)
			require.Equal(t, changed, observed.Observation.StdoutDigest == Digest([]byte(check.Healthy.Stdout)))
			require.Equal(t, "17", observed.Observation.Application.APIBaseline.ResourceVersion)
			require.Empty(t, observed.Observation.Application.Actions)
		})
	}
}
