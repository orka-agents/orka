/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// parkedDelegatedChild returns a delegated child that parked its turn and the
// continuation the controller created for it: the continuation inherits the
// coordinator parent labels but is owned, not controlled, by the child.
func parkedDelegatedChild(childName string) (*corev1alpha1.Task, *corev1alpha1.Task) {
	const coordinator = "coordinator-a"
	child := internalCallerAuthTaskObject(childName, childName+"-uid", "", coordinator, "")
	child.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	continuation := internalCallerAuthTaskObject(childName+"-resume", childName+"-resume-uid", "", "", "")
	continuation.Labels[labels.LabelParentTask] = labels.SelectorValue(coordinator)
	continuation.Annotations[labels.AnnotationParentTaskName] = coordinator
	continuation.Annotations[labels.AnnotationContinuationOfName] = child.Name
	continuation.Annotations[labels.AnnotationContinuationOfUID] = string(child.UID)
	continuation.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: kubernetesTaskKind, Name: child.Name, UID: child.UID,
	}}
	child.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, Kind: corev1alpha1.TaskWaitKindTimer,
		ContinuationTaskName: continuation.Name, ContinuationTaskUID: string(continuation.UID),
	}
	return child, continuation
}

func requireFiberStatus(t *testing.T, err error, status int) {
	t.Helper()
	var fiberErr *fiber.Error
	require.True(t, errors.As(err, &fiberErr), "want fiber error %d, got %v", status, err)
	require.Equal(t, status, fiberErr.Code)
}

func TestContinuationOfADelegatedChildKeepsItsCoordinationParent(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	coordinator := internalCallerAuthTaskObject("coordinator-a", "coordinator-a-uid", "", "", "")
	child, continuation := parkedDelegatedChild("worker")
	sibling := internalCallerAuthTaskObject("peer", "peer-uid", "", "coordinator-a", "")
	siblingParked, siblingContinuation := parkedDelegatedChild("peer-parked")
	other := internalCallerAuthTaskObject("coordinator-b", "coordinator-b-uid", "", "", "")
	stranger := internalCallerAuthTaskObject("stranger", "stranger-uid", "", "coordinator-b", "")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(coordinator, child, continuation, sibling, siblingParked, siblingContinuation, other, stranger).Build()
	authorizer := internalCallerAuthorizer{k8sReader: k8sClient, taskProvenanceProtected: true}
	ctx := context.Background()

	// check_messages: the continuation reads its coordinator's channel.
	parent, err := authorizer.verifiedCoordinationParent(ctx, continuation, "coordinator-a")
	require.NoError(t, err)
	require.Equal(t, coordinator.UID, parent.UID)
	// send_message: to a sibling, to a sibling's continuation, and to the coordinator.
	require.NoError(t, authorizer.verifyTaskMessageScope(ctx, continuation, "peer", "coordinator-a"))
	require.NoError(t, authorizer.verifyTaskMessageScope(ctx, continuation, siblingContinuation.Name, "coordinator-a"))
	require.NoError(t, authorizer.verifyTaskMessageScope(ctx, sibling, continuation.Name, "coordinator-a"))
	require.NoError(t, authorizer.verifyTaskMessageScope(ctx, continuation, "coordinator-a", "coordinator-a"))
	// The coordinator still controls its child's continuation.
	_, err = authorizer.verifiedCoordinationChild(ctx, coordinator, continuation.Name)
	require.NoError(t, err)

	requireFiberStatus(t, authorizer.verifyTaskMessageScope(ctx, continuation, "stranger", "coordinator-a"), http.StatusForbidden)
	_, err = authorizer.verifiedCoordinationParent(ctx, continuation, "coordinator-b")
	requireFiberStatus(t, err, http.StatusForbidden)
}

