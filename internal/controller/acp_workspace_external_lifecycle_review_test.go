// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func externalACPSessionLifecycleFixture(t *testing.T) (*TaskReconciler, *corev1alpha1.Task, ACPRuntimePlan, *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	ctx := context.Background()
	r, _, _, _ := externalACPFixture(t, true)
	r.Scheme = r.Client.Scheme()
	r.WorkspaceSettlementProtected = true
	task := acpClassTestTask(func(task *corev1alpha1.Task) {
		task.Finalizers = []string{labels.TaskFinalizer}
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName, Create: true}
		task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
		task.Spec.Execution.Workspace.OnDetach = corev1alpha1.WorkspaceOnDetachSuspend
	})
	if err := r.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "session-uid", resolved)
	if err != nil {
		t.Fatal(err)
	}
	plan := ACPRuntimePlan{PoolName: "external-session-pool", Workspace: binding}
	workspace, err := r.createACPClassWorkspace(ctx, task, binding, plan.PoolName, acpClassWorkspaceName(task, binding))
	if err != nil {
		t.Fatal(err)
	}
	workspace.UID = "external-session-workspace-uid"
	workspace.Generation = 2
	workspace.CreationTimestamp = metav1.Now()
	workspace.Spec.AttachmentEpoch = 4
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	return r, task, plan, workspace
}

func TestExternalACPSessionColdResume(t *testing.T) {
	for _, state := range []workspacev1alpha1.ExecutionWorkspaceState{
		workspacev1alpha1.ExecutionWorkspaceStateSuspended, workspacev1alpha1.ExecutionWorkspaceStateSuspending,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			r, task, plan, workspace := externalACPSessionLifecycleFixture(t)
			continuation := task.DeepCopy()
			continuation.Name, continuation.UID, continuation.ResourceVersion = "continuation", "continuation-uid", ""
			if err := r.Create(ctx, continuation); err != nil {
				t.Fatal(err)
			}
			task = continuation
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
			workspace.Status.State = state
			workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			if err := r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			before := workspace.DeepCopy()
			name, ready, err := r.ensureACPClassWorkspace(ctx, task, plan)
			if err != nil || ready || name != "" {
				t.Fatalf("cold resume = (%q, %v, %v), want pending", name, ready, err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
				t.Fatal(err)
			}
			wantDesired := workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
			if state == workspacev1alpha1.ExecutionWorkspaceStateSuspended {
				wantDesired = workspacev1alpha1.ExecutionWorkspaceDesiredReady
				if workspace.Annotations[acpWorkspaceResumedLineageAnnotation] != booleanTrueValue {
					t.Fatal("cold resume did not preserve resumed lineage")
				}
			}
			if workspace.Spec.DesiredState != wantDesired || workspace.Spec.Attachment != nil || workspace.Spec.AttachmentEpoch != 4 {
				t.Fatalf("resume changed authority before the new runtime boot: %#v", workspace.Spec)
			}
			demand := strings.Fields(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation])
			if len(demand) != 3 || demand[1] != task.Name || demand[2] != string(task.UID) {
				t.Fatal("resume did not persist exact requesting Task identity")
			}
			if workspace.UID != before.UID || !reflect.DeepEqual(workspace.Spec.ClassBinding, before.Spec.ClassBinding) ||
				!reflect.DeepEqual(workspace.Spec.ProviderBinding, before.Spec.ProviderBinding) ||
				!reflect.DeepEqual(workspace.Spec.SessionRef, before.Spec.SessionRef) ||
				workspace.Annotations[acpWorkspaceLastDetachedAnnotation] != before.Annotations[acpWorkspaceLastDetachedAnnotation] ||
				workspace.Spec.Workload != nil || workspace.Status.Allocation != nil {
				t.Fatal("resume replaced frozen identity, detach time, or fabricated runtime compute")
			}
		})
	}
}

