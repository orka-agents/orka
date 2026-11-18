package connectors

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// The status mirrors the grant custody assigned to the committed material,
// so a snapshot or approval bound to one grant can tell a re-link of the
// same object apart.
func TestApplyLinkedStatusRaisesGrantSequenceOnEveryCommit(t *testing.T) {
	provider := &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "orka-system", UID: "provider-uid"},
		Spec: corev1alpha1.ConnectorProviderSpec{
			OAuth: corev1alpha1.ConnectorOAuthConfig{Scopes: corev1alpha1.ConnectorScopes{Read: []string{"read:user"}, Write: []string{"read:user"}}},
		},
	}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: "orka-system", Generation: 1},
		Spec:       corev1alpha1.ConnectionSpec{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
	}
	now := metav1.NewTime(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	credential := store.ConnectorCredential{Scopes: []string{"read:user"}, GrantSequence: 1}

	ApplyLinkedStatus(connection, provider, credential, now)
	if connection.Status.GrantSequence != 1 {
		t.Fatalf("first commit must record grant 1, got %d", connection.Status.GrantSequence)
	}
	credential.GrantSequence = 2
	ApplyLinkedStatus(connection, provider, credential, now)
	if connection.Status.GrantSequence != 2 {
		t.Fatalf("re-link of the same Connection must record grant 2, got %d", connection.Status.GrantSequence)
	}
	if connection.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("expected Ready after a covering consent, got %q", connection.Status.State)
	}
}
