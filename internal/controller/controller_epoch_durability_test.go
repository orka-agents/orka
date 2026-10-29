package controller

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const durabilityTestInterval = 20 * time.Millisecond

// fastEpochManager returns a manager whose watermark loop and hold polling
// run every few milliseconds.
func fastEpochManager(authority store.ControllerEpochStore, holder string) *ControllerEpochManager {
	manager := NewControllerEpochManager(authority, holder)
	manager.WatermarkInterval = durabilityTestInterval
	manager.HoldPollInterval = durabilityTestInterval
	return manager
}

// eventually polls condition until it holds or the deadline passes.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func storageRecord(t *testing.T, authority *kubestore.Store) store.StorageRecord {
	t.Helper()
	record, err := authority.GetStorageRecord(context.Background(), store.DefaultControllerEpochName)
	if err != nil {
		t.Fatalf("read the storage record: %v", err)
	}
	return record
}

func recordedHold(t *testing.T, authority *kubestore.Store) *store.StorageRecoveryHold {
	t.Helper()
	return storageRecord(t, authority).Hold
}

func recordedWatermark(t *testing.T, authority *kubestore.Store) int64 {
	t.Helper()
	ack, _ := storageRecord(t, authority).Acknowledgement(storagev1.StateContract)
	return ack.Watermark
}

// acknowledgeHold sets the operator's release annotation on the
// ControllerEpoch object.
func acknowledgeHold(t *testing.T, kubeClient client.Client, id string) {
	t.Helper()
	ctx := context.Background()
	var objects corev1alpha1.ControllerEpochList
	if err := kubeClient.List(ctx, &objects, client.InNamespace(controllerEpochManagerTestNamespace)); err != nil || len(objects.Items) != 1 {
		t.Fatalf("list controller epoch objects: %d, %v", len(objects.Items), err)
	}
	object := objects.Items[0]
	if object.Annotations == nil {
		object.Annotations = map[string]string{}
	}
	object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = id
	if err := kubeClient.Update(ctx, &object); err != nil {
		t.Fatalf("acknowledge the storage recovery hold: %v", err)
	}
}

func requireHeld(t *testing.T, manager *ControllerEpochManager, contract string) {
	t.Helper()
	_, _, err := writeFenceWithin(manager, contract, time.Second)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) || !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("WriteFence(%s) while held = %v, want a storage recovery hold that matches ErrNotReady", contract, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := manager.CurrentFence(ctx); !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("CurrentFence while held = %v, want a storage recovery hold", err)
	}
	if _, ok := manager.Current(); ok {
		t.Fatal("Current reported an epoch while storage is held")
	}
	if err := manager.StorageHold(); !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("StorageHold = %v", err)
	}
}

// At takeover, each contract's acknowledgement is recorded in Kubernetes
// before its fence opens, and the watermark loop then records every
// watermark the dataset acknowledged.
func TestControllerEpochManagerRecordsStorageAcknowledgements(t *testing.T) {
	authority, _ := newKubernetesControllerEpochAuthority(t)
	dataset := newRecordingEpochDataset("dataset-shared")
	manager := fastEpochManager(authority, "controller-a").
		WithStorage(storagev1.StateContract, dataset).
		WithStorage(storagev1.ArtifactsContract, dataset)
	startEpochManager(t, manager)
	if _, err := fenceWithTimeout(manager); err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{storagev1.StateContract, storagev1.ArtifactsContract} {
		if _, _, err := writeFenceWithin(manager, contract, 5*time.Second); err != nil {
			t.Fatalf("%s fence: %v", contract, err)
		}
		ack, ok := storageRecord(t, authority).Acknowledgement(contract)
		if !ok || ack.DatasetID != "dataset-shared" || ack.Epoch != 1 {
			t.Fatalf("%s acknowledgement = %#v, want epoch 1 of dataset-shared recorded before the fence opened", contract, ack)
		}
	}
	eventually(t, "a recorded watermark of at least 3", func() bool {
		return recordedWatermark(t, authority) >= 3
	})
	record := storageRecord(t, authority)
	state, _ := record.Acknowledgement(storagev1.StateContract)
	artifacts, _ := record.Acknowledgement(storagev1.ArtifactsContract)
	stored, _ := dataset.GetWriteWatermark(context.Background())
	if state.Watermark != artifacts.Watermark || state.Watermark > stored.Value {
		t.Fatalf("recorded watermarks %d and %d, dataset holds %d: both contracts follow their shared dataset and never run ahead of it",
			state.Watermark, artifacts.Watermark, stored.Value)
	}
	if manager.StorageHold() != nil || record.Hold != nil {
		t.Fatal("a healthy dataset raised a hold")
	}
}

// openRollbackStore opens a SQLite store at path with a storage client bound
// to manager.
func openRollbackStore(t *testing.T, path string, manager *ControllerEpochManager) (*sqlite.Store, interface {
	SaveResult(context.Context, string, string, []byte) error
}) {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	backend.RequireWriteEnvelopes()
	return backend, newFencedStorageClient(t, backend, manager)
}

