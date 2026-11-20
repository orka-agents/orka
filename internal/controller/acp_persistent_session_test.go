/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
	"github.com/orka-agents/orka/internal/store"
)

const (
	persistentTestNamespace = "tenant"
	persistentTestPoolName  = "acp-ws-pool"
	persistentTestWorkspace = "acp-ws-workspace"
)

var persistentTestNow = time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)

func persistentTestDigest(label string) string {
	digest, _ := acpDomainDigest("persistent-session-test", label)
	return digest
}

func persistentTestPlan() ACPRuntimePlan {
	return ACPRuntimePlan{
		PoolName: persistentTestPoolName,
		Workspace: &ACPRuntimeWorkspaceBinding{
			ReusePolicy: corev1alpha1.WorkspaceReusePolicySession, SessionUID: "session-uid",
			Class: &ACPWorkspaceClassBinding{Name: "substrate", UID: "class-uid", Generation: 1, ControllerName: "substrate.workspace.orka.ai"},
		},
	}
}

func persistentTestTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: persistentTestNamespace, Name: "task-n1", UID: "task-n1-uid",
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: persistentTestWorkspace},
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: "workspace-uid", acpTaskAttachmentEpochAnnotation: "3"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "continue",
			SessionRef: &corev1alpha1.SessionReference{Name: "session", Append: true},
		},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: 1, PromptID: "prompt-n1", RuntimePoolName: persistentTestPoolName, RuntimePoolUID: "pool-uid",
		}},
	}
}

func persistentTestRecord(t *testing.T) *store.SessionResidentRuntime {
	t.Helper()
	record, err := store.NewSessionResidentRuntime(store.SessionResidentRuntime{
		SourceTaskUID: "task-n-uid", SourceAttempt: 1, SourcePromptAttemptID: "attempt-n", SourceTurnID: "turn-n",
		SourceLeaseGeneration: 2, ControllerEpochName: "orka", ControllerEpoch: 7,
		WorkspaceName: persistentTestWorkspace, WorkspaceUID: "workspace-uid", WorkspaceAttachmentEpoch: 2,
		RuntimePoolName: persistentTestPoolName, RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeInstanceID: "instance-1", SupervisorBootID: "boot-1", ServedRecordDigest: persistentServedDigest(),
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 2,
		ProfileDigest: persistentTestDigest("profile"), MCPDigest: persistentTestDigest("mcp"),
		WorkspaceDigest: persistentTestDigest("workspace"), LineageConfigDigest: persistentTestDigest("lineage"),
		ReuseDigest: persistentTestDigest("reuse"), TranscriptHeadDigest: persistentTestHead(),
		PromptDelivered: true, KeptAt: persistentTestNow, ExpiresAt: persistentTestNow.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewSessionResidentRuntime: %v", err)
	}
	return &record
}

// persistentTestHead is turn N's head on an empty transcript: it appended
// "question n" and "answer n".
func persistentTestHead() string {
	return acpPersistentSessionTranscriptHeadDigest("", []acpTranscriptHeadMessage{
		{Role: acpBootstrapRoleUser, Content: "question n"}, {Role: acpSessionTurnAssistantRole, Content: "answer n"},
	})
}

const persistentServedRecord = "sha256:spec 3 allocation-1 instance-1 rev-1 pod-uid boot-1"

func persistentServedDigest() string {
	return acpRuntimePoolServedRecordDigest(&corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{externalRuntimeServedAnnotation: persistentServedRecord},
	}})
}

func persistentTestPool() *corev1alpha1.RuntimePool {
	return &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: persistentTestNamespace, Name: persistentTestPoolName, UID: "pool-uid", Generation: 4,
			Annotations: map[string]string{externalRuntimeServedAnnotation: persistentServedRecord},
		},
		Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{}},
		Status: corev1alpha1.RuntimePoolStatus{
			Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
			ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: "instance-1", BootID: "boot-1", ControllerEpoch: 7},
		},
	}
}

func persistentTestFence() store.ControllerEpochFence {
	return store.ControllerEpochFence{Name: "orka", Epoch: 7, HolderID: "controller"}
}

func persistentClaimInput(record *store.SessionResidentRuntime) acpPersistentSessionClaimInput {
	return acpPersistentSessionClaimInput{
		handoffID: record.HandoffID, fence: persistentTestFence(),
		runtimeFence: harnessv2.Fence{
			RuntimeInstanceID: "instance-1", SupervisorBootID: "boot-1",
			RuntimeProfileDigest: harnessv2.ProfileDigest(record.ProfileDigest),
		},
		mcpDigest: record.MCPDigest, reuseDigest: record.ReuseDigest, lineageDigest: record.LineageConfigDigest,
		pool: persistentTestPool(), now: persistentTestNow.Add(time.Minute),
	}
}

func TestACPPersistentSessionStaticScopeRefusals(t *testing.T) {
	plan := persistentTestPlan()
	if reason := acpPersistentSessionStaticScope(true, persistentTestTask(), plan, false); reason != "" {
		t.Fatalf("eligible Task refused: %s", reason)
	}
	cases := map[string]struct {
		enabled bool
		task    func(*corev1alpha1.Task)
		plan    func(*ACPRuntimePlan)
		gateway bool
		want    string
	}{
		"flag disabled":     {want: acpPersistentReasonFlagDisabled},
		"no sessionRef":     {enabled: true, task: func(t *corev1alpha1.Task) { t.Spec.SessionRef = nil }, want: acpPersistentReasonIneligibleScope},
		"not appending":     {enabled: true, task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.Append = false }, want: acpPersistentReasonIneligibleScope},
		"no workspace":      {enabled: true, plan: func(p *ACPRuntimePlan) { p.Workspace = nil }, want: acpPersistentReasonIneligibleScope},
		"per-task reuse":    {enabled: true, plan: func(p *ACPRuntimePlan) { p.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicyNone }, want: acpPersistentReasonIneligibleScope},
		"legacy class":      {enabled: true, plan: func(p *ACPRuntimePlan) { p.Workspace.Class.ControllerName = "" }, want: acpPersistentReasonIneligibleScope},
		"class opt-out":     {enabled: true, plan: func(p *ACPRuntimePlan) { p.Workspace.Class.PersistentSessionDisabled = true }, want: acpPersistentReasonClassDisabled},
		"gateway grant":     {enabled: true, gateway: true, want: acpPersistentReasonIneligibleScope},
		"gateway message":   {enabled: true, task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.ThroughMessageID = "gateway:message-1" }, want: acpPersistentReasonIneligibleScope},
		"transaction bound": {enabled: true, task: func(t *corev1alpha1.Task) { t.Spec.Transaction = &corev1alpha1.TaskTransaction{} }, want: acpPersistentReasonIneligibleScope},
		"agent identity": {enabled: true, task: func(t *corev1alpha1.Task) {
			t.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{Agent: &corev1alpha1.AgentExecutionAgentRef{
				Identity: &corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: "agent"},
			}}
		}, want: acpPersistentReasonIneligibleScope},
		"operator tainted": {enabled: true, task: func(t *corev1alpha1.Task) {
			t.Annotations = map[string]string{operatoraccess.TaintAnnotation: operatoraccess.EncodeTaint(operatoraccess.Taint{At: persistentTestNow})}
		}, want: acpPersistentReasonIneligibleScope},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			task := persistentTestTask()
			if tc.task != nil {
				tc.task(task)
			}
			plan := persistentTestPlan()
			if tc.plan != nil {
				tc.plan(&plan)
			}
			if got := acpPersistentSessionStaticScope(tc.enabled, task, plan, tc.gateway); got != tc.want {
				t.Fatalf("scope = %q, want %q", got, tc.want)
			}
		})
	}
}

type residentSupportStore struct {
	store.DurableControlStore
	supported   bool
	retired     []string
	publication *store.Publication
}

func (s *residentSupportStore) GetPublication(context.Context, string) (*store.Publication, error) {
	if s.publication == nil {
		return nil, store.ErrNotFound
	}
	copied := *s.publication
	return &copied, nil
}

func (s *residentSupportStore) SupportsSessionResidentRuntime() bool { return s.supported }

func (s *residentSupportStore) RetireSessionResidentRuntime(_ context.Context, request store.RetireSessionResidentRuntimeRequest) (*store.SessionControl, error) {
	s.retired = append(s.retired, request.HandoffID)
	return &store.SessionControl{}, nil
}

