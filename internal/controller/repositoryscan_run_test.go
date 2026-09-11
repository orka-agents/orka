package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

// Existing controller fixtures predate owner checks. Give their declared scan
// Tasks the same controller reference that production task creation installs.
func repositoryScanTestObjects(scan *corev1alpha1.RepositoryScan, objects ...client.Object) []client.Object {
	for _, object := range objects {
		if task, ok := object.(*corev1alpha1.Task); ok && metav1.GetControllerOf(task) == nil &&
			task.Labels[labels.LabelSecurityTarget] == labels.SelectorValue(scan.Name) {
			task.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))}
		}
	}
	return append([]client.Object{scan}, objects...)
}

func repositoryScanRunTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}, &corev1alpha1.Task{}).WithObjects(objects...).Build()
}

func TestRepositoryScanReconcileRetiresStaleIdentityBeforeIngestion(t *testing.T) {
	for _, tt := range []struct {
		name       string
		uid        string
		generation int64
	}{
		{name: "edited", uid: "current-uid", generation: 1},
		{name: "recreated", uid: "previous-uid", generation: 2},
		{name: "unbound legacy run"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "identity-scan", Namespace: defaultNS, UID: "current-uid", Generation: 2},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
				Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "scan_old"},
			}
			run := &store.ScanRun{
				ID: "scan_old", Namespace: scan.Namespace, RepositoryScan: scan.Name,
				RepositoryScanUID: tt.uid, RepositoryScanGeneration: tt.generation,
				TaskName: "old-threat-model", Mode: "initial", Phase: scanRunPhaseRunning,
				PolicyDigest: security.ScannerPolicyDigest(security.ScannerPolicy{}),
			}
			require.NoError(t, db.CreateScanRun(ctx, run))
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: run.TaskName, Namespace: scan.Namespace,
					Labels: map[string]string{
						labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: run.ID,
						labels.LabelSecurityStage: security.StageThreatModel,
					},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, ResultRef: &corev1alpha1.ResultReference{Available: true}},
			}
			result, err := json.Marshal(security.ThreatModelResultEnvelope{
				SchemaVersion: security.AgentResultSchemaVersion, Kind: security.AgentResultKindThreatModel,
				RepositoryScan: scan.Name, ScanID: run.ID, PolicyDigest: run.PolicyDigest, ThreatModel: "# Stale threat model",
			})
			require.NoError(t, err)
			require.NoError(t, db.SaveResult(ctx, scan.Namespace, task.Name, result))
			objects := repositoryScanTestObjects(scan, task, repositoryScanTestAgent(scan.Spec.AnalysisAgentRef.Name))
			task.OwnerReferences[0].UID = types.UID(tt.uid)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(scan).WithObjects(objects...).Build()
			r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: db, ResultStore: db}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)}
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			pending, err := db.GetScanRun(ctx, scan.Namespace, run.ID)
			require.NoError(t, err)
			require.True(t, pending.CancellationPending)
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			retired, err := db.GetScanRun(ctx, scan.Namespace, run.ID)
			require.NoError(t, err)
			require.Equal(t, scanRunPhaseFailed, retired.Phase)
			require.Equal(t, tt.uid, retired.RepositoryScanUID)
			require.Equal(t, tt.generation, retired.RepositoryScanGeneration)
			require.NotNil(t, retired.CompletedAt)
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(ctx, request.NamespacedName, current))
			require.Empty(t, current.Status.LastScanID)
			require.Equal(t, repositoryScanPhasePending, current.Status.Phase)

			// An unscheduled scan starts a replacement instead of remaining stuck
			// in Scanning, and late output from the old run stays unconsumed.
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			require.NoError(t, cl.Get(ctx, request.NamespacedName, current))
			require.NotEqual(t, run.ID, current.Status.LastScanID)
			replacement, err := db.GetScanRun(ctx, scan.Namespace, current.Status.LastScanID)
			require.NoError(t, err)
			require.True(t, security.ScanRunMatchesRepositoryScan(replacement, current))
			_, err = db.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestRepositoryScanDeletionReleasesRunReservation(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "deleted-scan", Namespace: defaultNS, UID: "deleted-uid", Generation: 1,
		Finalizers: []string{security.RepositoryScanRunFinalizer},
	}}
	run := &store.ScanRun{
		ID: "scan_deleted", Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, Phase: scanRunPhasePending,
	}
	require.NoError(t, db.CreateScanRun(ctx, run))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(scan).WithObjects(scan).Build()
	require.NoError(t, cl.Delete(ctx, scan))
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: db}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)})
	require.NoError(t, err)
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(scan), &corev1alpha1.RepositoryScan{}), "cleanup must retain the finalizer for confirmation")
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)})
	require.NoError(t, err)
	err = cl.Get(ctx, client.ObjectKeyFromObject(scan), &corev1alpha1.RepositoryScan{})
	require.True(t, apierrors.IsNotFound(err))
	retired, err := db.GetScanRun(ctx, scan.Namespace, run.ID)
	require.NoError(t, err)
	require.Equal(t, scanRunPhaseFailed, retired.Phase)
	require.NotNil(t, retired.CompletedAt)
}

func TestRepositoryScanStatusRejectsChangedIdentity(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	current := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "edited-scan", Namespace: defaultNS, UID: "current-uid", Generation: 2,
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(current).WithObjects(current).Build()
	r := &RepositoryScanReconciler{Client: cl}
	stale := current.DeepCopy()
	stale.Generation = 1
	err := r.updateStatusWithRetry(ctx, stale, func(scan *corev1alpha1.RepositoryScan) {
		scan.Status.LastScanID = "stale-run"
	})
	require.ErrorIs(t, err, store.ErrConflict)
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(current), current))
	require.Empty(t, current.Status.LastScanID)
}

