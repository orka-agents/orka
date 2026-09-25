package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func TestACPWorkspaceRuntimeDeadline(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"fresh", "continuation", "task deadline wins", "already expired", "unbounded workspace", "plain pool", "foreign workspace", "foreign reverse link", "missing workspace", "read failure", "invalid lifetime"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			created := now.Add(-time.Minute).Truncate(time.Second)
			workspace := &workspacev1alpha1.ExecutionWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "workspace", UID: types.UID("workspace-uid"),
					CreationTimestamp: metav1.NewTime(created),
					Annotations:       map[string]string{acpExecutionWorkspacePoolAnnotation: "pool"},
				},
				Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
					Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{MaxLifetime: &metav1.Duration{Duration: 5 * time.Minute}},
				},
			}
			pool := &corev1alpha1.RuntimePool{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "pool", UID: types.UID("pool-uid"),
					Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
					Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)},
				},
				Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{}},
			}
			parentDeadline := now.Add(10 * time.Minute)
			wantDeadline := created.Add(5 * time.Minute)
			wantError := false
			switch name {
			case "continuation":
				workspace.CreationTimestamp = metav1.NewTime(created.Add(-3 * time.Minute))
				wantDeadline = workspace.CreationTimestamp.Add(5 * time.Minute)
			case "task deadline wins":
				parentDeadline = now.Add(time.Minute)
				wantDeadline = parentDeadline
			case "already expired":
				workspace.CreationTimestamp = metav1.NewTime(created.Add(-10 * time.Minute))
				wantDeadline = workspace.CreationTimestamp.Add(5 * time.Minute)
			case "unbounded workspace":
				workspace.Spec.Lifecycle.MaxLifetime = nil
				wantDeadline = parentDeadline
			case "plain pool":
				pool.Spec.ExecutionWorkspace = nil
				wantDeadline = parentDeadline
			case "foreign workspace":
				workspace.UID = types.UID("replacement-uid")
				wantError = true
			case "foreign reverse link":
				workspace.Annotations[acpExecutionWorkspacePoolAnnotation] = "another-pool"
				wantError = true
			case "missing workspace", "read failure":
				wantError = true
			case "invalid lifetime":
				workspace.Spec.Lifecycle.MaxLifetime.Duration = 0
				wantError = true
			}
			scheme := runtime.NewScheme()
			if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if name != "missing workspace" {
				builder = builder.WithObjects(workspace)
			}
			kubeClient := builder.Build()
			reader := client.Reader(kubeClient)
			if name == "read failure" {
				reader = interceptor.NewClient(kubeClient, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New("API temporarily unavailable")
					},
				})
			}
			parent, stop := context.WithDeadline(context.Background(), parentDeadline)
			defer stop()
			ctx, cancel, err := (&ACPDispatcher{APIReader: reader}).newWorkspaceRuntimeContext(parent, pool)
			if wantError {
				if err == nil {
					cancel()
					t.Fatal("unverified workspace lifetime admitted execution")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(wantDeadline) {
				t.Fatalf("runtime deadline = %s, %v; want %s", deadline, ok, wantDeadline)
			}
			if name == "already expired" && !errors.Is(runtimeContextError(ctx), context.DeadlineExceeded) {
				t.Fatal("expired workspace did not trigger normal prompt deadline cancellation")
			}
		})
	}
}
