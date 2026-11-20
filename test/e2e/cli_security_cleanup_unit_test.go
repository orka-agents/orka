//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func cliSecurityCleanupFixture(t *testing.T) (client.WithWatch, *corev1alpha1.RepositoryScan, *corev1alpha1.Task) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parent := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "scan", UID: "scan-uid"}}
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "child", UID: "child-uid", OwnerReferences: []metav1.OwnerReference{{Name: parent.Name, UID: parent.UID}}}}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, child).Build(), parent, child
}

func TestCLISecurityCleanupWaitsForDeletingAndLateOwnedTasks(t *testing.T) {
	c, parent, child := cliSecurityCleanupFixture(t)
	child.Finalizers = []string{"orka.ai/task-cleanup"}
	if err := c.Update(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	foreign := child.DeepCopy()
	foreign.Name, foreign.UID, foreign.ResourceVersion, foreign.Finalizers = "foreign-child", "foreign-child-uid", "", nil
	foreign.OwnerReferences[0].UID = "foreign-scan-uid"
	if err := c.Create(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureCLISecurityChildTasks(t.Context(), c, parent.DeepCopy())
	if err != nil || len(snapshot.children) != 1 || snapshot.children[child.Name] != child.UID {
		t.Fatalf("exact owned inventory = %v, %v", snapshot, err)
	}
	if err := c.Delete(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	late := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "late-child", UID: "late-child-uid", OwnerReferences: []metav1.OwnerReference{{Name: parent.Name, UID: parent.UID}}}}
	if err := c.Create(t.Context(), late); err != nil {
		t.Fatal(err)
	}
	childReads, finalInventories := 0, 0
	reader := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if key.Name == child.Name {
				childReads++
				if childReads == 3 {
					current := &corev1alpha1.Task{}
					if err := delegate.Get(ctx, key, current); err != nil {
						return err
					}
					// Simulate the product controller completing normal cleanup.
					current.Finalizers = nil
					if err := delegate.Update(ctx, current); err != nil {
						return err
					}
				}
			}
			return delegate.Get(ctx, key, object, options...)
		},
		List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			finalInventories++
			if finalInventories == 2 {
				if err := delegate.Delete(ctx, late); err != nil {
					return err
				}
			}
			return delegate.List(ctx, list, options...)
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitCLISecurityChildTasksDeleted(ctx, reader, snapshot, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if childReads < 3 || finalInventories < 2 {
		t.Fatal("cleanup accepted parent absence before known or late child deletion")
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(foreign), &corev1alpha1.Task{}); err != nil {
		t.Fatal("cleanup disturbed a child owned by a different parent UID")
	}
}

func TestCLISecurityCleanupRejectsReplacementIdentities(t *testing.T) {
	for _, replaceParent := range []bool{true, false} {
		name := "child"
		if replaceParent {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			c, parent, child := cliSecurityCleanupFixture(t)
			snapshot, err := captureCLISecurityChildTasks(t.Context(), c, parent.DeepCopy())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			if replaceParent {
				parent.UID, parent.ResourceVersion = "replacement-scan-uid", ""
				if err := c.Create(t.Context(), parent); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Delete(t.Context(), child); err != nil {
					t.Fatal(err)
				}
				child.UID, child.ResourceVersion = "replacement-child-uid", ""
				if err := c.Create(t.Context(), child); err != nil {
					t.Fatal(err)
				}
			}
			if err := waitCLISecurityChildTasksDeleted(t.Context(), c, snapshot, time.Millisecond); err == nil || !strings.Contains(err.Error(), "was replaced") {
				t.Fatalf("replacement identity accepted: %v", err)
			}
		})
	}
}

func TestCLISecurityCleanupPreservesUnprovedDeletion(t *testing.T) {
	c, parent, child := cliSecurityCleanupFixture(t)
	snapshot, err := captureCLISecurityChildTasks(t.Context(), c, parent.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Millisecond)
	defer cancel()
	if err := waitCLISecurityChildTasksDeleted(ctx, c, snapshot, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retained child accepted as deleted: %v", err)
	}
	current := &corev1alpha1.Task{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(child), current); err != nil || current.UID != child.UID || !current.DeletionTimestamp.IsZero() {
		t.Fatal("observer changed the child's normal lifecycle")
	}
}

func TestCLISecurityCleanupFailsClosedOnInventoryFailure(t *testing.T) {
	c, parent, child := cliSecurityCleanupFixture(t)
	snapshot, err := captureCLISecurityChildTasks(t.Context(), c, parent.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	reader := interceptor.NewClient(c, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("synthetic API outage")
	}})
	if err := waitCLISecurityChildTasksDeleted(t.Context(), reader, snapshot, time.Millisecond); err == nil {
		t.Fatal("inventory failure accepted as absence")
	}
}

func TestCLISecurityCleanupSupportsExactMonitorParent(t *testing.T) {
	c, _, scanChild := cliSecurityCleanupFixture(t)
	parent := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "monitor", UID: "monitor-uid"}}
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "monitor-child", UID: "monitor-child-uid", OwnerReferences: []metav1.OwnerReference{{Name: parent.Name, UID: parent.UID}}}}
	for _, object := range []client.Object{parent, child} {
		if err := c.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := captureCLISecurityChildTasks(t.Context(), c, parent.DeepCopy())
	if err != nil || len(snapshot.children) != 1 || snapshot.children[child.Name] != child.UID {
		t.Fatalf("exact monitor inventory = %v, %v", snapshot, err)
	}
	for _, object := range []client.Object{parent, child} {
		if err := c.Delete(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitCLISecurityChildTasksDeleted(t.Context(), c, snapshot, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(scanChild), &corev1alpha1.Task{}); err != nil {
		t.Fatal("monitor cleanup changed a separately owned scan Task")
	}
}
