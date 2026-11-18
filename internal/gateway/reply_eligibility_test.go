package gateway

import (
	"context"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model a task-index miss immediately before dispatch commits the link. The
// fallback still reads the real durable event; it must not become a label grant.
type replyOriginLookupMiss struct{ store.GatewayEventStore }

func (s replyOriginLookupMiss) GetGatewayEventForTask(context.Context, string, string, string) (*store.GatewayEvent, error) {
	return nil, store.ErrNotFound
}

func TestGatewayReplyOriginLinkedBetweenReads(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(map[bool]string{false: "planning", true: "origin bootstrap"}[prepare], func(t *testing.T) {
			s, db, _, _, task := newGatewayMessageFixture(t)
			s.EventStore = replyOriginLookupMiss{db}
			if prepare {
				prepared, err := s.PrepareTaskReplyOrigin(t.Context(), task.Namespace, task.Name, string(task.UID))
				require.NoError(t, err, "a link committed between reads must not produce a fatal origin denial")
				s.EventStore = db
				origin, err := s.ReadPreparedTaskReplyOrigin(t.Context(), prepared)
				require.NoError(t, err)
				require.Equal(t, string(task.UID), origin.TaskUID)
			} else {
				eligible, err := s.ResolveReplyEligibility(t.Context(), task)
				require.NoError(t, err)
				require.True(t, eligible, "a link committed between reads must not freeze a tool-less execution")
			}
		})
	}
}

func TestGatewayReplyOriginFallbackRetainsIdentityChecks(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *Service, *corev1alpha1.Task)
		code   int
	}{
		{"wrong task UID", func(_ *testing.T, _ *Service, task *corev1alpha1.Task) { task.UID = "other" }, 0},
		{"forged provenance", func(_ *testing.T, _ *Service, task *corev1alpha1.Task) {
			task.Spec.RequestedBy = nil
		}, 0},
		{"execution outcome", func(_ *testing.T, _ *Service, task *corev1alpha1.Task) {
			task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
		}, 0},
		{"deleting task", func(_ *testing.T, _ *Service, task *corev1alpha1.Task) {
			now := metav1.Now()
			task.DeletionTimestamp = &now
		}, 0},
		{"expired event", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			require.NoError(t, s.EventStore.ExpireGatewayEvent(t.Context(), task.Namespace,
				task.Annotations[TaskGatewayEventAnnotation], "", "expired", time.Now()))
		}, 0},
		{"namespace replacement", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			ns := &corev1.Namespace{}
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Name: "default"}, ns))
			ns.UID = "replacement"
			require.NoError(t, s.Client.Update(t.Context(), ns))
		}, 409},
		{"gateway replacement", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.UID = "replacement" })
		}, 409},
		{"gateway generation", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Generation++ })
		}, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, db, _, _, task := newGatewayMessageFixture(t)
			test.change(t, s, task)
			s.EventStore = replyOriginLookupMiss{db}
			eligible, err := s.ResolveReplyEligibility(t.Context(), task)
			require.False(t, eligible)
			if test.code == 0 {
				require.NoError(t, err)
			} else {
				var httpErr *HTTPError
				require.ErrorAs(t, err, &httpErr)
				require.Equal(t, test.code, httpErr.Code)
			}
		})
	}
}
