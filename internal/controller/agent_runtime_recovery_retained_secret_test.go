package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestAgentRuntimeRecoveryRetainedDeletingBootSecretAllowsOriginalDrain(t *testing.T) {
	for _, provider := range []string{"local", "foundry"} {
		for _, deleting := range []bool{false, true} {
			name := provider + "/ordinary"
			if deleting {
				name = provider + "/retained-deleting"
			}
			t.Run(name, func(t *testing.T) {
				f := newRuntimeRecoveryFixture(t)
				if provider == "foundry" {
					configureFoundryRuntimeRecoveryFixture(t, f)
				}
				f.reconcile(t)
				if !f.runtime.Status.Ready {
					t.Fatal("fixture did not conform")
				}
				witness := f.witness(t)
				key := client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}
				secret := &corev1.Secret{}
				if err := f.r.Get(t.Context(), key, secret); err != nil {
					t.Fatal(err)
				}
				before := secret.DeepCopy()
				if deleting {
					if err := f.r.Delete(t.Context(), secret); err != nil {
						t.Fatal(err)
					}
					if err := f.r.Get(t.Context(), key, secret); err != nil {
						t.Fatal(err)
					}
					if secret.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(secret, agentRuntimeSecretFinalizer) ||
						secret.Immutable == nil || !*secret.Immutable || !reflect.DeepEqual(before.Data, secret.Data) ||
						!reflect.DeepEqual(before.OwnerReferences, secret.OwnerReferences) {
						t.Fatal("DELETE did not retain the exact immutable original authority")
					}
				}
				counts := f.server.Counts()
				f.advanceEpoch(t)
				f.reconcile(t)
				f.reconcile(t)
				retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
				if err != nil || !retired {
					t.Fatalf("original authenticated drain was blocked by retained authority: retirement=%t error=%v status=%s", retired, err, f.runtime.Status.Message)
				}
				if f.server.Counts() != counts {
					t.Fatal("recovery replayed inference")
				}
			})
		}
	}
}
