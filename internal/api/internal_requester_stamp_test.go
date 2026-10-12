/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/labels"
)

func TestSealChildRequesterStampAuthenticatesTheLink(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	SetRequesterStampKey(key)
	t.Cleanup(func() { SetRequesterStampKey(nil) })
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	parent := internalCallerAuthTask()
	parent.Spec.RequestedBy = requester
	parent.Annotations = map[string]string{
		labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
		labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(key, parent.UID, requester.Issuer, requester.Subject),
	}
	job := internalCallerAuthJob(parent, "job-a", "job-uid")
	pod := internalCallerAuthPod(parent, "pod-a", "pod-uid", job)
	controller := true
	owned := func(name, uid string, by *corev1alpha1.RequestedBy) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid), OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &controller,
			}}},
			Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, RequestedBy: by},
		}
	}
	child := owned("child", "child-uid", requester)
	impostor := owned("impostor", "impostor-uid", &corev1alpha1.RequestedBy{Issuer: requester.Issuer, Subject: "victim"})
	stranger := owned("stranger", "stranger-uid", requester)
	stranger.OwnerReferences[0].UID = "other-parent-uid"
	swapped := owned("swapped", "swapped-uid", requester)
	var swapOnce atomic.Bool
	c := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(parent, job, pod, child, impostor, stranger, swapped).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if task, ok := obj.(*corev1alpha1.Task); ok && task.Name == "swapped" && swapOnce.CompareAndSwap(false, true) {
					// A trusted worker swaps the requester between validation and the seal.
					stored := &corev1alpha1.Task{}
					if err := cl.Get(ctx, client.ObjectKeyFromObject(task), stored); err != nil {
						return err
					}
					stored.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: requester.Issuer, Subject: "victim"}
					if err := cl.Update(ctx, stored); err != nil {
						return err
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	handlers := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{Client: c, APIReader: c})
	app := fiber.New()
	app.Use(func(ctx fiber.Ctx) error {
		ctx.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
		return ctx.Next()
	})
	app.Post("/internal/v1/tasks/:namespace/:taskName/children/:child/requester-stamp", handlers.SealChildRequesterStamp)
	post := func(name string) int {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/task-a/children/"+name+"/requester-stamp", nil), fiber.TestConfig{Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status := post("child"); status != http.StatusOK {
		t.Fatalf("owned child with the same requester = %d", status)
	}
	sealed := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "child"}, sealed); err != nil {
		t.Fatal(err)
	}
	if !connectors.RequesterStampValid(key, sealed) {
		t.Fatalf("the child must carry a stamp valid for its own UID: %v", sealed.Annotations)
	}
	if status := post("impostor"); status != http.StatusForbidden {
		t.Fatalf("child naming another requester = %d, want 403", status)
	}
	if status := post("stranger"); status != http.StatusForbidden {
		t.Fatalf("child owned by another task = %d, want 403", status)
	}
	if status := post("missing"); status != http.StatusNotFound {
		t.Fatalf("missing child = %d, want 404", status)
	}
	// A requester swapped between validation and the seal makes the fenced
	// write conflict; nothing is signed for the substituted identity.
	if status := post("swapped"); status != http.StatusConflict {
		t.Fatalf("swapped requester = %d, want 409", status)
	}
	unsealed := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "swapped"}, unsealed); err != nil {
		t.Fatal(err)
	}
	if unsealed.Annotations[labels.AnnotationRequestedByStamp] != "" || connectors.RequesterStampValid(key, unsealed) {
		t.Fatalf("the swapped child must not be sealed: %v", unsealed.Annotations)
	}
	// A parent without a verified requester seals nothing.
	delete(parent.Annotations, labels.AnnotationRequestedByStamp)
	if err := c.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if status := post("child"); status != http.StatusForbidden {
		t.Fatalf("unverified parent = %d, want 403", status)
	}
}