func TestACPPersistentSessionDispatchScopeRefusals(t *testing.T) {
	bound := &verifiedAgentExecution{plan: persistentTestPlan()}
	target := acpDispatchTarget{pool: persistentTestPool()}
	dispatcher := &ACPDispatcher{PersistentSessionEnabled: true, Store: &residentSupportStore{supported: true}}
	if reason := dispatcher.acpPersistentSessionScope(persistentTestTask(), bound, target, false, true); reason != "" {
		t.Fatalf("eligible dispatch refused: %s", reason)
	}
	for name, tc := range map[string]struct {
		dispatcher *ACPDispatcher
		target     acpDispatchTarget
		native     bool
		rebase     bool
		want       string
	}{
		"external runtimeRef":  {dispatcher: dispatcher, target: acpDispatchTarget{external: &corev1alpha1.AgentRuntime{}}, rebase: true, want: acpPersistentReasonIneligibleScope},
		"codex native session": {dispatcher: dispatcher, target: target, native: true, rebase: true, want: acpPersistentReasonIneligibleScope},
		"store without records": {
			dispatcher: &ACPDispatcher{PersistentSessionEnabled: true, Store: &residentSupportStore{}}, target: target, rebase: true,
			want: acpPersistentReasonIneligibleScope,
		},
		"supervisor without rebase": {dispatcher: dispatcher, target: target, want: acpPersistentReasonRebaseUnsupported},
		"flag disabled":             {dispatcher: &ACPDispatcher{Store: &residentSupportStore{supported: true}}, target: target, rebase: true, want: acpPersistentReasonFlagDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.dispatcher.acpPersistentSessionScope(persistentTestTask(), bound, tc.target, tc.native, tc.rebase); got != tc.want {
				t.Fatalf("scope = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestACPResidentRuntimeStaticRefusalReasons(t *testing.T) {
	record := persistentTestRecord(t)
	if stage, reason := acpResidentRuntimeStaticRefusal(record, persistentClaimInput(record)); reason != "" {
		t.Fatalf("matching record refused at %s: %s", stage, reason)
	}
	cases := map[string]struct {
		record func(*store.SessionResidentRuntime) *store.SessionResidentRuntime
		input  func(*acpPersistentSessionClaimInput)
		want   string
	}{
		"no record":           {record: func(*store.SessionResidentRuntime) *store.SessionResidentRuntime { return nil }, want: acpPersistentReasonNoRecord},
		"other handoff":       {input: func(in *acpPersistentSessionClaimInput) { in.handoffID = persistentTestDigest("other") }, want: acpPersistentReasonNoRecord},
		"already claimed":     {record: stateResidentRuntime(store.SessionResidentRuntimeClaimed), want: acpPersistentReasonClaimLost},
		"retiring":            {record: stateResidentRuntime(store.SessionResidentRuntimeRetiring), want: acpPersistentReasonClaimLost},
		"epoch changed":       {input: func(in *acpPersistentSessionClaimInput) { in.fence.Epoch = 8 }, want: acpPersistentReasonEpochChanged},
		"expired":             {input: func(in *acpPersistentSessionClaimInput) { in.now = record.ExpiresAt }, want: acpPersistentReasonRecordExpired},
		"pool recreated":      {input: func(in *acpPersistentSessionClaimInput) { in.pool.UID = "pool-uid-2" }, want: acpPersistentReasonPoolReplaced},
		"pool generation":     {input: func(in *acpPersistentSessionClaimInput) { in.pool.Generation = 5 }, want: acpPersistentReasonPoolReplaced},
		"instance replaced":   {input: func(in *acpPersistentSessionClaimInput) { in.runtimeFence.RuntimeInstanceID = "instance-2" }, want: acpPersistentReasonRuntimeReplaced},
		"supervisor rebooted": {input: func(in *acpPersistentSessionClaimInput) { in.runtimeFence.SupervisorBootID = "boot-2" }, want: acpPersistentReasonRuntimeReplaced},
		"profile changed": {input: func(in *acpPersistentSessionClaimInput) {
			in.runtimeFence.RuntimeProfileDigest = harnessv2.ProfileDigest(persistentTestDigest("profile-2"))
		}, want: acpPersistentReasonSnapshotMismatch},
		"mcp changed":      {input: func(in *acpPersistentSessionClaimInput) { in.mcpDigest = persistentTestDigest("mcp-2") }, want: acpPersistentReasonSnapshotMismatch},
		"snapshot changed": {input: func(in *acpPersistentSessionClaimInput) { in.reuseDigest = persistentTestDigest("reuse-2") }, want: acpPersistentReasonSnapshotMismatch},
		"lineage changed":  {input: func(in *acpPersistentSessionClaimInput) { in.lineageDigest = persistentTestDigest("lineage-2") }, want: acpPersistentReasonSnapshotMismatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := store.CloneSessionResidentRuntime(record)
			if tc.record != nil {
				candidate = tc.record(candidate)
			}
			input := persistentClaimInput(record)
			if tc.input != nil {
				tc.input(&input)
			}
			if _, got := acpResidentRuntimeStaticRefusal(candidate, input); got != tc.want {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

func stateResidentRuntime(state store.SessionResidentRuntimeState) func(*store.SessionResidentRuntime) *store.SessionResidentRuntime {
	return func(record *store.SessionResidentRuntime) *store.SessionResidentRuntime {
		record.State = state
		if state == store.SessionResidentRuntimeClaimed {
			record.Claim = &store.SessionResidentRuntimeClaim{TaskUID: "other", Attempt: 1, PromptID: "p", LeaseGeneration: 9}
		}
		return record
	}
}

// historyTranscriptStore serves a fixed canonical transcript.
type historyTranscriptStore struct {
	store.SessionStore
	messages []store.SessionMessage
}

func (s *historyTranscriptStore) LoadTranscript(_ context.Context, _, _ string, maxMessages int) ([]store.SessionMessage, error) {
	if maxMessages <= 0 || maxMessages >= len(s.messages) {
		return append([]store.SessionMessage(nil), s.messages...), nil
	}
	return append([]store.SessionMessage(nil), s.messages[len(s.messages)-maxMessages:]...), nil
}

func TestACPResidentRuntimeHistoryRefusalReasons(t *testing.T) {
	record := persistentTestRecord(t)
	transcript := []store.SessionMessage{
		{ID: "m1", Role: "user", Content: "question n"},
		{ID: "m2", Role: acpSessionTurnAssistantRole, Content: "answer n"},
	}
	control := &store.SessionControl{Namespace: persistentTestNamespace, SessionName: "session", Availability: store.SessionAvailable}
	dispatcher := &ACPDispatcher{Sessions: &ACPSessionContinuity{transcripts: &historyTranscriptStore{messages: transcript}}}
	ctx := context.Background()
	if reason, _, err := dispatcher.acpResidentRuntimeHistoryRefusal(ctx, persistentTestTask(), control, record); err != nil || reason != "" {
		t.Fatalf("unchanged history refused: %q, %v", reason, err)
	}
	cases := map[string]struct {
		task     func(*corev1alpha1.Task)
		messages []store.SessionMessage
		control  func(*store.SessionControl)
		want     string
	}{
		"prompt included":  {task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.PromptIncluded = true }, want: acpPersistentReasonHistoryBoundary},
		"other cutoff":     {task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.ThroughMessageID = "m1" }, want: acpPersistentReasonHistoryBoundary},
		"narrower history": {task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.MaxMessages = 1 }, want: acpPersistentReasonHistoryBoundary},
		"appended later":   {messages: append(append([]store.SessionMessage(nil), transcript...), store.SessionMessage{ID: "m3", Role: "user", Content: "out of band"}), want: acpPersistentReasonTranscriptChanged},
		"edited head":      {messages: []store.SessionMessage{transcript[0], {ID: "m2", Role: acpSessionTurnAssistantRole, Content: "edited"}}, want: acpPersistentReasonTranscriptChanged},
		"empty transcript": {messages: []store.SessionMessage{}, want: acpPersistentReasonTranscriptChanged},
		"repeated reply after an out-of-band exchange": {messages: append(append([]store.SessionMessage(nil), transcript...),
			store.SessionMessage{ID: "m3", Role: "user", Content: "question n"}, store.SessionMessage{ID: "m4", Role: acpSessionTurnAssistantRole, Content: "answer n"}),
			want: acpPersistentReasonTranscriptChanged},
		"exchange inserted before turn n": {messages: []store.SessionMessage{{ID: "m0", Role: "user", Content: "earlier"}, transcript[0], transcript[1]},
			want: acpPersistentReasonTranscriptChanged},
		"session blocked":   {control: func(c *store.SessionControl) { c.Availability = store.SessionReconciliationBlocked }, want: acpPersistentReasonSessionUnavailable},
		"exact cutoff kept": {task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.ThroughMessageID = "m2" }, want: ""},
		"wide enough limit": {task: func(t *corev1alpha1.Task) { t.Spec.SessionRef.MaxMessages = 2 }, want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			task := persistentTestTask()
			if tc.task != nil {
				tc.task(task)
			}
			messages := transcript
			if tc.messages != nil {
				messages = tc.messages
			}
			candidate := *control
			if tc.control != nil {
				tc.control(&candidate)
			}
			d := &ACPDispatcher{Sessions: &ACPSessionContinuity{transcripts: &historyTranscriptStore{messages: messages}}}
			reason, lastID, err := d.acpResidentRuntimeHistoryRefusal(ctx, task, &candidate, record)
			if err == nil && reason == "" && lastID != "m2" {
				t.Fatalf("reused process head = %q, want m2", lastID)
			}
			if err != nil || reason != tc.want {
				t.Fatalf("history refusal = %q, %v; want %q", reason, err, tc.want)
			}
		})
	}
}

func TestACPRuntimePoolResidentRefusalReasons(t *testing.T) {
	fence := persistentTestFence()
	if reason := acpRuntimePoolResidentRefusal(persistentTestPool(), "instance-1", "boot-1", fence); reason != "" {
		t.Fatalf("serving pool refused: %s", reason)
	}
	for name, tc := range map[string]struct {
		mutate func(*corev1alpha1.RuntimePool)
		want   string
	}{
		"draining":  {mutate: func(p *corev1alpha1.RuntimePool) { p.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining }, want: acpPersistentReasonPoolNotAccepting},
		"degraded":  {mutate: func(p *corev1alpha1.RuntimePool) { p.Status.AdmissionState = "" }, want: acpPersistentReasonPoolNotAccepting},
		"replaced":  {mutate: func(p *corev1alpha1.RuntimePool) { p.Status.ActiveInstance.RuntimeInstanceID = "instance-2" }, want: acpPersistentReasonRuntimeReplaced},
		"rebooted":  {mutate: func(p *corev1alpha1.RuntimePool) { p.Status.ActiveInstance.BootID = "boot-2" }, want: acpPersistentReasonRuntimeReplaced},
		"no active": {mutate: func(p *corev1alpha1.RuntimePool) { p.Status.ActiveInstance = nil }, want: acpPersistentReasonRuntimeReplaced},
		"old epoch": {mutate: func(p *corev1alpha1.RuntimePool) { p.Status.ActiveInstance.ControllerEpoch = 6 }, want: acpPersistentReasonEpochChanged},
		"retirement requested": {mutate: func(p *corev1alpha1.RuntimePool) {
			p.Annotations[externalRuntimeRetirementRequestedAnnotation] = booleanTrueValue
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"evicting": {mutate: func(p *corev1alpha1.RuntimePool) {
			p.Annotations[externalRuntimeEvictionAnnotation] = persistentTestNow.Format(time.RFC3339)
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"deleting": {mutate: func(p *corev1alpha1.RuntimePool) {
			now := metav1.NewTime(persistentTestNow)
			p.DeletionTimestamp = &now
		}, want: acpPersistentReasonPoolReplaced},
	} {
		t.Run(name, func(t *testing.T) {
			pool := persistentTestPool()
			tc.mutate(pool)
			if got := acpRuntimePoolResidentRefusal(pool, "instance-1", "boot-1", fence); got != tc.want {
				t.Fatalf("pool refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

func persistentWarmWorkspace(task *corev1alpha1.Task, record *store.SessionResidentRuntime) *workspacev1alpha1.ExecutionWorkspace {
	epoch := acpTaskRecordedAttachmentEpoch(task)
	return &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: persistentTestNamespace, Name: persistentTestWorkspace, UID: types.UID(record.WorkspaceUID),
			Annotations: map[string]string{
				acpWorkspaceAttachedFromWarmAnnotation: formatACPWorkspaceAttachedFromWarm(
					task.UID, epoch, record.KeptAt.Add(4*time.Minute).Format(time.RFC3339Nano), record.SourceTaskUID,
				),
			},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			DesiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			AttachmentEpoch: epoch,
			Attachment: &workspacev1alpha1.ExecutionWorkspaceAttachment{
				TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: epoch,
			},
		},
	}
}

func TestACPPersistentSessionWorkspaceRefusalReasons(t *testing.T) {
	task := persistentTestTask()
	record := persistentTestRecord(t)
	if reason := acpPersistentSessionWorkspaceRefusal(persistentWarmWorkspace(task, record), task, record); reason != "" {
		t.Fatalf("warm workspace refused: %s", reason)
	}
	for name, tc := range map[string]struct {
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
		want   string
	}{
		"replaced": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.UID = "workspace-uid-2" }, want: acpPersistentReasonWorkspaceIncarnation},
		"suspending": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"failed": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"quarantined": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Labels = map[string]string{workspacev1alpha1.QuarantinedLabel: booleanTrueValue}
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"tainted": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.Annotations[operatoraccess.TaintAnnotation] = "true" }, want: acpPersistentReasonWorkspaceNotWarm},
		"window drained": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceWarmWindowDrainedAnnotation] = "now"
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"suspend pending": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSuspendPendingAnnotation] = "now"
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"detached": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.Attachment = nil }, want: acpPersistentReasonWorkspaceNotWarm},
		"other holder": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Attachment.TaskRef.UID = "rival"
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"no evidence": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceAttachedFromWarmAnnotation)
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"settled by another task": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceAttachedFromWarmAnnotation] = formatACPWorkspaceAttachedFromWarm(task.UID, 3, persistentTestNow.Add(time.Minute).Format(time.RFC3339Nano), "someone-else")
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"older window": {mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceAttachedFromWarmAnnotation] = formatACPWorkspaceAttachedFromWarm(task.UID, 3, persistentTestNow.Add(-time.Minute).Format(time.RFC3339Nano), record.SourceTaskUID)
		}, want: acpPersistentReasonWorkspaceNotWarm},
	} {
		t.Run(name, func(t *testing.T) {
			workspace := persistentWarmWorkspace(task, record)
			tc.mutate(workspace)
			if got := acpPersistentSessionWorkspaceRefusal(workspace, task, record); got != tc.want {
				t.Fatalf("workspace refusal = %q, want %q", got, tc.want)
			}
		})
	}
	stale := persistentTestTask()
	stale.Annotations[acpTaskAttachmentEpochAnnotation] = "2"
	if got := acpPersistentSessionWorkspaceRefusal(persistentWarmWorkspace(stale, record), stale, record); got != acpPersistentReasonWorkspaceNotWarm {
		t.Fatalf("an attachment no newer than the kept one = %q, want workspace-not-warm", got)
	}
}

