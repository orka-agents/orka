package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func frozenRequirementsPoolFixture(t *testing.T, native bool) (*TaskReconciler, ACPRuntimePlan, *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	r, class, provider, config := externalACPFixture(t, true)
	r.Scheme = r.Client.Scheme()
	provider.Status.Adapter = &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: "v1"}
	provider.Status.Conditions = append(provider.Status.Conditions,
		metav1.Condition{Type: string(workspacev1alpha1.ConditionProviderHeartbeat), Status: metav1.ConditionTrue, ObservedGeneration: provider.Generation},
		metav1.Condition{Type: string(workspacev1alpha1.ConditionProviderCompatible), Status: metav1.ConditionTrue, ObservedGeneration: provider.Generation})
	if err := r.Status().Update(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	if native {
		class.Spec.RequiredFeatures = append(class.Spec.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
		if err := r.Update(t.Context(), class); err != nil {
			t.Fatal(err)
		}
		provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
		if err := r.Status().Update(t.Context(), provider); err != nil {
			t.Fatal(err)
		}
		profile := &unstructured.Unstructured{}
		profile.SetAPIVersion("example.workspace.orka.ai/v1alpha1")
		profile.SetKind("WorkspaceProfile")
		if err := r.Get(t.Context(), client.ObjectKey{Namespace: class.Namespace, Name: "profile"}, profile); err != nil {
			t.Fatal(err)
		}
		hash, err := externalACPClassProfileHash(class, provider, config, profile)
		if err != nil {
			t.Fatal(err)
		}
		class.Status.ProfileHash = hash
		if err := r.Status().Update(t.Context(), class); err != nil {
			t.Fatal(err)
		}
	}
	task := acpClassTestTask()
	if err := r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.resolveACPWorkspaceClass(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	base := runtimePoolTestObject(1)
	profile, err := runtimePoolHarnessProfile(base.Spec.Runtime.Profile)
	if err != nil {
		t.Fatal(err)
	}
	plan := ACPRuntimePlan{PoolName: "frozen-requirements-pool", Workspace: binding, Image: base.Spec.Runtime.Image, Digest: harnessv2.ProfileDigest(base.Spec.Runtime.Profile.Digest), Profile: profile}
	w, err := r.createACPClassWorkspace(t.Context(), task, binding, plan.PoolName, acpClassWorkspaceName(task, binding))
	if err != nil {
		t.Fatal(err)
	}
	w.UID, w.Generation = "frozen-workspace-uid", 1
	w.CreationTimestamp = metav1.Now()
	w.Spec.AttachmentEpoch = 1
	w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: 1, ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour))}
	markWorkspaceAdmittedForPolicyReview(w, w.Generation)
	w.Status.State, w.Status.AttachedEpoch = workspacev1alpha1.ExecutionWorkspaceStateAttached, 1
	w.Status.Conditions = append(w.Status.Conditions, metav1.Condition{Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue, ObservedGeneration: w.Generation})
	if err := r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	profileObject := &unstructured.Unstructured{}
	profileObject.SetAPIVersion("example.workspace.orka.ai/v1alpha1")
	profileObject.SetKind("WorkspaceProfile")
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: class.Namespace, Name: "profile"}, profileObject); err != nil {
		t.Fatal(err)
	}
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithRESTMapper(r.RESTMapper()).
		WithStatusSubresource(class, provider, w, &corev1alpha1.RuntimePool{}).
		WithObjects(class, provider, config, profileObject, task, w).Build()
	r.APIReader = r.Client
	return r, plan, w
}

func TestACPWorkspacePoolFreezesResolvedRequiredFeatures(t *testing.T) {
	r, plan, w := frozenRequirementsPoolFixture(t, true)
	pool, _, err := r.ensureACPRuntimePoolWithPolicy(t.Context(), w.Namespace, plan, w.Name, string(w.UID), string(w.Spec.Attachment.TaskRef.UID), true, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime, workspacev1alpha1.WorkspaceFeatureNativeProcess, workspacev1alpha1.WorkspaceFeatureSuspend} {
		if !slices.Contains(pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures, feature) {
			t.Fatalf("immutable pool lost required feature %q", feature)
		}
	}
}

func TestACPWorkspacePoolRejectsBoundClassDriftBeforeCreation(t *testing.T) {
	for _, name := range []string{"UID", "generation", "profile", "spec without generation"} {
		t.Run(name, func(t *testing.T) {
			r, plan, w := frozenRequirementsPoolFixture(t, true)
			class := &workspacev1alpha1.ExecutionWorkspaceClass{}
			if err := r.Get(t.Context(), client.ObjectKey{Namespace: w.Namespace, Name: plan.Workspace.Class.Name}, class); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "UID":
				class.UID = "replaced-class"
			case "generation":
				class.Generation++
			case "profile":
				class.Status.ProfileHash = "sha256:" + strings.Repeat("f", 64)
				if err := r.Status().Update(t.Context(), class); err != nil {
					t.Fatal(err)
				}
			case "spec without generation":
				class.Spec.RequiredFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}
			}
			if name != "profile" {
				if err := r.Update(t.Context(), class); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := r.ensureACPRuntimePoolWithPolicy(t.Context(), w.Namespace, plan, w.Name, string(w.UID), string(w.Spec.Attachment.TaskRef.UID), true, "")
			if err == nil {
				t.Fatal("drifted class created a new pool from mutable requirements")
			}
			pool := &corev1alpha1.RuntimePool{}
			if getErr := r.Get(t.Context(), client.ObjectKey{Namespace: w.Namespace, Name: plan.PoolName}, pool); !apierrors.IsNotFound(getErr) {
				t.Fatalf("drifted class had pool effects: %v", getErr)
			}
		})
	}
}