// writeThenRestoreOlderCopy runs a controller over a SQLite store at path
// until it has recorded a watermark, copies the database, keeps writing until
// it has recorded two more, stops, and puts the copy back in place. It
// returns the watermark the copy holds and the one Kubernetes recorded last.
func writeThenRestoreOlderCopy(t *testing.T, authority *kubestore.Store, path string) (copied, acknowledged int64) {
	t.Helper()
	ctx := context.Background()
	first := fastEpochManager(authority, "controller-a")
	firstStore, firstClient := openRollbackStore(t, path, first)
	cancelFirst, firstDone := startEpochManager(t, first)
	if _, err := fenceWithTimeout(first); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a recorded watermark", func() bool { return recordedWatermark(t, authority) >= 1 })
	backup := filepath.Join(filepath.Dir(path), "backup.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	snapshot, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.QueryRowContext(ctx, `SELECT write_watermark FROM store_dataset`).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	_ = snapshot.Close()
	backupWatermark := recordedWatermark(t, authority)
	if err := firstClient.SaveResult(ctx, "tenant", "task", []byte("written after the backup")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a watermark recorded after the backup", func() bool {
		return recordedWatermark(t, authority) >= backupWatermark+2
	})
	cancelFirst()
	if err := <-firstDone; err != nil {
		t.Fatalf("first manager: %v", err)
	}
	acknowledged = recordedWatermark(t, authority)
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	return copied, acknowledged
}

func requireStorageRecoveryCondition(t *testing.T, kubeClient client.Client, want metav1.ConditionStatus) {
	t.Helper()
	object, _ := kubernetesControllerEpochObjects(t, context.Background(), kubeClient)
	condition := meta.FindStatusCondition(object.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired)
	if condition == nil || condition.Status != want {
		t.Fatalf("StorageRecoveryRequired condition = %#v, want %s", condition, want)
	}
}

// A dataset restored from an older copy keeps its identity, so only the
// recorded watermark shows that acknowledged writes are gone. The successor
// holds the installation: no storage write, control-store change or Task
// result goes through, a restart keeps the hold, and only the operator's
// acknowledgement of that exact hold releases it, recording what the
// restored dataset holds as the new baseline.
func TestRestoredSQLiteDatasetHoldsTheInstallation(t *testing.T) {
	ctx := context.Background()
	authority, kubeClient := newKubernetesControllerEpochAuthority(t)
	path := filepath.Join(t.TempDir(), "orka.db")
	copied, acknowledged := writeThenRestoreOlderCopy(t, authority, path)

	successor := fastEpochManager(authority, "controller-b")
	restored, restoredClient := openRollbackStore(t, path, successor)
	t.Cleanup(func() { _ = restored.Close() })
	cancelSuccessor, successorDone := startEpochManager(t, successor)
	eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
	hold := recordedHold(t, authority)
	if hold.Reason != store.StorageRecoveryWatermarkRegressed || hold.Contract == "" ||
		hold.AcknowledgedWatermark != acknowledged || hold.StoredWatermark != copied {
		t.Fatalf("hold = %#v, want the restored watermark %d below the acknowledged %d", hold, copied, acknowledged)
	}
	// The takeover check holds before any fence opens, so nothing, not even
	// a watermark write, reaches the restored dataset.
	if stored, err := restored.GetWriteWatermark(ctx); err != nil || stored.Value != copied {
		t.Fatalf("restored watermark = %d, %v; want %d, untouched", stored.Value, err, copied)
	}
	eventually(t, "the successor to hold", func() bool { return successor.StorageHold() != nil })
	requireHeld(t, successor, storagev1.StateContract)
	requireHeld(t, successor, storagev1.ArtifactsContract)
	if err := restoredClient.SaveResult(ctx, "tenant", "task", []byte("must not land")); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("a write while held = %v, want ErrNotReady", err)
	}
	if _, err := restored.GetResult(ctx, "tenant", "task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the restored dataset holds a result: %v", err)
	}
	requireStorageRecoveryCondition(t, kubeClient, metav1.ConditionTrue)
	// Once the hold is recorded, the held dataset is advanced so the replaced
	// controller cannot write to it; its watermark stays as found.
	requireRestoredEpoch(t, restored, 2)
	if stored, err := restored.GetWriteWatermark(ctx); err != nil || stored.Value != copied {
		t.Fatalf("restored watermark after fencing = %d, %v; want %d", stored.Value, err, copied)
	}

	// A restart keeps the hold: the next controller checks and opens nothing
	// until the hold is acknowledged, and only fences the held dataset.
	cancelSuccessor()
	if err := <-successorDone; err != nil {
		t.Fatalf("successor: %v", err)
	}
	restarted := fastEpochManager(authority, "controller-c")
	restartedClient := newFencedStorageClient(t, restored, restarted)
	startEpochManager(t, restarted)
	eventually(t, "the restarted controller to hold", func() bool {
		return errors.Is(restarted.StorageHold(), store.ErrStorageRecoveryRequired)
	})
	requireHeld(t, restarted, storagev1.StateContract)
	requireRestoredEpoch(t, restored, 3)
	if got := recordedHold(t, authority); got == nil || got.ID != hold.ID {
		t.Fatalf("hold after restart = %#v, want %s kept", got, hold.ID)
	}
	acknowledgeHold(t, kubeClient, "some-other-hold")
	time.Sleep(5 * durabilityTestInterval)
	requireHeld(t, restarted, storagev1.StateContract)

	acknowledgeHold(t, kubeClient, hold.ID)
	eventually(t, "the acknowledged hold to clear", func() bool { return restarted.StorageHold() == nil })
	if _, err := fenceWithTimeout(restarted); err != nil {
		t.Fatalf("fence after the acknowledgement: %v", err)
	}
	if recordedHold(t, authority) != nil {
		t.Fatal("the acknowledged hold is still recorded")
	}
	if err := restartedClient.SaveResult(ctx, "tenant", "task", []byte("after recovery")); err != nil {
		t.Fatalf("a write after the acknowledgement: %v", err)
	}
	stored, err := restored.GetWriteWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ack, _ := storageRecord(t, authority).Acknowledgement(storagev1.StateContract)
	if ack.Epoch != 3 || ack.Watermark > stored.Value {
		t.Fatalf("acknowledgement after the release = %#v, dataset holds %d: the restored dataset is the new baseline", ack, stored.Value)
	}
	requireStorageRecoveryCondition(t, kubeClient, metav1.ConditionFalse)
}

