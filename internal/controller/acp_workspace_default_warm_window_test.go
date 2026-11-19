package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func defaultWindowTestBinding(mutate ...func(*ACPRuntimeWorkspaceBinding)) *ACPRuntimeWorkspaceBinding {
	binding := &ACPRuntimeWorkspaceBinding{
		ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
		Class: &ACPWorkspaceClassBinding{
			ControllerName:  "substrate.workspace.orka.ai",
			SuspendMode:     acpWorkspaceSuspendDataOnly,
			AllowedOnDetach: []string{string(workspacev1alpha1.WorkspaceOnDetachSuspend), string(workspacev1alpha1.WorkspaceOnDetachDelete)},
			ParametersRef:   &workspacev1alpha1.TypedObjectReference{Group: substrateWorkspaceProfileGroup, Kind: substrateWorkspaceProfileKind, Name: "profile"},
			IdleTimeout:     (30 * time.Minute).String(),
			MaxLifetime:     (8 * time.Hour).String(),
		},
	}
	for _, m := range mutate {
		m(binding)
	}
	return binding
}

func TestValidateACPWorkspaceDefaultSuspendAfterDetach(t *testing.T) {
	t.Parallel()
	for window, valid := range map[time.Duration]bool{
		0: true, DefaultACPWorkspaceSuspendAfterDetach: true, time.Hour: true,
		-time.Second: false, time.Hour + time.Second: false,
	} {
		if err := ValidateACPWorkspaceDefaultSuspendAfterDetach(window); (err == nil) != valid {
			t.Fatalf("%s: error = %v, want valid=%v", window, err, valid)
		}
	}
}

func TestResolveACPWorkspaceDefaultSuspendAfterDetach(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		mutate     func(*ACPRuntimeWorkspaceBinding)
		class      map[string]string
		configured time.Duration
		want       time.Duration
	}{
		{name: "eligible", configured: 5 * time.Minute, want: 5 * time.Minute},
		{name: "disabled controller default", configured: 0},
		{name: "profile sets its own window", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.SuspendAfterDetach = (10 * time.Minute).String()
		}},
		{name: "per-Task workspace", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.ReusePolicy = corev1alpha1.WorkspaceReusePolicyNone
		}},
		{name: "no DataOnly profile", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.SuspendMode = ""
		}},
		{name: "Suspend not allowed", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.AllowedOnDetach = []string{string(workspacev1alpha1.WorkspaceOnDetachDelete)}
		}},
		{name: "not a Substrate profile", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: "sandbox.workspace.orka.ai", Kind: "SandboxWorkspaceProfile", Name: "profile"}
		}},
		{name: "legacy in-tree binding", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.ControllerName = ""
		}},
		{name: "class override", configured: 5 * time.Minute, want: 15 * time.Minute,
			class: map[string]string{ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation: "15m"}},
		{name: "class override with the controller default off", want: 15 * time.Minute,
			class: map[string]string{ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation: "15m"}},
		{name: "class opt-out", configured: 5 * time.Minute,
			class: map[string]string{ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation: "disabled"}},
		{name: "invalid class override", configured: 5 * time.Minute,
			class: map[string]string{ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation: "0s"}},
		{name: "not shorter than idleTimeout", configured: 30 * time.Minute},
		{name: "not shorter than maxLifetime", configured: 5 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.IdleTimeout = ""
			b.Class.MaxLifetime = (5 * time.Minute).String()
		}},
		{name: "over one hour", configured: 61 * time.Minute, mutate: func(b *ACPRuntimeWorkspaceBinding) {
			b.Class.IdleTimeout = (2 * time.Hour).String()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			binding := defaultWindowTestBinding()
			if test.mutate != nil {
				test.mutate(binding)
			}
			if got := resolveACPWorkspaceDefaultSuspendAfterDetach(binding, test.class, test.configured); got != test.want {
				t.Fatalf("default window = %s, want %s", got, test.want)
			}
		})
	}
}

// defaultWindowFixture is a session-reused Substrate class whose profile sets
// no window, with a 5m controller default.
func defaultWindowFixture(t *testing.T) *speculativeTaskFixture {
	t.Helper()
	f := newSpeculativeTaskFixtureWith(t, func(r *TaskReconciler, binding *ACPRuntimeWorkspaceBinding) {
		r.ACPWorkspaceDefaultSuspendAfterDetach = 5 * time.Minute
		// The fixture's profile kind stands in for SubstrateWorkspaceProfile.
		binding.Class.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: substrateWorkspaceProfileGroup, Kind: substrateWorkspaceProfileKind, Name: binding.Class.ParametersRef.Name}
	})
	return f
}

