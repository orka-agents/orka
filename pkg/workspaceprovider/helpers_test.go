package workspaceprovider

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func TestControllerNamePredicate(t *testing.T) {
	t.Parallel()
	predicate := ControllerNamePredicate("fake.workspace.orka.ai/v1")
	matching := &workspacev1alpha1.ExecutionWorkspaceProvider{
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
			ControllerName: "fake.workspace.orka.ai/v1",
		},
	}
	other := matching.DeepCopy()
	other.Spec.ControllerName = "other.workspace.orka.ai/v1"
	if !predicate.Create(event.CreateEvent{Object: matching}) {
		t.Fatal("matching provider was filtered")
	}
	if predicate.Create(event.CreateEvent{Object: other}) {
		t.Fatal("other provider was accepted")
	}
}

func TestClassProfileHashDeterministic(t *testing.T) {
	t.Parallel()
	spec := workspacev1alpha1.ExecutionWorkspaceClassSpec{
		ProviderRef: &workspacev1alpha1.ClusterObjectReference{Name: "provider"},
		ParametersRef: &workspacev1alpha1.TypedObjectReference{
			Group: "fake.workspace.orka.ai",
			Kind:  "Profile",
			Name:  "default",
		},
		Mode:               workspacev1alpha1.ExecutionWorkspaceModeInteractive,
		AllowedReuseScopes: []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone},
		Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{
			DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachDelete,
			AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete},
			DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
				ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
				PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionDelete,
				Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
			},
		},
	}
	first, err := ClassProfileHash(spec)
	if err != nil {
		t.Fatalf("ClassProfileHash: %v", err)
	}
	second, err := ClassProfileHash(spec)
	if err != nil {
		t.Fatalf("ClassProfileHash second: %v", err)
	}
	if first != second || len(first) != len("sha256:")+64 {
		t.Fatalf("hashes = %q %q", first, second)
	}
	spec.RequiredFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureTLS, workspacev1alpha1.WorkspaceFeatureExec,
	}
	spec.AllowedReuseScopes = []workspacev1alpha1.WorkspaceReuseScope{
		workspacev1alpha1.WorkspaceReuseScopeSession, workspacev1alpha1.WorkspaceReuseScopeNone,
	}
	spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
		workspacev1alpha1.WorkspaceOnDetachDelete, workspacev1alpha1.WorkspaceOnDetachSuspend,
	}
	orderedHash, err := ClassProfileHash(spec)
	if err != nil {
		t.Fatalf("ClassProfileHash ordered sets: %v", err)
	}
	slices.Reverse(spec.RequiredFeatures)
	slices.Reverse(spec.AllowedReuseScopes)
	slices.Reverse(spec.Lifecycle.AllowedOnDetach)
	reorderedHash, err := ClassProfileHash(spec)
	if err != nil {
		t.Fatalf("ClassProfileHash reordered sets: %v", err)
	}
	if orderedHash != reorderedHash {
		t.Fatalf("set reorder changed hash: %q != %q", orderedHash, reorderedHash)
	}
	spec.Mode = workspacev1alpha1.ExecutionWorkspaceModeService
	third, err := ClassProfileHash(spec)
	if err != nil {
		t.Fatalf("ClassProfileHash changed: %v", err)
	}
	if third == first {
		t.Fatal("functional class change did not change profile hash")
	}
}

func TestWorkspaceIdentityScopes(t *testing.T) {
	t.Parallel()
	classUID := types.UID("class")
	sessionIdentity := InteractiveWorkspaceIdentity(
		"default", types.UID("session"), types.UID("task-1"), classUID, "default",
	)
	sameSession := InteractiveWorkspaceIdentity("default", types.UID("session"), types.UID("task-2"), classUID, "default")
	if sessionIdentity != sameSession {
		t.Fatal("session-scoped identity changed across Tasks")
	}
	spacedSlot := InteractiveWorkspaceIdentity(
		"default", types.UID("session"), types.UID("task-1"), classUID, " default ",
	)
	if sessionIdentity == spacedSlot {
		t.Fatal("non-canonical session slot collided with canonical identity")
	}
	freshOne := InteractiveWorkspaceIdentity("default", "", types.UID("task-1"), classUID, "default")
	freshTwo := InteractiveWorkspaceIdentity("default", "", types.UID("task-2"), classUID, "default")
	freshDifferentSlot := InteractiveWorkspaceIdentity("default", "", types.UID("task-1"), classUID, "other")
	if freshOne != freshDifferentSlot {
		t.Fatal("non-session workspace slot changed Task-scoped identity")
	}
	if freshOne == freshTwo || freshOne == sessionIdentity {
		t.Fatal("fresh Task identities collided")
	}
	name := WorkspaceName("Coding_Workspace", sessionIdentity)
	if len(name) > 63 || name == "" {
		t.Fatalf("workspace name = %q", name)
	}
}

func TestSetConditionPreservesTransitionSemantics(t *testing.T) {
	t.Parallel()
	conditions := []metav1.Condition{}
	SetCondition(&conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"})
	if !ConditionIsTrue(conditions, "Ready") || FindCondition(conditions, "Ready") == nil {
		t.Fatalf("conditions = %#v", conditions)
	}
}

func TestValidateEndpointsRejectsCredentials(t *testing.T) {
	t.Parallel()
	valid := []workspacev1alpha1.ExecutionWorkspaceEndpoint{{
		Name: "mcp", URL: "https://service.example.test/mcp", Protocol: "HTTPS",
	}}
	if err := ValidateEndpoints(valid); err != nil {
		t.Fatalf("ValidateEndpoints(valid): %v", err)
	}
	invalid := []workspacev1alpha1.ExecutionWorkspaceEndpoint{{
		Name: "mcp", URL: "https://user:password@service.example.test/mcp", Protocol: "HTTPS",
	}}
	if err := ValidateEndpoints(invalid); err == nil {
		t.Fatal("ValidateEndpoints accepted credential-bearing URL")
	}
}
