package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/opencodestate"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/types"
)

type nativeSnapshotTestStore struct {
	snapshot *store.NativeSessionSnapshot
}

func (s *nativeSnapshotTestStore) GetNativeSessionSnapshot(context.Context, string, string, string) (*store.NativeSessionSnapshot, error) {
	if s.snapshot == nil {
		return nil, store.ErrNotFound
	}
	return s.snapshot, nil
}

func (*nativeSnapshotTestStore) StageNativeSessionSnapshot(context.Context, store.NativeSessionSnapshot, string) error {
	return nil
}

type nativeControllerFixture struct {
	dispatcher  *ACPDispatcher
	store       *sqlite.Store
	task        *corev1alpha1.Task
	preparation *acpTaskSessionPreparation
	lineage     acpSessionLineageIdentity
	snapshots   *nativeSnapshotTestStore
}

func newNativeControllerFixture(t *testing.T) nativeControllerFixture {
	t.Helper()
	s, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "native.db"))
	t.Cleanup(closeStore)
	continuity := newACPSessionTestContinuity(t, s, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "native-session")
	appendBootstrapHistoryForTest(t, s, control)
	task := newBootstrapSessionTaskForTest(t, s, fence, control, "native-task", types.UID("native-task-uid"))
	control.LeaseGeneration = 1
	profile := harnessv2.ProfileDigest(acpSessionTestDigest("profile"))
	preparation := &acpTaskSessionPreparation{control: control,
		plan: ACPRuntimeSessionPlan{Binding: ACPRuntimeSessionBinding{SessionUID: control.SessionUID,
			Generation: 2, ProfileDigest: profile, WorkspaceDigest: acpSessionTestDigest("workspace")}}}
	snapshots := &nativeSnapshotTestStore{}
	dispatcher := &ACPDispatcher{Store: s, Sessions: continuity, NativeSessions: snapshots}
	lineage := acpSessionLineageIdentity{NamespaceUID: "native-namespace-uid", ConfigDigest: acpSessionTestDigest("lineage"),
		Native: &acpNativeSessionPolicy{WorkspaceBindingDigest: acpSessionTestDigest("workspace-binding"), MCPDigest: acpSessionTestDigest("mcp")}}
	native, err := dispatcher.planNativeSession(context.Background(), task, preparation, lineage)
	if err != nil {
		t.Fatal(err)
	}
	snapshots.snapshot = &store.NativeSessionSnapshot{ID: "native-snapshot", SessionUID: control.SessionUID,
		NamespaceUID: lineage.NamespaceUID, Key: store.SessionTurnKey{LeaseGeneration: 1}, ProviderSessionID: "provider-native-id",
		ProviderVersion: opencodestate.ProviderVersion, ProfileDigest: string(profile), ConfigurationDigest: native.ConfigurationDigest,
		WorkspaceBindingDigest: lineage.Native.WorkspaceBindingDigest, WorkspaceDigest: preparation.plan.Binding.WorkspaceDigest,
		HistoryDigest: native.HistoryDigest, HistoryMessageCount: 2, ThroughMessageID: "prior-assistant"}
	return nativeControllerFixture{dispatcher, s, task, preparation, lineage, snapshots}
}

func TestNativeSessionHistoryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(nativeControllerFixture)
		restore bool
		capture bool
	}{
		{name: "complete history", restore: true, capture: true},
		{name: "message limit", mutate: func(f nativeControllerFixture) { f.task.Spec.SessionRef.MaxMessages = 1 }},
		{name: "cutoff excludes newer messages", mutate: func(f nativeControllerFixture) { f.task.Spec.SessionRef.ThroughMessageID = "prior-user" }},
		{name: "matching cutoff", mutate: func(f nativeControllerFixture) { f.task.Spec.SessionRef.ThroughMessageID = "prior-assistant" }, restore: true, capture: true},
		{name: "changed settings", mutate: func(f nativeControllerFixture) { f.lineage.Native.MCPDigest = acpSessionTestDigest("changed-mcp") }, capture: true},
		{name: "other namespace", mutate: func(f nativeControllerFixture) { f.snapshots.snapshot.NamespaceUID = "other-namespace" }, capture: true},
		{name: "older turn after append false", mutate: func(f nativeControllerFixture) { f.preparation.control.LeaseGeneration++ }, capture: true},
		{name: "same-turn retry", mutate: func(f nativeControllerFixture) {
			f.preparation.control.LeaseGeneration++
			f.preparation.control.Lease = &store.SessionMutationLease{TaskUID: string(f.task.UID), Attempt: 1, PromptID: f.task.Status.Execution.PromptID}
		}, restore: true, capture: true},
		{name: "oversized history message", mutate: func(f nativeControllerFixture) {
			if err := f.store.AppendMessages(context.Background(), f.task.Namespace, f.task.Spec.SessionRef.Name,
				[]store.SessionMessage{{ID: "oversized", Role: "assistant", Content: strings.Repeat("x", DefaultACPBootstrapMaxMessageBytes+1), Timestamp: time.Now().UTC()}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeControllerFixture(t)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			native, err := f.dispatcher.planNativeSession(context.Background(), f.task, f.preparation, f.lineage)
			if err != nil {
				t.Fatal(err)
			}
			if (native.Snapshot != nil) != tc.restore || native.Capture != tc.capture {
				t.Fatalf("restore=%v capture=%v reason=%s", native.Snapshot != nil, native.Capture, native.Reason)
			}
		})
	}
}

func TestNativeSessionPromptIncludedIsExcludedFromRestoredPrefix(t *testing.T) {
	f := newNativeControllerFixture(t)
	if err := f.store.AppendMessages(context.Background(), f.task.Namespace, f.task.Spec.SessionRef.Name,
		[]store.SessionMessage{{ID: "current-user", Role: "user", Content: "current canonical request", Timestamp: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	f.task.Spec.SessionRef.PromptIncluded = true
	f.task.Spec.SessionRef.ThroughMessageID = "current-user"
	native, err := f.dispatcher.planNativeSession(context.Background(), f.task, f.preparation, f.lineage)
	if err != nil {
		t.Fatal(err)
	}
	if native.Snapshot == nil || !native.Capture || native.HistoryBeforeCount != 3 ||
		f.preparation.userPrompt != "current canonical request" || strings.Contains(bootstrapPromptText(f.preparation.bootstrap), "current canonical request") {
		t.Fatal("promptIncluded did not keep the current request separate from prior native history")
	}
	f.task.Spec.SessionRef.MaxMessages = 2
	native, err = f.dispatcher.planNativeSession(context.Background(), f.task, f.preparation, f.lineage)
	if err != nil || native.Snapshot != nil || native.Capture {
		t.Fatal("promptIncluded widened the message limit")
	}
}

func TestNativeSessionWarmTaintAndInterruptedCreate(t *testing.T) {
	f := newNativeControllerFixture(t)
	native, err := f.dispatcher.planNativeSession(context.Background(), f.task, f.preparation, f.lineage)
	if err != nil {
		t.Fatal(err)
	}
	session := &acpTaskSession{Binding: f.preparation.plan.Binding, Bootstrap: f.preparation.bootstrap, Native: native, LeaseGeneration: 2}
	warm := harnessv2.RuntimeSessionStatus{ProviderSessionID: f.snapshots.snapshot.ProviderSessionID, CreationTaskUID: "earlier-task", CreationTaskAttempt: 1}
	if !session.adoptNativeSession(f.task, warm) || session.Bootstrap != nil {
		t.Fatal("complete live conversation was not reused without bootstrap")
	}
	// A turn that did not save history advances the lease but leaves the older
	// snapshot. Even the same provider process must now be retired.
	f.preparation.control.LeaseGeneration++
	session.Native, err = f.dispatcher.planNativeSession(context.Background(), f.task, f.preparation, f.lineage)
	if err != nil {
		t.Fatal(err)
	}
	session.Bootstrap = f.preparation.bootstrap
	if session.adoptNativeSession(f.task, warm) {
		t.Fatal("append=false exchange could leak into a later native save")
	}
	warm.CreationTaskUID = harnessv2.TaskUID(f.task.UID)
	if !session.adoptNativeSession(f.task, warm) || session.Bootstrap == nil {
		t.Fatal("fresh same-turn reconstruction lost its canonical bootstrap during adoption")
	}
	// A lost create response must be recovered from its authenticated result,
	// never inferred from the mere presence of a runtime session.
	warm.NativeRestoration = &harnessv2.NativeSessionRestoration{SnapshotID: "native-snapshot", Method: "session/load"}
	if session.adoptNativeSession(f.task, warm) {
		t.Fatal("restored create was adopted with an incompatible history prefix")
	}
	session.Native.Snapshot = f.snapshots.snapshot
	if !session.adoptNativeSession(f.task, warm) || session.Bootstrap != nil || session.Native.Method != "session/load" {
		t.Fatal("proven native create adoption would duplicate transcript history")
	}
}

func TestNativeSessionSelectionIsOptIn(t *testing.T) {
	f := newNativeControllerFixture(t)
	f.dispatcher.Sessions.lineages = f.store
	f.dispatcher.NativeSessionWorkspaceClass = "opencode-durable"
	plan := ACPRuntimePlan{Profile: harnessv2.RuntimeProfile{ProviderKind: acpNativeProviderKind}, Workspace: &ACPRuntimeWorkspaceBinding{
		Provider: corev1alpha1.WorkspaceProviderAgentSandbox, ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
		Class: &ACPWorkspaceClassBinding{Name: "opencode-durable", SuspendMode: "DataOnly", SandboxVolume: &ACPSandboxDurableVolume{}},
	}}
	caps := harnessv2.CapabilitiesResponse{SupportsNativeSessionRestore: true}
	if f.dispatcher.nativeSessionPolicy(f.task, plan, caps, "") == nil {
		t.Fatal("eligible class was not selected")
	}
	f.task.Spec.SessionRef.Append = false
	if f.dispatcher.nativeSessionPolicy(f.task, plan, caps, "") != nil {
		t.Fatal("append=false enabled native restore or capture")
	}
	f.task.Spec.SessionRef.Append = true
	for _, provider := range []string{"codex", "claude", "copilot"} {
		plan.Profile.ProviderKind = provider
		if f.dispatcher.nativeSessionPolicy(f.task, plan, caps, "") != nil {
			t.Fatalf("unvalidated provider %s was enabled", provider)
		}
	}
}