func TestRepositoryScanStatusRejectsChangedRunBindingBeforeMutation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	current := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1},
		Status:     corev1alpha1.RepositoryScanStatus{LastScanID: "scan_manual", Phase: repositoryScanPhaseScanning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(current).WithObjects(current).Build()
	r := &RepositoryScanReconciler{Client: cl}
	stale := current.DeepCopy()
	stale.Status = corev1alpha1.RepositoryScanStatus{}
	mutated := false
	err := r.updateStatusWithRetry(ctx, stale, func(scan *corev1alpha1.RepositoryScan) {
		mutated = true
		scan.Status.Phase = repositoryScanPhasePending
	})
	require.ErrorIs(t, err, store.ErrConflict)
	require.False(t, mutated)
	after := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(current), after))
	require.Equal(t, current.Status, after.Status)
}

func TestRepositoryScanReconcileUsesLiveStatusAfterFinalizerPatch(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 1,
	}}
	base := repositoryScanRunTestClient(t, scan)
	stale := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), stale))
	cached := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if current, ok := object.(*corev1alpha1.RepositoryScan); ok {
				*current = *stale.DeepCopy()
				return nil
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	r := &RepositoryScanReconciler{Client: cached, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)})
	require.NoError(t, err)
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	require.Contains(t, current.Finalizers, security.RepositoryScanRunFinalizer)
	require.Equal(t, repositoryScanPhasePending, current.Status.Phase)
	require.NotEqual(t, stale.ResourceVersion, current.ResourceVersion)
}

func TestRepositoryScanReconcileDoesNotRetireNewerRunFromStaleCache(t *testing.T) {
	for _, transition := range []string{"edited", "recreated", "deletion started"} {
		t.Run(transition, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			stale := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 2,
					Finalizers: []string{security.RepositoryScanRunFinalizer},
				},
				Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "scan_old"},
			}
			current := stale.DeepCopy()
			current.Status.LastScanID = "scan_new"
			switch transition {
			case "edited":
				current.Generation++
			case "recreated":
				current.UID, current.Generation = "replacement-uid", 1
			case "deletion started":
				current.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			}
			run := &store.ScanRun{
				ID: "scan_new", Namespace: current.Namespace, RepositoryScan: current.Name,
				RepositoryScanUID: string(current.UID), RepositoryScanGeneration: current.Generation,
				Phase: scanRunPhaseRunning,
			}
			require.NoError(t, db.CreateScanRun(ctx, run))
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "current-mapper", Namespace: current.Namespace, UID: "task-uid",
					Labels: map[string]string{
						labels.LabelSecurityTarget: current.Name, labels.LabelSecurityScanID: run.ID, labels.LabelSecurityStage: security.StageMapper,
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(current, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			live := repositoryScanRunTestClient(t, current, task)
			cached := interceptor.NewClient(live, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if scan, ok := object.(*corev1alpha1.RepositoryScan); ok {
						*scan = *stale.DeepCopy()
						return nil
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			r := &RepositoryScanReconciler{Client: cached, APIReader: live, Scheme: live.Scheme(), SecurityStore: db}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(current)})
			require.ErrorIs(t, err, store.ErrConflict)
			// Identity may change after finalizer admission and before cleanup
			// collects the replacement object's newly admitted run.
			_, err = security.RetireStaleScanRuns(ctx, db, cached, live, stale)
			require.ErrorIs(t, err, store.ErrConflict)
			after, err := db.GetScanRun(ctx, run.Namespace, run.ID)
			require.NoError(t, err)
			require.Equal(t, scanRunPhaseRunning, after.Phase)
			require.Zero(t, after.CancellationVersion)
			require.False(t, after.CancellationPending)
			preserved := &corev1alpha1.Task{}
			require.NoError(t, live.Get(ctx, client.ObjectKeyFromObject(task), preserved))
			require.True(t, preserved.DeletionTimestamp.IsZero())
		})
	}
}

func TestRepositoryScanReconcileUsesLiveCompletionForSchedule(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	completed := metav1.Now()
	current := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scheduled-scan", Namespace: defaultNS, UID: "scan-uid", Generation: 1,
			Finalizers: []string{security.RepositoryScanRunFinalizer},
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Schedule: "@every 1h",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
		Status: corev1alpha1.RepositoryScanStatus{
			Phase: repositoryScanPhaseReady, LastScanID: "scan_completed",
			LastSuccessfulScanAt: &completed, LastProcessedCommit: "completed-head",
		},
	}
	run := &store.ScanRun{
		ID: current.Status.LastScanID, Namespace: current.Namespace, RepositoryScan: current.Name,
		RepositoryScanUID: string(current.UID), RepositoryScanGeneration: current.Generation,
		Phase: scanRunPhaseSucceeded, HeadCommit: current.Status.LastProcessedCommit, CompletedAt: &completed.Time,
	}
	require.NoError(t, db.CreateScanRun(ctx, run))
	stale := current.DeepCopy()
	stale.Status.Phase = repositoryScanPhaseScanning
	stale.Status.LastSuccessfulScanAt = &metav1.Time{Time: completed.Add(-2 * time.Hour)}
	stale.Status.LastProcessedCommit = "previous-head"
	agent := repositoryScanTestAgent(current.Spec.AnalysisAgentRef.Name)
	cached := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(stale).WithObjects(stale, agent).Build()
	live := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(current).WithObjects(current, agent).Build()
	r := &RepositoryScanReconciler{Client: cached, APIReader: live, Scheme: scheme, SecurityStore: db}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(current)})
	require.NoError(t, err)
	runs, _, err := db.ListScanRuns(ctx, current.Namespace, current.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1, "stale cache data must not admit another run after completion")
	require.Equal(t, run.ID, runs[0].ID)
	require.Greater(t, result.RequeueAfter, 59*time.Minute)
}