func TestContinuationCoordinationParentFailsClosed(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	ctx := context.Background()
	coordinator := internalCallerAuthTaskObject("coordinator-a", "coordinator-a-uid", "", "", "")

	// The predecessor's controller-written wait does not name this Task, so
	// its lineage annotations prove nothing.
	child, forged := parkedDelegatedChild("worker")
	child.Status.Wait.ContinuationTaskUID = "someone-else"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(coordinator, child, forged).Build()
	authorizer := internalCallerAuthorizer{k8sReader: k8sClient, taskProvenanceProtected: true}
	_, err := authorizer.verifiedCoordinationParent(ctx, forged, "coordinator-a")
	requireFiberStatus(t, err, http.StatusForbidden)

	// A predecessor delegated by another coordinator does not grant this one.
	child, continuation := parkedDelegatedChild("worker")
	child.Annotations[labels.AnnotationParentTaskName] = "coordinator-b"
	child.Labels[labels.LabelParentTask] = labels.SelectorValue("coordinator-b")
	child.OwnerReferences[0].Name, child.OwnerReferences[0].UID = "coordinator-b", "coordinator-b-uid"
	k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(coordinator, child, continuation).Build()
	authorizer = internalCallerAuthorizer{k8sReader: k8sClient, taskProvenanceProtected: true}
	_, err = authorizer.verifiedCoordinationParent(ctx, continuation, "coordinator-a")
	requireFiberStatus(t, err, http.StatusForbidden)

	// A failed predecessor read never grants the parent.
	child, continuation = parkedDelegatedChild("worker")
	failing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(coordinator, child, continuation).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key == (types.NamespacedName{Namespace: "default", Name: "worker"}) {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	authorizer = internalCallerAuthorizer{k8sReader: failing, taskProvenanceProtected: true}
	_, err = authorizer.verifiedCoordinationParent(ctx, continuation, "coordinator-a")
	requireFiberStatus(t, err, http.StatusInternalServerError)

	// Without provenance admission the lineage annotations may be forged.
	authorizer = internalCallerAuthorizer{k8sReader: k8sClient, taskProvenanceProtected: false}
	_, err = authorizer.verifiedCoordinationParent(ctx, continuation, "coordinator-a")
	requireFiberStatus(t, err, http.StatusForbidden)
}

// transcriptSessionNames returns the Session names caller may search.
func transcriptSessionNames(t *testing.T, objects []client.Object, caller *corev1alpha1.Task) []string {
	t.Helper()
	k8sClient := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
		WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(objects...).Build()
	authorizer := internalCallerAuthorizer{k8sReader: k8sClient, taskProvenanceProtected: true}
	allowed, err := authorizer.coordinationTreeSessionReferences(context.Background(), caller)
	require.NoError(t, err)
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	return names
}

// transcriptTree returns a coordinator with a parked child, its continuation
// and a sibling, plus an unrelated coordinator and its child. Each Task has a
// Session of its own; a continuation appends to its predecessor's.
func transcriptTree() (continuation, sibling *corev1alpha1.Task, objects []client.Object) {
	session := func(task *corev1alpha1.Task, name string) *corev1alpha1.Task {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: name}
		return task
	}
	child, continuation := parkedDelegatedChild("worker")
	session(child, "worker-session")
	continuation.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "worker-session", Append: true}
	sibling = session(internalCallerAuthTaskObject("peer", "peer-uid", "", "coordinator-a", ""), "peer-session")
	return continuation, sibling, []client.Object{
		session(internalCallerAuthTaskObject("coordinator-a", "coordinator-a-uid", "", "", ""), "coordinator-session"),
		child, continuation, sibling,
		session(internalCallerAuthTaskObject("coordinator-b", "coordinator-b-uid", "", "", ""), "other-coordinator-session"),
		session(internalCallerAuthTaskObject("stranger", "stranger-uid", "", "coordinator-b", ""), "stranger-session"),
	}
}

func TestResumedDelegatedChildKeepsItsTranscriptTree(t *testing.T) {
	continuation, sibling, objects := transcriptTree()

	// search_transcript from the continuation still reaches its coordinator
	// and siblings, and nothing of another coordinator's tree.
	require.ElementsMatch(t, []string{"coordinator-session", "worker-session", "peer-session"},
		transcriptSessionNames(t, objects, continuation))
	// The tree still reaches the continuation's Session reference.
	k8sClient := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
		WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(objects...).Build()
	authorizer := internalCallerAuthorizer{k8sReader: k8sClient, taskProvenanceProtected: true}
	allowed, err := authorizer.coordinationTreeSessionReferences(context.Background(), sibling)
	require.NoError(t, err)
	require.Contains(t, allowed["worker-session"], *continuation.Spec.SessionRef)
	require.NotContains(t, allowed, "stranger-session")
}

func TestTranscriptTreeIgnoresUnprovenContinuationLineage(t *testing.T) {
	find := func(objects []client.Object, name string) *corev1alpha1.Task {
		for _, object := range objects {
			if object.GetName() == name {
				return object.(*corev1alpha1.Task)
			}
		}
		t.Fatalf("no Task %q", name)
		return nil
	}
	for name, breakLink := range map[string]func(objects []client.Object){
		// The predecessor's controller-written wait names another Task, so
		// the lineage annotations are only a claim.
		"wait names another Task": func(objects []client.Object) {
			find(objects, "worker").Status.Wait.ContinuationTaskUID = "someone-else"
		},
		"predecessor never resumed": func(objects []client.Object) {
			find(objects, "worker").Status.Wait.State = corev1alpha1.TaskWaitStateWaiting
		},
		"annotation names another incarnation": func(objects []client.Object) {
			find(objects, "worker-resume").Annotations[labels.AnnotationContinuationOfUID] = "earlier-worker-uid"
		},
		// The proven predecessor belongs to another coordinator's tree.
		"predecessor has another coordinator": func(objects []client.Object) {
			worker := find(objects, "worker")
			worker.Annotations[labels.AnnotationParentTaskName] = "coordinator-b"
			worker.Labels[labels.LabelParentTask] = labels.SelectorValue("coordinator-b")
			worker.OwnerReferences[0].Name, worker.OwnerReferences[0].UID = "coordinator-b", "coordinator-b-uid"
		},
		"predecessor is being deleted": func(objects []client.Object) {
			worker := find(objects, "worker")
			now := metav1.Now()
			worker.DeletionTimestamp, worker.Finalizers = &now, []string{"orka.ai/test"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			continuation, sibling, objects := transcriptTree()
			continuation.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "claimed-session"}
			breakLink(objects)

			// The claimant reaches only its own Session, and no tree reaches it.
			require.Equal(t, []string{"claimed-session"}, transcriptSessionNames(t, objects, continuation))
			require.NotContains(t, transcriptSessionNames(t, objects, sibling), "claimed-session")
			require.NotContains(t, transcriptSessionNames(t, objects, find(objects, "stranger")), "claimed-session")
		})
	}
}
