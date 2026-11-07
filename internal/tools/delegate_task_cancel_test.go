package tools

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// TestDelegationSeesAParentCancelledAroundChildCreation covers a parent
// cancelled while a brokered delegation creates its child: a parent already
// cancelling delegates nothing, and one cancelled right after the child was
// created cancels that child, which its cascade may have missed.
func TestDelegationSeesAParentCancelledAroundChildCreation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"}}
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "other", UID: "child-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent.DeepCopy(), child.DeepCopy()).Build()
	ctx := context.Background()

	if cancelled, err := delegationParentCancelled(ctx, c, parent); err != nil || cancelled {
		t.Fatalf("a live parent: cancelled = %t, %v", cancelled, err)
	}
	if err := cancelChildOfCancelledParent(ctx, c, c, parent, child.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	got := &corev1alpha1.Task{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(child), got); err != nil {
		t.Fatal(err)
	}
	if _, requested := got.Annotations[labels.AnnotationCancelRequested]; requested {
		t.Fatal("a child of a live parent must not be cancelled")
	}

	live := &corev1alpha1.Task{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(parent), live); err != nil {
		t.Fatal(err)
	}
	live.Annotations = map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"}
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := delegationParentCancelled(ctx, c, parent); err != nil || !cancelled {
		t.Fatalf("a cancelling parent: cancelled = %t, %v; want delegation refused", cancelled, err)
	}
	if err := cancelChildOfCancelledParent(ctx, c, c, parent, got); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(child), got); err != nil {
		t.Fatal(err)
	}
	if _, requested := got.Annotations[labels.AnnotationCancelRequested]; !requested {
		t.Fatal("a child created as its parent was cancelled must be cancelled too")
	}

	replaced := parent.DeepCopy()
	replaced.UID = "older-uid"
	if cancelled, err := delegationParentCancelled(ctx, c, replaced); err != nil || !cancelled {
		t.Fatalf("a replaced parent: cancelled = %t, %v; want it treated as gone", cancelled, err)
	}
}
