package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
)

func TestTaskSecurityIDsUseExactEnvironmentBindings(t *testing.T) {
	findingID := "fnd_" + strings.Repeat("a", 64)
	occurrenceID := "occ_" + strings.Repeat("b", 64)
	valid := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			labels.LabelSecurityFindingID:    labels.SelectorValue(findingID),
			labels.LabelSecurityOccurrenceID: labels.SelectorValue(occurrenceID),
		}},
		Spec: corev1alpha1.TaskSpec{Env: []corev1.EnvVar{
			{Name: security.EnvFindingID, Value: findingID},
			{Name: security.EnvOccurrenceID, Value: occurrenceID},
		}},
	}
	gotFinding, err := taskSecurityFindingID(valid)
	if err != nil || gotFinding != findingID {
		t.Fatalf("taskSecurityFindingID() = (%q, %v), want exact ID", gotFinding, err)
	}
	gotOccurrence, err := taskSecurityOccurrenceID(valid)
	if err != nil || gotOccurrence != occurrenceID {
		t.Fatalf("taskSecurityOccurrenceID() = (%q, %v), want exact ID", gotOccurrence, err)
	}
	kind, scopeID, err := securityTaskScope(valid)
	if err != nil || kind != "occurrence" || scopeID != occurrenceID {
		t.Fatalf("securityTaskScope() = (%q, %q, %v)", kind, scopeID, err)
	}

	legacy := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		labels.LabelSecurityFindingID: "legacy-finding",
	}}}
	if got, err := taskSecurityFindingID(legacy); err != nil || got != "legacy-finding" {
		t.Fatalf("legacy taskSecurityFindingID() = (%q, %v)", got, err)
	}

	tests := []struct {
		name   string
		mutate func(*corev1alpha1.Task)
	}{
		{name: "mismatched label", mutate: func(task *corev1alpha1.Task) {
			task.Labels[labels.LabelSecurityFindingID] = labels.SelectorValue("fnd_" + strings.Repeat("c", 64))
		}},
		{name: "duplicate environment", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: security.EnvFindingID, Value: findingID})
		}},
		{name: "value from environment", mutate: func(task *corev1alpha1.Task) {
			for i := range task.Spec.Env {
				if task.Spec.Env[i].Name == security.EnvFindingID {
					task.Spec.Env[i].Value = ""
					task.Spec.Env[i].ValueFrom = &corev1.EnvVarSource{}
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := valid.DeepCopy()
			tt.mutate(task)
			if _, err := taskSecurityFindingID(task); err == nil {
				t.Fatal("taskSecurityFindingID() error = nil, want fail-closed rejection")
			}
		})
	}
}

func TestCreateOrValidateSecurityTaskRejectsForgedExistingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid")},
	}
	expected := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "security-task", Namespace: "ns", Labels: map[string]string{
			labels.LabelSecurityTarget: "repo", labels.LabelSecurityScanID: "scan-1", labels.LabelSecurityStage: "mapper",
		}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Command: []string{"--security-mapper"}},
	}
	if err := controllerutil.SetControllerReference(scan, expected, scheme); err != nil {
		t.Fatal(err)
	}
	forged := expected.DeepCopy()
	forged.OwnerReferences = nil
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, forged).Build()
	reconciler := &RepositoryScanReconciler{Client: client, Scheme: scheme}

	err := reconciler.createOrValidateSecurityTask(context.Background(), scan, expected.DeepCopy())
	if err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("createOrValidateSecurityTask() error = %v, want owner rejection", err)
	}
}

