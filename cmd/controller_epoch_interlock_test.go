package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

// The hold (ADR 0058) is opt-in: without the flag the control store gets
// the option that turns it off, and with it the store keeps its default.
func TestControllerEpochInterlockOptionsMakeTheHoldOptIn(t *testing.T) {
	require.Empty(t, controllerEpochInterlockOptions(true))

	off := controllerEpochInterlockOptions(false)
	require.Len(t, off, 1)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	_, err := storekube.New(fake.NewClientBuilder().WithScheme(scheme).Build(), "orka-system", off...)
	require.NoError(t, err)
}
