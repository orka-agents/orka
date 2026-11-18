package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/labels"
)

// The post-create seal is the only thing that lets the controller trust an
// API-created Task's requester; a transient write failure is retried before
// the Task is left unverified.
func TestSealRequesterStampRetriesTransientWriteFailures(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	SetRequesterStampKey(key)
	t.Cleanup(func() { SetRequesterStampKey(nil) })
	previous := requesterStampSealBackoff
	requesterStampSealBackoff = time.Millisecond
	t.Cleanup(func() { requesterStampSealBackoff = previous })
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "created", Namespace: "default", UID: "created-uid", Annotations: map[string]string{
			labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
		}},
		Spec: corev1alpha1.TaskSpec{RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}},
	}
	var patches atomic.Int32
	c := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(task).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patches.Add(1) < 3 {
				return errors.New("transient")
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	sealRequesterStamp(context.Background(), c, task.DeepCopy())
	sealed := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(task), sealed); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 3 || !connectors.RequesterStampValid(key, sealed) {
		t.Fatalf("patches = %d valid = %t, want two failures then a valid seal", patches.Load(), connectors.RequesterStampValid(key, sealed))
	}
	// A write that keeps failing is given up after the bound; the Task is
	// left intact and unverified.
	patches.Store(-1000)
	fresh := task.DeepCopy()
	fresh.Name, fresh.UID, fresh.ResourceVersion = "unsealed", "unsealed-uid", ""
	if err := c.Create(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	sealRequesterStamp(context.Background(), c, fresh.DeepCopy())
	if got := patches.Load(); got != -1000+int32(requesterStampSealAttempts) {
		t.Fatalf("attempts = %d, want %d", got+1000, requesterStampSealAttempts)
	}
}
