/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

func TestAgentExecutionAdjudicationAppliesTaskCleanupBothIdempotently(t *testing.T) {
	now := time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, true)
	adjudication := adjudicationTestTaskRecord(t, namespace, task,
		corev1alpha1.AgentExecutionAdjudicationCleanupBoth, now)
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("prepare adjudication: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("prepare adjudication did not request application requeue")
	}
	applying := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	if applying.Status.State != corev1alpha1.AgentExecutionAdjudicationApplying ||
		applying.Status.OperationID == "" || applying.Status.OperationDigest == "" {
		t.Fatalf("applying status = %#v", applying.Status)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("apply adjudication: %v", err)
	}
	applied := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	ref := currentTask.Status.AgentExecutionResolutionRef
	if applied.Status.State != corev1alpha1.AgentExecutionAdjudicationApplied || ref == nil {
		t.Fatalf("applied status = %#v, resolution ref = %#v", applied.Status, ref)
	}
	if ref.AdjudicationName != adjudication.Name || ref.AdjudicationUID != adjudication.UID ||
		ref.Action != corev1alpha1.AgentExecutionAdjudicationCleanupBoth ||
		ref.OperationDigest != applied.Status.OperationDigest ||
		ref.ResolutionDigest != applied.Status.ResolutionRefDigest {
		t.Fatalf("resolution ref = %#v, applied status = %#v", ref, applied.Status)
	}
	if err := validateAgentExecutionResolutionRefDigest(task.Namespace, ref); err != nil {
		t.Fatalf("resolution ref digest: %v", err)
	}
	if currentTask.Status.AgentExecutionQuarantine == nil ||
		currentTask.Status.AgentExecutionQuarantine.V1EvidenceDigest != task.Status.AgentExecutionQuarantine.V1EvidenceDigest ||
		currentTask.Status.AgentExecutionQuarantine.V2EvidenceDigest != task.Status.AgentExecutionQuarantine.V2EvidenceDigest {
		t.Fatalf("immutable quarantine was changed: %#v", currentTask.Status.AgentExecutionQuarantine)
	}

	resourceVersion := currentTask.ResourceVersion
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("idempotent terminal reconcile: %v", err)
	}
	after := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	if after.ResourceVersion != resourceVersion || *after.Status.AgentExecutionResolutionRef != *ref {
		t.Fatalf("terminal retry mutated Task: before rv=%s ref=%#v, after rv=%s ref=%#v",
			resourceVersion, ref, after.ResourceVersion, after.Status.AgentExecutionResolutionRef)
	}
}

func TestAgentExecutionAdjudicationRecoversCommittedSubjectWrite(t *testing.T) {
	now := time.Date(2026, 8, 6, 20, 30, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, false)
	adjudication := adjudicationTestTaskRecord(t, namespace, task,
		corev1alpha1.AgentExecutionAdjudicationCleanupV1, now)
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("prepare adjudication: %v", err)
	}

	applying := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	ref, err := newAgentExecutionResolutionRef(applying, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	currentTask.Status.AgentExecutionResolutionRef = ref
	if err := kubeClient.Status().Update(context.Background(), currentTask); err != nil {
		t.Fatalf("simulate committed subject write: %v", err)
	}
	committedResourceVersion := currentTask.ResourceVersion

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("recover committed subject write: %v", err)
	}
	applied := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	if applied.Status.State != corev1alpha1.AgentExecutionAdjudicationApplied ||
		applied.Status.ResolutionRefDigest != ref.ResolutionDigest ||
		applied.Status.ResultingSubjectResourceVersion != committedResourceVersion {
		t.Fatalf("recovered applied status = %#v", applied.Status)
	}
}

func TestAgentExecutionAdjudicationSupersedesChangedSubject(t *testing.T) {
	now := time.Date(2026, 8, 6, 21, 0, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(false, true)
	adjudication := adjudicationTestTaskRecord(t, namespace, task,
		corev1alpha1.AgentExecutionAdjudicationCleanupV2, now)
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("prepare adjudication: %v", err)
	}

	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	currentTask.Status.Message = "new route evidence was observed"
	if err := kubeClient.Status().Update(context.Background(), currentTask); err != nil {
		t.Fatalf("advance Task resourceVersion: %v", err)
	}
	if currentTask.ResourceVersion == task.ResourceVersion {
		t.Fatal("test setup did not advance Task resourceVersion")
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("supersede adjudication: %v", err)
	}
	superseded := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	currentTask = getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	if superseded.Status.State != corev1alpha1.AgentExecutionAdjudicationSuperseded ||
		!strings.Contains(superseded.Status.Message, "no longer matches") ||
		currentTask.Status.AgentExecutionResolutionRef != nil {
		t.Fatalf("superseded status = %#v, Task ref = %#v",
			superseded.Status, currentTask.Status.AgentExecutionResolutionRef)
	}
}