// The snapshot test (ADR 0059 section 5): every frozen input of the create
// request must change the reuse digest when it changes.
func TestACPPersistentSessionReuseDigestCoversEveryInput(t *testing.T) {
	base := acpPersistentSessionReuseInput{
		ProfileDigest: persistentTestDigest("profile"), RuntimeImage: "image@sha256:1",
		RuntimeProfile:     harnessv2.RuntimeProfile{ACPProfile: harnessv2.ACPProfileV1, ProviderKind: "claude", Model: "model"},
		AgentConfiguration: harnessv2.AgentSessionConfiguration{AgentUID: "agent", AgentGeneration: 1, Model: "model"},
		MCPDigest:          persistentTestDigest("mcp"), WorkspaceIntent: "write",
		Workspace:          &corev1alpha1.WorkspaceConfig{GitRepo: "https://github.com/orka-agents/orka"},
		CredentialBindings: []store.PromptCredentialBinding{{Role: store.PromptCredentialSourceRead, SecretName: "read", ResourceVersion: "1"}},
		ExecutionWorkspace: &agentExecutionSnapshotWorkspaceBinding{BindingDigest: persistentTestDigest("binding")},
		Connections:        []agentExecutionSnapshotConnection{{PolicyName: "policy", UID: "connection"}},
		AgentUID:           "agent", AgentGeneration: 1,
		RuntimeOverride: &corev1alpha1.AgentRuntimeSpec{}, DefaultTools: &agentExecutionSnapshotToolPolicy{},
	}
	baseDigest, err := acpPersistentSessionReuseDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	value := reflect.ValueOf(&base).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		changed := base
		mutatePersistentReuseField(t, reflect.ValueOf(&changed).Elem().Field(i))
		digest, err := acpPersistentSessionReuseDigest(changed)
		if err != nil {
			t.Fatalf("digest with changed %s: %v", field.Name, err)
		}
		if digest == baseDigest {
			t.Fatalf("changing %s does not change the reuse digest", field.Name)
		}
	}
}

func mutatePersistentReuseField(t *testing.T, field reflect.Value) {
	t.Helper()
	switch field.Kind() {
	case reflect.String:
		field.SetString(field.String() + "-changed")
	case reflect.Int, reflect.Int32, reflect.Int64:
		field.SetInt(field.Int() + 1)
	case reflect.Bool:
		field.SetBool(!field.Bool())
	case reflect.Pointer:
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		} else {
			field.Set(reflect.Zero(field.Type()))
		}
	case reflect.Slice:
		field.Set(reflect.Append(field, reflect.Zero(field.Type().Elem())))
	case reflect.Struct:
		for _, field0 := range field.Fields() {
			if field0.CanSet() && field0.Kind() == reflect.String {
				field0.SetString(field0.String() + "-changed")
				return
			}
		}
		t.Fatalf("no settable string field to mutate in %s", field.Type())
	default:
		t.Fatalf("unsupported reuse input kind %s", field.Kind())
	}
}

func TestACPPersistentSessionRecreatePlanMovesAboveTheResidentGeneration(t *testing.T) {
	record := persistentTestRecord(t)
	plan := acpPersistentSessionRecreatePlan(ACPRuntimeSessionPlan{
		Binding: acpRuntimeSessionBindingFromResidentRuntime(record), Reason: "reuse-live-runtime-session",
	}, record, 3)
	if !plan.Recreate || !plan.BootstrapRequired || plan.Binding.Generation <= uint64(record.RuntimeSessionGeneration) ||
		plan.Binding.Generation < 3 || plan.Binding.PromptDelivered || plan.Binding.WorkspaceDigest != "" {
		t.Fatalf("recreate plan = %#v", plan)
	}
	already := ACPRuntimeSessionPlan{Recreate: true, Binding: ACPRuntimeSessionBinding{Generation: 9}}
	if got := acpPersistentSessionRecreatePlan(already, record, 3); got.Binding.Generation != 9 {
		t.Fatalf("an existing recreation plan must be kept, got %#v", got)
	}
}