func TestCreateOrValidateSecurityTaskRejectsUnexpectedProtectedMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1alpha1.Task)
	}{
		{
			name: "harness attempt annotation",
			mutate: func(task *corev1alpha1.Task) {
				if task.Annotations == nil {
					task.Annotations = map[string]string{}
				}
				task.Annotations["orka.ai/harness-wrapper-attempt"] = "9"
			},
		},
		{
			name: "extra security label",
			mutate: func(task *corev1alpha1.Task) {
				task.Labels[labels.LabelSecurityOccurrenceID] = "occ_forged"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid")},
			}
			expected := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "security-task", Namespace: "ns", Labels: map[string]string{
					labels.LabelSecurityTarget: "repo", labels.LabelSecurityScanID: "scan-1", labels.LabelSecurityStage: "mapper",
				}},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Command: []string{"--security-mapper"}},
			}
			if err := controllerutil.SetControllerReference(scan, expected, scheme); err != nil {
				t.Fatal(err)
			}
			existing := expected.DeepCopy()
			tt.mutate(existing)
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, existing).Build()
			reconciler := &RepositoryScanReconciler{Client: client, Scheme: scheme}
			err := reconciler.createOrValidateSecurityTask(context.Background(), scan, expected.DeepCopy())
			if err == nil || !strings.Contains(err.Error(), "unexpected protected") {
				t.Fatalf("createOrValidateSecurityTask() error = %v, want protected metadata rejection", err)
			}
		})
	}
}

