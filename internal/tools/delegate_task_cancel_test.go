package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// TestCancelChildOfCancelledParentKeepsOtherRequestsAndReplacements covers
// the post-create cancel finding a child someone already asked to cancel, or
// a different Task under the child's name: neither is overwritten.
func TestCancelChildOfCancelledParentKeepsOtherRequestsAndReplacements(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "default", UID: "parent-uid",
		Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"},
	}}
	requested := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "requested", Namespace: "other", UID: "requested-uid",
		Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by bob: wrong repository"},
	}}
	replacement := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "replaced", Namespace: "other", UID: "new-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, requested, replacement).Build()
	ctx := context.Background()

	if err := cancelChildOfCancelledParent(ctx, c, c, parent, requested.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	got := &corev1alpha1.Task{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(requested), got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[labels.AnnotationCancelRequested] != "cancelled by bob: wrong repository" {
		t.Fatalf("an existing cancel request was overwritten: %q", got.Annotations[labels.AnnotationCancelRequested])
	}

	created := replacement.DeepCopy()
	created.UID = "created-uid" // the child this delegation created, since replaced
	if err := cancelChildOfCancelledParent(ctx, c, c, parent, created); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(replacement), got); err != nil {
		t.Fatal(err)
	}
	if _, cancelled := got.Annotations[labels.AnnotationCancelRequested]; cancelled {
		t.Fatal("a different Task under the child's name must not be cancelled")
	}
}

// TestLateChildCancelSurvivesTheCallersCancellation covers the broker
// cancelling the delegation's context as the caller's authority ends, right
// after the child was created: the child of the cancelled parent is still
// cancelled, and a transient write failure is retried.
func TestLateChildCancelSurvivesTheCallersCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "default", UID: "parent-uid",
		Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"},
	}}
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "other", UID: "child-uid"}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, child.DeepCopy()).Build()
	failures := 1
	flaky := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if failures > 0 {
				failures--
				return errors.New("apiserver unavailable")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's authority already ended
	if err := ensureLateChildCancelled(ctx, base, flaky, parent, child); err != nil {
		t.Fatalf("ensureLateChildCancelled() = %v, want the child cancelled", err)
	}
	got := &corev1alpha1.Task{}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(child), got); err != nil {
		t.Fatal(err)
	}
	if _, requested := got.Annotations[labels.AnnotationCancelRequested]; !requested {
		t.Fatal("the late child of a cancelled parent was not cancelled")
	}
}

// TestChildOfAnUnknownCreateIsCancelled covers a Create the caller's
// cancellation cut short after the API server committed the child: its name
// was never learned, and the parent's cascade had already listed, so the
// child is found by the parent that delegated it and cancelled. Siblings of
// another delegation effect and other parents' children are left alone.
func TestChildOfAnUnknownCreateIsCancelled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	saved := unknownCreateBackoff
	unknownCreateBackoff.Duration = time.Millisecond
	t.Cleanup(func() { unknownCreateBackoff = saved })
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "default", UID: "parent-uid",
		Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"},
	}}
	child := func(name, parentUID, effect string) *corev1alpha1.Task {
		return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "other", UID: types.UID(name + "-uid"),
			Labels: map[string]string{labels.LabelParentTask: "parent"},
			Annotations: map[string]string{
				labels.AnnotationParentTaskUID:      parentUID,
				labels.AnnotationDelegationEffectID: effect,
			},
		}}
	}
	committed := child("parent-child-x7k2p", "parent-uid", "effect-1")
	sibling := child("parent-child-a1b2c", "parent-uid", "effect-0")
	stranger := child("parent-child-zz9zz", "earlier-parent-uid", "effect-1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, committed, sibling, stranger).Build()

	// The Create's own object never learned the generated name.
	attempted := committed.DeepCopy()
	attempted.Name, attempted.UID = "", ""
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cancelChildrenAfterUnknownCreate(ctx, c, c, parent, attempted); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		task *corev1alpha1.Task
		want bool
	}{{committed, true}, {sibling, false}, {stranger, false}} {
		got := &corev1alpha1.Task{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(tc.task), got); err != nil {
			t.Fatal(err)
		}
		if _, requested := got.Annotations[labels.AnnotationCancelRequested]; requested != tc.want {
			t.Fatalf("%s cancel requested = %t, want %t", tc.task.Name, requested, tc.want)
		}
	}

	// A live parent's own cascade lists the child, so nothing is cancelled.
	live := parent.DeepCopy()
	live.Annotations = nil
	other := child("parent-child-live1", "parent-uid", "effect-2")
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, other).Build()
	if err := cancelChildrenAfterUnknownCreate(context.Background(), c, c, live, other); err != nil {
		t.Fatal(err)
	}
	got := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(other), got); err != nil {
		t.Fatal(err)
	}
	if _, requested := got.Annotations[labels.AnnotationCancelRequested]; requested {
		t.Fatal("the child of a live parent was cancelled")
	}
}

func TestChildCreateOutcomeUnknown(t *testing.T) {
	gr := schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"caller cancelled", context.Canceled, true},
		{"connection reset", errors.New("connection reset by peer"), true},
		{"server timeout", apierrors.NewServerTimeout(gr, "create", 1), true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "core.orka.ai", Kind: "Task"}, "x", nil), false},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("no")), false},
	} {
		if got := childCreateOutcomeUnknown(tc.err); got != tc.want {
			t.Errorf("%s: unknown = %t, want %t", tc.name, got, tc.want)
		}
	}
}