// A dataset that holds a lower controller epoch than it acknowledged lost an
// acknowledged advance, and a contract served by another dataset than the
// one that acknowledged it is a storage switch; both hold the installation.
func TestControllerEpochManagerHoldsForLostEpochsAndChangedDatasets(t *testing.T) {
	for name, test := range map[string]struct {
		successor *recordingEpochDataset
		reason    store.StorageRecoveryReason
	}{
		"lost epoch":      {newRecordingEpochDataset("dataset-a"), store.StorageRecoveryEpochRegressed},
		"changed dataset": {newRecordingEpochDataset("dataset-b"), store.StorageRecoveryDatasetChanged},
	} {
		t.Run(name, func(t *testing.T) {
			authority, _ := newKubernetesControllerEpochAuthority(t)
			first := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, newRecordingEpochDataset("dataset-a"))
			cancelFirst, firstDone := startEpochManager(t, first)
			if _, err := fenceWithTimeout(first); err != nil {
				t.Fatal(err)
			}
			cancelFirst()
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}

			successor := fastEpochManager(authority, "controller-b").WithStorage(storagev1.StateContract, test.successor)
			startEpochManager(t, successor)
			eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
			hold := recordedHold(t, authority)
			if hold.Reason != test.reason || hold.Contract != storagev1.StateContract {
				t.Fatalf("hold = %#v, want %s", hold, test.reason)
			}
			eventually(t, "the successor to hold", func() bool { return successor.StorageHold() != nil })
			requireHeld(t, successor, storagev1.StateContract)
			if test.reason == store.StorageRecoveryDatasetChanged {
				time.Sleep(5 * durabilityTestInterval)
				if len(test.successor.advanceRequests()) != 0 {
					t.Fatal("the manager advanced a dataset that is not the one that acknowledged the contract")
				}
			} else {
				eventually(t, "the held dataset to be fenced", func() bool {
					requests := test.successor.advanceRequests()
					return len(requests) == 1 && requests[0].Epoch.Epoch == 2
				})
			}
			if test.reason == store.StorageRecoveryEpochRegressed && (hold.AcknowledgedEpoch != 1 || hold.StoredEpoch != 0) {
				t.Fatalf("hold evidence = %#v, want acknowledged epoch 1 and stored epoch 0", hold)
			}
		})
	}
}

// switchingEpochDataset reports a new incarnation, as a database that
// restarted or switched does, once switched is set.
type switchingEpochDataset struct {
	*recordingEpochDataset
	durability storagev1.DurabilityClass
	switched   chan struct{}
	// lose, when set, drops the watermark to zero at the switch.
	lose bool
	lost bool
	// mirrorErrs fails that many epoch reads after the switch.
	mirrorErrs atomic.Int32
}

func (d *switchingEpochDataset) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	info, err := d.recordingEpochDataset.Describe(ctx)
	info.Durability, info.Incarnation, info.State = d.durability, "incarnation-1", storagev1.DatasetReady
	select {
	case <-d.switched:
		info.Incarnation = "incarnation-2"
		d.mu.Lock()
		if d.lose && !d.lost {
			d.watermark, d.lost = 0, true
		}
		d.mu.Unlock()
	default:
	}
	return info, err
}

