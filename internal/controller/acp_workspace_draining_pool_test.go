package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func drainingSessionPoolFixture(t *testing.T) (*TaskReconciler, *corev1alpha1.Task, ACPRuntimePlan, *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	r, plan, previous := frozenRequirementsPoolFixture(t, true)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if review, ok := object.(*authorizationv1.SubjectAccessReview); ok {
			review.Status.Allowed = true
			return nil
		}
		return c.Create(ctx, object, options...)
	}})
	r.APIReader = r.Client
	task := &corev1alpha1.Task{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: previous.Namespace, Name: previous.Spec.Attachment.TaskRef.Name}, task); err != nil {
		t.Fatal(err)
	}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName}
	task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
	if err := r.Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.resolveACPWorkspaceClassWithSessionUID(t.Context(), task, "draining-session-uid")
	if err != nil {
		t.Fatal(err)
	}
	plan.Workspace, err = resolveACPWorkspaceBindingWithClass(task, "draining-session-uid", resolved)
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.createACPClassWorkspace(t.Context(), task, plan.Workspace, plan.PoolName, acpClassWorkspaceName(task, plan.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	w.UID, w.Generation = "draining-session-workspace-uid", 1
	w.CreationTimestamp = metav1.Now()
	w.Spec.AttachmentEpoch = 1
	w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: 1,
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	markWorkspaceAdmittedForPolicyReview(w, w.Generation)
	w.Status.State, w.Status.AttachedEpoch = workspacev1alpha1.ExecutionWorkspaceStateAttached, 1
	w.Status.Conditions = append(w.Status.Conditions, metav1.Condition{Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue, ObservedGeneration: w.Generation})
	persistDrainingWorkspaceFixture(t, r, w)
	return r, task, plan, w
}

func persistDrainingWorkspaceFixture(t *testing.T, r *TaskReconciler, w *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	status := w.DeepCopy().Status
	if err := r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	w.Status = status
	if err := r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
}

func transitionPoolProviderToDraining(t *testing.T, r *TaskReconciler, plan ACPRuntimePlan) *workspacev1alpha1.ExecutionWorkspaceProvider {
	t.Helper()
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: plan.Workspace.Class.ProviderName}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
	provider.Generation++ // The fake client does not advance spec generations.
	if err := r.Update(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.ObservedGeneration = provider.Generation
	for i := range provider.Status.Conditions {
		condition := &provider.Status.Conditions[i]
		condition.ObservedGeneration = provider.Generation
		if condition.Type == string(workspacev1alpha1.ConditionProviderReady) {
			condition.Status, condition.Reason = metav1.ConditionFalse, string(workspacev1alpha1.ReasonProviderDraining)
		}
	}
	if err := r.Status().Update(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: acpTestNamespace, Name: plan.Workspace.Class.Name}, class); err != nil {
		t.Fatal(err)
	}
	for i := range class.Status.Conditions {
		if class.Status.Conditions[i].Type == string(workspacev1alpha1.ConditionClassReady) {
			class.Status.Conditions[i].Status = metav1.ConditionFalse
			class.Status.Conditions[i].Reason = string(workspacev1alpha1.ReasonProviderDraining)
		}
	}
	if err := r.Status().Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestACPDrainingProviderCreatesPoolForFrozenSession(t *testing.T) {
	for _, coldResume := range []bool{false, true} {
		name := "running continuation"
		if coldResume {
			name = "cold resume"
		}
		t.Run(name, func(t *testing.T) {
			r, task, plan, w := drainingSessionPoolFixture(t)
			pool, _, err := r.ensureACPRuntimePoolWithPolicy(t.Context(), w.Namespace, plan, w.Name, string(w.UID), string(task.UID), true, "")
			if err != nil {
				t.Fatal(err)
			}
			if coldResume {
				w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
				w.Spec.Attachment = nil
				w.Status.State, w.Status.AttachedEpoch = workspacev1alpha1.ExecutionWorkspaceStateSuspended, 0
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
				persistDrainingWorkspaceFixture(t, r, w)
			}
			provider := transitionPoolProviderToDraining(t, r, plan)
			resolved, err := r.resolveACPWorkspaceClassWithSessionUID(t.Context(), task, plan.Workspace.SessionUID)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Binding.ProviderGeneration != w.Spec.ProviderBinding.Generation || resolved.Binding.ProviderGeneration == provider.Generation {
				t.Fatal("continuation replaced the historical provider generation")
			}
			plan.Workspace, err = resolveACPWorkspaceBindingWithClass(task, plan.Workspace.SessionUID, resolved)
			if err != nil {
				t.Fatal(err)
			}
			// Recreate the pool after the exact logical Session binding is resolved.
			if err := r.Delete(t.Context(), pool); err != nil {
				t.Fatal(err)
			}
			if coldResume {
				if _, ready, err := r.ensureACPClassWorkspace(t.Context(), task, plan); err != nil || ready {
					t.Fatalf("request cold resume = ready %v, error %v", ready, err)
				}
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
					t.Fatal(err)
				}
				if w.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || w.Annotations[acpWorkspaceResumedLineageAnnotation] != booleanTrueValue {
					t.Fatal("cold resume lost the original workspace lineage")
				}
				w.Spec.AttachmentEpoch++
				w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: w.Spec.AttachmentEpoch, ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour))}
				w.Generation++
				markWorkspaceAdmittedForPolicyReview(w, w.Generation)
				w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateProvisioning
				w.Status.Conditions = append(w.Status.Conditions[:1],
					metav1.Condition{Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse, Reason: string(workspacev1alpha1.ReasonProgressing), ObservedGeneration: w.Generation},
					metav1.Condition{Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse, Reason: string(workspacev1alpha1.ReasonProgressing), ObservedGeneration: w.Generation})
				persistDrainingWorkspaceFixture(t, r, w)
			}
			pool, _, err = r.ensureACPRuntimePoolWithPolicy(t.Context(), w.Namespace, plan, w.Name, string(w.UID), string(task.UID), true, "")
			if coldResume {
				if !errors.Is(err, store.ErrNotReady) {
					t.Fatalf("cold boot handshake = %v, want readiness wait", err)
				}
				pool = &corev1alpha1.RuntimePool{}
				if err := r.Get(t.Context(), client.ObjectKey{Namespace: w.Namespace, Name: plan.PoolName}, pool); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if pool.Spec.ExecutionWorkspace.WorkspaceRef.UID != w.UID || pool.Spec.ExecutionWorkspace.ParametersBinding.UID != plan.Workspace.Class.ParametersBinding.UID ||
				!slices.Contains(pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) ||
				pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
				t.Fatal("pool recreation lost its frozen workspace requirements or fabricated Serving")
			}
		})
	}
}

