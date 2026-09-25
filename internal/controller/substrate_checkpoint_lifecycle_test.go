package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func nativeCheckpointWorkspace(t *testing.T, h *nativeRuntimeTestHarness, name string) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	ws := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: h.pool.Namespace, Name: name, UID: types.UID(name + "-uid"), Generation: 1,
		Labels:      map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceControllerLabelValue},
		Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: h.pool.Name}},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			ClassBinding:    workspacev1alpha1.ImmutableObjectBinding{Name: "substrate", UID: "class-uid", Generation: 1, ProfileHash: "sha256:" + strings.Repeat("1", 64)},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{Name: "substrate", UID: "provider-uid", Generation: 1},
			DesiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
		},
	}
	if err := h.r.Create(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	if pool.Labels == nil {
		pool.Labels = map[string]string{}
	}
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Labels[acpExecutionWorkspaceLinkLabel] = ws.Name
	pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = string(ws.UID)
	if err := h.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	return ws
}

func nativeExportCheckpoint(t *testing.T, h *nativeRuntimeTestHarness, ws *workspacev1alpha1.ExecutionWorkspace, recoverLast bool) *workspacev1alpha1.ExecutionWorkspaceCheckpoint {
	t.Helper()
	if !recoverLast {
		ws.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
		if err := h.r.Status().Update(t.Context(), ws); err != nil {
			t.Fatal(err)
		}
	}
	cp := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: "save-" + ws.Name, UID: types.UID("checkpoint-" + ws.Name), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspacev1alpha1.ObjectIdentityReference{Name: ws.Name, UID: ws.UID}, RecoverLastCheckpoint: recoverLast}}
	if err := h.r.Create(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	r := &SubstrateCheckpointReconciler{RuntimePools: h.r, CheckpointAPIInstalled: true}
	for range 8 {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
			t.Fatal(err)
		}
		if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); err != nil {
			t.Fatal(err)
		}
		if cp.Status.Phase == "Ready" {
			return cp
		}
	}
	t.Fatalf("checkpoint did not become Ready: %+v", cp.Status)
	return nil
}