func TestACPRebasedRuntimeWorkspaceUsesTheRecordIdentityOnlyForThePublishedCommit(t *testing.T) {
	record := persistentTestRecord(t)
	record.RebasedTreeDigest = persistentTestDigest("rebased-tree")
	record.VerifiedBaseline = &store.VerifiedBranchBaseline{
		RepositoryID: "github.com/orka-agents/orka", Ref: "refs/heads/orka/session", SHA: strings.Repeat("b", 40),
	}
	prepared := preparedACPRuntimeWorkspace{
		baseline: harnessv2.WorkspaceBaseline{
			RepositoryIdentity: "github.com/orka-agents/orka", Revision: strings.Repeat("b", 40), TreeDigest: persistentTestDigest("publisher-tree"),
			Artifact: &harnessv2.ArtifactReference{},
		},
		spec:          harnessv2.WorkspaceSpec{Intent: harnessv2.WorkspaceIntentWrite},
		bindingDigest: persistentTestDigest("publisher-binding"),
		authorization: &harnessv2.ArtifactAuthorization{Capability: "capability"},
	}
	rebased, applied, err := acpRebasedRuntimeWorkspace(prepared, record)
	if err != nil || !applied {
		t.Fatalf("rebased workspace = %v, %v", applied, err)
	}
	if rebased.baseline.TreeDigest != record.RebasedTreeDigest || rebased.baseline.Artifact != nil || rebased.authorization != nil {
		t.Fatalf("rebased workspace keeps a download or the publisher tree: %#v", rebased)
	}
	sourceRef, _ := runtimeWorkspaceSourceRef(&corev1alpha1.WorkspaceConfig{Ref: record.VerifiedBaseline.Ref})
	want, _ := acpRuntimeWorkspaceBindingDigest(sourceRef, harnessv2.WorkspaceSpec{Intent: harnessv2.WorkspaceIntentWrite, Baseline: rebased.baseline})
	if rebased.bindingDigest != want {
		t.Fatal("the reused binding digest must be the rebased identity turn N recorded")
	}
	moved := prepared
	moved.baseline.Revision = strings.Repeat("c", 40)
	if _, applied, err := acpRebasedRuntimeWorkspace(moved, record); err != nil || applied {
		t.Fatalf("a branch that moved after publication must not reuse the rebased identity: %v, %v", applied, err)
	}
}

func TestACPWorkspaceAttachedFromWarmEvidenceRoundTrip(t *testing.T) {
	task := persistentTestTask()
	workspace := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		acpWorkspaceSuspendAtAnnotation:       persistentTestNow.Format(time.RFC3339Nano),
		acpWorkspaceLastSettledTaskAnnotation: "task-n-uid 2",
	}}}
	evidence := acpWorkspaceAttachedFromWarmEvidence(workspace, task, 3)
	parsed, ok := parseACPWorkspaceAttachedFromWarm(evidence[acpWorkspaceAttachedFromWarmAnnotation])
	if !ok || parsed.taskUID != string(task.UID) || parsed.epoch != 3 || parsed.lastSettledTaskUID != "task-n-uid" ||
		!parsed.suspendAt.Equal(persistentTestNow) {
		t.Fatalf("parsed evidence = %#v, %v", parsed, ok)
	}
	delete(workspace.Annotations, acpWorkspaceSuspendAtAnnotation)
	if acpWorkspaceAttachedFromWarmEvidence(workspace, task, 3) != nil {
		t.Fatal("an attach outside a warm window records no evidence")
	}
}

// fakePersistentSupervisor serves status and rebase for the keep and gate.
type fakePersistentSupervisor struct {
	status    harnessv2.RuntimeSessionStatus
	rebaseErr bool
	rebased   int
	headers   http.Header
	// fence, when set, is the runtime fence the status reports.
	fence *harnessv2.Fence
}

func (f *fakePersistentSupervisor) client(t *testing.T, profileDigest harnessv2.ProfileDigest) *harnessv2.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath:
			f.headers = r.Header.Clone()
			response := dispatcherRuntimeStatusResponse(profileDigest, harnessv2.RuntimeSessionDescriptor{})
			response.Fence.RuntimeInstanceID, response.Fence.SupervisorBootID = "instance-1", "boot-1"
			if f.fence != nil {
				response.Fence = runtimePoolFenceWithSession(*f.fence, "", 0)
			}
			if f.status.RuntimeSessionUID != "" {
				response.Sessions = []harnessv2.RuntimeSessionStatus{f.status}
				response.Pressure.ResidentSessions = 1
			}
			writeDispatcherJSON(w, response)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/rebase"):
			f.rebased++
			if f.rebaseErr {
				writeDispatcherJSONStatus(w, http.StatusConflict, harnessv2.ErrorResponse{
					Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeDigestConflict, Message: "refused",
				})
				return
			}
			var request harnessv2.RebaseRuntimeSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fence := request.Metadata.Fence
			now := time.Now().UTC()
			writeDispatcherJSON(w, harnessv2.RebaseRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				Session: harnessv2.RuntimeSessionDescriptor{
					RuntimeSessionID: harnessv2.RuntimeSessionID(runtimeSessionID(fence)), RuntimeSessionUID: fence.RuntimeSessionUID,
					Generation: fence.RuntimeSessionGeneration, RuntimeInstanceID: fence.RuntimeInstanceID, SupervisorBootID: fence.SupervisorBootID,
					RuntimeProfileDigest: fence.RuntimeProfileDigest, State: harnessv2.RuntimeSessionStateIdle, ProviderSessionID: "provider",
					WorkspaceBaseline: request.NextBaseline, CreatedAt: now, LastTransitionAt: now,
				},
				Rebase: harnessv2.RuntimeSessionRebaseReceipt{
					WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID,
					TerminalReceiptDigest: request.TerminalReceiptDigest, PublishedManifestDigest: request.PublishedManifestDigest,
					PreviousTreeDigest: persistentTestDigest("previous-tree"), NextBaseline: request.NextBaseline, AppliedAt: now,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtimeClient, err := harnessv2.NewClient(server.URL,
		harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)),
		harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: profileDigest}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runtimeClient
}

