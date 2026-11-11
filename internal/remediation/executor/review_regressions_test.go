package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCancelledPreparationCleansInputBeforeJobAdmission(t *testing.T) {
	f := newFixture(t, false)
	f.backend.client = interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return apierrors.NewInternalError(errors.New("synthetic Job admission failure"))
			}
			return c.Create(ctx, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.ConfigMapList); ok {
				t.Fatal("cleanup must not list namespace ConfigMap data")
			}
			return c.List(ctx, list, opts...)
		},
	})
	f.backend.reader = f.backend.client
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.Error(t, err)
	var noEffects interface{ NoEffects() bool }
	require.False(t, errors.As(err, &noEffects), "a write-attempt failure is not a pure input rejection")
	input := &corev1.ConfigMap{}
	inputKey := client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName + "-input"}
	require.NoError(t, f.backend.reader.Get(t.Context(), inputKey, input))
	require.NotEmpty(t, input.UID)
	require.Empty(t, f.jobs)
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	require.True(t, apierrors.IsNotFound(f.backend.reader.Get(t.Context(), inputKey, input)))
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, task.Status.Phase)
	require.NotNil(t, task.Status.CompletionTime)
	require.NotContains(t, task.Finalizers, preparationFinalizer)
	completed := task.Status.CompletionTime.DeepCopy()
	mutations := 0
	f.backend.client = interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			mutations++
			return errors.New("unexpected create")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			mutations++
			return errors.New("unexpected delete")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			mutations++
			return errors.New("unexpected patch")
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			mutations++
			return errors.New("unexpected status patch")
		},
	})
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	_, handled, err := f.backend.ReconcileValidationTask(t.Context(), task)
	require.True(t, handled)
	require.NoError(t, err)
	require.Zero(t, mutations)
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Equal(t, completed, task.Status.CompletionTime)
}

func TestCancelledInputCleanupRejectsForeignOwner(t *testing.T) {
	f := newFixture(t, false)
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return errors.New("pause") })
	require.Error(t, err)
	input := &corev1.ConfigMap{}
	input.Namespace, input.Name = receipt.Namespace, receipt.TaskName+"-input"
	require.NoError(t, f.backend.client.Create(t.Context(), input))
	require.ErrorIs(t, f.backend.Cancel(t.Context(), receipt), contract.ErrConflict)
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKeyFromObject(input), input))
}

func TestPureRequestDigestAndStartShareNoEffectsValidation(t *testing.T) {
	for name, change := range map[string]func(*contract.ExecutionRequest){
		"check ID": func(r *contract.ExecutionRequest) { r.Checks.Checks[0].ID = "invalid/check" },
		"declared changes": func(r *contract.ExecutionRequest) {
			r.DeclaredChanges = []pv.DeclaredChange{{Kind: "unknown", Description: "invalid"}}
		},
		"requirement": func(r *contract.ExecutionRequest) {
			r.Checks.Requirements = []pv.EnvironmentRequirement{{Kind: "cluster", Name: "unavailable"}}
		},
		"control character": func(r *contract.ExecutionRequest) { r.Problem = "invalid\x00report" },
		"encoded size": func(r *contract.ExecutionRequest) {
			r.Checks.Gaps = []string{strings.Repeat("x", pv.MaxPreparationInputBytes)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, false)
			change(&f.request)
			blockedClient := interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					t.Fatal("pure input validation performed an API read")
					return nil
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Fatal("invalid input created a resource")
					return nil
				},
			})
			f.backend.client, f.backend.reader = blockedClient, blockedClient
			_, digestErr := f.backend.RequestDigest(f.request)
			require.Error(t, digestErr)
			record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error {
				t.Fatal("invalid input reached acceptance")
				return nil
			})
			require.Nil(t, record)
			require.Error(t, err)
			require.Equal(t, digestErr.Error(), err.Error())
			var noEffects interface{ NoEffects() bool }
			require.ErrorAs(t, err, &noEffects)
			require.True(t, noEffects.NoEffects())
			_, err = f.backend.Resume(t.Context(), f.request, contract.ExecutionReceipt{})
			require.Error(t, err)
			require.False(t, errors.As(err, &noEffects), "Resume must not erase an already accepted execution")
		})
	}
}

func TestStartReadFailureHasNoEffectsButAdmissionFailureDoesNot(t *testing.T) {
	for _, stage := range []string{"read", "write"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, false)
			failure := apierrors.NewTimeoutError("synthetic admission outage", 1)
			f.backend.client = interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if stage == "read" {
						return failure
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return failure },
			})
			f.backend.reader = f.backend.client
			_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
			require.Error(t, err)
			var noEffects interface{ NoEffects() bool }
			require.Equal(t, stage == "read", errors.As(err, &noEffects))
			require.Empty(t, f.jobs)
		})
	}
}