// After the database restarts or switches, the manager checks the dataset
// again: a synchronous-replicated dataset that kept every acknowledged write
// continues, an async-replicated one holds, and one that lost writes holds.
func TestControllerEpochManagerChecksASwitchedDataset(t *testing.T) {
	for name, test := range map[string]struct {
		durability storagev1.DurabilityClass
		lose       bool
		reason     store.StorageRecoveryReason
	}{
		"synchronous-replicated continues":    {durability: storagev1.DurabilitySynchronousReplicated},
		"async-replicated holds":              {durability: storagev1.DurabilityAsyncReplicated, reason: store.StorageRecoveryUnsafeSwitch},
		"lost acknowledged watermarks hold":   {durability: storagev1.DurabilitySynchronousReplicated, lose: true, reason: store.StorageRecoveryWatermarkRegressed},
		"local-buffered restart continues":    {durability: storagev1.DurabilityLocalBuffered},
		"local-buffered restart that lost it": {durability: storagev1.DurabilityLocalBuffered, lose: true, reason: store.StorageRecoveryWatermarkRegressed},
	} {
		t.Run(name, func(t *testing.T) {
			authority, _ := newKubernetesControllerEpochAuthority(t)
			dataset := &switchingEpochDataset{
				recordingEpochDataset: newRecordingEpochDataset("dataset-a"), durability: test.durability,
				switched: make(chan struct{}), lose: test.lose,
			}
			manager := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, dataset)
			startEpochManager(t, manager)
			if _, err := fenceWithTimeout(manager); err != nil {
				t.Fatal(err)
			}
			eventually(t, "a recorded watermark", func() bool { return recordedWatermark(t, authority) >= 2 })
			close(dataset.switched)
			if test.reason == "" {
				before := recordedWatermark(t, authority)
				eventually(t, "the watermark to keep advancing", func() bool {
					return recordedWatermark(t, authority) >= before+2
				})
				if manager.StorageHold() != nil || recordedHold(t, authority) != nil {
					t.Fatal("a switch that lost nothing held the installation")
				}
				return
			}
			eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
			hold := recordedHold(t, authority)
			if hold.Reason != test.reason || hold.Durability != string(test.durability) {
				t.Fatalf("hold = %#v, want %s", hold, test.reason)
			}
			if test.lose {
				// The switch check holds before the loop's next watermark
				// write, so the dataset that lost writes gets none.
				stored, _ := dataset.GetWriteWatermark(context.Background())
				if hold.StoredWatermark != 0 || stored.Value != 0 || hold.AcknowledgedWatermark != recordedWatermark(t, authority) {
					t.Fatalf("hold = %#v with the dataset at %d, want the lost watermark held before another write", hold, stored.Value)
				}
			}
			eventually(t, "the manager to hold", func() bool { return manager.StorageHold() != nil })
			requireHeld(t, manager, storagev1.StateContract)
		})
	}
}

// A hold closes fences that were already open and wakes callers waiting for
// a fence; clearing it lets the opened fences through again.
func TestStorageHoldClosesOpenFences(t *testing.T) {
	epoch := &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: 4, HolderID: "controller-a"}
	manager := &ControllerEpochManager{ready: make(chan struct{})}
	manager.WithStorage(storagev1.StateContract, newRecordingEpochDataset("dataset-a"))
	manager.WithStorage(storagev1.ArtifactsContract, newRecordingEpochDataset("dataset-b"))
	manager.openContractFence(storagev1.StateContract, "dataset-a", *epoch)

	waiting := make(chan error, 1)
	go func() {
		_, _, err := writeFenceWithin(manager, storagev1.ArtifactsContract, 10*time.Second)
		waiting <- err
	}()
	hold := store.StorageRecoveryHold{ID: "hold-1", Reason: store.StorageRecoveryUnsafeSwitch, Message: "switched"}
	manager.setHold(hold)
	select {
	case err := <-waiting:
		var holdErr *store.StorageHoldError
		if !errors.As(err, &holdErr) || holdErr.Hold.ID != "hold-1" {
			t.Fatalf("waiting WriteFence = %v, want the hold", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hold did not wake a caller waiting for a fence")
	}
	requireHeld(t, manager, storagev1.StateContract)

	manager.clearHold()
	if datasetID, fence, err := writeFenceWithin(manager, storagev1.StateContract, time.Second); err != nil ||
		datasetID != "dataset-a" || fence.Epoch != 4 {
		t.Fatalf("state fence after the hold cleared = %s %#v %v", datasetID, fence, err)
	}
	if _, err := fenceWithTimeout(manager); err != nil {
		t.Fatalf("CurrentFence after the hold cleared: %v", err)
	}
}

// While storage is held, a pending Task gets no Job and keeps its status; it
// starts once the hold is released.
func TestTaskJobWaitsWhileStorageIsHeld(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "prompt"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1}}
	r := newUnitReconciler(newTestScheme(), task, agent)
	r.ControllerEpochManager = &ControllerEpochManager{ready: make(chan struct{})}
	r.ControllerEpochManager.setHold(store.StorageRecoveryHold{ID: "hold-1", Reason: store.StorageRecoveryWatermarkRegressed})

	result, err := r.createTaskJob(ctx, task.DeepCopy(), agent, nil)
	if err != nil || result.RequeueAfter != storageHoldRequeue {
		t.Fatalf("createTaskJob while held = %#v, %v; want a requeue", result, err)
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("a Job was created while storage is held: %d, %v", len(jobs.Items), err)
	}
	current := &corev1alpha1.Task{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhasePending || current.Status.Attempts != 0 {
		t.Fatalf("held Task status = %s with %d attempts, want it untouched", current.Status.Phase, current.Status.Attempts)
	}

	r.ControllerEpochManager.clearHold()
	if _, err := r.createTaskJob(ctx, current, agent, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("Jobs after the hold cleared = %d, %v; want one", len(jobs.Items), err)
	}
}