func TestCreateScanRunRollsBackChangedIdentity(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	base := repositoryScanRunTestClient(t, scan, repositoryScanTestAgent(scan.Spec.AnalysisAgentRef.Name))
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if _, ok := object.(*corev1alpha1.Task); !ok {
				return nil
			}
			current := &corev1alpha1.RepositoryScan{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
				return err
			}
			current.Generation++
			current.Spec.SubPath = "edited"
			return c.Update(ctx, current)
		},
	})
	r := &RepositoryScanReconciler{Client: cl, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
	require.ErrorIs(t, r.createScanRun(ctx, scan, "initial", "", ""), store.ErrConflict)
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	_, err := security.RetireStaleScanRuns(ctx, db, base, base, current)
	require.NoError(t, err)
	runs, _, err := db.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, scanRunPhaseFailed, runs[0].Phase)
	require.NotNil(t, runs[0].CompletedAt)
	require.True(t, apierrors.IsNotFound(base.Get(ctx, client.ObjectKey{Namespace: scan.Namespace, Name: runs[0].TaskName}, &corev1alpha1.Task{})))
}

type scanRunAdmissionHookStore struct {
	store.SecurityStore
	afterCreate func()
}

func (s *scanRunAdmissionHookStore) CreateScanRun(ctx context.Context, run *store.ScanRun) error {
	if err := s.SecurityStore.CreateScanRun(ctx, run); err != nil {
		return err
	}
	s.afterCreate()
	return nil
}

func TestCreateScanRunFencesInitialTaskAdmission(t *testing.T) {
	for _, failure := range []string{"retired reservation", "lost create response", "failed ownership read"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 1},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			base := repositoryScanRunTestClient(t, scan, repositoryScanTestAgent(scan.Spec.AnalysisAgentRef.Name))
			retire := func() {
				current := &corev1alpha1.RepositoryScan{}
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
				current.Generation++
				require.NoError(t, base.Update(ctx, current))
				_, err := security.RetireStaleScanRuns(ctx, db, base, base, current)
				require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
				_, err = security.RetireStaleScanRuns(ctx, db, base, base, current)
				require.NoError(t, err)
			}
			admissionErr := fmt.Errorf("injected admission failure")
			created := false
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if task, ok := object.(*corev1alpha1.Task); ok {
						retire()
						task.Finalizers = []string{"test.orka.ai/hold-cleanup"}
					}
					if err := c.Create(ctx, object, opts...); err != nil {
						return err
					}
					created = true
					if failure == "lost create response" {
						return admissionErr
					}
					return nil
				},
			})
			reader := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1alpha1.RepositoryScan); ok && created && failure == "failed ownership read" {
						return admissionErr
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			r := &RepositoryScanReconciler{Client: cl, APIReader: reader, Scheme: base.Scheme(), SecurityStore: db}
			if failure == "retired reservation" {
				r.SecurityStore = &scanRunAdmissionHookStore{SecurityStore: db, afterCreate: retire}
				admissionErr = store.ErrConflict
			}
			require.ErrorIs(t, r.createScanRun(ctx, scan, "initial", "", ""), admissionErr)
			runs, _, err := db.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.True(t, runs[0].CancellationPending, "late creation must request durable cleanup even after retirement completed")
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
			if failure == "retired reservation" {
				require.False(t, created)
			} else {
				task := &corev1alpha1.Task{}
				require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: scan.Namespace, Name: runs[0].TaskName}, task))
				require.False(t, task.DeletionTimestamp.IsZero())
				_, err = security.RetireStaleScanRuns(ctx, db, base, base, current)
				require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
				task.Finalizers = nil
				require.NoError(t, base.Update(ctx, task))
			}
			_, err = security.RetireStaleScanRuns(ctx, db, base, base, current)
			require.NoError(t, err)
			retired, err := db.GetScanRun(ctx, scan.Namespace, runs[0].ID)
			require.NoError(t, err)
			require.False(t, retired.CancellationPending)
		})
	}
}

func TestCurrentRepositoryScanTasksIgnoresLateTasksFromOlderRun(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1}}
	old := &store.ScanRun{
		ID: "scan_old", Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		Phase: scanRunPhaseFailed, StartedAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, db.CreateScanRun(ctx, old))
	newer := *old
	newer.ID, newer.Phase, newer.StartedAt = "scan_new", scanRunPhaseSucceeded, time.Now()
	require.NoError(t, db.CreateScanRun(ctx, &newer))
	late := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "late", Namespace: scan.Namespace,
		Labels: map[string]string{labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: old.ID, labels.LabelSecurityStage: security.StageMapper},
	}}
	repositoryScanTestObjects(scan, late)
	tasks, err := security.CurrentRepositoryScanTasks(ctx, db, scan, []corev1alpha1.Task{*late})
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestRetireStaleScanRunsFindsReservationsBeyondFirstPage(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "current-uid", Generation: 2}}
	old := &store.ScanRun{ID: "scan_old", Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhasePending}
	require.NoError(t, db.CreateScanRun(ctx, old))
	for i := range 101 {
		require.NoError(t, db.CreateScanRun(ctx, &store.ScanRun{
			ID: fmt.Sprintf("scan_history_%d", i), Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhaseSucceeded,
			RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		}))
	}
	cleanupStore := &boundedScanRunCleanupStore{SecurityStore: db, t: t}
	_, err := security.RetireStaleScanRuns(ctx, cleanupStore, repositoryScanRunTestClient(t, scan), nil, scan)
	require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
	require.Equal(t, 1, cleanupStore.historyQueries)
	_, err = security.RetireStaleScanRuns(ctx, cleanupStore, repositoryScanRunTestClient(t, scan), nil, scan)
	require.NoError(t, err)
	require.Equal(t, 2, cleanupStore.historyQueries)
	after, err := db.GetScanRun(ctx, scan.Namespace, old.ID)
	require.NoError(t, err)
	require.Equal(t, scanRunPhaseFailed, after.Phase)
}

type boundedScanRunCleanupStore struct {
	store.SecurityStore
	t              *testing.T
	historyQueries int
}

func (s *boundedScanRunCleanupStore) ListScanRuns(ctx context.Context, namespace, repositoryScan string, limit int, cursor string) ([]store.ScanRun, string, error) {
	s.t.Helper()
	require.Equal(s.t, 1, limit, "cleanup must only read the newest historical run")
	require.Empty(s.t, cursor, "cleanup must not page through history")
	s.historyQueries++
	return s.SecurityStore.ListScanRuns(ctx, namespace, repositoryScan, limit, cursor)
}

