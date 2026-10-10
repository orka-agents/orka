package controller

import (
	"context"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func preRequestDeletionFixture(t *testing.T) (*externalRuntimePoolFixture, *corev1.Secret, *coordinationv1.Lease) {
	t.Helper()
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	if err := coordinationv1.AddToScheme(f.r.Scheme); err != nil {
		t.Fatal(err)
	}
	w := f.currentWorkspace(t)
	w.Finalizers = []string{executionWorkspaceFinalizer}
	w.Spec.AttachmentEpoch = 1
	w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{Epoch: 1,
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: "attached-task", UID: "attached-task-uid"}}
	if err := f.r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Finalizers = []string{runtimePoolFinalizer}
	if err := f.r.Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	// Core can create its private credentials and stop before publishing the
	// first public workload request. No provider allocation has existed yet.
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.r.ensureRuntimePoolSecrets(ctx, &pool, cfg); err != nil {
		t.Fatal(err)
	}
	owner := *metav1.NewControllerRef(w, workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: w.Namespace,
		Name: attachmentSecretName(w.Name, 1), UID: "attachment-secret-uid",
		Labels: map[string]string{workspaceAttachmentLabel: string(w.UID)}, OwnerReferences: []metav1.OwnerReference{owner}}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: w.Namespace,
		Name: attachmentLeaseName(w.Name), UID: "attachment-lease-uid", OwnerReferences: []metav1.OwnerReference{owner}}}
	for _, object := range []client.Object{secret, lease} {
		if err := f.r.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	return f, secret, lease
}

func observePreRequestDeleted(t *testing.T, f *externalRuntimePoolFixture) {
	t.Helper()
	w := f.currentWorkspace(t)
	if w.Spec.Workload != nil {
		t.Fatal("fixture published a workload before deletion")
	}
	w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleted
	w.Status.ObservedGeneration = w.Generation
	w.Status.AttachedEpoch = 0
	w.Status.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{
		Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionNotApplicable,
		EphemeralSecrets: workspacev1alpha1.DispositionNotApplicable, WorkspaceData: workspacev1alpha1.DispositionNotApplicable,
		PersistentVolumes: workspacev1alpha1.DispositionNotApplicable, Checkpoints: workspacev1alpha1.DispositionNotApplicable,
		ProviderResources: workspacev1alpha1.DispositionDeleted}
	if err := f.r.Status().Update(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func assertPreRequestResourcesGone(t *testing.T, f *externalRuntimePoolFixture, secret *corev1.Secret, lease *coordinationv1.Lease) {
	t.Helper()
	ctx := context.Background()
	for _, object := range []client.Object{f.workspace.DeepCopy(), f.pool.DeepCopy(), secret.DeepCopy(), lease.DeepCopy()} {
		if err := f.r.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("%T %s remains after finalization: %v", object, object.GetName(), err)
		}
	}
	var credentials corev1.SecretList
	if err := f.r.List(ctx, &credentials, client.InNamespace(f.pool.Namespace), client.MatchingLabels{runtimePoolUIDLabel: string(f.pool.UID)}); err != nil {
		t.Fatal(err)
	}
	if len(credentials.Items) != 0 || f.seeds != 0 {
		t.Fatalf("prerequest cleanup retained %d runtime credentials or released credentials %d times", len(credentials.Items), f.seeds)
	}
}

func TestExecutionWorkspacePreRequestDeletionCleansCoreCredentialsWithExactOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		foreign string
	}{
		{name: "exact prerequest cleanup"},
		{name: "foreign pool incarnation", foreign: "pool"},
		{name: "foreign attachment Secret", foreign: "secret"},
		{name: "foreign attachment Lease", foreign: "lease"},
		{name: "stale provider deletion proof", foreign: "status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f, secret, lease := preRequestDeletionFixture(t)
			switch test.foreign {
			case "pool":
				pool := runtimePoolTestGetPool(t, f.r, f.pool)
				pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = "replacement-workspace-uid"
				if err := f.r.Update(ctx, &pool); err != nil {
					t.Fatal(err)
				}
			case "secret", "lease":
				var object client.Object = secret
				if test.foreign == "lease" {
					object = lease
				}
				object.GetOwnerReferences()[0].UID = "replacement-workspace-uid"
				if err := f.r.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			core := &ExecutionWorkspaceReconciler{Client: f.r.Client, APIReader: f.r.Client}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.workspace)}
			if err := f.r.Delete(ctx, f.currentWorkspace(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := core.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			observePreRequestDeleted(t, f)
			if test.foreign == "status" {
				w := f.currentWorkspace(t)
				w.Status.ObservedGeneration--
				if err := f.r.Status().Update(ctx, w); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := core.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(f.currentWorkspace(t), executionWorkspaceFinalizer) {
				t.Fatal("core finalized before its linked pool and credentials were gone")
			}
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if test.foreign == "pool" || test.foreign == "status" {
				if !pool.DeletionTimestamp.IsZero() {
					t.Fatal("foreign incarnation or stale proof authorized pool deletion")
				}
				if err := f.r.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
					t.Fatalf("attachment credentials were removed without exact cleanup proof: %v", err)
				}
				return
			}
			if pool.DeletionTimestamp.IsZero() {
				t.Fatal("prerequest deletion did not retire the exact linked pool")
			}
			for range 3 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			_, err := core.Reconcile(ctx, request)
			if test.foreign != "" {
				if err == nil || !controllerutil.ContainsFinalizer(f.currentWorkspace(t), executionWorkspaceFinalizer) {
					t.Fatalf("foreign attachment object authorized finalization: %v", err)
				}
				var object client.Object = &corev1.Secret{}
				key := client.ObjectKeyFromObject(secret)
				if test.foreign == "lease" {
					object, key = &coordinationv1.Lease{}, client.ObjectKeyFromObject(lease)
				}
				if err := f.r.Get(ctx, key, object); err != nil || object.GetOwnerReferences()[0].UID != "replacement-workspace-uid" {
					t.Fatalf("foreign attachment object was changed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertPreRequestResourcesGone(t, f, secret, lease)
		})
	}
}

func TestWorkspaceMaxLifetimePoolFirstReconcileDeletesAndFinalizesWorkspace(t *testing.T) {
	ctx := context.Background()
	f, secret, lease := preRequestDeletionFixture(t)
	w := f.currentWorkspace(t)
	w.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	w.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	if err := f.r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}
	link := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client}
	if _, err := link.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	current := f.currentWorkspace(t)
	if current.UID != w.UID || current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || current.Spec.Attachment == nil {
		t.Fatal("pool-first reconciliation preempted retention's metadata deletion")
	}
	retention := &ACPWorkspaceRetentionReconciler{Client: f.r.Client, APIReader: f.r.Client}
	if _, err := retention.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	current = f.currentWorkspace(t)
	if current.UID != w.UID || current.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(current, executionWorkspaceFinalizer) {
		t.Fatal("lifetime expiry did not request finalizer-held metadata deletion of the exact workspace")
	}
	core := &ExecutionWorkspaceReconciler{Client: f.r.Client, APIReader: f.r.Client}
	if _, err := core.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	current = f.currentWorkspace(t)
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted || current.Spec.Attachment != nil {
		t.Fatal("metadata deletion did not close the attachment and request provider deletion")
	}
	if _, err := link.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	observePreRequestDeleted(t, f)
	for range 4 {
		if _, err := core.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		runtimePoolReconcile(t, f.r, f.pool)
	}
	assertPreRequestResourcesGone(t, f, secret, lease)
}