// holdRecordingStore blocks HoldStorage until release closes, as a slow or
// unavailable API server does.
type holdRecordingStore struct {
	installationEpochStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *holdRecordingStore) HoldStorage(
	ctx context.Context, fence store.ControllerEpochFence, hold store.StorageRecoveryHold,
) (store.StorageRecoveryHold, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return store.StorageRecoveryHold{}, ctx.Err()
	}
	return s.memoryStorageRecords.HoldStorage(ctx, fence, hold)
}

// A check that fails closes every fence before the hold reaches Kubernetes,
// so a slow API server never lets writes reach a dataset known to have lost
// some, and the manager keeps the hold even if recording it fails.
func TestStorageHoldClosesFencesBeforeItIsRecorded(t *testing.T) {
	authorityDB, err := sqlite.NewDB(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authorityDB.Close() })
	authority := &holdRecordingStore{
		installationEpochStore: installationEpochStore{
			ControllerEpochStore: sqlite.NewStore(authorityDB, "authority"), memoryStorageRecords: newMemoryStorageRecords(),
			installationID: "installation-a",
		},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	state := &switchingEpochDataset{
		recordingEpochDataset: newRecordingEpochDataset("dataset-a"), durability: storagev1.DurabilityAsyncReplicated,
		switched: make(chan struct{}),
	}
	manager := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, state)
	cancel, done := startEpochManager(t, manager)
	if _, _, err := writeFenceWithin(manager, storagev1.StateContract, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	close(state.switched)
	select {
	case <-authority.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the switch never raised a hold")
	}
	requireHeld(t, manager, storagev1.StateContract)
	if record, _ := authority.GetStorageRecord(context.Background(), store.DefaultControllerEpochName); record.Hold != nil {
		t.Fatal("the hold reached the record before HoldStorage returned")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireHeld(t, manager, storagev1.StateContract)
}

// After an operator acknowledges a hold raised because a contract moved to
// another dataset, the contract's fence names the new dataset.
func TestReleasedDatasetChangeMovesTheFence(t *testing.T) {
	authority, kubeClient := newKubernetesControllerEpochAuthority(t)
	moving := &movingEpochDataset{
		recordingEpochDataset: newRecordingEpochDataset("dataset-a"),
		next:                  newRecordingEpochDataset("dataset-b"),
		moved:                 make(chan struct{}),
		gate:                  make(chan struct{}),
	}
	manager := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, moving)
	startEpochManager(t, manager)
	if datasetID, _, err := writeFenceWithin(manager, storagev1.StateContract, 5*time.Second); err != nil || datasetID != "dataset-a" {
		t.Fatalf("fence = %s, %v", datasetID, err)
	}
	close(moving.moved)
	eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
	hold := recordedHold(t, authority)
	if hold.Reason != store.StorageRecoveryDatasetChanged || hold.DatasetID != "dataset-b" || hold.AcknowledgedDatasetID != "dataset-a" {
		t.Fatalf("hold = %#v, want the contract moved to dataset-b", hold)
	}
	acknowledgeHold(t, kubeClient, hold.ID)
	eventually(t, "the acknowledged hold to clear", func() bool {
		return !errors.Is(manager.StorageHold(), store.ErrStorageRecoveryRequired)
	})
	// The release advanced dataset-b; its takeover step now waits at the
	// gate, and until it passes, no write may get the old dataset's fence.
	if datasetID, _, err := writeFenceWithin(manager, storagev1.StateContract, 200*time.Millisecond); err == nil {
		t.Fatalf("fence before the new dataset passed = %s, want none", datasetID)
	}
	if err := manager.StorageHold(); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("StorageHold before the new dataset passed = %v, want ErrNotReady", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := manager.CurrentFence(ctx); err == nil {
		t.Fatal("CurrentFence opened before the new state dataset passed")
	}
	close(moving.gate)
	if datasetID, _, err := writeFenceWithin(manager, storagev1.StateContract, 5*time.Second); err != nil || datasetID != "dataset-b" {
		t.Fatalf("fence after the release = %s, %v; want dataset-b", datasetID, err)
	}
	if ack, _ := storageRecord(t, authority).Acknowledgement(storagev1.StateContract); ack.DatasetID != "dataset-b" {
		t.Fatalf("acknowledgement after the release = %#v, want dataset-b", ack)
	}
}

// movingEpochDataset serves dataset next once moved closes, as a contract
// moved to another dataset does.
type movingEpochDataset struct {
	*recordingEpochDataset
	next  *recordingEpochDataset
	moved chan struct{}
	// gate holds every advance of next after its first until it closes.
	gate         chan struct{}
	nextAdvances atomic.Int32
}

func (d *movingEpochDataset) current() *recordingEpochDataset {
	select {
	case <-d.moved:
		return d.next
	default:
		return d.recordingEpochDataset
	}
}

func (d *movingEpochDataset) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return d.current().Describe(ctx)
}