func persistentGateFixture(t *testing.T, record *store.SessionResidentRuntime, objects ...client.Object) (*ACPDispatcher, *corev1alpha1.Task, harnessv2.Fence) {
	t.Helper()
	task := persistentTestTask()
	pool := persistentTestPool()
	workspace := persistentWarmWorkspace(task, record)
	objects = append([]client.Object{pool, workspace}, objects...)
	kubeClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithObjects(objects...).Build()
	// The dispatcher's runtime fence at the gate names the exact runtime but
	// no RuntimeSession yet: it gains the session only after the gates run.
	fence := harnessv2.Fence{
		RuntimeInstanceID: "instance-1", SupervisorBootID: "boot-1", ControllerEpoch: 1,
		RuntimePoolUID: acpDispatcherTestPoolUID, RuntimePoolGeneration: 1,
		RuntimeProfileDigest: harnessv2.ProfileDigest(record.ProfileDigest), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	return &ACPDispatcher{Client: kubeClient, APIReader: kubeClient}, task, fence
}

// quiescentStatus is the supervisor's status of the record's exact session.
func quiescentStatus(record *store.SessionResidentRuntime, fence harnessv2.Fence) harnessv2.RuntimeSessionStatus {
	identity := runtimePoolFenceWithSession(fence, harnessv2.RuntimeSessionUID(record.RuntimeSessionUID), uint64(record.RuntimeSessionGeneration))
	return harnessv2.RuntimeSessionStatus{
		RuntimeSessionID:  harnessv2.RuntimeSessionID(runtimeSessionID(identity)),
		RuntimeSessionUID: harnessv2.RuntimeSessionUID(record.RuntimeSessionUID), Generation: uint64(record.RuntimeSessionGeneration),
		State: harnessv2.RuntimeSessionStateIdle, Quiescent: true, LiveDescendantCount: 1, LastTransitionAt: persistentTestNow,
	}
}

func TestACPPersistentSessionRuntimeGateReasons(t *testing.T) {
	record := persistentTestRecord(t)
	profileDigest := harnessv2.ProfileDigest(record.ProfileDigest)
	ctx := context.Background()
	dispatcher, task, fence := persistentGateFixture(t, record)
	supervisor := &fakePersistentSupervisor{status: quiescentStatus(record, fence)}
	runtimeClient := supervisor.client(t, profileDigest)
	if reason, detail, err := dispatcher.acpPersistentSessionRuntimeGate(ctx, task, record, runtimeClient, fence, persistentTestFence(), persistentTestPool()); err != nil || reason != "" || detail != "" {
		t.Fatalf("matching runtime refused: %q (%q), %v", reason, detail, err)
	}
	if supervisor.headers.Get(harnessv2.RuntimeSessionRebaseHeader) != "1" {
		t.Fatal("the gate's status read must opt into the quiescent field")
	}
	for name, tc := range map[string]struct {
		status  func(*harnessv2.RuntimeSessionStatus)
		pool    func(*corev1alpha1.RuntimePool)
		reached func(*harnessv2.Fence)
		served  func(*harnessv2.Fence)
		record  func(*store.SessionResidentRuntime)
		want    string
		detail  string
	}{
		"session missing":   {status: func(s *harnessv2.RuntimeSessionStatus) { *s = harnessv2.RuntimeSessionStatus{} }, want: acpPersistentReasonRuntimeSessionMissing, detail: acpPersistentGateDetailSessionAbsent},
		"session recreated": {status: func(s *harnessv2.RuntimeSessionStatus) { s.Generation = 9 }, want: acpPersistentReasonRuntimeSessionMissing, detail: acpPersistentGateDetailGeneration},
		"another generation's session ID": {status: func(s *harnessv2.RuntimeSessionStatus) {
			s.RuntimeSessionID = harnessv2.RuntimeSessionID(runtimeSessionID(runtimePoolFenceWithSession(fence, s.RuntimeSessionUID, 3)))
		}, want: acpPersistentReasonRuntimeSessionMissing, detail: acpPersistentGateDetailSessionID},
		"another session's ID": {status: func(s *harnessv2.RuntimeSessionStatus) {
			s.RuntimeSessionID = harnessv2.RuntimeSessionID(runtimeSessionID(runtimePoolFenceWithSession(fence, "other-session-uid", s.Generation)))
		}, want: acpPersistentReasonRuntimeSessionMissing, detail: acpPersistentGateDetailSessionID},
		"the dispatcher's session-less ID": {status: func(s *harnessv2.RuntimeSessionStatus) {
			s.RuntimeSessionID = harnessv2.RuntimeSessionID(runtimeSessionID(fence))
		}, want: acpPersistentReasonRuntimeSessionMissing, detail: acpPersistentGateDetailSessionID},
		"supervisor reports another boot":     {served: func(f *harnessv2.Fence) { f.SupervisorBootID = "boot-2" }, want: acpPersistentReasonRuntimeReplaced, detail: "status-fence-supervisor_boot"},
		"supervisor reports another instance": {served: func(f *harnessv2.Fence) { f.RuntimeInstanceID = "instance-2" }, want: acpPersistentReasonRuntimeReplaced, detail: "status-fence-runtime_instance"},
		"supervisor reports another epoch":    {served: func(f *harnessv2.Fence) { f.ControllerEpoch = 2 }, want: acpPersistentReasonRuntimeReplaced, detail: "status-fence-controller_epoch"},
		"turn reached another boot":           {reached: func(f *harnessv2.Fence) { f.SupervisorBootID = "boot-2" }, want: acpPersistentReasonRuntimeReplaced, detail: acpPersistentGateDetailReachedBoot},
		"turn reached another instance":       {reached: func(f *harnessv2.Fence) { f.RuntimeInstanceID = "instance-2" }, want: acpPersistentReasonRuntimeReplaced, detail: acpPersistentGateDetailReachedInstance},
		"record names no session generation":  {record: func(r *store.SessionResidentRuntime) { r.RuntimeSessionGeneration = 0 }, want: acpPersistentReasonRuntimeReplaced, detail: acpPersistentGateDetailRecordIncomplete},
		"session busy":                        {status: func(s *harnessv2.RuntimeSessionStatus) { s.Quiescent = false }, want: acpPersistentReasonRuntimeSessionNotIdle, detail: acpPersistentGateDetailNotQuiescent},
		"session finalizing": {status: func(s *harnessv2.RuntimeSessionStatus) {
			s.State, s.Quiescent, s.ReservedForFinalization = harnessv2.RuntimeSessionStateFinalizing, false, true
		}, want: acpPersistentReasonRuntimeSessionNotIdle, detail: "state-finalizing"},
		"served record moved": {pool: func(p *corev1alpha1.RuntimePool) {
			p.Annotations[externalRuntimeServedAnnotation] = "sha256:spec 4 allocation-2 instance-1 rev-2 pod-uid boot-1"
		}, want: acpPersistentReasonProviderEpochChanged},
		"pool regenerated": {pool: func(p *corev1alpha1.RuntimePool) { p.Generation = 5 }, want: acpPersistentReasonPoolReplaced},
		"pool draining":    {pool: func(p *corev1alpha1.RuntimePool) { p.Status.AdmissionState = "Draining" }, want: acpPersistentReasonPoolNotAccepting},
	} {
		t.Run(name, func(t *testing.T) {
			pool := persistentTestPool()
			if tc.pool != nil {
				tc.pool(pool)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
				WithObjects(pool, persistentWarmWorkspace(task, record)).WithStatusSubresource(pool).Build()
			d := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient}
			status := quiescentStatus(record, fence)
			if tc.status != nil {
				tc.status(&status)
			}
			served, reached, claimed := fence, fence, store.CloneSessionResidentRuntime(record)
			if tc.served != nil {
				tc.served(&served)
			}
			if tc.reached != nil {
				tc.reached(&reached)
			}
			if tc.record != nil {
				tc.record(claimed)
			}
			client := (&fakePersistentSupervisor{status: status, fence: &served}).client(t, profileDigest)
			reason, detail, err := d.acpPersistentSessionRuntimeGate(ctx, task, claimed, client, reached, persistentTestFence(), persistentTestPool())
			if err != nil || reason != tc.want || detail != tc.detail {
				t.Fatalf("gate = %q (%q), %v; want %q (%q)", reason, detail, err, tc.want, tc.detail)
			}
		})
	}
}

func TestACPPersistentSessionHandshakeGateTimesOutIntoARequeueReason(t *testing.T) {
	record := persistentTestRecord(t)
	dispatcher, task, _ := persistentGateFixture(t, record)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// The provider never acknowledged the new epoch: the gate waits within the
	// pre-submission budget and then reports a timeout, never a prompt.
	if _, err := dispatcher.acpPersistentSessionHandshakeGate(ctx, task, persistentTestPool()); !errors.Is(err, errACPPersistentSessionHandshakeTimeout) {
		t.Fatalf("handshake gate = %v, want timeout", err)
	}
}

func persistentKeepInput(t *testing.T, record *store.SessionResidentRuntime) acpPersistentSessionKeepInput {
	t.Helper()
	task := persistentTestTask()
	task.UID = types.UID(record.SourceTaskUID)
	return acpPersistentSessionKeepInput{
		task: task, bound: &verifiedAgentExecution{plan: persistentTestPlan()}, target: acpDispatchTarget{pool: persistentTestPool()},
		fence: persistentTestFence(),
		session: &acpTaskSession{Bootstrap: &ACPBootstrapTranscript{}, Turn: &ACPSessionTurn{Turn: store.SessionTurn{
			ID: "turn-n", UserPrompt: "question n", Key: store.SessionTurnKey{SessionUID: record.RuntimeSessionUID, LeaseGeneration: 2, TaskUID: record.SourceTaskUID, Attempt: 1, PromptID: "prompt-n"},
		}}},
		attemptID: "attempt-n",
		attempt:   &store.PromptAttempt{ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryReadValidated},
		intent:    harnessv2.WorkspaceIntentRead,
		delta:     harnessv2.WorkspaceDeltaDescriptor{State: harnessv2.WorkspaceDeltaNoChange},
		mcpDigest: record.MCPDigest, lineageDigest: record.LineageConfigDigest, workspaceDigest: record.WorkspaceDigest,
		resultText: "answer n", rebaseSupported: true,
	}
}

func TestACPPersistentSessionKeepRefusalReasons(t *testing.T) {
	record := persistentTestRecord(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mutate func(*acpPersistentSessionKeepInput)
		want   string
	}{
		"scope":            {mutate: func(in *acpPersistentSessionKeepInput) { in.scopeRefusal = acpPersistentReasonClassDisabled }, want: acpPersistentReasonClassDisabled},
		"no session":       {mutate: func(in *acpPersistentSessionKeepInput) { in.session = nil }, want: acpPersistentReasonIneligibleScope},
		"native capture":   {mutate: func(in *acpPersistentSessionKeepInput) { in.nativeCapture = true }, want: acpPersistentReasonIneligibleScope},
		"parked":           {mutate: func(in *acpPersistentSessionKeepInput) { in.parked = true }, want: acpPersistentReasonPreviousParked},
		"skipped append":   {mutate: func(in *acpPersistentSessionKeepInput) { in.transcriptSkip = true }, want: acpPersistentReasonHistoryBoundary},
		"partial history":  {mutate: func(in *acpPersistentSessionKeepInput) { in.historyIncomplete = true }, want: acpPersistentReasonHistoryBoundary},
		"failed prompt":    {mutate: func(in *acpPersistentSessionKeepInput) { in.attempt.ExecutionState = store.PromptExecutionFailed }, want: acpPersistentReasonPreviousNotSucceeded},
		"modified read":    {mutate: func(in *acpPersistentSessionKeepInput) { in.delta.State = harnessv2.WorkspaceDeltaReadOnlyModified }, want: acpPersistentReasonPreviousNotSucceeded},
		"superseded write": {mutate: supersededWriteKeep, want: acpPersistentReasonPreviousNotSucceeded},
		"no workspace link": {mutate: func(in *acpPersistentSessionKeepInput) {
			delete(in.task.Labels, acpExecutionWorkspaceLinkLabel)
		}, want: acpPersistentReasonIneligibleScope},
	} {
		t.Run(name, func(t *testing.T) {
			input := persistentKeepInput(t, record)
			attempt := *input.attempt
			input.attempt = &attempt
			tc.mutate(&input)
			kept, reason, _ := (&ACPDispatcher{}).prepareACPPersistentSessionKeep(ctx, input)
			if kept != nil || reason != tc.want {
				t.Fatalf("keep = %v, %q; want refusal %q", kept != nil, reason, tc.want)
			}
		})
	}
}

func supersededWriteKeep(in *acpPersistentSessionKeepInput) {
	in.intent = harnessv2.WorkspaceIntentWrite
	in.delta.State = harnessv2.WorkspaceDeltaPrepared
	in.attempt.DeliveryState = store.PromptDeliveryDeliveredSuperseded
	in.publicationState = store.PublicationDeliveredSuperseded
}

func TestACPPersistentSessionKeepLiveRefusalReasonsAndRecord(t *testing.T) {
	record := persistentTestRecord(t)
	ctx := context.Background()
	warmWorkspace := func(task *corev1alpha1.Task) *workspacev1alpha1.ExecutionWorkspace {
		return &workspacev1alpha1.ExecutionWorkspace{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: persistentTestNamespace, Name: persistentTestWorkspace, UID: "workspace-uid",
				Annotations: map[string]string{
					acpWorkspaceSuspendAfterDetachAnnotation:      "5m",
					acpWorkspaceDetachActionAnnotation:            string(workspacev1alpha1.WorkspaceOnDetachSuspend),
					acpWorkspaceDurableSessionCommittedAnnotation: "1",
				},
			},
			Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
				DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
				SessionRef:   &workspacev1alpha1.ObjectIdentityReference{Name: "session", UID: "session-uid"},
				Attachment:   &workspacev1alpha1.ExecutionWorkspaceAttachment{TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: 3},
			},
		}
	}
	run := func(t *testing.T, mutateWorkspace func(*workspacev1alpha1.ExecutionWorkspace), mutatePool func(*corev1alpha1.RuntimePool), supervisor *fakePersistentSupervisor, mutateInput func(*acpPersistentSessionKeepInput)) (*store.SessionResidentRuntime, string) {
		t.Helper()
		input := persistentKeepInput(t, record)
		workspace := warmWorkspace(input.task)
		if mutateWorkspace != nil {
			mutateWorkspace(workspace)
		}
		pool := persistentTestPool()
		if mutatePool != nil {
			mutatePool(pool)
		}
		kubeClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithObjects(pool, workspace).Build()
		input.runtimeFence = harnessv2.Fence{
			RuntimeInstanceID: "instance-1", SupervisorBootID: "boot-1", ControllerEpoch: 7,
			RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
			RuntimeSessionUID: harnessv2.RuntimeSessionUID(record.RuntimeSessionUID), RuntimeSessionGeneration: 2,
			RuntimeProfileDigest: harnessv2.ProfileDigest(record.ProfileDigest), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
		}
		if supervisor.status.RuntimeSessionUID == "" && supervisor.status.Generation == 0 && supervisor.status.State == "" {
			supervisor.status = quiescentStatus(record, input.runtimeFence)
		}
		input.runtimeClient = supervisor.client(t, harnessv2.ProfileDigest(record.ProfileDigest))
		if mutateInput != nil {
			mutateInput(&input)
		}
		commit := strings.Repeat("b", 40)
		publications := &residentSupportStore{supported: true, publication: &store.Publication{
			ID: "publication-n", State: store.PublicationVerifiedExact, TargetRepositoryID: "github.com/orka-agents/orka",
			TargetRef: "refs/heads/orka/session", PreparedReceipt: &store.PreparedPublicationReceipt{CommitSHA: commit},
			VerificationReceipt: &store.PublicationVerificationReceipt{ObservedRemote: store.RemoteRefState{SHA: commit}},
		}}
		dispatcher := &ACPDispatcher{
			Client: kubeClient, APIReader: kubeClient, Store: publications,
			Sessions: &ACPSessionContinuity{transcripts: &historyTranscriptStore{messages: keepTranscript(input.session)}},
		}
		kept, reason, _ := dispatcher.prepareACPPersistentSessionKeep(ctx, input)
		return kept, reason
	}

	kept, reason := run(t, nil, nil, &fakePersistentSupervisor{}, nil)
	if kept == nil || reason != "" {
		t.Fatalf("eligible read turn was not kept: %q", reason)
	}
	if kept.State != store.SessionResidentRuntimeResident || kept.SourceTurnID != "turn-n" || !kept.ExpiresAt.After(kept.KeptAt) ||
		kept.TranscriptHeadDigest != persistentTestHead() || kept.RebasedTreeDigest != "" {
		t.Fatalf("kept record = %#v", kept)
	}

	// A verified-exact write turn is rebased, not retired: the record carries
	// the rebased identity turn N+1 must resolve.
	writer := &fakePersistentSupervisor{}
	rebasedRecord, reason := run(t, nil, nil, writer, verifiedExactWriteKeep)
	if rebasedRecord == nil || reason != "" || writer.rebased != 1 {
		t.Fatalf("verified-exact write turn: kept=%v reason=%q rebases=%d", rebasedRecord != nil, reason, writer.rebased)
	}
	if rebasedRecord.RebasedTreeDigest == "" || rebasedRecord.VerifiedBaseline == nil ||
		rebasedRecord.VerifiedBaseline.SHA != strings.Repeat("b", 40) || rebasedRecord.WorkspaceDigest == record.WorkspaceDigest {
		t.Fatalf("rebased record = %#v", rebasedRecord)
	}

	// A reused rebased session whose next write turn published nothing keeps
	// its derived identity for the turn after it.
	carried, reason := run(t, nil, nil, &fakePersistentSupervisor{}, func(in *acpPersistentSessionKeepInput) {
		claimed := store.CloneSessionResidentRuntime(rebasedRecord)
		in.session.ResidentClaimed = claimed
		in.session.Reused = true
		in.session.ClaimedTranscriptHeadID = "m-head"
		in.session.VerifiedBaseline = claimed.VerifiedBaseline
		in.intent = harnessv2.WorkspaceIntentWrite
		in.delta = harnessv2.WorkspaceDeltaDescriptor{State: harnessv2.WorkspaceDeltaNoChange}
		in.attempt.DeliveryState = store.PromptDeliveryNoChange
		in.baseline = harnessv2.WorkspaceBaseline{RepositoryIdentity: claimed.VerifiedBaseline.RepositoryID, Revision: claimed.VerifiedBaseline.SHA, TreeDigest: claimed.RebasedTreeDigest}
		in.workspaceDigest = claimed.WorkspaceDigest
	})
	if carried == nil || reason != "" || carried.RebasedTreeDigest != rebasedRecord.RebasedTreeDigest ||
		carried.WorkspaceDigest != rebasedRecord.WorkspaceDigest {
		t.Fatalf("no-change turn after a rebase dropped the rebased identity: %#v, %q", carried, reason)
	}

	for name, tc := range map[string]struct {
		workspace  func(*workspacev1alpha1.ExecutionWorkspace)
		pool       func(*corev1alpha1.RuntimePool)
		supervisor *fakePersistentSupervisor
		input      func(*acpPersistentSessionKeepInput)
		want       string
	}{
		"no warm window": {workspace: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceSuspendAfterDetachAnnotation)
		}, want: acpPersistentReasonWorkspaceNotWarm},
		"workspace replaced": {workspace: func(w *workspacev1alpha1.ExecutionWorkspace) { w.UID = "workspace-uid-2" }, want: acpPersistentReasonWorkspaceIncarnation},
		"pool draining":      {pool: func(p *corev1alpha1.RuntimePool) { p.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining }, want: acpPersistentReasonPoolNotAccepting},
		"no served record":   {pool: func(p *corev1alpha1.RuntimePool) { delete(p.Annotations, externalRuntimeServedAnnotation) }, want: acpPersistentReasonProviderEpochChanged},
		"session busy": {supervisor: &fakePersistentSupervisor{status: harnessv2.RuntimeSessionStatus{
			RuntimeSessionID: "x", RuntimeSessionUID: harnessv2.RuntimeSessionUID(record.RuntimeSessionUID), Generation: 2,
			State: harnessv2.RuntimeSessionStateIdle, LastTransitionAt: persistentTestNow,
		}}, want: acpPersistentReasonRuntimeSessionNotIdle},
		"rebase refused": {supervisor: &fakePersistentSupervisor{rebaseErr: true}, input: verifiedExactWriteKeep, want: acpPersistentReasonRebaseFailed},
	} {
		t.Run(name, func(t *testing.T) {
			supervisor := tc.supervisor
			if supervisor == nil {
				supervisor = &fakePersistentSupervisor{}
			}
			kept, reason := run(t, tc.workspace, tc.pool, supervisor, tc.input)
			if kept != nil || reason != tc.want {
				t.Fatalf("keep = %v, %q; want refusal %q", kept != nil, reason, tc.want)
			}
		})
	}
}

