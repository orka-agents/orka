/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orka-agents/orka/internal/executionmode"
)

func TestClaimNamespaceModeClaimsAnUnlabeledNamespace(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orka-system"}})
	namespaces := client.CoreV1().Namespaces()

	ctx := context.Background()
	if err := claimNamespaceMode(ctx, namespaces, "orka-system", executionmode.HarnessV2, true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed, err := namespaces.Get(context.Background(), "orka-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := claimed.Labels[executionmode.NamespaceLabel]; got != "harness-v2" {
		t.Fatalf("label = %q, want harness-v2", got)
	}
	// A later start finds the claim and must accept it without writing.
	if err := claimNamespaceMode(ctx, namespaces, "orka-system", executionmode.HarnessV2, true); err != nil {
		t.Fatalf("second start: %v", err)
	}
}

func TestClaimNamespaceModeRejectsTheOtherMode(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "orka-system",
		Labels: map[string]string{executionmode.NamespaceLabel: "harness-v1"},
	}})
	err := claimNamespaceMode(context.Background(), client.CoreV1().Namespaces(), "orka-system",
		executionmode.HarnessV2, true)
	if err == nil || !strings.Contains(err.Error(), `claimed by execution mode "harness-v1"`) {
		t.Fatalf("error = %v, want the other mode's claim to be rejected", err)
	}
}

func TestClaimNamespaceModeWithoutClaimingRequiresTheLabel(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orka-system"}})
	err := claimNamespaceMode(context.Background(), client.CoreV1().Namespaces(), "orka-system",
		executionmode.HarnessV2, false)
	if err == nil {
		t.Fatal("an unlabeled namespace must fail when claiming is disabled")
	}
	unchanged, getErr := client.CoreV1().Namespaces().Get(context.Background(), "orka-system", metav1.GetOptions{})
	if getErr != nil || unchanged.Labels[executionmode.NamespaceLabel] != "" {
		t.Fatalf("namespace was labeled although claiming is disabled: %v %v", unchanged.Labels, getErr)
	}
}

func TestClaimNamespaceModeLosesTheRaceToAnotherMode(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orka-system"}})
	// The first update conflicts, as if another controller claimed the
	// namespace first; the retry then reads that claim.
	conflicted := false
	client.PrependReactor("update", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "orka-system",
			Labels: map[string]string{executionmode.NamespaceLabel: "harness-v1"},
		}}
		gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
		if err := client.Tracker().Update(gvr, other, ""); err != nil {
			t.Fatal(err)
		}
		conflict := apierrors.NewConflict(schema.GroupResource{Resource: "namespaces"}, "orka-system", errors.New("modified"))
		return true, nil, conflict
	})
	err := claimNamespaceMode(context.Background(), client.CoreV1().Namespaces(), "orka-system",
		executionmode.HarnessV2, true)
	if err == nil || !strings.Contains(err.Error(), `claimed by execution mode "harness-v1"`) {
		t.Fatalf("error = %v, want the winner's claim to be enforced", err)
	}
}

func TestClaimNamespaceModeFailsClosedOnMissingNamespace(t *testing.T) {
	client := fake.NewClientset()
	err := claimNamespaceMode(context.Background(), client.CoreV1().Namespaces(), "orka-system",
		executionmode.HarnessV2, true)
	if err == nil || !strings.Contains(err.Error(), "read controller-mode namespace") {
		t.Fatalf("error = %v, want a read failure", err)
	}
}