func (d *movingEpochDataset) AdvanceControllerEpoch(
	ctx context.Context, request storagev1.AdvanceControllerEpochRequest,
) (storagev1.ControllerEpochMirrorState, error) {
	current := d.current()
	if current == d.next && d.nextAdvances.Add(1) > 1 {
		select {
		case <-d.gate:
		case <-ctx.Done():
			return storagev1.ControllerEpochMirrorState{}, ctx.Err()
		}
	}
	return current.AdvanceControllerEpoch(ctx, request)
}

func (d *movingEpochDataset) GetControllerEpochMirror(ctx context.Context, name string) (storagev1.ControllerEpochMirrorState, error) {
	return d.current().GetControllerEpochMirror(ctx, name)
}

func (d *movingEpochDataset) AdvanceWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	return d.current().AdvanceWriteWatermark(ctx)
}

func (d *movingEpochDataset) GetWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	return d.current().GetWriteWatermark(ctx)
}

// A dataset that passed is watched by the watermark loop while another
// contract's dataset is still unreachable at takeover.
func TestWatermarkLoopWatchesOpenedDatasetsWhileOthersAreUnreachable(t *testing.T) {
	authority, _ := newKubernetesControllerEpochAuthority(t)
	state := &switchingEpochDataset{
		recordingEpochDataset: newRecordingEpochDataset("dataset-state"), durability: storagev1.DurabilityAsyncReplicated,
		switched: make(chan struct{}),
	}
	artifacts := &blockingEpochDataset{recordingEpochDataset: newRecordingEpochDataset("dataset-artifacts"), release: make(chan struct{})}
	manager := fastEpochManager(authority, "controller-a").
		WithStorage(storagev1.StateContract, state).
		WithStorage(storagev1.ArtifactsContract, artifacts)
	startEpochManager(t, manager)
	if _, _, err := writeFenceWithin(manager, storagev1.StateContract, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the state watermark to be recorded", func() bool { return recordedWatermark(t, authority) >= 2 })
	close(state.switched)
	eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
	if hold := recordedHold(t, authority); hold.Reason != store.StorageRecoveryUnsafeSwitch || hold.DatasetID != "dataset-state" {
		t.Fatalf("hold = %#v, want the state dataset's unsafe switch", hold)
	}
	requireHeld(t, manager, storagev1.StateContract)
	if len(artifacts.advanceRequests()) != 0 {
		t.Fatal("the unreachable artifacts dataset was advanced")
	}
}

func (d *switchingEpochDataset) GetControllerEpochMirror(ctx context.Context, name string) (storagev1.ControllerEpochMirrorState, error) {
	select {
	case <-d.switched:
		if d.mirrorErrs.Add(-1) >= 0 {
			return storagev1.ControllerEpochMirrorState{}, errors.New("connection reset after the failover")
		}
	default:
	}
	return d.recordingEpochDataset.GetControllerEpochMirror(ctx, name)
}

func requireRestoredEpoch(t *testing.T, restored *sqlite.Store, epoch int64) {
	t.Helper()
	eventually(t, "the held dataset to be fenced", func() bool {
		mirror, err := restored.GetControllerEpochMirror(context.Background(), store.DefaultControllerEpochName)
		return err == nil && mirror.Epoch.Epoch == epoch
	})
}

// A switch check that cannot read the dataset is not a pass: the dataset gets
// no write until a later check finishes, and that check still holds.
func TestSwitchCheckThatCannotReadTheDatasetIsNotAPass(t *testing.T) {
	authority, _ := newKubernetesControllerEpochAuthority(t)
	dataset := &switchingEpochDataset{
		recordingEpochDataset: newRecordingEpochDataset("dataset-a"), durability: storagev1.DurabilitySynchronousReplicated,
		switched: make(chan struct{}), lose: true,
	}
	dataset.mirrorErrs.Store(1 << 20)
	manager := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, dataset)
	startEpochManager(t, manager)
	if _, err := fenceWithTimeout(manager); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a recorded watermark", func() bool { return recordedWatermark(t, authority) >= 2 })
	close(dataset.switched)
	// While the switched dataset cannot be read, its fence is closed: no
	// application write, control-store change or Job reaches it.
	eventually(t, "the switched dataset's fence to close", func() bool {
		_, _, err := writeFenceWithin(manager, storagev1.StateContract, 20*time.Millisecond)
		return err != nil
	})
	time.Sleep(5 * durabilityTestInterval)
	if _, _, err := writeFenceWithin(manager, storagev1.StateContract, 20*time.Millisecond); err == nil {
		t.Fatal("the fence reopened before the switched dataset was checked")
	}
	if err := manager.StorageHold(); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("StorageHold while the switched dataset is unchecked = %v, want ErrNotReady", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := manager.CurrentFence(ctx); err == nil {
		t.Fatal("CurrentFence opened while the switched state dataset is unchecked")
	}
	if recordedHold(t, authority) != nil {
		t.Fatal("a hold was raised although the switched dataset could not be read")
	}
	dataset.mirrorErrs.Store(0)
	eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
	hold := recordedHold(t, authority)
	stored, _ := dataset.GetWriteWatermark(context.Background())
	if hold.Reason != store.StorageRecoveryWatermarkRegressed || hold.StoredWatermark != 0 || stored.Value != 0 {
		t.Fatalf("hold = %#v with the dataset at %d; want the lost watermark held with no write in between", hold, stored.Value)
	}
}

// A dataset found holding a lower epoch is advanced, to fence out the
// replaced controller, only after the hold that keeps the evidence is
// recorded.
func TestHeldDatasetIsFencedOnlyAfterTheHoldIsRecorded(t *testing.T) {
	authorityDB, err := sqlite.NewDB(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authorityDB.Close() })
	records := newMemoryStorageRecords()
	records.record.Acknowledgements = []store.StorageAcknowledgement{{
		Contract: storagev1.StateContract, DatasetID: "dataset-a", Epoch: 5, AcknowledgedAt: time.Now().UTC(),
	}}
	authority := &holdRecordingStore{
		installationEpochStore: installationEpochStore{
			ControllerEpochStore: sqlite.NewStore(authorityDB, "authority"), memoryStorageRecords: records,
			installationID: "installation-a",
		},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	dataset := newRecordingEpochDataset("dataset-a")
	manager := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, dataset)
	startEpochManager(t, manager)
	select {
	case <-authority.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the lost epoch never raised a hold")
	}
	requireHeld(t, manager, storagev1.StateContract)
	time.Sleep(5 * durabilityTestInterval)
	if len(dataset.advanceRequests()) != 0 {
		t.Fatal("the dataset was advanced before the hold that keeps its evidence was recorded")
	}
	close(authority.release)
	eventually(t, "the held dataset to be fenced", func() bool { return len(dataset.advanceRequests()) == 1 })
	record, _ := authority.GetStorageRecord(context.Background(), store.DefaultControllerEpochName)
	if record.Hold == nil || record.Hold.Reason != store.StorageRecoveryEpochRegressed || record.Hold.StoredEpoch != 0 || record.Hold.AcknowledgedEpoch != 5 {
		t.Fatalf("recorded hold = %#v, want the lost epoch with its evidence", record.Hold)
	}
	requireHeld(t, manager, storagev1.StateContract)
}

