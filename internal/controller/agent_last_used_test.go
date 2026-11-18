/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// lastUsedRaceClient lands a concurrent LastUsed write of newer just before
// the first Agent status update, so that update conflicts and its retry must
// re-read the Agent.
func lastUsedRaceClient(t *testing.T, agent *corev1alpha1.Agent, newer metav1.Time) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	raced := false
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).WithStatusSubresource(agent).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if _, ok := object.(*corev1alpha1.Agent); ok && !raced {
				raced = true
				concurrent := &corev1alpha1.Agent{}
				if err := delegate.Get(ctx, client.ObjectKeyFromObject(object), concurrent); err != nil {
					return err
				}
				concurrent.Status.LastUsed = &newer
				if err := delegate.SubResource(subResource).Update(ctx, concurrent); err != nil {
					return err
				}
			}
			return delegate.SubResource(subResource).Update(ctx, object, options...)
		},
	})
}

func agentLastUsed(t *testing.T, c client.Client, agent *corev1alpha1.Agent) time.Time {
	t.Helper()
	current := &corev1alpha1.Agent{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(agent), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.LastUsed == nil {
		t.Fatal("LastUsed is unset")
	}
	return current.Status.LastUsed.Time
}

// Tasks of one Agent can complete out of order, concurrently when Tasks
// reconcile in parallel. An earlier completion's timestamp must never move
// LastUsed backward, which would expire the Agent's TTL early.
func TestUpdateAgentLastUsedNeverMovesBackward(t *testing.T) {
	base := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	earlier, newer := metav1.NewTime(base), metav1.NewTime(base.Add(time.Minute))
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent"}}
	c := lastUsedRaceClient(t, agent, newer)
	r := &TaskReconciler{Client: c}

	if err := r.updateAgentLastUsed(context.Background(), agent.Namespace, agent.Name, earlier); err != nil {
		t.Fatal(err)
	}
	if got := agentLastUsed(t, c, agent); !got.Equal(newer.Time) {
		t.Fatalf("LastUsed = %s after an earlier completion retried past a newer one, want %s", got, newer.Time)
	}
	latest := metav1.NewTime(base.Add(2 * time.Minute))
	if err := r.updateAgentLastUsed(context.Background(), agent.Namespace, agent.Name, latest); err != nil {
		t.Fatal(err)
	}
	if got := agentLastUsed(t, c, agent); !got.Equal(latest.Time) {
		t.Fatalf("LastUsed = %s, want a later completion to advance it to %s", got, latest.Time)
	}
}

// The Agent controller's status pass captures LastUsed before its write; a
// conflict retry must keep a newer LastUsed a Task completion recorded since.
func TestAgentStatusUpdateKeepsNewerLastUsed(t *testing.T) {
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent"}}
	newer := metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))
	c := lastUsedRaceClient(t, agent, newer)
	r := &AgentReconciler{Client: c}
	current := &corev1alpha1.Agent{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(agent), current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.updateStatus(context.Background(), current, 1, nil, nil, agentIdentityState{}); err != nil {
		t.Fatal(err)
	}
	if got := agentLastUsed(t, c, agent); !got.Equal(newer.Time) {
		t.Fatalf("LastUsed = %s after the status retry, want the newer %s kept", got, newer.Time)
	}
}