func nativeDeletePool(t *testing.T, h *nativeRuntimeTestHarness, pool *corev1alpha1.RuntimePool) {
	t.Helper()
	current := runtimePoolTestGetPool(t, h.r, pool)
	if err := h.r.Delete(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	for range 80 {
		h.step(t)
		err := h.r.Get(t.Context(), client.ObjectKeyFromObject(pool), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("pool deletion did not settle: %s", current.Status.Message)
}

func TestNativeSubstratePublicCheckpointSurvivesSourceDeletionAndForks(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	ws := nativeCheckpointWorkspace(t, h, "source")
	h.until(t, nativeTestServing)
	h.api.data[h.record(t).Attempt.Name] = "source file contents"
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	cp := nativeExportCheckpoint(t, h, ws, false)
	checkpoint := *h.record(t).Checkpoint
	source := runtimePoolTestGetPool(t, h.r, h.pool)
	nativeDeletePool(t, h, &source)
	if err := h.r.Delete(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	if len(h.api.tags) != 1 || h.api.templates[substrateTestTemplateNamespace+"/"+checkpoint.Template.Name] == nil {
		t.Fatal("source deletion collected exported data or its immutable restore template")
	}

	fork := runtimePoolSubstrateTestObject()
	fork.Name, fork.UID = "acp-ws-codex-0123456789abcdef", "fork-pool-uid"
	fork.Spec = *source.Spec.DeepCopy()
	fork.Spec.DesiredReplicas = 1
	fork.Spec.ExecutionWorkspace.Substrate.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{Name: cp.Name, UID: string(cp.UID), Digest: cp.Status.Digest}
	if err := h.r.Create(t.Context(), fork); err != nil {
		t.Fatal(err)
	}
	h.pool = fork
	nativeCheckpointWorkspace(t, h, "fork")
	h.until(t, nativeTestServing)
	if h.api.data[h.record(t).Attempt.Name] != "source file contents" || h.api.updates != 1 {
		t.Fatal("fork did not cold-restore the immutable Data Tag under its own template")
	}
	if len(h.seeds) != 2 || h.seeds[0].ControllerToken == h.seeds[1].ControllerToken {
		t.Fatal("fork reused source runtime credentials")
	}
	if err := h.r.Delete(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	if _, err := (&SubstrateCheckpointReconciler{RuntimePools: h.r, CheckpointAPIInstalled: true}).Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatal(err)
	}
	_, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), checkpoint.Digest)
	if err != nil || artifact == nil || !artifact.Owners[substratePoolCheckpointOwner(fork)] || len(h.api.tags) != 1 {
		t.Fatal("deleting checkpoint reference invalidated a committed restore")
	}
	nativeDeletePool(t, h, fork)
	if len(h.api.actors) != 0 || len(h.api.tags) != 0 {
		t.Fatal("last owner deletion leaked native actors or checkpoint data")
	}
}

func TestNativeSubstrateExplicitRecoveryUsesLastVerifiedDataWithoutReplayingSource(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	ws := nativeCheckpointWorkspace(t, h, "recovery")
	h.until(t, nativeTestServing)
	h.api.data[h.record(t).Attempt.Name] = "accepted checkpoint"
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	digest := h.record(t).Checkpoint.Digest
	substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
	h.until(t, nativeTestServing)
	lost := h.record(t).Attempt.Name
	delete(h.api.actors, lost)
	before := h.api.resumes
	h.step(t)
	if h.record(t).Phase != substrateNativeFailed {
		t.Fatal("Actor loss did not close runtime admission")
	}
	cp := nativeExportCheckpoint(t, h, ws, true)
	if cp.Status.Digest != digest || h.api.resumes != before {
		t.Fatal("explicit recovery changed the checkpoint or replayed source work")
	}
}

func TestNativeSubstrateRecoveryReexportsImportedCheckpoint(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	ws := nativeCheckpointWorkspace(t, h, "original")
	h.until(t, nativeTestServing)
	h.api.data[h.record(t).Attempt.Name] = "retained source data"
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	cp := nativeExportCheckpoint(t, h, ws, false)
	source := runtimePoolTestGetPool(t, h.r, h.pool)
	digest := cp.Status.Digest
	nativeDeletePool(t, h, &source)
	if err := h.r.Delete(t.Context(), ws); err != nil {
		t.Fatal(err)
	}

	restore := runtimePoolSubstrateTestObject()
	restore.Name, restore.UID = "acp-ws-codex-0123456789abcdef", "imported-pool-uid"
	restore.Spec = *source.Spec.DeepCopy()
	restore.Spec.DesiredReplicas = 1
	restore.Spec.ExecutionWorkspace.Substrate.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{
		Name: cp.Name, UID: string(cp.UID), Digest: digest,
	}
	if err := h.r.Create(t.Context(), restore); err != nil {
		t.Fatal(err)
	}
	h.pool = restore
	importedWS := nativeCheckpointWorkspace(t, h, "imported")
	h.until(t, nativeTestServing)
	if err := h.r.Delete(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	r := &SubstrateCheckpointReconciler{RuntimePools: h.r, CheckpointAPIInstalled: true}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatal(err)
	}
	delete(h.api.actors, h.record(t).Attempt.Name)
	h.step(t)
	before := h.api.resumes
	recovered := nativeExportCheckpoint(t, h, importedWS, true)
	if recovered.Status.Digest != digest || h.api.resumes != before {
		t.Fatal("recovery lost the imported data or replayed the failed source")
	}
	nativeDeletePool(t, h, restore)
	if err := h.r.Delete(t.Context(), importedWS); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(recovered)}); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(recovered), recovered); err != nil {
		t.Fatal(err)
	}
	_, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), digest)
	if err != nil || artifact == nil || recovered.Status.Phase != "Ready" ||
		!artifact.Owners[substratePublicCheckpointOwner(recovered)] || len(h.api.tags) != 1 {
		t.Fatal("re-exported data did not survive deletion of the failed importing workspace")
	}
	if artifact.SourceWorkspace.UID != ws.UID {
		t.Fatal("recovery rewrote the immutable artifact provenance")
	}
	destination := runtimePoolSubstrateTestObject()
	destination.Name, destination.UID = "acp-ws-codex-3333333333333333", "recovered-pool-uid"
	destination.Spec = *restore.Spec.DeepCopy()
	destination.Spec.ExecutionWorkspace.Substrate.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{
		Name: recovered.Name, UID: string(recovered.UID), Digest: recovered.Status.Digest,
	}
	if err := h.r.Create(t.Context(), destination); err != nil {
		t.Fatal(err)
	}
	h.pool = destination
	nativeCheckpointWorkspace(t, h, "recovered-destination")
	h.until(t, nativeTestServing)
	if h.api.data[h.record(t).Attempt.Name] != "retained source data" {
		t.Fatal("re-exported checkpoint could not restore its original data into a new workspace")
	}
}