// A manager with storage datasets reports storage as not ready until each
// dataset has passed its takeover checks, so a restarted controller creates
// no Job against storage it has not checked yet.
func TestStorageHoldFailsClosedUntilTheTakeoverChecksPass(t *testing.T) {
	authority, _ := newKubernetesControllerEpochAuthority(t)
	dataset := &gatedMirrorEpochDataset{recordingEpochDataset: newRecordingEpochDataset("dataset-a"), gate: make(chan struct{})}
	manager := fastEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, dataset)
	if err := manager.StorageHold(); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("StorageHold before the takeover = %v, want ErrNotReady", err)
	}
	startEpochManager(t, manager)
	eventually(t, "the takeover to read the dataset", func() bool { return dataset.reads.Load() > 0 })
	if err := manager.StorageHold(); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("StorageHold while the dataset is being checked = %v, want ErrNotReady", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "prompt"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1}}
	r := newUnitReconciler(newTestScheme(), task, agent)
	r.ControllerEpochManager = manager
	if result, err := r.createTaskJob(context.Background(), task.DeepCopy(), agent, nil); err != nil || result.RequeueAfter != storageHoldRequeue {
		t.Fatalf("createTaskJob before the record was read = %#v, %v; want a requeue", result, err)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("a Job was created before the storage record was read: %d, %v", len(jobs.Items), err)
	}
	close(dataset.gate)
	eventually(t, "the dataset to pass its checks", func() bool { return manager.StorageHold() == nil })
}

// gatedMirrorEpochDataset holds every epoch read until gate closes.
type gatedMirrorEpochDataset struct {
	*recordingEpochDataset
	gate  chan struct{}
	reads atomic.Int32
}

func (d *gatedMirrorEpochDataset) GetControllerEpochMirror(ctx context.Context, name string) (storagev1.ControllerEpochMirrorState, error) {
	d.reads.Add(1)
	select {
	case <-d.gate:
	case <-ctx.Done():
		return storagev1.ControllerEpochMirrorState{}, ctx.Err()
	}
	return d.recordingEpochDataset.GetControllerEpochMirror(ctx, name)
}

