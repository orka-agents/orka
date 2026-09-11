package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// An API timeout does not prove a submitted write stopped. Keep the exact
// serialized request so the test can apply it after another controller has
// completed cleanup and a new object owns the same name.
type delayedAgentRuntimeCleanupClient struct {
	client.Client
	pending func(context.Context) error
}

func (c *delayedAgentRuntimeCleanupClient) Patch(_ context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	data, err := patch.Data(object)
	if err != nil {
		return err
	}
	target := object.DeepCopyObject().(client.Object)
	c.pending = func(ctx context.Context) error {
		return c.Client.Patch(ctx, target, client.RawPatch(patch.Type(), data), options...)
	}
	return apierrors.NewTimeoutError("cleanup response unavailable", 0)
}

func (c *delayedAgentRuntimeCleanupClient) Delete(_ context.Context, object client.Object, options ...client.DeleteOption) error {
	target := object.DeepCopyObject().(client.Object)
	bound := (&client.DeleteOptions{}).ApplyOptions(options)
	if bound.Preconditions != nil {
		bound.Preconditions = bound.Preconditions.DeepCopy()
	}
	c.pending = func(ctx context.Context) error {
		return c.Client.Delete(ctx, target, bound)
	}
	return apierrors.NewTimeoutError("cleanup response unavailable", 0)
}

func TestAgentRuntimeCleanupDelayedWritePreservesReplacementOwner(t *testing.T) {
	for _, operation := range []string{"runtime-finalizer", "secret-finalizer", "secret-delete"} {
		t.Run(operation, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			deletedAt := metav1.NewTime(time.Unix(1, 0))
			original := &corev1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{
				Namespace: "runtime-cleanup", Name: "external-runtime", UID: "original-runtime", ResourceVersion: "10",
				DeletionTimestamp: &deletedAt, Finalizers: []string{agentRuntimeSecretGCFinalizer},
			}}
			secretName, err := agentRuntimeCleanupSecretName(original)
			if err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: original.Namespace, Name: secretName, UID: "original-secret", ResourceVersion: "20",
				Labels:          map[string]string{agentRuntimeCleanupSecretLabel: scheduledRunLabelValue},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(original, corev1alpha1.GroupVersion.WithKind("AgentRuntime"))},
				Finalizers:      []string{agentRuntimeSecretFinalizer},
			}, Type: agentRuntimeCleanupSecretType}
			if operation == "secret-delete" {
				secret.Finalizers = nil
			}
			server := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original, secret).Build()
			delayed := &delayedAgentRuntimeCleanupClient{Client: server}
			reconciler := &AgentRuntimeReconciler{Client: delayed, APIReader: server}
			var target client.Object = secret
			if operation == "runtime-finalizer" {
				target = original
				_, err = reconciler.removeAgentRuntimeSecretGCFinalizer(t.Context(), original)
			} else {
				_, err = reconciler.finalizeAgentRuntimeCleanupSecret(t.Context(), original)
			}
			if !apierrors.IsTimeout(err) || delayed.pending == nil {
				t.Fatalf("old cleanup did not return an ambiguous submitted write: %v", err)
			}

			// The successor finishes deletion while the old request is pending.
			current := target.DeepCopyObject().(client.Object)
			if err := server.Get(t.Context(), client.ObjectKeyFromObject(target), current); err != nil {
				t.Fatal(err)
			}
			current.SetFinalizers(nil)
			if err := server.Update(t.Context(), current); err != nil {
				t.Fatal(err)
			}
			if err := server.Delete(t.Context(), current); client.IgnoreNotFound(err) != nil {
				t.Fatal(err)
			}
			replacement := target.DeepCopyObject().(client.Object)
			replacement.SetUID("replacement-owner")
			replacement.SetResourceVersion("")
			replacement.SetDeletionTimestamp(nil)
			replacement.SetFinalizers([]string{agentRuntimeFinalizer, agentRuntimeSecretFinalizer, agentRuntimeSecretGCFinalizer})
			if err := server.Create(t.Context(), replacement); err != nil {
				t.Fatal(err)
			}
			if err := delayed.pending(t.Context()); !apierrors.IsConflict(err) {
				t.Errorf("delayed old %s was not rejected for the replacement owner: %v", operation, err)
			}
			observed := replacement.DeepCopyObject().(client.Object)
			if err := server.Get(t.Context(), client.ObjectKeyFromObject(replacement), observed); err != nil {
				t.Fatal(err)
			}
			if observed.GetUID() != replacement.GetUID() || observed.GetResourceVersion() != replacement.GetResourceVersion() ||
				observed.GetDeletionTimestamp() != nil || !reflect.DeepEqual(observed.GetFinalizers(), replacement.GetFinalizers()) {
				t.Errorf("delayed old cleanup changed replacement ownership: %#v", observed.GetObjectKind().GroupVersionKind())
			}
		})
	}
}
