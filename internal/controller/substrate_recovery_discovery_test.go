package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type unavailableSubstrateRecoveryReader struct{ client.Reader }

func (unavailableSubstrateRecoveryReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("Kubernetes read unavailable")
}

func TestSubstrateRecoveryDiscoveryRequiresReadableControllerNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "other-controller", Namespace: "elsewhere", Labels: map[string]string{substrateCatalogLabel: substrateOwnedLabelValue},
	}}).Build()
	if state, err := FindSubstrateRecoveryJournal(t.Context(), reader, "controller-system"); err != nil || state != "" {
		t.Fatalf("discovery crossed controller namespace: state=%q err=%v", state, err)
	}
	if _, err := FindSubstrateRecoveryJournal(t.Context(), reader, ""); err == nil {
		t.Fatal("discovery admitted an unscoped controller namespace")
	}
	if _, err := FindSubstrateRecoveryJournal(t.Context(), unavailableSubstrateRecoveryReader{reader}, "controller-system"); err == nil || !strings.Contains(err.Error(), "Kubernetes read unavailable") {
		t.Fatalf("unreadable journal inventory treated as absent: %v", err)
	}
}

func TestSubstrateMCPRecoveryDiscoveryRequiresReadableInventory(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"SubstrateActorPools", "Tools"} {
		t.Run(resource, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					_, actorPools := list.(*corev1alpha1.SubstrateActorPoolList)
					_, tools := list.(*corev1alpha1.ToolList)
					if (actorPools && resource == "SubstrateActorPools") || (tools && resource == "Tools") {
						return errors.New("Kubernetes read unavailable")
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()
			state, err := FindSubstrateMCPRecoveryState(t.Context(), reader, "team-a")
			if state != "" || err == nil || !strings.Contains(err.Error(), "list "+resource) {
				t.Fatalf("unreadable MCP inventory treated as absent: state=%q err=%v", state, err)
			}
		})
	}
}
