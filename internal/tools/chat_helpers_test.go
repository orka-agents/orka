/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"math"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestChatGetIntArgSaturates(t *testing.T) {
	for value, want := range map[float64]int{9.3e18: math.MaxInt, -9.3e18: math.MinInt, 42: 42, -5: -5} {
		if got := chatGetIntArg(map[string]any{"n": value}, "n", 30); got != want {
			t.Errorf("chatGetIntArg(%v) = %d, want %d", value, got, want)
		}
	}
	if got := min(chatGetIntArg(map[string]any{"timeout": 9223372036854775807.0}, "timeout", 30), 60); got != 60 {
		t.Errorf("a huge wait_for_task timeout clamps to %d, want 60", got)
	}
}

// A chat turn stopped while it creates a Task still learns the Create's
// outcome: the Create runs on a context the turn's stop does not end, so the
// Task is committed before the turn reports done, where a Session cancel's
// list finds it. A turn already stopped starts no Create at all.
func TestCreateChatTaskOutlivesTheStoppedTurn(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	turn, stop := context.WithCancel(context.Background())
	// The turn is stopped while the Create is in flight; the client abandons
	// a Create whose context is cancelled, as a real API call does.
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			stop()
			if err := ctx.Err(); err != nil {
				return err
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "chat-task", Namespace: "default"}}
	if err := createChatTask(turn, c, task); err != nil {
		t.Fatalf("createChatTask() stopped mid-flight = %v, want the Create to finish", err)
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(task), &corev1alpha1.Task{}); err != nil {
		t.Fatalf("the Task was not created: %v", err)
	}

	queued := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "queued-task", Namespace: "default"}}
	if err := createChatTask(turn, c, queued); err == nil {
		t.Fatal("a Create queued after the turn stopped was started")
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(queued), &corev1alpha1.Task{}); err == nil {
		t.Fatal("a Task queued after the turn stopped was created")
	}
}