func TestImmutableRunThreatModelDoesNotUseMutableLatestModel(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	run := &store.ScanRun{
		ID: "scan-1", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", TaskName: "task", Mode: "manual", Phase: "running",
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	if err := s.CreateScanRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSecurityRunThreatModel(context.Background(), &store.SecurityRunThreatModel{
		RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID,
		Version: 1, Content: "immutable model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveThreatModel(context.Background(), &store.ThreatModel{
		Namespace: "ns", RepositoryScan: "repo", Content: "later edited model", Source: "edited",
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := &RepositoryScanReconciler{SecurityStore: s, RunThreatModelStore: s}
	got, err := reconciler.immutableRunThreatModel(context.Background(), run)
	if err != nil {
		t.Fatalf("immutableRunThreatModel() error = %v", err)
	}
	if got != "immutable model" {
		t.Fatalf("immutableRunThreatModel() = %q, want immutable model", got)
	}
}

func TestImmutableRunThreatModelRejectsMalformedNonEmptyRunUID(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	run := &store.ScanRun{
		ID: "scan-malformed", RunUID: "legacy-but-nonempty",
		Namespace: "ns", RepositoryScan: "repo", TaskName: "task", Mode: "manual", Phase: "running",
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	if err := s.SaveThreatModel(context.Background(), &store.ThreatModel{
		Namespace: "ns", RepositoryScan: "repo", Content: "mutable fallback", Source: "generated",
		GeneratedByScan: run.ID,
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := &RepositoryScanReconciler{SecurityStore: s, RunThreatModelStore: s}
	got, err := reconciler.immutableRunThreatModel(context.Background(), run)
	if err == nil || !strings.Contains(err.Error(), "valid non-empty run UID") {
		t.Fatalf("immutableRunThreatModel() = %q, error = %v, want malformed RunUID rejection", got, err)
	}
}

func TestImmutableRunThreatModelAllowsExplicitLegacyEmptyRunUID(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	run := &store.ScanRun{
		ID: "scan-legacy", RunUID: "",
		Namespace: "ns", RepositoryScan: "repo", TaskName: "task", Mode: "manual", Phase: "running",
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	if err := s.SaveThreatModel(context.Background(), &store.ThreatModel{
		Namespace: "ns", RepositoryScan: "repo", Content: "legacy model", Source: "generated",
		GeneratedByScan: run.ID,
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := &RepositoryScanReconciler{SecurityStore: s, RunThreatModelStore: s}
	got, err := reconciler.immutableRunThreatModel(context.Background(), run)
	if err != nil {
		t.Fatalf("immutableRunThreatModel() error = %v", err)
	}
	if got != "legacy model" {
		t.Fatalf("immutableRunThreatModel() = %q, want legacy model", got)
	}
}

func TestAppendStageReceiptRejectsMixedArtifactGeneration(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	const taskUID = "task-uid-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-task", Namespace: "ns", UID: types.UID(taskUID), CreationTimestamp: metav1.Now(),
			Labels: map[string]string{labels.LabelSecurityStage: "review", labels.LabelSecuritySliceID: "slice-1"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1},
	}
	run := &store.ScanRun{
		ID: "scan-1", RunUID: "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Namespace: "ns", RepositoryScan: "repo", TaskName: task.Name, Mode: "manual", Phase: "running",
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	if err := s.SaveBoundArtifact(context.Background(), &store.BoundArtifact{
		Namespace: "ns", TaskName: task.Name, Filename: "security-findings.v2.json", ContentType: "application/json",
		Data: []byte(`{"version":"new"}`),
		Provenance: store.OutputProvenance{
			TaskUID: taskUID, TaskAttempt: 1, ProducerKind: store.OutputProducerController,
			SubmissionNonceDigest: "writer-binding-1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := &RepositoryScanReconciler{IntegrityStore: s, ArtifactStore: s}
	err = reconciler.appendStageReceipt(
		context.Background(), task, run, "security-findings.v2.json",
		[]byte(`{"version":"parsed"}`), []byte(`{"version":"normalized"}`),
		store.StageReceiptAccepted, "", "",
	)
	if err == nil || !strings.Contains(err.Error(), "changed after parsing") {
		t.Fatalf("appendStageReceipt() error = %v, want mixed-generation rejection", err)
	}
}

func TestAppendStageReceiptPreservesRawArtifactBytes(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	const taskUID = "task-uid-raw"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "threat-task", Namespace: "ns", UID: types.UID(taskUID), CreationTimestamp: metav1.Now(),
			Labels: map[string]string{labels.LabelSecurityStage: "threat-model"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1},
	}
	run := &store.ScanRun{
		ID: "scan-raw", RunUID: "run_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Namespace: "ns", RepositoryScan: "repo", TaskName: task.Name, Mode: "manual", Phase: "running",
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	raw := []byte("# Threat model\n")
	if err := s.SaveBoundArtifact(context.Background(), &store.BoundArtifact{
		Namespace: "ns", TaskName: task.Name, Filename: "security-threat-model.md", ContentType: "text/markdown",
		Data: raw,
		Provenance: store.OutputProvenance{
			TaskUID: taskUID, TaskAttempt: 1, ProducerKind: store.OutputProducerController,
			SubmissionNonceDigest: "writer-binding-raw",
		},
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := &RepositoryScanReconciler{IntegrityStore: s, ArtifactStore: s}
	if err := reconciler.appendStageReceipt(
		context.Background(), task, run, "security-threat-model.md", raw, []byte("# Threat model"),
		store.StageReceiptAccepted, "", "",
	); err != nil {
		t.Fatalf("appendStageReceipt() error = %v", err)
	}
	receipts, _, err := s.ListStageReceipts(context.Background(), store.StageReceiptFilter{
		Namespace: "ns", ScanRunID: run.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].SourceArtifactSize != int64(len(raw)) ||
		receipts[0].SourceArtifactDigest != securityDigest(raw) {
		t.Fatalf("receipt raw binding = %#v", receipts)
	}
}

func TestSaveControllerArtifactHonorsBindingOff(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "security-task", Namespace: "ns", UID: types.UID("security-task-uid"),
		Labels: map[string]string{labels.LabelCreatedBy: repositorySecurityCreatedBy},
	}}
	reconciler := &RepositoryScanReconciler{
		ArtifactStore:   s,
		IntegrityConfig: security.IntegrityConfig{WorkerOutputBindingMode: security.WorkerOutputBindingOff},
	}
	if err := reconciler.saveControllerArtifact(
		context.Background(), task, "diagnostic.json", "application/json", []byte(`{"ok":true}`),
	); err != nil {
		t.Fatal(err)
	}
	data, _, err := s.GetArtifact(context.Background(), task.Namespace, task.Name, "diagnostic.json")
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("legacy controller artifact = %q, %v", data, err)
	}
}

func TestStageReceiptReplayUsesAttemptBoundTargets(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	created := metav1.NewTime(time.Date(2026, time.August, 4, 19, 0, 0, 0, time.UTC))
	completed := metav1.NewTime(created.Add(time.Minute))
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run := &store.ScanRun{
		ID: "scan-target-replay", RunUID: "run_dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Namespace: "ns", RepositoryScan: "repo", Mode: "initial", Phase: "running",
		StartedAt: created.Time, Quality: store.LegacyScanQuality(),
	}
	reconciler := &RepositoryScanReconciler{IntegrityStore: s, ArtifactStore: s}

	threatTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "threat-task-target-replay", Namespace: "ns", UID: types.UID("threat-task-uid-target-replay"),
			CreationTimestamp: created,
			Labels:            map[string]string{labels.LabelSecurityStage: security.StageThreatModel},
		},
		Spec: corev1alpha1.TaskSpec{
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{Ref: head}},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1, CompletionTime: &completed,
		},
	}
	threatRaw := []byte("# Threat model\n")
	if err := s.SaveBoundArtifact(context.Background(), &store.BoundArtifact{
		Namespace: "ns", TaskName: threatTask.Name, Filename: security.ArtifactThreatModel, ContentType: "text/markdown",
		Data: threatRaw,
		Provenance: store.OutputProvenance{
			TaskUID: string(threatTask.UID), TaskAttempt: 1, ProducerKind: store.OutputProducerController,
			SubmissionNonceDigest: "writer-binding-threat-target-replay",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.appendStageReceipt(
		context.Background(), threatTask, run, security.ArtifactThreatModel, threatRaw, []byte("# Threat model"),
		store.StageReceiptAccepted, "", "",
	); err != nil {
		t.Fatalf("append threat-model receipt(first) error = %v", err)
	}
	run.HeadCommit = head
	run.TargetReceiptID = "target_" + strings.Repeat("f", 64)
	if err := reconciler.appendStageReceipt(
		context.Background(), threatTask, run, security.ArtifactThreatModel, threatRaw, []byte("# Threat model"),
		store.StageReceiptAccepted, "", "",
	); err != nil {
		t.Fatalf("append threat-model receipt(replay) error = %v", err)
	}

	mapperTarget := &security.MapperTargetReceipt{HeadOID: head, ObjectFormat: "sha1"}
	mapperArtifact := &security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlicesV2,
		HeadCommit:    head,
		TargetReceipt: mapperTarget,
	}
	mapperRaw, err := json.Marshal(mapperArtifact)
	if err != nil {
		t.Fatal(err)
	}
	mapperTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mapper-task-target-replay", Namespace: "ns", UID: types.UID("mapper-task-uid-target-replay"),
			CreationTimestamp: created,
			Labels:            map[string]string{labels.LabelSecurityStage: security.StageMapper},
		},
		Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{Ref: "main"}},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1, CompletionTime: &completed,
		},
	}
	if err := s.SaveBoundArtifact(context.Background(), &store.BoundArtifact{
		Namespace: "ns", TaskName: mapperTask.Name, Filename: security.ArtifactSlices, ContentType: "application/json",
		Data: mapperRaw,
		Provenance: store.OutputProvenance{
			TaskUID: string(mapperTask.UID), TaskAttempt: 1, ProducerKind: store.OutputProducerController,
			SubmissionNonceDigest: "writer-binding-mapper-target-replay",
		},
	}); err != nil {
		t.Fatal(err)
	}
	run.HeadCommit = ""
	run.TargetReceiptID = ""
	if err := reconciler.appendStageReceipt(
		context.Background(), mapperTask, run, security.ArtifactSlices, mapperRaw, mapperRaw,
		store.StageReceiptAccepted, "", "",
	); err != nil {
		t.Fatalf("append mapper receipt(first) error = %v", err)
	}
	targetBytes, err := json.Marshal(mapperTarget)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadCommit = head
	run.TargetReceiptID = securityTargetReceiptID(run.RunUID, securityDigest(targetBytes))
	if err := reconciler.appendStageReceipt(
		context.Background(), mapperTask, run, security.ArtifactSlices, mapperRaw, mapperRaw,
		store.StageReceiptAccepted, "", "",
	); err != nil {
		t.Fatalf("append mapper receipt(replay) error = %v", err)
	}

	receipts, _, err := s.ListStageReceipts(context.Background(), store.StageReceiptFilter{
		Namespace: run.Namespace, ScanRunID: run.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 {
		t.Fatalf("stage receipt count = %d, want 2: %#v", len(receipts), receipts)
	}
	for i := range receipts {
		receipt := receipts[i]
		switch receipt.Stage {
		case security.StageThreatModel:
			if receipt.ExpectedTargetSHA != head || receipt.ObservedTargetSHA != "" || receipt.TargetReceiptID != "" {
				t.Fatalf("threat-model target binding = %#v", receipt)
			}
		case security.StageMapper:
			if receipt.ExpectedTargetSHA != "" || receipt.ObservedTargetSHA != head || receipt.TargetReceiptID != run.TargetReceiptID {
				t.Fatalf("mapper target binding = %#v", receipt)
			}
		default:
			t.Fatalf("unexpected stage receipt = %#v", receipt)
		}
	}
}

func TestLegacyMapperStageReceiptBindingIgnoresLaterRunProjection(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labels.LabelSecurityStage: security.StageMapper}},
		Spec:       corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{Ref: "main"}},
	}
	run := &store.ScanRun{RunUID: "run_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	normalized, err := json.Marshal(&security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlices,
		HeadCommit:    head,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstExpected, firstObserved, firstTargetReceipt, err := stageReceiptTargetBinding(
		task, run, security.ArtifactSlices, normalized,
	)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadCommit = head
	run.TargetReceiptID = "target_" + strings.Repeat("f", 64)
	secondExpected, secondObserved, secondTargetReceipt, err := stageReceiptTargetBinding(
		task, run, security.ArtifactSlices, normalized,
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstExpected != "" || firstObserved != head || firstTargetReceipt != "" {
		t.Fatalf("initial legacy mapper binding = (%q, %q, %q)", firstExpected, firstObserved, firstTargetReceipt)
	}
	if secondExpected != firstExpected || secondObserved != firstObserved || secondTargetReceipt != firstTargetReceipt {
		t.Fatalf(
			"replayed legacy mapper binding = (%q, %q, %q), want (%q, %q, %q)",
			secondExpected, secondObserved, secondTargetReceipt,
			firstExpected, firstObserved, firstTargetReceipt,
		)
	}
}

func TestHasActiveValidationTaskIgnoresUncontrolledMalformedBinding(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const findingID = "fnd_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid")},
	}
	uncontrolled := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "forged-validation", Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
				labels.LabelSecurityFindingID: labels.SelectorValue(findingID),
				labels.LabelSecurityStage:     security.StageValidation,
			},
		},
		Spec: corev1alpha1.TaskSpec{Env: []corev1.EnvVar{
			{Name: security.EnvFindingID, Value: findingID},
			{Name: security.EnvFindingID, Value: findingID},
		}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := &RepositoryScanReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, uncontrolled).Build(),
		Scheme: scheme,
	}
	active, err := r.hasActiveValidationTask(context.Background(), scan, &store.Finding{ID: findingID})
	if err != nil {
		t.Fatalf("hasActiveValidationTask() error = %v", err)
	}
	if active {
		t.Fatal("hasActiveValidationTask() = true for uncontrolled task")
	}
}

func TestHasActiveValidationTaskScopesToCurrentRunAndOccurrence(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid")},
	}
	finding := &store.Finding{
		ID:                  "fnd_" + strings.Repeat("a", 64),
		ScanRunID:           "scan-current",
		CurrentOccurrenceID: "occ_" + strings.Repeat("b", 64),
	}
	validationTask := func(name, scanRunID, occurrenceID string) *corev1alpha1.Task {
		t.Helper()
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: scan.Namespace,
				Labels: map[string]string{
					labels.LabelSecurityTarget:       labels.SelectorValue(scan.Name),
					labels.LabelSecurityFindingID:    labels.SelectorValue(finding.ID),
					labels.LabelSecurityScanID:       scanRunID,
					labels.LabelSecurityOccurrenceID: labels.SelectorValue(occurrenceID),
					labels.LabelSecurityStage:        security.StageValidation,
				},
			},
			Spec: corev1alpha1.TaskSpec{Env: []corev1.EnvVar{
				{Name: security.EnvScanID, Value: scanRunID},
				{Name: security.EnvFindingID, Value: finding.ID},
				{Name: security.EnvOccurrenceID, Value: occurrenceID},
			}},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
		}
		if err := controllerutil.SetControllerReference(scan, task, scheme); err != nil {
			t.Fatalf("SetControllerReference() error = %v", err)
		}
		return task
	}

	previousRun := validationTask("previous-run-validation", "scan-previous", finding.CurrentOccurrenceID)
	previousOccurrence := validationTask("previous-occurrence-validation", finding.ScanRunID, "occ_"+strings.Repeat("c", 64))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, previousRun, previousOccurrence).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme}

	active, err := r.hasActiveValidationTask(context.Background(), scan, finding)
	if err != nil {
		t.Fatalf("hasActiveValidationTask() error = %v", err)
	}
	if active {
		t.Fatal("hasActiveValidationTask() = true for a previous run or occurrence")
	}

	current := validationTask("current-occurrence-validation", finding.ScanRunID, finding.CurrentOccurrenceID)
	if err := cl.Create(context.Background(), current); err != nil {
		t.Fatalf("Create(current validation task) error = %v", err)
	}
	active, err = r.hasActiveValidationTask(context.Background(), scan, finding)
	if err != nil {
		t.Fatalf("hasActiveValidationTask() with current task error = %v", err)
	}
	if !active {
		t.Fatal("hasActiveValidationTask() = false for the current run and occurrence")
	}
}