func TestRetireStaleScanRunsChecksTerminalStatusBinding(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 2},
		Status:     corev1alpha1.RepositoryScanStatus{LastScanID: "scan_old"},
	}
	old := &store.ScanRun{
		ID: scan.Status.LastScanID, Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: 1, Phase: scanRunPhaseSucceeded,
	}
	require.NoError(t, db.CreateScanRun(ctx, old))
	newer := *old
	newer.ID, newer.RepositoryScanGeneration = "scan_new", scan.Generation
	require.NoError(t, db.CreateScanRun(ctx, &newer))
	cl := repositoryScanRunTestClient(t, scan)
	stale, err := security.RetireStaleScanRuns(ctx, db, cl, nil, scan)
	require.NoError(t, err)
	require.True(t, stale)
	after, err := db.GetScanRun(ctx, scan.Namespace, old.ID)
	require.NoError(t, err)
	require.Equal(t, scanRunPhaseSucceeded, after.Phase)

	scan.Generation = 1
	_, err = security.RetireStaleScanRuns(ctx, db, cl, nil, scan)
	require.ErrorIs(t, err, store.ErrConflict, "a completed newer generation still fences a stale caller")
}

func TestRetireStaleScanRunsCleansTasksFromTerminalHistory(t *testing.T) {
	for _, tt := range []struct {
		name       string
		phase      string
		generation int64
		want       string
	}{
		{name: "failed history", phase: scanRunPhaseFailed, generation: 1, want: "cleanup"},
		{name: "succeeded history", phase: scanRunPhaseSucceeded, generation: 1, want: "cleanup"},
		{name: "current configuration", phase: scanRunPhaseFailed, generation: 2, want: "preserve"},
		{name: "newer configuration", phase: scanRunPhaseFailed, generation: 3, want: "conflict"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 2},
				Status:     corev1alpha1.RepositoryScanStatus{LastScanID: "latest"},
			}
			completed := time.Now().UTC().Truncate(time.Second)
			history := &store.ScanRun{
				ID: "history", Namespace: scan.Namespace, RepositoryScan: scan.Name,
				RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: tt.generation,
				Phase: tt.phase, CompletedAt: &completed, Summary: "original result", ReviewedSliceCount: 3,
			}
			if tt.phase == scanRunPhaseFailed {
				history.ErrorMessage = "review stage failed"
			}
			require.NoError(t, db.CreateScanRun(ctx, history))
			latest := &store.ScanRun{
				ID: scan.Status.LastScanID, Namespace: scan.Namespace, RepositoryScan: scan.Name,
				RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, Phase: scanRunPhaseSucceeded,
			}
			require.NoError(t, db.CreateScanRun(ctx, latest))
			active := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "active-review", Namespace: scan.Namespace, UID: "active-uid", Finalizers: []string{"test.orka.ai/hold-cleanup"},
					Labels: map[string]string{
						labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: history.ID, labels.LabelSecurityStage: security.StageReview,
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			failed := active.DeepCopy()
			failed.Name, failed.UID, failed.Status.Phase = "failed-review", "failed-uid", corev1alpha1.TaskPhaseFailed
			cl := repositoryScanRunTestClient(t, scan, active, failed)
			cleanupStore := &boundedScanRunCleanupStore{SecurityStore: db, t: t}
			_, err := security.RetireStaleScanRuns(ctx, cleanupStore, cl, cl, scan)
			if tt.want != "cleanup" {
				if tt.want == "conflict" {
					require.ErrorContains(t, err, "a newer repository scan generation has already admitted a run")
				} else {
					require.NoError(t, err)
				}
				after, err := db.GetScanRun(ctx, scan.Namespace, history.ID)
				require.NoError(t, err)
				require.Zero(t, after.CancellationVersion)
				preserved := &corev1alpha1.Task{}
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(active), preserved))
				require.True(t, preserved.DeletionTimestamp.IsZero())
				return
			}
			require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
			_, err = security.RetireStaleScanRuns(ctx, cleanupStore, cl, cl, scan)
			require.ErrorIs(t, err, security.ErrScanRunCancellationPending, "deleting siblings must keep admission blocked")
			pending, err := db.GetScanRun(ctx, scan.Namespace, history.ID)
			require.NoError(t, err)
			require.True(t, pending.CancellationPending)
			deleting := &corev1alpha1.Task{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(active), deleting))
			require.False(t, deleting.DeletionTimestamp.IsZero())
			deleting.Finalizers = nil
			require.NoError(t, cl.Update(ctx, deleting))
			stale, err := security.RetireStaleScanRuns(ctx, cleanupStore, cl, cl, scan)
			require.NoError(t, err)
			require.False(t, stale, "the latest run still owns status")
			after, err := db.GetScanRun(ctx, scan.Namespace, history.ID)
			require.NoError(t, err)
			require.False(t, after.CancellationPending)
			require.Equal(t, tt.phase, after.Phase)
			require.Equal(t, history.CompletedAt, after.CompletedAt)
			require.Equal(t, history.Summary, after.Summary)
			require.Equal(t, history.ErrorMessage, after.ErrorMessage)
			require.Equal(t, history.ReviewedSliceCount, after.ReviewedSliceCount)
			untouched, err := db.GetScanRun(ctx, scan.Namespace, latest.ID)
			require.NoError(t, err)
			require.Zero(t, untouched.CancellationVersion)
			preserved := &corev1alpha1.Task{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(failed), preserved))
			require.True(t, preserved.DeletionTimestamp.IsZero())
		})
	}
}