func TestDefaultWarmWindowIsFrozenIntoNewWorkspacesOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := defaultWindowFixture(t)
	workspace := f.workspace(t)
	if got := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; got != (5*time.Minute).String() ||
		workspace.Annotations[acpWorkspaceSuspendAfterDetachSourceAnnotation] != acpWorkspaceSuspendAfterDetachSourceDefault {
		t.Fatalf("new workspace froze window %q source %q, want the controller default",
			got, workspace.Annotations[acpWorkspaceSuspendAfterDetachSourceAnnotation])
	}
	if window, frozen := acpWorkspaceSuspendAfterDetachFromAnnotation(workspace); !frozen || window != 5*time.Minute {
		t.Fatalf("ADR 0055 reads window %s/%v, want the frozen default", window, frozen)
	}
	if f.plan.Workspace.Class.SuspendAfterDetach != "" {
		t.Fatal("the controller default entered the frozen class binding")
	}

	// A later controller default applies to new workspaces only: the
	// existing workspace keeps the window it froze and stays admissible.
	f.r.ACPWorkspaceDefaultSuspendAfterDetach = 10 * time.Minute
	if _, _, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); err != nil {
		t.Fatalf("continuation under a changed default: %v", err)
	}
	if got := f.workspace(t).Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; got != (5 * time.Minute).String() {
		t.Fatalf("changed default rewrote the frozen window to %q", got)
	}
	f.r.ACPWorkspaceDefaultSuspendAfterDetach = 0
	if _, _, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); err != nil {
		t.Fatalf("continuation after disabling the default: %v", err)
	}
	if got := f.workspace(t).Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; got != (5 * time.Minute).String() {
		t.Fatalf("disabling the default removed the frozen window: %q", got)
	}

	// A profile window and a default-sourced window never mix.
	explicit := *f.plan.Workspace
	explicitClass := *explicit.Class
	explicitClass.SuspendAfterDetach = (5 * time.Minute).String()
	explicit.Class = &explicitClass
	if err := validateACPWorkspaceSuspendAfterDetach(f.workspace(t), explicit.Class.SuspendAfterDetach); err == nil ||
		!strings.Contains(err.Error(), "default warm-detach window does not match") {
		t.Fatalf("profile window over a default-sourced workspace error = %v", err)
	}
	orphanSource := f.workspace(t)
	delete(orphanSource.Annotations, acpWorkspaceSuspendAfterDetachAnnotation)
	if err := validateACPWorkspaceSuspendAfterDetach(orphanSource, ""); err == nil {
		t.Fatal("a default source marker without a window was admissible")
	}
	forged := f.workspace(t)
	forged.Annotations[acpWorkspaceSuspendAfterDetachSourceAnnotation] = "profile"
	if err := validateACPWorkspaceSuspendAfterDetach(forged, ""); err == nil {
		t.Fatal("an unknown window source was admissible")
	}
}

func TestDefaultWarmWindowNeverBackfillsAnExistingWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSpeculativeTaskFixtureWith(t, func(r *TaskReconciler, binding *ACPRuntimeWorkspaceBinding) {
		// The workspace predates the default.
		binding.Class.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: substrateWorkspaceProfileGroup, Kind: substrateWorkspaceProfileKind, Name: binding.Class.ParametersRef.Name}
	})
	if _, present := f.workspace(t).Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; present {
		t.Fatal("a disabled default froze a window")
	}
	f.r.ACPWorkspaceDefaultSuspendAfterDetach = 5 * time.Minute
	if _, _, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); err != nil {
		t.Fatalf("continuation after enabling the default: %v", err)
	}
	after := f.workspace(t)
	if _, present := after.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; present {
		t.Fatal("enabling the default backfilled an existing workspace")
	}
	if _, present := after.Annotations[acpWorkspaceSuspendAfterDetachSourceAnnotation]; present {
		t.Fatal("enabling the default marked an existing workspace")
	}
}

func TestDefaultWarmWindowHonorsClassOverrideAndOptOut(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "override", value: "15m", want: (15 * time.Minute).String()},
		{name: "opt-out", value: "disabled"},
		{name: "invalid", value: "2h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSpeculativeTaskFixtureWith(t, func(r *TaskReconciler, binding *ACPRuntimeWorkspaceBinding) {
				r.ACPWorkspaceDefaultSuspendAfterDetach = 5 * time.Minute
				binding.Class.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: substrateWorkspaceProfileGroup, Kind: substrateWorkspaceProfileKind, Name: binding.Class.ParametersRef.Name}
				class := &workspacev1alpha1.ExecutionWorkspaceClass{}
				if err := r.Get(context.Background(), types.NamespacedName{Namespace: acpTestNamespace, Name: binding.Class.Name}, class); err != nil {
					t.Fatal(err)
				}
				base := class.DeepCopy()
				if class.Annotations == nil {
					class.Annotations = map[string]string{}
				}
				class.Annotations[ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation] = test.value
				if err := r.Patch(context.Background(), class, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
			})
			workspace := f.workspace(t)
			if got := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; got != test.want {
				t.Fatalf("frozen window = %q, want %q", got, test.want)
			}
			if _, sourced := workspace.Annotations[acpWorkspaceSuspendAfterDetachSourceAnnotation]; sourced != (test.want != "") {
				t.Fatalf("source marker present = %v, want %v", sourced, test.want != "")
			}
		})
	}
}

func TestDefaultWarmWindowLeavesBindingAndProfileHashesUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolve := func(window time.Duration) *ACPRuntimeWorkspaceBinding {
		r.ACPWorkspaceDefaultSuspendAfterDetach = window
		resolved, err := r.resolveACPWorkspaceClass(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := resolveACPWorkspaceBindingWithClass(task, suspendTestSessionUID, resolved)
		if err != nil {
			t.Fatal(err)
		}
		return binding
	}
	off, on := resolve(0), resolve(DefaultACPWorkspaceSuspendAfterDetach)
	if off.BindingDigest != on.BindingDigest || off.Class.ProfileHash != on.Class.ProfileHash || on.Class.SuspendAfterDetach != "" {
		t.Fatalf("the controller default changed frozen hashes: %s/%s vs %s/%s", off.BindingDigest, off.Class.ProfileHash, on.BindingDigest, on.Class.ProfileHash)
	}
}