func TestExternalACPSessionResumeRejectsBindingDrift(t *testing.T) {
	tests := map[string]func(*workspacev1alpha1.ExecutionWorkspace, *ACPRuntimeWorkspaceBinding){
		"controller route": func(w *workspacev1alpha1.ExecutionWorkspace, _ *ACPRuntimeWorkspaceBinding) {
			w.Labels[workspacev1alpha1.ProviderControllerLabel] = "other.workspace.orka.ai"
		},
		"class UID": func(w *workspacev1alpha1.ExecutionWorkspace, _ *ACPRuntimeWorkspaceBinding) {
			w.Spec.ClassBinding.UID = "replacement"
		},
		"provider UID": func(w *workspacev1alpha1.ExecutionWorkspace, _ *ACPRuntimeWorkspaceBinding) {
			w.Spec.ProviderBinding.UID = "replacement"
		},
		"session UID": func(w *workspacev1alpha1.ExecutionWorkspace, _ *ACPRuntimeWorkspaceBinding) {
			w.Spec.SessionRef.UID = "replacement"
		},
		"provider identity": func(_ *workspacev1alpha1.ExecutionWorkspace, b *ACPRuntimeWorkspaceBinding) {
			b.Provider = "other.workspace.orka.ai"
		},
		"missing controller contract": func(_ *workspacev1alpha1.ExecutionWorkspace, b *ACPRuntimeWorkspaceBinding) {
			b.Class.LifecycleContractVersion = ""
		},
		"non DataOnly": func(w *workspacev1alpha1.ExecutionWorkspace, b *ACPRuntimeWorkspaceBinding) {
			b.Class.SuspendMode = "FullMemory"
			w.Annotations[acpWorkspaceSuspendModeAnnotation] = b.Class.SuspendMode
		},
		"missing session": func(w *workspacev1alpha1.ExecutionWorkspace, _ *ACPRuntimeWorkspaceBinding) {
			w.Spec.SessionRef = nil
		},
		"generic workspace": func(w *workspacev1alpha1.ExecutionWorkspace, _ *ACPRuntimeWorkspaceBinding) {
			delete(w.Annotations, acpExecutionWorkspacePoolAnnotation)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, task, plan, workspace := externalACPSessionLifecycleFixture(t)
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
			workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
			mutate(workspace, plan.Workspace)
			if err := r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			before := workspace.DeepCopy()
			if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err == nil || ready {
				t.Fatalf("binding drift resumed workspace: ready=%v error=%v", ready, err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(workspace.Spec, before.Spec) || !reflect.DeepEqual(workspace.Annotations, before.Annotations) {
				t.Fatal("failed resume mutated workspace")
			}
		})
	}
}

func TestProviderRoutedGenericTaskFinalizationKeepsGenericTimeout(t *testing.T) {
	for _, attached := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse} {
		t.Run(string(attached), func(t *testing.T) {
			ctx := context.Background()
			_, task, _, workspace := externalACPSessionLifecycleFixture(t)
			finalizingExternalACPTask(task, workspace, attached)
			delete(workspace.Annotations, acpExecutionWorkspacePoolAnnotation)
			workspace.Spec.Lifecycle.DetachTimeout.Duration = 10 * time.Minute
			workspace.Status.AttachedEpoch = 4
			task.Status.ExecutionWorkspace.AttachedEpoch = 4
			task.Status.ExecutionOutcome.RecordedAt = metav1.NewTime(time.Now().Add(-6 * time.Minute))
			r := externalACPFinalizationReconciler(task, workspace)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
				t.Fatal(err)
			}
			if task.Status.Phase != corev1alpha1.TaskPhaseFailed || workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
				t.Fatal("provider route alone selected the ACP detach timeout")
			}
			if workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] != "" || acpTaskRecordedAttachmentEpoch(task) != 0 {
				t.Fatal("generic workspace received ACP settlement markers")
			}
		})
	}
}

func finalizingExternalACPTask(task *corev1alpha1.Task, workspace *workspacev1alpha1.ExecutionWorkspace, attached metav1.ConditionStatus) {
	task.Finalizers = []string{labels.TaskFinalizer}
	task.Status.Phase = corev1alpha1.TaskPhaseFinalizing
	task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{
		Phase: corev1alpha1.TaskPhaseSucceeded, Attempt: 1, RecordedAt: metav1.Now(),
	}
	task.Status.ExecutionWorkspace = &corev1alpha1.ExecutionWorkspaceStatus{
		WorkspaceRef:  &corev1alpha1.WorkspaceObjectReference{Name: workspace.Name, UID: string(workspace.UID)},
		AttachedEpoch: 3, Conditions: []metav1.Condition{{Type: "Attached", Status: attached}},
	}
}

func externalACPFinalizationReconciler(task *corev1alpha1.Task, objects ...client.Object) *TaskReconciler {
	objects = append(objects, task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}})
	r := newUnitReconciler(newTestScheme(), objects...)
	r.APIReader = r.Client
	return r
}