// verifiedExactWriteKeep is a write turn whose publication was verified
// exact, so its settlement rebases the session.
func verifiedExactWriteKeep(in *acpPersistentSessionKeepInput) {
	in.intent = harnessv2.WorkspaceIntentWrite
	in.baseline = harnessv2.WorkspaceBaseline{RepositoryIdentity: "github.com/orka-agents/orka", Revision: strings.Repeat("a", 40), TreeDigest: persistentTestDigest("tree-n")}
	in.delta = harnessv2.WorkspaceDeltaDescriptor{State: harnessv2.WorkspaceDeltaPrepared, DeltaID: "delta-n", ManifestDigest: persistentTestDigest("manifest")}
	in.attempt.DeliveryState = store.PromptDeliveryVerifiedExact
	in.publicationState = store.PublicationVerifiedExact
	in.publicationID = "publication-n"
	in.finalization = &runtimeSessionPublicationFinalization{TerminalReceiptDigest: persistentTestDigest("receipt")}
}

// No per-turn authority survives a turn boundary: the record holds no
// credential, token or authorization, and the claim input is rebuilt from
// the new turn's own frozen snapshot.
func TestACPPersistentSessionRecordCarriesNoPerTurnAuthority(t *testing.T) {
	// Lease and attempt fields are identities of the source turn, not authority.
	forbidden := []string{"token", "secret", "credential", "password", "bearer", "authorization", "capability", "approval", "permission"}
	typ := reflect.TypeFor[store.SessionResidentRuntime]()
	for field := range typ.Fields() {
		name := strings.ToLower(field.Name)
		for _, word := range forbidden {
			if strings.Contains(name, word) {
				t.Fatalf("resident runtime field %s could carry per-turn authority", field.Name)
			}
		}
	}
	record := persistentTestRecord(t)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"txn", "Txn-Token", "Bearer"} {
		if strings.Contains(string(encoded), word) {
			t.Fatalf("encoded record mentions %q", word)
		}
	}
	// The reuse digest never includes a transaction context: a
	// transaction-bound Task is refused before any digest is computed.
	task := persistentTestTask()
	task.Spec.Transaction = &corev1alpha1.TaskTransaction{}
	if acpPersistentSessionStaticScope(true, task, persistentTestPlan(), false) != acpPersistentReasonIneligibleScope {
		t.Fatal("a transaction-bound Task must never reuse a RuntimeSession")
	}
}