func TestHasActiveValidationTaskRejectsMalformedCurrentOccurrenceBinding(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid")},
	}
	finding := &store.Finding{
		ID:                  "fnd_" + strings.Repeat("a", 64),
		ScanRunID:           "scan-current",
		CurrentOccurrenceID: "occ_" + strings.Repeat("b", 64),
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "malformed-current-validation", Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelSecurityTarget:       labels.SelectorValue(scan.Name),
				labels.LabelSecurityFindingID:    labels.SelectorValue(finding.ID),
				labels.LabelSecurityScanID:       finding.ScanRunID,
				labels.LabelSecurityOccurrenceID: labels.SelectorValue(finding.CurrentOccurrenceID),
				labels.LabelSecurityStage:        security.StageValidation,
			},
		},
		Spec: corev1alpha1.TaskSpec{Env: []corev1.EnvVar{
			{Name: security.EnvScanID, Value: finding.ScanRunID},
			{Name: security.EnvFindingID, Value: finding.ID},
			{Name: security.EnvOccurrenceID, Value: "occ_" + strings.Repeat("c", 64)},
		}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	if err := controllerutil.SetControllerReference(scan, task, scheme); err != nil {
		t.Fatalf("SetControllerReference() error = %v", err)
	}
	r := &RepositoryScanReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, task).Build(),
		Scheme: scheme,
	}

	if active, err := r.hasActiveValidationTask(context.Background(), scan, finding); err == nil {
		t.Fatalf("hasActiveValidationTask() = %v, nil error for mismatched occurrence binding", active)
	}
}