func TestAgentExecutionAdjudicationRejectsUnsupportedOutcomeConfirmation(t *testing.T) {
	now := time.Date(2026, 8, 6, 21, 30, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, false)
	adjudication := adjudicationTestTaskRecord(t, namespace, task,
		corev1alpha1.AgentExecutionAdjudicationConfirmV1Outcome, now)
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reject unsupported action: %v", err)
	}
	rejected := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	if rejected.Status.State != corev1alpha1.AgentExecutionAdjudicationRejected ||
		!strings.Contains(rejected.Status.Message, "exact terminal receipt") ||
		currentTask.Status.AgentExecutionResolutionRef != nil {
		t.Fatalf("rejected status = %#v, Task ref = %#v",
			rejected.Status, currentTask.Status.AgentExecutionResolutionRef)
	}
}

func TestAgentExecutionAdjudicationAppliesBlockedV2SessionCleanup(t *testing.T) {
	now := time.Date(2026, 8, 6, 22, 0, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestSessionTask()
	session := adjudicationTestBlockedSession(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	adjudication := adjudicationTestSessionRecord(t, namespace, task, session,
		corev1alpha1.AgentExecutionAdjudicationCleanupV2, now)
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, session, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("prepare Session adjudication: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("apply Session adjudication: %v", err)
	}
	applied := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	currentSession := getAdjudicationSessionForTest(t, kubeClient, task.Namespace, session.Name)
	if applied.Status.State != corev1alpha1.AgentExecutionAdjudicationApplied ||
		currentSession.Status.AgentExecutionResolutionRef == nil ||
		currentSession.Status.AgentExecutionResolutionRef.Action != corev1alpha1.AgentExecutionAdjudicationCleanupV2 {
		t.Fatalf("applied status = %#v, Session status = %#v", applied.Status, currentSession.Status)
	}
	if currentSession.Status.Availability != corev1alpha1.RuntimeSessionControlAvailability(store.SessionReconciliationBlocked) ||
		currentSession.Status.BlockedReason != session.Status.BlockedReason ||
		currentSession.Status.Lineage == nil ||
		currentSession.Status.Lineage.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("blocked evidence or lineage was changed: %#v", currentSession.Status)
	}
	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	if currentTask.Status.AgentExecutionResolutionRef != nil {
		t.Fatalf("Session adjudication unexpectedly mutated Task resolution: %#v",
			currentTask.Status.AgentExecutionResolutionRef)
	}
}

func TestAgentExecutionAdjudicationSupersedesAutomaticallyRecoveredSession(t *testing.T) {
	now := time.Date(2026, 8, 6, 22, 30, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestSessionTask()
	session := adjudicationTestBlockedSession(task, corev1alpha1.AgentRuntimeContractHarnessV1)
	adjudication := adjudicationTestSessionRecord(t, namespace, task, session,
		corev1alpha1.AgentExecutionAdjudicationCleanupV1, now)
	session.Status.Availability = corev1alpha1.RuntimeSessionControlAvailability(store.SessionAvailable)
	session.Status.BlockedReason = ""
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, session, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("supersede recovered Session adjudication: %v", err)
	}
	superseded := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	currentSession := getAdjudicationSessionForTest(t, kubeClient, task.Namespace, session.Name)
	if superseded.Status.State != corev1alpha1.AgentExecutionAdjudicationSuperseded ||
		!strings.Contains(superseded.Status.Message, "recovered automatically") ||
		currentSession.Status.AgentExecutionResolutionRef != nil {
		t.Fatalf("superseded status = %#v, Session ref = %#v",
			superseded.Status, currentSession.Status.AgentExecutionResolutionRef)
	}
}

func TestAgentExecutionAdjudicationSupersedesCompetingResolution(t *testing.T) {
	now := time.Date(2026, 8, 6, 23, 0, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, false)
	adjudication := adjudicationTestTaskRecord(t, namespace, task,
		corev1alpha1.AgentExecutionAdjudicationCleanupV1, now)
	reconciler, kubeClient := newAdjudicationTestReconciler(t, now, namespace, task, adjudication)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("prepare adjudication: %v", err)
	}

	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	competing := &corev1alpha1.AgentExecutionResolutionRef{
		AdjudicationName: "other-adjudication", AdjudicationUID: types.UID("other-adjudication-uid"),
		Action:          corev1alpha1.AgentExecutionAdjudicationCleanupV1,
		OperationDigest: adjudicationTestDigest("other-operation"), AppliedAt: metav1.NewTime(now.Add(time.Second)),
	}
	var err error
	competing.ResolutionDigest, err = canonicalAgentExecutionResolutionRefDigest(task.Namespace, competing)
	if err != nil {
		t.Fatal(err)
	}
	currentTask.Status.AgentExecutionResolutionRef = competing
	if err := kubeClient.Status().Update(context.Background(), currentTask); err != nil {
		t.Fatalf("write competing resolution: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("supersede competing adjudication: %v", err)
	}
	superseded := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	if superseded.Status.State != corev1alpha1.AgentExecutionAdjudicationSuperseded ||
		!strings.Contains(superseded.Status.Message, "competing") {
		t.Fatalf("superseded status = %#v", superseded.Status)
	}
}

func newAdjudicationTestReconciler(
	t *testing.T,
	now time.Time,
	objects ...client.Object,
) (*AgentExecutionAdjudicationReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.AgentExecutionAdjudication{},
			&corev1alpha1.Task{},
			&corev1alpha1.RuntimeSessionControl{},
		).
		WithObjects(objects...).Build()
	return &AgentExecutionAdjudicationReconciler{
		Client: kubeClient, APIReader: kubeClient, Recorder: record.NewFakeRecorder(100),
		Now: func() time.Time { return now },
	}, kubeClient
}

func adjudicationTestNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-a", UID: types.UID("tenant-a-uid"), ResourceVersion: "1",
	}}
}

func adjudicationTestQuarantinedTask(hasV1, hasV2 bool) *corev1alpha1.Task {
	quarantine := &corev1alpha1.AgentExecutionQuarantine{
		SchemaVersion: 1, Reason: corev1alpha1.AgentExecutionQuarantineMixedEvidence,
		MigrationInventoryID: "inventory-2026-08-06",
		RecordedAt:           metav1.NewTime(time.Date(2026, 8, 6, 18, 0, 0, 0, time.UTC)),
	}
	if hasV1 {
		quarantine.V1EvidenceDigest = adjudicationTestDigest("v1-route-evidence")
	}
	if hasV2 {
		quarantine.V2EvidenceDigest = adjudicationTestDigest("v2-route-evidence")
	}
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "task-a", UID: types.UID("task-a-uid"), ResourceVersion: "7",
		},
		Status: corev1alpha1.TaskStatus{AgentExecutionQuarantine: quarantine},
	}
}

func adjudicationTestTaskRecord(
	t *testing.T,
	namespace *corev1.Namespace,
	task *corev1alpha1.Task,
	action corev1alpha1.AgentExecutionAdjudicationAction,
	now time.Time,
) *corev1alpha1.AgentExecutionAdjudication {
	t.Helper()
	quarantineDigest, err := canonicalAgentExecutionQuarantineDigest(task.Status.AgentExecutionQuarantine)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := canonicalAgentExecutionTaskEvidenceClosure(namespace.UID, task, nil, quarantineDigest)
	if err != nil {
		t.Fatal(err)
	}
	evidence := make([]string, 0, 2)
	if task.Status.AgentExecutionQuarantine.V1EvidenceDigest != "" {
		evidence = append(evidence, task.Status.AgentExecutionQuarantine.V1EvidenceDigest)
	}
	if task.Status.AgentExecutionQuarantine.V2EvidenceDigest != "" {
		evidence = append(evidence, task.Status.AgentExecutionQuarantine.V2EvidenceDigest)
	}
	return &corev1alpha1.AgentExecutionAdjudication{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: "adjudication-a", UID: types.UID("adjudication-a-uid"),
			ResourceVersion: "3", CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
		},
		Spec: corev1alpha1.AgentExecutionAdjudicationSpec{
			TaskRef: corev1alpha1.AgentExecutionSubjectReference{Name: task.Name, UID: task.UID},
			ExpectedState: corev1alpha1.AgentExecutionExpectedSubjectState{
				SubjectResourceVersion: task.ResourceVersion, EvidenceClosureWatermark: closure,
			},
			QuarantineDigest: quarantineDigest, Action: action, EvidenceDigests: evidence,
			Justification: "independently verified route cleanup", RequestedBy: "operator@example.com",
		},
	}
}

func adjudicationTestSessionTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "session-task", UID: types.UID("session-task-uid"), ResourceVersion: "11",
		},
		Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: "chat-a", Append: true}},
	}
}