func TestNativeSubstrateBootDeadlineAndConsentIdentity(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record != nil && record.Attempt != nil && record.Attempt.BootRequested && record.Attempt.BootID == ""
	})
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	cm, record, err := h.r.readNativeSubstrateState(t.Context(), &pool)
	if err != nil {
		t.Fatal(err)
	}
	record.Attempt.StartedAt = metav1.NewTime(h.r.now().Add(-time.Hour))
	record.Attempt.BootStartedAt = record.Attempt.StartedAt
	if err := h.r.saveNativeSubstrateState(t.Context(), cm, record); err != nil {
		t.Fatal(err)
	}
	before := h.api.resumes
	h.step(t)
	if h.record(t).Phase != substrateNativeFailed || before != h.api.resumes {
		t.Fatal("ambiguous boot was replayed or left unbounded")
	}
	pool.Annotations[substrateNativeCheckpointConsent] = "sha256:" + strings.Repeat("a", 64)
	pool.Annotations[substrateNativeDataProtection] = "sha256:" + strings.Repeat("b", 64)
	if runtimePoolWorkspaceSuspendConsentRecorded(&pool) {
		t.Fatal("unbound digests were accepted as a suspension record")
	}
}

func TestNativeSubstrateCatalogAcquisitionCannotRaceCollection(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, nativeTestServing)
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	digest := h.record(t).Checkpoint.Digest
	cm, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.releaseSubstrateCheckpointArtifact(t.Context(), digest, substratePoolCheckpointOwner(h.pool)); err != nil {
		t.Fatal(err)
	}
	if err := h.r.acquireSubstrateCheckpointArtifact(t.Context(), cm, artifact, substratePoolCheckpointOwner(h.pool), "checkpoint:default/new:uid"); !apierrors.IsConflict(err) {
		t.Fatalf("stale acquisition bypassed CAS: %v", err)
	}
	cm, artifact, err = h.r.readSubstrateCheckpointArtifact(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.acquireSubstrateCheckpointArtifact(context.Background(), cm, artifact, substratePoolCheckpointOwner(h.pool), "checkpoint:default/new:uid"); err == nil {
		t.Fatal("acquisition reopened a collecting artifact")
	}
	if err := h.r.collectSubstrateCheckpointArtifact(t.Context(), cm, artifact); err != nil {
		t.Fatal(err)
	}
	if len(h.api.tags) != 0 {
		t.Fatal("catalog collection retained unowned data")
	}
	if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatal("collected catalog remains")
	}
}

func TestNativeSubstrateCheckpointStatusAndHistoricalTemplateCollection(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	ws := nativeCheckpointWorkspace(t, h, "history")
	h.until(t, nativeTestServing)
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	cp := nativeExportCheckpoint(t, h, ws, false)
	old := *h.record(t).Checkpoint
	r := &SubstrateCheckpointReconciler{RuntimePools: h.r, CheckpointAPIInstalled: true}
	version := cp.ResourceVersion
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(cp), cp); err != nil {
		t.Fatal(err)
	}
	if cp.ResourceVersion != version {
		t.Fatal("unchanged Ready status wrote another version")
	}
	substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
	h.until(t, nativeTestServing)
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	h.step(t)
	store := &nativeSubstrateTemplateStore{r: h.r}
	collect := func() {
		t.Helper()
		list := &corev1.ConfigMapList{}
		if err := h.r.List(t.Context(), list, client.MatchingLabels{substrateTemplateBindingLabel: "true"}); err != nil {
			t.Fatal(err)
		}
		for _, cm := range list.Items {
			if _, err := r.collect(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cm.Namespace, Name: "template/" + cm.Name}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	collect()
	if h.api.templates[substrateTestTemplateNamespace+"/"+old.Template.Name] == nil {
		t.Fatal("history GC deleted an exported checkpoint template")
	}
	if err := h.r.Delete(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatal(err)
	}
	cm, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), old.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.collectSubstrateCheckpointArtifact(t.Context(), cm, artifact); err != nil {
		t.Fatal(err)
	}
	collect()
	collect()
	if h.api.templates[substrateTestTemplateNamespace+"/"+old.Template.Name] != nil {
		t.Fatal("history GC retained an unused immutable template")
	}
	_, binding, err := store.read(t.Context(), h.record(t).Atespace, runtimePoolSubstrateTemplateName(h.pool.Name))
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil && binding.Collecting != nil {
		t.Fatal("completed history collection left its mutation barrier")
	}
}