func TestRetireStaleScanRunsCancelsOwnedPipelineBeforeRelease(t *testing.T) {
	for _, scenario := range []string{"edited", "legacy", "deletion fails"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
				Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 2,
			}}
			run := &store.ScanRun{
				ID: "scan_old", Namespace: scan.Namespace, RepositoryScan: scan.Name,
				RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: 1, Phase: scanRunPhaseRunning,
			}
			if scenario == "legacy" {
				run.RepositoryScanUID, run.RepositoryScanGeneration = "", 0
			}
			require.NoError(t, db.CreateScanRun(ctx, run))
			owned := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "owned", Namespace: scan.Namespace, UID: "owned-uid",
					Labels: map[string]string{
						labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: run.ID, labels.LabelSecurityStage: security.StageMapper,
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			foreign := owned.DeepCopy()
			foreign.Name, foreign.UID, foreign.OwnerReferences[0].UID = "foreign", "foreign-uid", "other-owner"
			terminal := owned.DeepCopy()
			terminal.Name, terminal.UID, terminal.Status.Phase = "terminal", "terminal-uid", corev1alpha1.TaskPhaseSucceeded
			validation := owned.DeepCopy()
			validation.Name, validation.UID = "validation", "validation-uid"
			validation.Labels[labels.LabelSecurityStage] = security.StageValidation
			base := repositoryScanRunTestClient(t, scan, owned, foreign, terminal, validation)
			deletionErr := fmt.Errorf("task deletion denied")
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					before, err := db.GetScanRun(ctx, run.Namespace, run.ID)
					require.NoError(t, err)
					require.Equal(t, scanRunPhaseRunning, before.Phase, "cancellation must precede reservation release")
					if scenario == "deletion fails" {
						return deletionErr
					}
					return c.Delete(ctx, object, opts...)
				},
			})
			_, err := security.RetireStaleScanRuns(ctx, db, cl, base, scan)
			after, getErr := db.GetScanRun(ctx, run.Namespace, run.ID)
			require.NoError(t, getErr)
			if scenario == "deletion fails" {
				require.ErrorIs(t, err, deletionErr)
				require.Equal(t, scanRunPhaseRunning, after.Phase)
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(owned), &corev1alpha1.Task{}))
			} else {
				require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
				require.True(t, after.CancellationPending)
				require.Equal(t, scanRunPhaseRunning, after.Phase)
				require.True(t, apierrors.IsNotFound(base.Get(ctx, client.ObjectKeyFromObject(owned), &corev1alpha1.Task{})))
				_, err = security.RetireStaleScanRuns(ctx, db, cl, base, scan)
				require.NoError(t, err)
				after, err = db.GetScanRun(ctx, run.Namespace, run.ID)
				require.NoError(t, err)
				require.Equal(t, scanRunPhaseFailed, after.Phase)
			}
			for _, preserved := range []*corev1alpha1.Task{foreign, terminal, validation} {
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(preserved), &corev1alpha1.Task{}))
			}
			if scenario != "deletion fails" {
				_, err = security.RetireStaleScanRuns(ctx, db, cl, base, scan)
				require.NoError(t, err)
				confirmed, err := db.GetScanRun(ctx, run.Namespace, run.ID)
				require.NoError(t, err)
				require.False(t, confirmed.CancellationPending)
				require.Equal(t, after.CancellationVersion, confirmed.CancellationVersion)
			}
		})
	}
}

func TestRepositoryScanReconcileRetriesScanRunCancellation(t *testing.T) {
	for _, failure := range []string{"list", "delete"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			suspend := true
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 1},
				Spec:       corev1alpha1.RepositoryScanSpec{Suspend: &suspend},
				Status:     corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseReady, LastScanID: "winner"},
			}
			winner := &store.ScanRun{
				ID: "winner", Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhaseSucceeded,
				RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
			}
			require.NoError(t, db.CreateScanRun(ctx, winner))
			run := *winner
			run.ID, run.Phase = "loser", scanRunPhasePending
			require.NoError(t, db.CreateScanRun(ctx, &run))
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "losing-task", Namespace: scan.Namespace, UID: "task-uid",
					Labels: map[string]string{
						labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: run.ID, labels.LabelSecurityStage: security.StageThreatModel,
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			base := repositoryScanRunTestClient(t, scan, task)
			cleanupErr := fmt.Errorf("cleanup temporarily unavailable")
			failing := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1alpha1.TaskList); ok && failure == "list" {
						return cleanupErr
					}
					return c.List(ctx, list, opts...)
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return cleanupErr
				},
			})
			require.ErrorIs(t, security.RollbackScanRunAdmission(ctx, db, failing, failing, scan, &run), cleanupErr)
			pending, err := db.GetScanRun(ctx, run.Namespace, run.ID)
			require.NoError(t, err)
			require.True(t, pending.CancellationPending)
			require.Equal(t, scanRunPhasePending, pending.Phase)
			require.True(t, security.ScanRunMatchesRepositoryScan(pending, scan), "retry must work for an unchanged generation")
			restarted := &RepositoryScanReconciler{Client: base, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
			_, err = restarted.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)})
			require.NoError(t, err)
			_, err = restarted.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)})
			require.NoError(t, err)
			after, err := db.GetScanRun(ctx, run.Namespace, run.ID)
			require.NoError(t, err)
			require.Equal(t, scanRunPhaseFailed, after.Phase)
			require.False(t, after.CancellationPending)
			require.True(t, apierrors.IsNotFound(base.Get(ctx, client.ObjectKeyFromObject(task), &corev1alpha1.Task{})))
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
			require.Equal(t, winner.ID, current.Status.LastScanID)
		})
	}
}