func TestACPPersistentSessionReuseLogIsBoundedAndLoggedOnce(t *testing.T) {
	reuse := &acpPersistentSessionReuse{attempted: true}
	reuse.miss(acpPersistentSessionStageClaim, "not-a-reason")
	if reuse.reason != acpPersistentReasonIneligibleScope {
		t.Fatalf("unbounded reason %q leaked into the reuse decision", reuse.reason)
	}
	task := persistentTestTask()
	reuse.log(context.Background(), task)
	reuse.miss(acpPersistentSessionStageGate, acpPersistentReasonHandshakeTimeout)
	if reuse.reason == acpPersistentReasonHandshakeTimeout {
		t.Fatal("a logged decision must not change")
	}
	var nilReuse *acpPersistentSessionReuse
	nilReuse.miss(acpPersistentSessionStageClaim, acpPersistentReasonNoRecord)
	nilReuse.log(context.Background(), task)
}

func TestACPPersistentSessionHeldWorkspaceRequiresEveryQueueCondition(t *testing.T) {
	ctx := context.Background()
	record := persistentTestRecord(t)
	task := persistentTestTask()
	task.UID = "task-n1-uid"
	plan := persistentTestPlan()
	name := acpClassWorkspaceName(task, plan.Workspace)
	holder := string(task.UID)
	leaseObject := func() *coordinationv1.Lease {
		ttl := int32(300)
		now := metav1.NewMicroTime(time.Now())
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: persistentTestNamespace, Name: attachmentLeaseName(name),
				Annotations: map[string]string{workspaceAttachmentLeaseEpochAnnotation: "3"}},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &ttl, RenewTime: &now},
		}
	}
	workspaceObject := func() *workspacev1alpha1.ExecutionWorkspace {
		workspace := persistentWarmWorkspace(task, record)
		workspace.Name = name
		workspace.Spec.Attachment.ExpiresAt = metav1.NewTime(time.Now().Add(time.Hour))
		return workspace
	}
	controlRecord := *record
	controlRecord.ExpiresAt = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	controlRecord.KeptAt = time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	kept, err := store.NewSessionResidentRuntime(controlRecord)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutateWorkspace func(*workspacev1alpha1.ExecutionWorkspace)
		mutateLease     func(*coordinationv1.Lease)
		record          *store.SessionResidentRuntime
		enabled         bool
		wantHeld        bool
	}{
		"held":            {enabled: true, record: &kept, wantHeld: true},
		"flag disabled":   {record: &kept},
		"no record":       {enabled: true},
		"lease elsewhere": {enabled: true, record: &kept, mutateLease: func(l *coordinationv1.Lease) { other := "rival"; l.Spec.HolderIdentity = &other }},
		"lease old epoch": {enabled: true, record: &kept, mutateLease: func(l *coordinationv1.Lease) { l.Annotations[workspaceAttachmentLeaseEpochAnnotation] = "2" }},
		"being revoked": {enabled: true, record: &kept, mutateWorkspace: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceRevocationStartedAnnotation] = "3 " + time.Now().UTC().Format(time.RFC3339Nano)
		}},
		"no evidence": {enabled: true, record: &kept, mutateWorkspace: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceAttachedFromWarmAnnotation)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			workspace := workspaceObject()
			if tc.mutateWorkspace != nil {
				tc.mutateWorkspace(workspace)
			}
			lease := leaseObject()
			if tc.mutateLease != nil {
				tc.mutateLease(lease)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithObjects(workspace, lease).Build()
			reconciler := &TaskReconciler{
				Client: kubeClient, APIReader: kubeClient, ACPPersistentSessionEnabled: tc.enabled,
				DurableControlStore:    &heldQueueControlStore{residentSupportStore: residentSupportStore{supported: true}, record: tc.record},
				ControllerEpochManager: staticPersistentEpochManager(t),
			}
			_, handoff, err := reconciler.acpPersistentSessionHeldWorkspace(ctx, task, plan, false)
			if err != nil {
				t.Fatal(err)
			}
			if (handoff != "") != tc.wantHeld {
				t.Fatalf("held = %v, want %v", handoff != "", tc.wantHeld)
			}
			if tc.wantHeld && handoff != kept.HandoffID {
				t.Fatalf("handoff = %s, want %s", handoff, kept.HandoffID)
			}
		})
	}
}

type heldQueueControlStore struct {
	residentSupportStore
	record *store.SessionResidentRuntime
}

func (s *heldQueueControlStore) GetSessionControl(context.Context, string, string) (*store.SessionControl, error) {
	return &store.SessionControl{ResidentRuntime: s.record}, nil
}

func staticPersistentEpochManager(t *testing.T) *ControllerEpochManager {
	t.Helper()
	ready := make(chan struct{})
	close(ready)
	fence := persistentTestFence()
	return &ControllerEpochManager{ready: ready, current: &store.ControllerEpoch{Name: fence.Name, Epoch: fence.Epoch, HolderID: fence.HolderID}}
}

func TestACPPersistentSessionProcessHoldsFullHistory(t *testing.T) {
	task := persistentTestTask()
	full := &ACPBootstrapTranscript{MessageCount: 4, TotalMessages: 4}
	for name, tc := range map[string]struct {
		session *acpTaskSession
		mutate  func(*corev1alpha1.Task)
		want    bool
	}{
		"complete bootstrap":       {session: &acpTaskSession{Bootstrap: full}, want: true},
		"claimed reuse":            {session: &acpTaskSession{Reused: true, ResidentClaimed: &store.SessionResidentRuntime{}}, want: true},
		"cache reuse":              {session: &acpTaskSession{Reused: true}},
		"no bootstrap":             {session: &acpTaskSession{}},
		"byte truncated":           {session: &acpTaskSession{Bootstrap: &ACPBootstrapTranscript{MessageCount: 4, TotalMessages: 4, Truncated: true}}},
		"messages dropped":         {session: &acpTaskSession{Bootstrap: &ACPBootstrapTranscript{MessageCount: 3, TotalMessages: 4}}},
		"limit reached":            {session: &acpTaskSession{Bootstrap: full}, mutate: func(t *corev1alpha1.Task) { t.Spec.SessionRef.MaxMessages = 4 }},
		"earlier cutoff":           {session: &acpTaskSession{Bootstrap: full}, mutate: func(t *corev1alpha1.Task) { t.Spec.SessionRef.ThroughMessageID = "m2" }},
		"prompt-included boundary": {session: &acpTaskSession{Bootstrap: full}, mutate: func(t *corev1alpha1.Task) { t.Spec.SessionRef.PromptIncluded = true }},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := task.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(candidate)
			}
			if got := acpPersistentSessionProcessHoldsFullHistory(candidate, tc.session); got != tc.want {
				t.Fatalf("full history = %v, want %v", got, tc.want)
			}
		})
	}
}

// keepTranscript is the canonical transcript a keep test's process observed.
func keepTranscript(session *acpTaskSession) []store.SessionMessage {
	if session != nil && session.Reused && session.ClaimedTranscriptHeadID != "" {
		return []store.SessionMessage{{ID: session.ClaimedTranscriptHeadID, Role: acpSessionTurnAssistantRole, Content: "earlier"}}
	}
	return nil
}