func TestExternalACPTaskFinalizationBlocksSessionContinuation(t *testing.T) {
	ctx := context.Background()
	_, task, plan, workspace := externalACPSessionLifecycleFixture(t)
	finalizingExternalACPTask(task, workspace, metav1.ConditionTrue)
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: 4,
	}
	workspace.Status.AttachedEpoch = 4
	continuation := task.DeepCopy()
	continuation.Name, continuation.UID = "continuation", "continuation-uid"
	continuation.Annotations, continuation.Status = nil, corev1alpha1.TaskStatus{}
	r := externalACPFinalizationReconciler(task, continuation, workspace)
	r.Client = authorizedACPFixtureClient{r.Client}
	r.APIReader = r.Client
	r.WorkspaceSettlementProtected = true
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
		t.Fatal(err)
	}
	stampEpoch, _, valid := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation])
	if workspace.Spec.Attachment != nil || !valid || stampEpoch != 4 || acpTaskRecordedAttachmentEpoch(task) != 4 {
		t.Fatal("first finalization pass cleared attachment without the exact epoch settlement barrier")
	}
	// The provider has released the bearer but the predecessor's Suspend
	// settlement has not yet changed desiredState. The continuation must wait.
	workspace.Status.AttachedEpoch = 0
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	workspace.Status.Conditions = nil
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	name, ready, err := r.ensureACPClassWorkspace(ctx, continuation, plan)
	if err != nil || ready || name != "" {
		t.Fatalf("continuation crossed predecessor Suspend barrier: (%q, %v, %v)", name, ready, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Spec.Attachment != nil || workspace.Spec.AttachmentEpoch != 4 {
		t.Fatal("continuation acquired authority before predecessor settlement")
	}
}

func TestExternalACPTaskFinalizationUsesFrozenDetachTimeout(t *testing.T) {
	for _, attached := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse} {
		for _, short := range []bool{true, false} {
			t.Run(fmt.Sprintf("attached-%s/short-%v", attached, short), func(t *testing.T) {
				ctx := context.Background()
				_, task, _, workspace := externalACPSessionLifecycleFixture(t)
				finalizingExternalACPTask(task, workspace, attached)
				// The status projection lags, while the protected Task stamp has
				// the actual attachment epoch for the second finalization pass.
				task.Annotations = map[string]string{acpTaskAttachmentEpochAnnotation: "4"}
				workspace.Status.AttachedEpoch = 4
				workspace.Spec.Lifecycle.DetachTimeout = metav1.Duration{Duration: 10 * time.Minute}
				revocationAge, outcomeAge := 6*time.Minute, 6*time.Minute
				wantPhase := corev1alpha1.TaskPhaseFinalizing
				wantDesired := workspacev1alpha1.ExecutionWorkspaceDesiredReady
				if short {
					workspace.Spec.Lifecycle.DetachTimeout.Duration = time.Minute
					revocationAge, outcomeAge = 2*time.Minute, 30*time.Second
					wantPhase, wantDesired = corev1alpha1.TaskPhaseFailed, workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
				}
				workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] = fmt.Sprintf("4 %s", time.Now().Add(-revocationAge).UTC().Format(time.RFC3339Nano))
				task.Status.ExecutionOutcome.RecordedAt = metav1.NewTime(time.Now().Add(-outcomeAge))
				if attached == metav1.ConditionTrue {
					workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
						TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: 4,
					}
				}
				r := externalACPFinalizationReconciler(task, workspace)
				r.APIReader = r.Client
				result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
				if err != nil {
					t.Fatal(err)
				}
				if attached == metav1.ConditionTrue && short {
					// Retry after the attachment-clearing write so quarantine's
					// optimistic resourceVersion fence uses the current object.
					result, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
					t.Fatal(err)
				}
				if task.Status.Phase != wantPhase || workspace.Spec.DesiredState != wantDesired || result.RequeueAfter <= 0 {
					t.Fatalf("frozen detach timeout ignored: phase=%s desired=%s result=%#v", task.Status.Phase, workspace.Spec.DesiredState, result)
				}
				epoch, _, valid := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation])
				if !valid || epoch != 4 {
					t.Fatal("finalization replaced the actual epoch with a stale status projection")
				}
			})
		}
	}
}

func TestExternalACPTaskFinalizationRejectsWorkspaceReplacement(t *testing.T) {
	for _, attached := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse} {
		t.Run(string(attached), func(t *testing.T) {
			ctx := context.Background()
			_, task, _, workspace := externalACPSessionLifecycleFixture(t)
			finalizingExternalACPTask(task, workspace, attached)
			workspace.UID = types.UID("replacement-workspace-uid")
			before := workspace.DeepCopy()
			r := externalACPFinalizationReconciler(task, workspace)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err == nil || !strings.Contains(err.Error(), "UID changed") {
				t.Fatalf("replacement workspace accepted: %v", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(workspace.Spec, before.Spec) || !reflect.DeepEqual(workspace.Annotations, before.Annotations) {
				t.Fatal("finalization mutated replacement workspace")
			}
		})
	}
}