func TestACPDrainingPoolRejectsIdentityAndAuthorityDrift(t *testing.T) {
	for _, name := range []string{"active generation", "future frozen generation", "provider UID", "controller", "config UID", "config generation", "config spec", "profile UID", "profile generation", "profile spec", "capability", "provider outage", "session UID", "slot", "unadmitted workspace", "non-session allocation"} {
		t.Run(name, func(t *testing.T) {
			r, task, plan, w := drainingSessionPoolFixture(t)
			provider := transitionPoolProviderToDraining(t, r, plan)
			switch name {
			case "active generation":
				provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderActive
			case "future frozen generation":
				plan.Workspace.Class.ProviderGeneration = provider.Generation + 1
				w.Spec.ProviderBinding.Generation = plan.Workspace.Class.ProviderGeneration
				w.Spec.CoreAdmission.ProviderBinding = w.Spec.ProviderBinding
			case "provider UID":
				provider.UID = "replacement-provider"
			case "controller":
				provider.Spec.ControllerName = "replacement.workspace.orka.ai"
			case "capability":
				provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}
			case "provider outage":
				provider.Status.ObservedGeneration--
			case "session UID":
				w.Spec.SessionRef.UID = "replacement-session"
			case "slot":
				w.Spec.Slot = "replacement-slot"
			case "unadmitted workspace":
				w.Spec.CoreAdmission = nil
			case "non-session allocation":
				plan.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicyNone
				provider.Generation = plan.Workspace.Class.ProviderGeneration
				provider.Status.ObservedGeneration = provider.Generation
				for i := range provider.Status.Conditions {
					provider.Status.Conditions[i].ObservedGeneration = provider.Generation
				}
			default:
				object := &unstructured.Unstructured{}
				ref, key := plan.Workspace.Class.ParametersRef, client.ObjectKey{Namespace: w.Namespace, Name: plan.Workspace.Class.ParametersRef.Name}
				if name[:6] == "config" {
					ref, key = plan.Workspace.Class.ProviderConfigRef, client.ObjectKey{Name: plan.Workspace.Class.ProviderConfigRef.Name}
				}
				object.SetAPIVersion(ref.Group + "/v1alpha1")
				object.SetKind(ref.Kind)
				if err := r.Get(t.Context(), key, object); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "config UID", "profile UID":
					object.SetUID("replacement-parameters")
				case "config generation", "profile generation":
					object.SetGeneration(object.GetGeneration() + 1)
				default:
					object.Object["spec"] = map[string]any{"changed": true}
				}
				if err := r.Update(t.Context(), object); err != nil {
					t.Fatal(err)
				}
			}
			providerStatus := provider.DeepCopy().Status
			if err := r.Update(t.Context(), provider); err != nil {
				t.Fatal(err)
			}
			provider.Status = providerStatus
			if err := r.Status().Update(t.Context(), provider); err != nil {
				t.Fatal(err)
			}
			if err := r.Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.ensureACPRuntimePoolWithPolicy(t.Context(), w.Namespace, plan, w.Name, string(w.UID), string(task.UID), true, ""); err == nil {
				t.Fatal("drifted continuation created a pool")
			}
			if err := r.Get(t.Context(), client.ObjectKey{Namespace: w.Namespace, Name: plan.PoolName}, &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
				t.Fatalf("rejected continuation left pool effects: %v", err)
			}
		})
	}
}

func TestACPDrainingProviderRejectsFreshSessionIdentity(t *testing.T) {
	r, _, plan, _ := drainingSessionPoolFixture(t)
	transitionPoolProviderToDraining(t, r, plan)
	task := acpClassTestTask(func(task *corev1alpha1.Task) {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "new-session"}
		task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
	})
	if _, err := r.resolveACPWorkspaceClassWithSessionUID(t.Context(), task, "new-session-uid"); err == nil {
		t.Fatal("draining registration admitted a new Session identity")
	}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: plan.PoolName}, &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("new draining identity had pool effects: %v", err)
	}
}