func TestMapperStageCreationFencesConcurrentRetirement(t *testing.T) {
	for _, scenario := range []string{"retired before create", "retired during create", "late cleanup fails"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "scan-uid", Generation: 1},
				Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo"},
				Status:     corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "run"},
			}
			run := &store.ScanRun{
				ID: "run", Namespace: scan.Namespace, RepositoryScan: scan.Name, Mode: "initial", Phase: scanRunPhaseRunning,
				RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
				PolicyDigest: security.ScannerPolicyDigest(security.ScannerPolicy{}),
			}
			target := newSucceededSecurityTask("scan-target", run.ID, security.StageThreatModel, metav1.Now())
			target.Labels[labels.LabelSecurityTarget] = scan.Name
			target.Spec.Workspace = repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentRead)
			run.TaskName = target.Name
			require.NoError(t, db.CreateScanRun(ctx, run))
			base := repositoryScanRunTestClient(t, repositoryScanTestObjects(scan, target)...)
			retire := func() *store.ScanRun {
				t.Helper()
				current := &corev1alpha1.RepositoryScan{}
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
				current.Generation++
				current.Spec.SubPath = "edited"
				require.NoError(t, base.Update(ctx, current))
				_, err := security.RetireStaleScanRuns(ctx, db, base, base, current)
				require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
				_, err = security.RetireStaleScanRuns(ctx, db, base, base, current)
				require.NoError(t, err)
				retired, err := db.GetScanRun(ctx, run.Namespace, run.ID)
				require.NoError(t, err)
				return retired
			}
			var retired *store.ScanRun
			if scenario == "retired before create" {
				retired = retire()
			}
			created := 0
			cleanupErr := fmt.Errorf("late Task deletion unavailable")
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if _, ok := object.(*corev1alpha1.Task); ok {
						created++
						retired = retire()
					}
					return c.Create(ctx, object, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					if scenario == "late cleanup fails" {
						return cleanupErr
					}
					return c.Delete(ctx, object, opts...)
				},
			})
			r := &RepositoryScanReconciler{Client: cl, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
			err := r.createMapperTask(ctx, scan, run)
			require.ErrorIs(t, err, store.ErrConflict)
			if scenario == "retired before create" {
				require.Zero(t, created)
			} else {
				require.Equal(t, 1, created)
			}
			if scenario == "late cleanup fails" {
				require.ErrorIs(t, err, cleanupErr)
				pending, err := db.ListScanRunsPendingCancellation(ctx, scan.Namespace, scan.Name)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.Equal(t, scanRunPhaseFailed, pending[0].Phase, "late cleanup must remain retryable for a terminal run")
			}
			restarted := &RepositoryScanReconciler{Client: base, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
			for range 2 {
				current := &corev1alpha1.RepositoryScan{}
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
				_, err = restarted.reconcileScanRunIdentity(ctx, current)
				require.NoError(t, err)
			}
			var tasks corev1alpha1.TaskList
			require.NoError(t, base.List(ctx, &tasks, client.MatchingLabels{labels.LabelSecurityStage: security.StageMapper}))
			require.Empty(t, tasks.Items)
			after, err := db.GetScanRun(ctx, run.Namespace, run.ID)
			require.NoError(t, err)
			require.Equal(t, scanRunPhaseFailed, after.Phase)
			require.False(t, after.CancellationPending)
			require.Equal(t, retired.CompletedAt, after.CompletedAt)
			require.Equal(t, retired.Summary, after.Summary)
		})
	}
}

func TestRepositoryScanCancellationConfirmsLateTasksAfterCreatorCrash(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1},
		Status:     corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "old-run"},
	}
	run := &store.ScanRun{
		ID: scan.Status.LastScanID, Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhaseRunning,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
	}
	require.NoError(t, db.CreateScanRun(ctx, run))
	base := repositoryScanRunTestClient(t, scan)
	creator := &RepositoryScanReconciler{Client: base, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
	require.NoError(t, creator.validateScanStageRun(ctx, scan, run))
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	current.Generation++
	require.NoError(t, base.Update(ctx, current))
	_, err := security.RetireStaleScanRuns(ctx, db, base, base, current)
	require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
	// The creator's preflight succeeded, but its Create response is lost and it
	// never reaches post-create validation. No in-memory compensation can run.
	late := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "late-mapper", Namespace: scan.Namespace, UID: "task-uid", Finalizers: []string{"test.orka.ai/hold-cleanup"},
			Labels: map[string]string{
				labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: run.ID, labels.LabelSecurityStage: security.StageMapper,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	require.NoError(t, base.Create(ctx, late))
	restarted := &RepositoryScanReconciler{Client: base, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)}
	for range 2 {
		_, err = restarted.Reconcile(ctx, req)
		require.NoError(t, err)
		pending, err := db.GetScanRun(ctx, run.Namespace, run.ID)
		require.NoError(t, err)
		require.True(t, pending.CancellationPending)
		require.Equal(t, scanRunPhaseRunning, pending.Phase)
	}
	deleting := &corev1alpha1.Task{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(late), deleting))
	require.False(t, deleting.DeletionTimestamp.IsZero())
	replacement := &store.ScanRun{ID: "replacement", Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhasePending}
	require.ErrorIs(t, db.CreateScanRun(ctx, replacement), store.ErrConflict)
	deleting.Finalizers = nil
	require.NoError(t, base.Update(ctx, deleting))
	_, err = restarted.Reconcile(ctx, req)
	require.NoError(t, err)
	retired, err := db.GetScanRun(ctx, run.Namespace, run.ID)
	require.NoError(t, err)
	require.Equal(t, scanRunPhaseFailed, retired.Phase)
	require.False(t, retired.CancellationPending)
	require.NoError(t, db.CreateScanRun(ctx, replacement))
}

