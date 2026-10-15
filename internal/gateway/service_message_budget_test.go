package gateway

import (
	"context"
	"fmt"
	"testing"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestGatewayTaskMessageBudgetConfiguredCapAndReplay(t *testing.T) {
	for _, limit := range []int{2, 12} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s, db, _, _, task := newGatewayMessageFixture(t)
			s.Config.InterimMessagesPerTask = limit
			budget := func(id string) (*TaskMessageBudget, error) {
				p, err := s.PrepareTaskMessageBudget(t.Context(), task.Namespace, task.Name, string(task.UID), id)
				if err != nil {
					return nil, err
				}
				return s.ReadPreparedTaskMessageBudget(t.Context(), p)
			}
			for i := range limit {
				b, err := budget(fmt.Sprint(i))
				require.NoError(t, err)
				require.Equal(t, limit, b.Limit)
				require.Equal(t, i, b.Accepted)
				require.False(t, b.RequestExists)
				_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), fmt.Sprint(i), "working")
				require.NoError(t, err)
			}
			b, err := budget("new")
			require.NoError(t, err)
			require.Equal(t, limit, b.Accepted)
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "new", "working")
			require.Error(t, err)
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false })
			b, err = budget("0")
			require.NoError(t, err)
			require.True(t, b.RequestExists)
			_, err = budget("new")
			require.ErrorIs(t, err, ErrInterimDeliveryUnsupported)
			rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: task.Namespace})
			require.NoError(t, err)
			require.Len(t, rows, limit)
		})
	}
}

func TestGatewayReplyEligibilityIndependentOfAdmissionAvailability(t *testing.T) {
	for _, state := range []string{"not ready", "unobserved generation", "capability withdrawn", "capability unknown", "contract withdrawn"} {
		t.Run(state, func(t *testing.T) {
			s, _, _, _, task := newGatewayMessageFixture(t)
			eligible, err := s.ResolveReplyEligibility(t.Context(), task)
			require.NoError(t, err)
			require.True(t, eligible)
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
				switch state {
				case "not ready":
					g.Status.Ready = false
				case "unobserved generation":
					g.Status.ObservedGeneration = 0
				case "capability withdrawn":
					g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false
				case "capability unknown":
					g.Status.ObservedCapabilities = nil
				case "contract withdrawn":
					g.Status.ObservedCapabilities.ContractVersion = "unsupported"
				}
			})
			eligible, err = s.ResolveReplyEligibility(t.Context(), task)
			require.NoError(t, err)
			require.True(t, eligible, "current admission availability must not freeze a tool-less execution")
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "new", "working")
			require.Error(t, err, "origin is not admission authority")
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
				g.Status.Ready = true
				g.Status.ObservedGeneration = g.Generation
				g.Status.ObservedCapabilities = &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: "orka.gateway.v1", Capabilities: gatewayv1alpha1.GatewayCapabilities{InterimDelivery: true}}
			})
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "new", "working")
			require.NoError(t, err)
		})
	}
}

func TestGatewayReplyEligibilityPendingLinkThenBound(t *testing.T) {
	s, _, _ := newGatewayServiceFixture(t)
	setGatewayAgentNativeAI(t, s, true)
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
		g.Status.ObservedCapabilities = &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: "orka.gateway.v1", Capabilities: gatewayv1alpha1.GatewayCapabilities{InterimDelivery: true}}
	})
	// Observe the actual create-before-link dispatch window, not a simulated label grant.
	var task *corev1alpha1.Task
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		err := c.Create(ctx, obj, opts...)
		if candidate, ok := obj.(*corev1alpha1.Task); ok && err == nil {
			task = candidate.DeepCopy()
			eligible, err := s.ResolveReplyEligibility(ctx, task)
			require.False(t, eligible)
			require.ErrorIs(t, err, store.ErrNotReady)
		}
		return err
	}})
	_, err := s.AdmitEvent(t.Context(), "default", "chat", "Bearer inbound-token", gatewayEventBody(t, "eligibility", "user-1"))
	require.NoError(t, err)
	require.NoError(t, s.DispatchOnce(t.Context()))
	require.NotNil(t, task)
	eligible, err := s.ResolveReplyEligibility(t.Context(), task)
	require.NoError(t, err)
	require.True(t, eligible)
	for _, mutate := range []func(*corev1alpha1.Task){
		func(task *corev1alpha1.Task) { task.UID = "forged" },
		func(task *corev1alpha1.Task) { task.Spec.RequestedBy = nil },
		func(task *corev1alpha1.Task) { task.Labels[labels.LabelParentTask] = "parent" },
		func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseSucceeded },
		func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseFinalizing },
	} {
		forged := task.DeepCopy()
		mutate(forged)
		eligible, err := s.ResolveReplyEligibility(t.Context(), forged)
		require.NoError(t, err)
		require.False(t, eligible)
	}
	var unavailable *Service
	eligible, err = unavailable.ResolveReplyEligibility(t.Context(), task)
	require.False(t, eligible)
	require.Error(t, err)
}