func TestACPPersistentSessionKeepRefusesAnAppendTheProcessNeverSaw(t *testing.T) {
	record := persistentTestRecord(t)
	input := persistentKeepInput(t, record)
	dispatcher := &ACPDispatcher{Sessions: &ACPSessionContinuity{transcripts: &historyTranscriptStore{
		messages: []store.SessionMessage{{ID: "oob", Role: "user", Content: "appended during the turn"}},
	}}}
	// The process was bootstrapped from an empty transcript.
	if _, err := dispatcher.acpPersistentSessionKeepTranscriptHead(context.Background(), input); err == nil {
		t.Fatal("a message appended after the bootstrap must refuse the keep")
	}
	input.session.Bootstrap = &ACPBootstrapTranscript{LastMessageID: "oob"}
	if head, err := dispatcher.acpPersistentSessionKeepTranscriptHead(context.Background(), input); err != nil || head == "" {
		t.Fatalf("an unchanged head = %q, %v", head, err)
	}
	input.session.Reused = true
	if _, err := dispatcher.acpPersistentSessionKeepTranscriptHead(context.Background(), input); err == nil {
		t.Fatal("a reused process without a claimed head must refuse the keep")
	}
}

// A turn reuses only the exact session the gate proved: a planned binding
// that names another generation, instance or boot than the claimed record
// falls back to a fresh session even when the runtime gate passes.
func TestACPPersistentSessionGatesRefuseABindingOtherThanTheClaimedRecord(t *testing.T) {
	record := persistentTestRecord(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*ACPRuntimeSessionBinding){
		"generation": func(b *ACPRuntimeSessionBinding) { b.Generation++ },
		"session":    func(b *ACPRuntimeSessionBinding) { b.SessionUID = "other-session-uid" },
		"instance":   func(b *ACPRuntimeSessionBinding) { b.RuntimeInstanceID = "instance-2" },
		"boot":       func(b *ACPRuntimeSessionBinding) { b.SupervisorBootID = "boot-2" },
	} {
		t.Run(name, func(t *testing.T) {
			dispatcher, task, fence := persistentGateFixture(t, record)
			runtimeClient := (&fakePersistentSupervisor{status: quiescentStatus(record, fence), fence: &fence}).client(t, harnessv2.ProfileDigest(record.ProfileDigest))
			binding := acpRuntimeSessionBindingFromResidentRuntime(record)
			mutate(&binding)
			session := &acpTaskSession{Binding: binding, Reused: true, ResidentClaimed: store.CloneSessionResidentRuntime(record), LeaseGeneration: 3}
			reuse := &acpPersistentSessionReuse{attempted: true}
			requeued, err := dispatcher.runACPPersistentSessionGates(ctx, ctx, task, "attempt", persistentTestFence(),
				acpDispatchTarget{pool: persistentTestPool()}, runtimeClient, fence, session, false, reuse, nil)
			require.NoError(t, err)
			require.False(t, requeued)
			require.False(t, reuse.hit)
			require.Equal(t, acpPersistentReasonRuntimeSessionMissing, reuse.reason)
			require.False(t, session.Reused, "the turn must plan a fresh session")
			require.Nil(t, session.ResidentClaimed)
		})
	}
	dispatcher, task, fence := persistentGateFixture(t, record)
	runtimeClient := (&fakePersistentSupervisor{status: quiescentStatus(record, fence), fence: &fence}).client(t, harnessv2.ProfileDigest(record.ProfileDigest))
	session := &acpTaskSession{Binding: acpRuntimeSessionBindingFromResidentRuntime(record), Reused: true, ResidentClaimed: store.CloneSessionResidentRuntime(record)}
	reuse := &acpPersistentSessionReuse{attempted: true}
	requeued, err := dispatcher.runACPPersistentSessionGates(ctx, ctx, task, "attempt", persistentTestFence(),
		acpDispatchTarget{pool: persistentTestPool()}, runtimeClient, fence, session, false, reuse, nil)
	require.NoError(t, err)
	require.False(t, requeued)
	require.True(t, reuse.hit, "the exact claimed binding reuses: %s", reuse.reason)
	require.True(t, session.Reused)
}

// The keep marker exempts a write Session Task's runtime from task-scoped
// cleanup only for the exact attempt and RuntimeSession incarnation it names,
// and only while the Task is not being deleted.
func TestACPPersistentSessionKeptMarkerNamesOneExactRuntime(t *testing.T) {
	kept := func() *corev1alpha1.Task {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "write-turn", UID: "write-turn-uid", Annotations: map[string]string{}},
			Spec: corev1alpha1.TaskSpec{
				SessionRef: &corev1alpha1.SessionReference{Name: "session"},
				Workspace:  &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
			},
			Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Attempt: 1, RuntimeInstanceID: "instance-1",
				RuntimeSessionSupervisorBootID: "boot-1", RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 2,
			}},
		}
		marker, err := acpPersistentSessionKeptDigest(task.UID, task.Status.Execution)
		require.NoError(t, err)
		task.Annotations[acpTaskPersistentSessionKeptAnnotation] = marker
		return task
	}
	task := kept()
	require.True(t, acpTaskWriteRuntimeSessionKept(task, task.UID))
	require.True(t, taskScopedRuntimeSessionCleanupComplete(task), "the Session owns a kept write runtime")
	for name, mutate := range map[string]func(*corev1alpha1.Task){
		"no marker":          func(task *corev1alpha1.Task) { delete(task.Annotations, acpTaskPersistentSessionKeptAnnotation) },
		"another attempt":    func(task *corev1alpha1.Task) { task.Status.Execution.Attempt = 2 },
		"another generation": func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionGeneration = 3 },
		"another instance":   func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeInstanceID = "instance-2" },
		"another boot":       func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionSupervisorBootID = "boot-2" },
		"recreation pending": func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionRecreationPending = true },
		"outcome unknown": func(task *corev1alpha1.Task) {
			task.Status.Execution.State = corev1alpha1.TaskExecutionStateOutcomeUnknown
		},
		"deleting":              func(task *corev1alpha1.Task) { task.DeletionTimestamp = new(metav1.Now()) },
		"standalone write Task": func(task *corev1alpha1.Task) { task.Spec.SessionRef = nil },
	} {
		t.Run(name, func(t *testing.T) {
			task := kept()
			mutate(task)
			require.False(t, taskScopedRuntimeSessionCleanupComplete(task), "the Task must still retire its runtime")
		})
	}
	// A read Session Task never needs the marker.
	read := kept()
	read.Spec.Workspace.Intent = corev1alpha1.WorkspaceIntentRead
	delete(read.Annotations, acpTaskPersistentSessionKeptAnnotation)
	require.True(t, acpTaskSessionOwnsRuntimeSession(read, read.UID))
	require.False(t, acpTaskWriteRuntimeSessionKept(read, read.UID))
}

// The keep marker survives a stale cached Task and a concurrent Task write:
// the stamp reads uncached and retries a conflicting patch, rechecking the
// exact RuntimeSession identity each time (round 10b: private write turns
// lost their kept session to a single 409). A changed identity still refuses.
func TestACPPersistentSessionKeepMarkerRetriesAConflictingPatch(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "write-turn", UID: "write-turn-uid"},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: "session"},
			Workspace:  &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
		},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, RuntimeInstanceID: "instance-1",
			RuntimeSessionSupervisorBootID: "boot-1", RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 2,
		}},
	}
	fence := harnessv2.Fence{
		RuntimeInstanceID: "instance-1", SupervisorBootID: "boot-1",
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 2,
	}
	build := func(conflicts int32) (client.Client, *atomic.Int32) {
		base := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
			WithObjects(task.DeepCopy()).WithStatusSubresource(&corev1alpha1.Task{}).Build()
		var patches atomic.Int32
		return interceptor.NewClient(base, interceptor.Funcs{
			Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if patches.Add(1) <= conflicts {
					// Another writer lands between the read and the patch.
					return apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), object.GetName(), errors.New("modified"))
				}
				return delegate.Patch(ctx, object, patch, options...)
			},
		}), &patches
	}

	kubeClient, patches := build(2)
	stored := &corev1alpha1.Task{}
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(task), stored))
	dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient}
	stale := stored.DeepCopy()
	require.NoError(t, dispatcher.stampACPPersistentSessionKept(t.Context(), stale, fence))
	require.EqualValues(t, 3, patches.Load(), "two conflicts, then the marker lands")
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(task), stored))
	require.True(t, acpTaskWriteRuntimeSessionKept(stored, stored.UID))
	require.Equal(t, stored.Annotations[acpTaskPersistentSessionKeptAnnotation], stale.Annotations[acpTaskPersistentSessionKeptAnnotation])

	// A different RuntimeSession on the fresh read is never marked.
	kubeClient, _ = build(0)
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(task), stored))
	dispatcher = &ACPDispatcher{Client: kubeClient, APIReader: kubeClient}
	other := fence
	other.RuntimeSessionGeneration = 3
	require.ErrorIs(t, dispatcher.stampACPPersistentSessionKept(t.Context(), stored.DeepCopy(), other), store.ErrConflict)
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(task), stored))
	require.Empty(t, stored.Annotations[acpTaskPersistentSessionKeptAnnotation])
}
