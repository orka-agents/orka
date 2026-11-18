package applicationexec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRejectedRootTaskCreateSettlesWithoutAdoptionOrReplay(t *testing.T) {
	resource := schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"}
	for name, rejection := range map[string]error{
		"forbidden":    apierrors.NewForbidden(resource, "task", errors.New("fixture rejection")),
		"unauthorized": apierrors.NewUnauthorized("fixture rejection"),
		"invalid": apierrors.NewInvalid(schema.GroupKind{Group: resource.Group, Kind: "Task"}, "task",
			field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "fixture rejection")}),
		"already-exists": apierrors.NewAlreadyExists(resource, "task"),
		"bad-request":    apierrors.NewBadRequest("fixture rejection"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.backend.qualified.Store(true)
			attempts, callbacks := 0, 0
			f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, ok := object.(*corev1alpha1.Task); ok {
						attempts++
						return rejection
					}
					return c.Create(ctx, object, options...)
				},
			})
			_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { callbacks++; return nil })
			require.ErrorIs(t, err, contract.ErrConflict)
			require.Zero(t, callbacks)
			r, err := f.backend.load(t.Context(), runName(f.request))
			require.NoError(t, err)
			require.True(t, r.state.Task.Rejected)
			require.Empty(t, r.state.Task.UID)
			require.True(t, r.state.Settled)
			require.Empty(t, r.state.Effects)
			require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
			require.ErrorIs(t, f.backend.ensureTask(t.Context(), r, ""), contract.ErrConflict)
			require.Equal(t, 1, attempts)
		})
	}
}

func TestRejectedRootTaskSettlementResumesAfterSaveFailure(t *testing.T) {
	f := newFixture(t)
	f.backend.qualified.Store(true)
	failSettlement := true
	f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, ok := object.(*corev1alpha1.Task); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "tasks"}, object.GetName(), errors.New("fixture rejection"))
			}
			return c.Create(ctx, object, options...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if cm, ok := object.(*corev1.ConfigMap); ok && failSettlement {
				var value state
				require.NoError(t, json.Unmarshal([]byte(cm.Data[stateKey]), &value))
				if value.Settled {
					return apierrors.NewServiceUnavailable("fixture save failed")
				}
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { t.Fatal("rejected Task cannot be accepted"); return nil })
	require.Error(t, err)
	require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
	failSettlement = false
	require.NoError(t, f.backend.recoverRetained(t.Context()))
	require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
}

func TestRejectedArmTaskDoesNotBlockCleanup(t *testing.T) {
	for _, side := range []string{pv.Original, pv.Patched} {
		t.Run(side, func(t *testing.T) {
			f := newFixture(t)
			f.request.Action = pv.VerifyPatch
			f.request.Patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
			f.request.DeclaredChanges = []pv.DeclaredChange{{Kind: "source", Description: "generic correction", Paths: []string{"main.go"}}}
			r := f.newRun()
			attempts := 0
			f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, ok := object.(*corev1alpha1.Task); ok {
						attempts++
						return apierrors.NewForbidden(schema.GroupResource{Resource: "tasks"}, object.GetName(), errors.New("fixture rejection"))
					}
					return c.Create(ctx, object, options...)
				},
			})
			require.ErrorIs(t, f.backend.ensureTask(t.Context(), r, side), contract.ErrConflict)
			require.True(t, r.state.ArmTasks[side].Rejected)
			require.ErrorIs(t, f.backend.ensureTask(t.Context(), r, side), contract.ErrConflict)
			require.Equal(t, 1, attempts)
			require.NoError(t, f.backend.recoverRetained(t.Context()))
			require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
			retained, err := f.backend.load(t.Context(), r.object.Name)
			require.NoError(t, err)
			require.True(t, retained.state.Settled)
		})
	}
}

func TestAmbiguousRootTaskCreateRetainsUncertainty(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobserved", true: "response-lost"}[committed], func(t *testing.T) {
			f := newFixture(t)
			f.backend.qualified.Store(true)
			attempts := 0
			f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, ok := object.(*corev1alpha1.Task); !ok {
						return c.Create(ctx, object, options...)
					}
					attempts++
					if committed {
						require.NoError(t, c.Create(ctx, object, options...))
					}
					return apierrors.NewTimeoutError("fixture unknown result", 0)
				},
			})
			_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
			require.ErrorIs(t, err, ErrUncertain)
			r, err := f.backend.load(t.Context(), runName(f.request))
			require.NoError(t, err)
			require.False(t, r.state.Task.Rejected)
			require.False(t, r.state.Settled)
			require.ErrorIs(t, f.backend.ensureTask(t.Context(), r, ""), ErrUncertain)
			require.ErrorIs(t, f.backend.recoverRetained(t.Context()), ErrUncertain)
			require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
			require.Equal(t, 1, attempts)
		})
	}
}
