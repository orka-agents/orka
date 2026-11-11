package applicationexec

import (
	"context"
	"encoding/json"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCompletedReceiverRecoversLostPublicationAcknowledgement(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact-publication", true: "changed-route"}[tamper], func(t *testing.T) {
			f, r, pod, service, receiver := receiverFixture(t)
			slice := readyReceiverSlice(service, receiver)
			require.NoError(t, f.target.Create(t.Context(), slice))
			key := resourceKey(pv.Original, "observe-ready")
			failAcknowledgement := true
			f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if cm, ok := object.(*corev1.ConfigMap); ok && failAcknowledgement {
						var proposed state
						require.NoError(t, json.Unmarshal([]byte(cm.Data[stateKey]), &proposed))
						if proposed.ReceiverRoutes[key].Published {
							return apierrors.NewServiceUnavailable("fixture crash before acknowledgement")
						}
					}
					return c.Patch(ctx, object, patch, options...)
				},
			})
			ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
			require.Error(t, err)
			require.False(t, ready)
			restarted, err := f.backend.load(t.Context(), r.object.Name)
			require.NoError(t, err)
			require.False(t, restarted.state.ReceiverRoutes[key].Published)
			current := &corev1.Service{}
			require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: service.Namespace, Name: service.Name}, current))
			annotation := pv.ApplicationReceiverRouteAnnotation(receiver.PodUID)
			require.NotEmpty(t, current.Annotations[annotation], "target publication already committed")
			if tamper {
				route := restarted.state.ReceiverRoutes[key].Route
				route.EndpointSliceResourceVersion = "changed-version"
				content, err := json.Marshal(route)
				require.NoError(t, err)
				current.Annotations[annotation] = string(content)
				require.NoError(t, f.target.Update(t.Context(), current))
			}
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses[0].Ready = false
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
			require.NoError(t, f.target.Status().Update(t.Context(), pod))
			uid := slice.UID
			require.NoError(t, f.target.Delete(t.Context(), slice, client.Preconditions{UID: &uid}))
			failAcknowledgement = false
			before := len(f.created)
			ready, err = f.backend.ensureReceiverRoute(t.Context(), restarted, pv.Original, "observe-ready", service, receiver)
			if tamper {
				require.ErrorIs(t, err, contract.ErrConflict)
				require.False(t, ready)
			} else {
				require.NoError(t, err)
				require.True(t, ready)
			}
			require.Len(t, f.created, before, "completion recovery cannot create or rebind any target resource")
			durable, err := f.backend.load(t.Context(), r.object.Name)
			require.NoError(t, err)
			require.Equal(t, !tamper, durable.state.ReceiverRoutes[key].Published)
		})
	}
}