func TestRepositoryScanCancellationRediscoversLateTasksAfterReplacement(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1},
		Status:     corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "old-run"},
	}
	run := &store.ScanRun{
		ID: scan.Status.LastScanID, Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhaseRunning,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
	}
	require.NoError(t, db.CreateScanRun(ctx, run))
	base := repositoryScanRunTestClient(t, scan)
	creator := &RepositoryScanReconciler{Client: base, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
	require.NoError(t, creator.validateScanStageRun(ctx, scan, run))
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	current.Generation++
	require.NoError(t, base.Update(ctx, current))
	_, err := security.RetireStaleScanRuns(ctx, db, base, base, current)
	require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
	_, err = security.RetireStaleScanRuns(ctx, db, base, base, current)
	require.NoError(t, err)
	confirmed, err := db.GetScanRun(ctx, scan.Namespace, run.ID)
	require.NoError(t, err)
	require.False(t, confirmed.CancellationPending)
	replacement := &store.ScanRun{
		ID: "replacement", Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhaseRunning,
		RepositoryScanUID: string(current.UID), RepositoryScanGeneration: current.Generation,
	}
	require.NoError(t, db.CreateScanRun(ctx, replacement))
	current.Status.LastScanID = replacement.ID
	require.NoError(t, base.Status().Update(ctx, current))
	// Create succeeds only after cleanup completed and a replacement became
	// newest. The creator crashes without reaching post-create validation.
	late := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "late-mapper", Namespace: scan.Namespace, UID: "late-uid", Finalizers: []string{"test.orka.ai/hold-cleanup"},
			Labels: map[string]string{
				labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: run.ID, labels.LabelSecurityStage: security.StageMapper,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	require.NoError(t, base.Create(ctx, late))
	newTask := late.DeepCopy()
	newTask.Name, newTask.UID = "current-mapper", "current-uid"
	newTask.ResourceVersion, newTask.Finalizers = "", nil
	newTask.Labels[labels.LabelSecurityScanID] = replacement.ID
	require.NoError(t, base.Create(ctx, newTask))
	restarted := &RepositoryScanReconciler{Client: base, APIReader: base, Scheme: base.Scheme(), SecurityStore: db}
	for range 2 {
		done, err := restarted.reconcileScanRunIdentity(ctx, current)
		require.NoError(t, err)
		require.True(t, done)
		pending, err := db.GetScanRun(ctx, scan.Namespace, run.ID)
		require.NoError(t, err)
		require.True(t, pending.CancellationPending)
		require.Equal(t, confirmed.CompletedAt, pending.CompletedAt)
	}
	deleting := &corev1alpha1.Task{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(late), deleting))
	require.False(t, deleting.DeletionTimestamp.IsZero())
	deleting.Finalizers = nil
	require.NoError(t, base.Update(ctx, deleting))
	_, err = restarted.reconcileScanRunIdentity(ctx, current)
	require.NoError(t, err)
	retired, err := db.GetScanRun(ctx, scan.Namespace, run.ID)
	require.NoError(t, err)
	require.False(t, retired.CancellationPending)
	require.Equal(t, confirmed.CompletedAt, retired.CompletedAt)
	untouched, err := db.GetScanRun(ctx, scan.Namespace, replacement.ID)
	require.NoError(t, err)
	require.Equal(t, scanRunPhaseRunning, untouched.Phase)
	require.Zero(t, untouched.CancellationVersion)
	preserved := &corev1alpha1.Task{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(newTask), preserved))
	require.True(t, preserved.DeletionTimestamp.IsZero())
}

func TestCreateScanRunBlocksCancellationObservedDuringTaskLookup(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
		Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseReady, LastScanID: "latest"},
	}
	completed := time.Now().UTC().Add(-time.Hour)
	old := &store.ScanRun{
		ID: "old", Namespace: scan.Namespace, RepositoryScan: scan.Name, Phase: scanRunPhaseFailed,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		StartedAt: completed.Add(-time.Hour), CompletedAt: &completed,
	}
	require.NoError(t, db.CreateScanRun(ctx, old))
	latest := *old
	latest.ID, latest.Phase, latest.StartedAt = scan.Status.LastScanID, scanRunPhaseSucceeded, completed
	require.NoError(t, db.CreateScanRun(ctx, &latest))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "late-mapper", Namespace: scan.Namespace, UID: "task-uid", Finalizers: []string{"test.orka.ai/hold-cleanup"},
			Labels: map[string]string{
				labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: old.ID, labels.LabelSecurityStage: security.StageMapper,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	base := repositoryScanRunTestClient(t, scan, task, repositoryScanTestAgent(scan.Spec.AnalysisAgentRef.Name))
	requested := false
	reader := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1alpha1.TaskList); ok && !requested {
				// Another cleanup caller records intent after the pending-run
				// snapshot, then stops before it can request Task deletion.
				requested = true
				if err := db.RequestScanRunCancellation(ctx, old, "concurrent late Task cleanup"); err != nil {
					return err
				}
			}
			return c.List(ctx, list, opts...)
		},
	})
	r := &RepositoryScanReconciler{Client: base, APIReader: reader, Scheme: base.Scheme(), SecurityStore: db}
	for range 2 {
		require.ErrorIs(t, r.createScanRun(ctx, scan, "initial", "", ""), security.ErrScanRunCancellationPending)
		runs, _, err := db.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
		require.NoError(t, err)
		require.Len(t, runs, 2, "a pending historical cancellation must block replacement admission")
	}
	deleting := &corev1alpha1.Task{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(task), deleting))
	require.False(t, deleting.DeletionTimestamp.IsZero())
	deleting.Finalizers = nil
	require.NoError(t, base.Update(ctx, deleting))
	require.NoError(t, r.createScanRun(ctx, scan, "initial", "", ""))
	retired, err := db.GetScanRun(ctx, scan.Namespace, old.ID)
	require.NoError(t, err)
	require.False(t, retired.CancellationPending)
	require.Equal(t, old.Phase, retired.Phase)
	require.Equal(t, old.CompletedAt, retired.CompletedAt)
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	require.NotEqual(t, latest.ID, current.Status.LastScanID)
	admitted, err := db.GetScanRun(ctx, scan.Namespace, current.Status.LastScanID)
	require.NoError(t, err)
	require.True(t, security.ScanRunMatchesRepositoryScan(admitted, current))
}