func adjudicationTestBlockedSession(
	task *corev1alpha1.Task,
	contract corev1alpha1.AgentRuntimeContractVersion,
) *corev1alpha1.RuntimeSessionControl {
	createdAt := metav1.NewTime(time.Date(2026, 8, 6, 17, 0, 0, 0, time.UTC))
	updatedAt := metav1.NewTime(time.Date(2026, 8, 6, 19, 0, 0, 0, time.UTC))
	return &corev1alpha1.RuntimeSessionControl{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: storekube.RuntimeSessionControlObjectName(task.Spec.SessionRef.Name),
			UID: types.UID("runtime-session-control-uid"), ResourceVersion: "13",
		},
		Spec: corev1alpha1.RuntimeSessionControlSpec{
			SessionName: task.Spec.SessionRef.Name, SessionUID: "logical-session-uid",
			RequestDigest: adjudicationTestDigest("session-create"),
			Owner:         corev1alpha1.ControlRecordOwner{Kind: "Session", UID: "logical-session-uid"},
		},
		Status: corev1alpha1.RuntimeSessionControlStatus{
			Generation: 1, Lifecycle: corev1alpha1.RuntimeSessionControlLifecycle("Poisoned"),
			Availability:            corev1alpha1.RuntimeSessionControlAvailability(store.SessionReconciliationBlocked),
			MutationLeaseGeneration: 2, BlockedReason: "terminal receipt requires reconciliation",
			RelatedPromptAttemptID: "prompt-attempt-a",
			Lineage: &corev1alpha1.RuntimeSessionLineageStatus{
				NamespaceUID: types.UID("tenant-a-uid"), SessionUID: "logical-session-uid",
				ContractVersion: contract, Generation: 1, RuntimeIdentity: "codex",
				ConfigDigest: adjudicationTestDigest("session-config"),
				Provenance:   corev1alpha1.RuntimeSessionLineageFirstUse, EstablishedAt: createdAt,
			},
			ControlRecordMutationStatus: corev1alpha1.ControlRecordMutationStatus{
				ControllerEpochName: "controller", ControllerEpoch: 4,
				LastOperationID: "block-session", LastOperationDigest: adjudicationTestDigest("block-session"),
				Version: 5, CreatedAt: &createdAt, UpdatedAt: &updatedAt,
			},
		},
	}
}

func adjudicationTestSessionRecord(
	t *testing.T,
	namespace *corev1.Namespace,
	task *corev1alpha1.Task,
	session *corev1alpha1.RuntimeSessionControl,
	action corev1alpha1.AgentExecutionAdjudicationAction,
	now time.Time,
) *corev1alpha1.AgentExecutionAdjudication {
	t.Helper()
	blockedDigest, err := canonicalAgentExecutionBlockedStateDigest(session)
	if err != nil {
		t.Fatal(err)
	}
	taskEvidence, err := buildAgentExecutionTaskRouteEvidence(task, "")
	if err != nil {
		t.Fatal(err)
	}
	closure, err := acpDomainDigest(agentExecutionEvidenceClosureDomain, agentExecutionSessionEvidenceClosure{
		SchemaVersion: 1, NamespaceUID: namespace.UID,
		TaskName: task.Name, TaskUID: task.UID, TaskResourceVersion: task.ResourceVersion,
		TaskRouteEvidence: taskEvidence,
		SessionName:       session.Spec.SessionName, SessionUID: session.UID,
		SessionResourceVersion: session.ResourceVersion, LogicalSessionUID: session.Spec.SessionUID,
		DomainVersion: session.Status.Version, BlockedStateDigest: blockedDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1alpha1.AgentExecutionAdjudication{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: "session-adjudication", UID: types.UID("session-adjudication-uid"),
			ResourceVersion: "4", CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
		},
		Spec: corev1alpha1.AgentExecutionAdjudicationSpec{
			TaskRef: corev1alpha1.AgentExecutionSubjectReference{Name: task.Name, UID: task.UID},
			SessionRef: &corev1alpha1.AgentExecutionSubjectReference{
				Name: session.Spec.SessionName, UID: session.UID,
			},
			ExpectedState: corev1alpha1.AgentExecutionExpectedSubjectState{
				SubjectResourceVersion: session.ResourceVersion, SubjectDomainVersion: session.Status.Version,
				EvidenceClosureWatermark: closure,
			},
			BlockedStateDigest: blockedDigest, Action: action,
			EvidenceDigests: []string{adjudicationTestDigest("independent-session-receipt")},
			Justification:   "independently verified Session cleanup", RequestedBy: "operator@example.com",
		},
	}
}

func getAdjudicationForTest(
	t *testing.T,
	kubeClient client.Client,
	key client.ObjectKey,
) *corev1alpha1.AgentExecutionAdjudication {
	t.Helper()
	result := &corev1alpha1.AgentExecutionAdjudication{}
	if err := kubeClient.Get(context.Background(), key, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func getAdjudicationTaskForTest(
	t *testing.T,
	kubeClient client.Client,
	namespace, name string,
) *corev1alpha1.Task {
	t.Helper()
	result := &corev1alpha1.Task{}
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func getAdjudicationSessionForTest(
	t *testing.T,
	kubeClient client.Client,
	namespace, name string,
) *corev1alpha1.RuntimeSessionControl {
	t.Helper()
	result := &corev1alpha1.RuntimeSessionControl{}
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func adjudicationTestDigest(seed string) string {
	return store.CanonicalAgentExecutionSnapshotDigest([]byte(seed))
}