// A hold raised while a Job is being prepared stops the Job at its creation:
// the hold is checked again there, under the admission lock that setting a
// hold takes.
func TestHoldRaisedWhileAJobIsPreparedStopsItsCreation(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "prompt"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1}}
	r := newUnitReconciler(newTestScheme(), task, agent)
	manager := &ControllerEpochManager{ready: make(chan struct{})}
	r.ControllerEpochManager = manager
	withWatch, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit reconciler client does not implement client.WithWatch")
	}
	var raised sync.Once
	r.APIReader = interceptor.NewClient(withWatch, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, isTask := object.(*corev1alpha1.Task); isTask {
				raised.Do(func() {
					manager.setHold(store.StorageRecoveryHold{ID: "hold-1", Reason: store.StorageRecoveryUnsafeSwitch})
				})
			}
			return delegate.Get(ctx, key, object, options...)
		},
	})

	result, err := r.createTaskJob(ctx, task.DeepCopy(), agent, nil)
	if err != nil || result.RequeueAfter != storageHoldRequeue {
		t.Fatalf("createTaskJob with a hold raised during preparation = %#v, %v; want a requeue", result, err)
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("a Job was created after the hold was raised: %d, %v", len(jobs.Items), err)
	}
}

// A hold takes effect at once, setting it returns only after an admitted
// creation finished, and nothing is admitted once it is set.
func TestSettingAHoldWaitsForAdmittedWork(t *testing.T) {
	manager := &ControllerEpochManager{ready: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	admitted := make(chan error, 1)
	go func() {
		admitted <- manager.AdmitWork(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	held := make(chan struct{})
	go func() {
		manager.setHold(store.StorageRecoveryHold{ID: "hold-1", Reason: store.StorageRecoveryUnsafeSwitch})
		close(held)
	}()
	// The hold takes effect at once for everything else, and setting it
	// returns only after the admitted work finished.
	eventually(t, "the hold to take effect", func() bool {
		return errors.Is(manager.StorageHold(), store.ErrStorageRecoveryRequired)
	})
	select {
	case <-held:
		t.Fatal("setting the hold returned while admitted work was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-admitted; err != nil {
		t.Fatalf("admitted work = %v", err)
	}
	<-held
	ran := false
	err := manager.AdmitWork(context.Background(), func(context.Context) error {
		ran = true
		return nil
	})
	if ran || !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("work after the hold ran=%v err=%v; want it refused", ran, err)
	}
}

// holdAfterRevokeStore runs revoked after each Task Job revocation the
// store accepts.
type holdAfterRevokeStore struct {
	*sqlite.Store
	revoked func()
}

func (s holdAfterRevokeStore) RevokeTaskJob(ctx context.Context, identity store.TaskJobIdentity) error {
	if err := s.Store.RevokeTaskJob(ctx, identity); err != nil {
		return err
	}
	s.revoked()
	return nil
}

// A hold raised after a finishing Task's Job authority was revoked, and
// before its status is written, keeps the Task from reporting success until
// the hold is released. Other status updates keep working while held.
func TestHoldRaisedAfterJobRevocationKeepsTheTaskFromSucceeding(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "prompt"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, JobName: "task-job", JobUID: "job-uid"},
	}
	r := newUnitReconciler(newTestScheme(), task)
	manager := &ControllerEpochManager{ready: make(chan struct{})}
	r.ControllerEpochManager = manager
	results, ok := r.ResultStore.(*sqlite.Store)
	if !ok {
		t.Fatalf("unit reconciler result store is %T, want *sqlite.Store", r.ResultStore)
	}
	revocations := 0
	var raised sync.Once
	r.ResultStore = holdAfterRevokeStore{Store: results, revoked: func() {
		revocations++
		raised.Do(func() {
			manager.setHold(store.StorageRecoveryHold{ID: "hold-1", Reason: store.StorageRecoveryUnsafeSwitch})
		})
	}}
	succeed := func(task *corev1alpha1.Task) {
		task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		task.Status.Message = "done"
	}
	phase := func() corev1alpha1.TaskPhase {
		t.Helper()
		var current corev1alpha1.Task
		if err := r.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
			t.Fatal(err)
		}
		return current.Status.Phase
	}

	err := r.updateStatusWithRetry(ctx, task.DeepCopy(), succeed)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("success reported after a hold was raised = %v; want ErrStorageRecoveryRequired", err)
	}
	if revocations != 1 {
		t.Fatalf("Job revocations before the status write = %d, want 1", revocations)
	}
	if got := phase(); got != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("Task phase while held = %q, want Running", got)
	}
	if err := r.updateStatusWithRetry(ctx, task.DeepCopy(), func(task *corev1alpha1.Task) {
		task.Status.Message = "waiting for storage recovery"
	}); err != nil {
		t.Fatalf("status update that reports no success while held = %v", err)
	}

	manager.clearHold()
	if err := r.updateStatusWithRetry(ctx, task.DeepCopy(), succeed); err != nil {
		t.Fatalf("success reported after the hold was released = %v", err)
	}
	if got := phase(); got != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("Task phase after the hold was released = %q, want Succeeded", got)
	}
}
