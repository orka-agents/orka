/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// namespaceReadCounter counts uncached Namespace reads.
type namespaceReadCounter struct {
	client.Reader
	namespaces atomic.Int32
}

func (r *namespaceReadCounter) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Namespace); ok {
		r.namespaces.Add(1)
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// A dispatched turn asks the exact runtime instance for its capabilities
// once; the checks that read the response still gate the turn (see the next
// test). TestACPDispatcherExecutesNoChangeTask counts the namespace reads of a
// turn that records Session lineage.
func TestACPDispatcherTurnAvoidsDuplicateRoundTrips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var capabilities atomic.Int32
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "round-trips", "77777777-7777-7777-7777-777777777771",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				onCapabilities: func() { capabilities.Add(1) },
			})
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "round-trips", Create: true, Append: true}
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sealedTestStore)
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity

	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))

	current := fixture.currentTask(t, ctx)
	require.Equal(t, corev1alpha1.TaskExecutionStateSucceeded, current.Status.Execution.State, "message: %s", current.Status.Execution.Message)
	// Before this change a turn sent two capabilities requests.
	require.Equal(t, int32(1), capabilities.Load(), "capabilities requests per dispatched turn")
}

// The reused capabilities response still gates the turn: a pool supervisor
// without Agent session configuration support is requeued, not dispatched.
func TestACPDispatcherReusedCapabilitiesStillGateTheTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var capabilities, creates atomic.Int32
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "capability-gate", "77777777-7777-7777-7777-777777777772",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				disableAgentSessionConfiguration: true,
				onCapabilities:                   func() { capabilities.Add(1) },
			}, func(harnessv2.CreateRuntimeSessionRequest) { creates.Add(1) })
		})
	defer fixture.stop()

	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	_ = fixture.dispatcher.executeReservedTask(ctx, reserved, target)

	current := fixture.currentTask(t, ctx)
	require.Equal(t, corev1alpha1.TaskExecutionStateReserved, current.Status.Execution.State)
	require.Contains(t, current.Status.Execution.Message, string(acpReservedRetrySessionConfiguration))
	require.Equal(t, int32(1), capabilities.Load())
	require.Zero(t, creates.Load(), "a gated turn must not create a RuntimeSession")
}
