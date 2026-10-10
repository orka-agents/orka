/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestRequesterStampBindsUIDAndRequester(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "tenant", UID: "task-uid",
			Annotations: map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI}},
		Spec: corev1alpha1.TaskSpec{RequestedBy: requester},
	}
	if RequesterStampValid(key, task) {
		t.Fatal("the source annotation alone must not verify")
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	if err := SealRequesterStamp(context.Background(), c, key, task); err != nil {
		t.Fatal(err)
	}
	if !RequesterStampValid(key, task) {
		t.Fatal("a sealed stamp must verify")
	}
	stamp := task.Annotations[labels.AnnotationRequestedByStamp]
	for name, mutate := range map[string]func(*corev1alpha1.Task){
		"other uid":       func(x *corev1alpha1.Task) { x.UID = "other-uid" },
		"other subject":   func(x *corev1alpha1.Task) { x.Spec.RequestedBy.Subject = "mallory" },
		"other issuer":    func(x *corev1alpha1.Task) { x.Spec.RequestedBy.Issuer = "https://other.example.test" },
		"no source":       func(x *corev1alpha1.Task) { delete(x.Annotations, labels.AnnotationRequestedBySource) },
		"truncated stamp": func(x *corev1alpha1.Task) { x.Annotations[labels.AnnotationRequestedByStamp] = stamp[:10] },
	} {
		copied := task.DeepCopy()
		mutate(copied)
		if RequesterStampValid(key, copied) {
			t.Fatalf("%s must not verify", name)
		}
	}
	if RequesterStampValid([]byte("short"), task) || RequesterStampValid(nil, task) {
		t.Fatal("an unconfigured or short key must never verify")
	}
	if RequesterStamp(key, "", requester.Issuer, requester.Subject) != "" {
		t.Fatal("a stamp needs a UID to bind")
	}
	// Tasks the API did not stamp are left alone; a missing key is an error.
	plain := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "tenant", UID: "plain-uid"}, Spec: corev1alpha1.TaskSpec{RequestedBy: requester}}
	if err := SealRequesterStamp(context.Background(), c, key, plain); err != nil || plain.Annotations[labels.AnnotationRequestedByStamp] != "" {
		t.Fatalf("unstamped task: err = %v annotations = %v", err, plain.Annotations)
	}
	unsealed := task.DeepCopy()
	delete(unsealed.Annotations, labels.AnnotationRequestedByStamp)
	if err := SealRequesterStamp(context.Background(), c, nil, unsealed); err == nil {
		t.Fatal("sealing without a key must fail")
	}
}