func TestRepositoryScanRecoversOrphanedStatusBinding(t *testing.T) {
	for _, binding := range []string{"missing", "foreign", "unlabeled"} {
		t.Run(binding, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 2},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
				Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_orphaned", LastProcessedCommit: "old-base", Phase: repositoryScanPhaseScanning},
			}
			if binding == "foreign" {
				require.NoError(t, db.CreateScanRun(ctx, &store.ScanRun{
					ID: scan.Status.LastScanID, Namespace: scan.Namespace, RepositoryScan: "other-scan", Phase: scanRunPhaseRunning,
				}))
			}
			orphan := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "orphan-mapper", Namespace: scan.Namespace, UID: "orphan-uid", Finalizers: []string{"test.orka.ai/hold-cleanup"},
					Labels: map[string]string{
						labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: scan.Status.LastScanID, labels.LabelSecurityStage: security.StageMapper,
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			if binding == "unlabeled" {
				delete(orphan.Labels, labels.LabelSecurityScanID)
			}
			foreign := orphan.DeepCopy()
			foreign.Name, foreign.UID = "foreign-mapper", "foreign-uid"
			foreign.OwnerReferences[0].UID = "other-owner"
			terminal := orphan.DeepCopy()
			terminal.Name, terminal.UID = "terminal-mapper", "terminal-uid"
			terminal.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			validation := orphan.DeepCopy()
			validation.Name, validation.UID = "validation", "validation-uid"
			validation.Labels[labels.LabelSecurityStage] = security.StageValidation
			patch := validation.DeepCopy()
			patch.Name, patch.UID = "patch", "patch-uid"
			patch.Labels[labels.LabelSecurityStage] = security.StagePatch
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(scan).
				WithObjects(scan, orphan, foreign, terminal, validation, patch, repositoryScanTestAgent(scan.Spec.AnalysisAgentRef.Name)).Build()
			r := &RepositoryScanReconciler{Client: cl, APIReader: cl, Scheme: scheme, SecurityStore: db}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)}
			for range 2 {
				_, err := r.Reconcile(ctx, request)
				require.NoError(t, err)
				current := &corev1alpha1.RepositoryScan{}
				require.NoError(t, cl.Get(ctx, request.NamespacedName, current))
				require.Equal(t, scan.Status.LastScanID, current.Status.LastScanID, "cleanup must finish before admitting a replacement")
				runs, _, err := db.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
				require.NoError(t, err)
				require.Empty(t, runs)
			}
			deleting := &corev1alpha1.Task{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(orphan), deleting))
			require.False(t, deleting.DeletionTimestamp.IsZero())
			deleting.Finalizers = nil
			require.NoError(t, cl.Update(ctx, deleting))
			_, err := r.Reconcile(ctx, request)
			require.NoError(t, err)
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(ctx, request.NamespacedName, current))
			require.Empty(t, current.Status.LastScanID)
			require.Empty(t, current.Status.LastProcessedCommit)
			require.Equal(t, repositoryScanPhasePending, current.Status.Phase)
			for range 2 {
				_, err = r.Reconcile(ctx, request)
				require.NoError(t, err)
			}
			require.NoError(t, cl.Get(ctx, request.NamespacedName, current))
			require.NotEmpty(t, current.Status.LastScanID)
			replacement, err := db.GetScanRun(ctx, scan.Namespace, current.Status.LastScanID)
			require.NoError(t, err)
			require.True(t, security.ScanRunMatchesRepositoryScan(replacement, scan))
			old, err := db.GetScanRun(ctx, scan.Namespace, scan.Status.LastScanID)
			if binding == "foreign" {
				require.NoError(t, err)
				require.Equal(t, scanRunPhaseRunning, old.Phase)
			} else {
				require.ErrorIs(t, err, store.ErrNotFound)
			}
			for _, task := range []*corev1alpha1.Task{foreign, terminal, validation, patch} {
				preserved := &corev1alpha1.Task{}
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(task), preserved))
				require.True(t, preserved.DeletionTimestamp.IsZero())
			}
		})
	}
}

func TestMapperStageTaskReplayValidatesRunAndSpec(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo"},
	}
	run := &store.ScanRun{
		ID: security.NewScanRunID(), Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		Mode: "initial", Phase: scanRunPhaseRunning, PolicyDigest: security.ScannerPolicyDigest(security.ScannerPolicy{}),
	}
	targetTask := newSucceededSecurityTask("scan-target", run.ID, security.StageThreatModel, metav1.Now())
	targetTask.Labels[labels.LabelSecurityTarget] = scan.Name
	targetTask.Spec.Workspace = repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentRead)
	run.TaskName = targetTask.Name
	require.NoError(t, db.CreateScanRun(ctx, run))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryScanTestObjects(scan, targetTask)...).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: db}
	require.NoError(t, r.createMapperTask(ctx, scan, run))
	require.NoError(t, r.createMapperTask(ctx, scan, run))
	var tasks corev1alpha1.TaskList
	require.NoError(t, cl.List(ctx, &tasks, client.MatchingLabels{labels.LabelSecurityStage: security.StageMapper}))
	require.Len(t, tasks.Items, 1)
	task := tasks.Items[0]
	task.Spec.Command = []string{"unrelated-command"}
	require.NoError(t, cl.Update(ctx, &task))
	require.ErrorIs(t, r.createMapperTask(ctx, scan, run), store.ErrConflict)
}

func TestCreateScanRunReleasesReservationWhenTaskAlreadyExists(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS, UID: "uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(scan).
		WithObjects(scan, repositoryScanTestAgent(scan.Spec.AnalysisAgentRef.Name)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				if task, ok := object.(*corev1alpha1.Task); ok {
					if err := c.Create(ctx, task.DeepCopy(), opts...); err != nil {
						return err
					}
				}
				return c.Create(ctx, object, opts...)
			},
		}).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: db}
	require.True(t, apierrors.IsAlreadyExists(r.createScanRun(ctx, scan, "initial", "", "")))
	runs, _, err := db.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.True(t, runs[0].CancellationPending)
	_, err = security.RetireStaleScanRuns(ctx, db, cl, cl, scan)
	require.NoError(t, err)
	runs, _, err = db.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, scanRunPhaseFailed, runs[0].Phase)
	require.NotNil(t, runs[0].CompletedAt)
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(scan), current))
	require.Empty(t, current.Status.LastScanID)
}
