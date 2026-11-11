package patchverification

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

type applicationReceiverTestState struct {
	receiver *applicationReceiver
	client   *kubefake.Clientset
	service  *corev1.Service
	pod      *corev1.Pod
	slice    *discoveryv1.EndpointSlice
	route    ApplicationReceiverRoute
}

func newApplicationReceiverTestState(t *testing.T) applicationReceiverTestState {
	t.Helper()
	ref, identity, endpointSlice := applicationReceiverRouteFixture(t)
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	ref.Port = listener.Addr().(*net.TCPAddr).Port
	endpointSlice.Ports[0].Port = new(int32(ref.Port))
	route, err := FreezeApplicationReceiverRoute(ref, identity, "127.0.0.1", "tcp", endpointSlice)
	require.NoError(t, err)
	raw, err := json.Marshal(route)
	require.NoError(t, err)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace, UID: types.UID(ref.UID),
			Annotations: map[string]string{ApplicationReceiverRouteAnnotation(identity.PodUID): string(raw)}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "127.0.0.1",
			Selector: ref.Selector, Ports: []corev1.ServicePort{{Name: "tcp", Protocol: corev1.ProtocolTCP,
				Port: int32(ref.Port), TargetPort: intstr.FromInt(ref.Port)}}},
	}
	image := "example/observer@" + Digest([]byte("observer-image"))
	pod := applicationLiveTestPod(ref.Namespace, identity, image, false)
	pod.Labels = map[string]string{"app": "receiver"}
	pod.Status.PodIP, pod.Status.Phase = "127.0.0.1", corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses[0].Ready = true
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: identity.ServiceAccountName, Namespace: ref.Namespace, UID: types.UID(identity.ServiceAccountUID)}}
	client := kubefake.NewSimpleClientset(service, pod, account, endpointSlice)
	marker := []byte(strings.Repeat("M", 48))
	input := ApplicationWorkerInput{Worker: identity, ReceiverService: &ref, MarkerDigest: Digest(marker),
		Target: ApplicationTarget{ClusterUID: "target-cluster", Namespace: "subject-run", NamespaceUID: "subject-namespace"}}
	input.Manifest.Environment.Dependencies = map[string]string{ApplicationObserverImageKey: image}
	receiver := &applicationReceiver{
		listener: listener, input: input, kube: client, marker: marker,
		selfIP: "127.0.0.1", subjectIP: net.ParseIP("127.0.0.2"),
		check: ApplicationTCPCheck{Mode: "receive", Target: "receiver", Port: ref.Port,
			MarkerID: "protected-value", MaxBytes: 256, WindowSeconds: 1},
	}
	t.Cleanup(receiver.close)
	return applicationReceiverTestState{receiver: receiver, client: client, service: service, pod: pod,
		slice: endpointSlice, route: route}
}

func (state applicationReceiverTestState) update(t *testing.T) {
	t.Helper()
	require.NoError(t, state.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("services"),
		state.service, state.service.Namespace))
	require.NoError(t, state.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"),
		state.pod, state.pod.Namespace))
	require.NoError(t, state.client.Tracker().Update(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"),
		state.slice, state.slice.Namespace))
}

func assertApplicationReceiverNamedReads(t *testing.T, state applicationReceiverTestState) {
	t.Helper()
	for _, action := range state.client.Actions() {
		require.Equal(t, "get", action.GetVerb(), "the observer must never discover names by listing")
		require.Equal(t, state.service.Namespace, action.GetNamespace())
		require.Empty(t, action.GetSubresource())
		get, ok := action.(kubetesting.GetAction)
		require.True(t, ok)
		group := ""
		var name string
		switch action.GetResource().Resource {
		case "services":
			name = state.service.Name
		case "pods":
			name = state.pod.Name
		case "serviceaccounts":
			name = state.pod.Spec.ServiceAccountName
		case "endpointslices":
			group, name = "discovery.k8s.io", state.route.EndpointSliceName
		default:
			t.Fatalf("unexpected observer resource %s", action.GetResource())
		}
		require.Equal(t, group, action.GetResource().Group)
		require.Equal(t, name, get.GetName())
	}
}

func TestApplicationReceiverStartsOnlyAfterFrozenSliceArrival(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "route arrived before start"
		if pending {
			name = "slice and capsule arrive during listener-only bootstrap"
		}
		t.Run(name, func(t *testing.T) {
			state := newApplicationReceiverTestState(t)
			if pending {
				require.NoError(t, state.client.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"),
					state.slice.Namespace, state.slice.Name))
				requests := 0
				state.client.PrependReactor("get", "services", func(kubetesting.Action) (bool, runtime.Object, error) {
					requests++
					if requests == 1 {
						service := state.service.DeepCopy()
						service.Annotations = nil
						return true, service, nil
					}
					if requests == 2 {
						if err := state.client.Tracker().Add(state.slice); err != nil {
							return true, nil, err
						}
					}
					return false, nil, nil
				})
			}
			require.NoError(t, state.receiver.listener.Close())
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			check := Check{Application: &ApplicationCheck{TCP: &state.receiver.check}}
			receiver, err := newApplicationReceiver(ctx, state.receiver.input, check, state.client,
				state.receiver.subjectIP.String(), state.receiver.marker)
			require.NoError(t, err)
			defer receiver.close()
			require.Equal(t, state.route, *receiver.route)
			require.Equal(t, "127.0.0.1", receiver.serviceIP)
			assertApplicationReceiverNamedReads(t, state)
		})
	}
}

func TestApplicationReceiverZeroSliceRemainsPending(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	state.service.Annotations = nil
	state.update(t)
	require.NoError(t, state.client.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"),
		state.slice.Namespace, state.slice.Name))
	require.ErrorIs(t, state.receiver.verifyService(t.Context()), errApplicationReceiverRoutePending)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	require.Error(t, state.receiver.waitForServiceRoute(ctx))
	require.Nil(t, state.receiver.route)
	for _, action := range state.client.Actions() {
		require.Equal(t, "services", action.GetResource().Resource, "no route means no guessed slice GET or discovery LIST")
	}
	assertApplicationReceiverNamedReads(t, state)
}

func TestApplicationReceiverRechecksBindingAfterNonceBeforeStimulus(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	requests := 0
	state.client.PrependReactor("get", "services", func(kubetesting.Action) (bool, runtime.Object, error) {
		requests++
		service := state.service.DeepCopy()
		if requests > 1 {
			service.UID = "replacement-service"
		}
		return true, service, nil
	})
	require.NoError(t, state.receiver.listener.Close())
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	check := Check{Application: &ApplicationCheck{TCP: &state.receiver.check}}
	receiver, err := newApplicationReceiver(ctx, state.receiver.input, check, state.client,
		state.receiver.subjectIP.String(), state.receiver.marker)
	require.ErrorIs(t, err, ErrBinding)
	require.Nil(t, receiver, "successful nonce traffic cannot conceal a changed API route before stimuli")
	require.Equal(t, 2, requests)
	assertApplicationReceiverNamedReads(t, state)
}

func TestApplicationReceiverPublishedInvalidRouteIsNotPending(t *testing.T) {
	for _, content := range []string{"", "null", `{"version":1}`, `{"version":1,"version":1}`} {
		t.Run(content, func(t *testing.T) {
			state := newApplicationReceiverTestState(t)
			state.service.Annotations[ApplicationReceiverRouteAnnotation(state.podUID())] = content
			state.update(t)
			require.ErrorIs(t, state.receiver.waitForServiceRoute(t.Context()), ErrBinding)
			require.Len(t, state.client.Actions(), 1, "published malformed input must fail immediately, not be retried")
		})
	}
	state := newApplicationReceiverTestState(t)
	require.NoError(t, state.client.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"),
		state.slice.Namespace, state.slice.Name))
	require.ErrorIs(t, state.receiver.waitForServiceRoute(t.Context()), ErrBinding,
		"a published route with a missing slice is not ordinary bootstrap pending")
}

func (state applicationReceiverTestState) podUID() string {
	return state.receiver.input.Worker.PodUID
}

func TestApplicationReceiverRejectsFrozenRouteAndLivePodDrift(t *testing.T) {
	for name, modify := range map[string]func(*applicationReceiverTestState){
		"slice replacement":     func(s *applicationReceiverTestState) { s.slice.UID = "replacement-slice" },
		"slice version":         func(s *applicationReceiverTestState) { s.slice.ResourceVersion = "18" },
		"slice owner":           func(s *applicationReceiverTestState) { s.slice.OwnerReferences[0].UID = "other-service" },
		"slice Pod UID":         func(s *applicationReceiverTestState) { s.slice.Endpoints[0].TargetRef.UID = "other-pod" },
		"slice address":         func(s *applicationReceiverTestState) { s.slice.Endpoints[0].Addresses[0] = "127.0.0.2" },
		"slice port":            func(s *applicationReceiverTestState) { s.slice.Ports[0].Port = new(int32(8081)) },
		"slice unready":         func(s *applicationReceiverTestState) { s.slice.Endpoints[0].Conditions.Ready = new(false) },
		"Pod replacement":       func(s *applicationReceiverTestState) { s.pod.UID = "replacement-pod" },
		"Pod IP":                func(s *applicationReceiverTestState) { s.pod.Status.PodIP = "127.0.0.2" },
		"Pod unready":           func(s *applicationReceiverTestState) { s.pod.Status.Conditions[0].Status = corev1.ConditionFalse },
		"Pod not running":       func(s *applicationReceiverTestState) { s.pod.Status.Phase = corev1.PodSucceeded },
		"Pod selector":          func(s *applicationReceiverTestState) { s.pod.Labels["app"] = "other" },
		"Service replacement":   func(s *applicationReceiverTestState) { s.service.UID = "replacement-service" },
		"Service IP":            func(s *applicationReceiverTestState) { s.service.Spec.ClusterIP = "127.0.0.2" },
		"Service target port":   func(s *applicationReceiverTestState) { s.service.Spec.Ports[0].TargetPort = intstr.FromInt(1) },
		"Service unready route": func(s *applicationReceiverTestState) { s.service.Spec.PublishNotReadyAddresses = true },
		"withdrawn capsule":     func(s *applicationReceiverTestState) { s.service.Annotations = nil },
		"silently rebound capsule": func(s *applicationReceiverTestState) {
			s.slice.ResourceVersion = "18"
			route := s.route
			route.EndpointSliceResourceVersion = "18"
			raw, err := json.Marshal(route)
			require.NoError(t, err)
			s.service.Annotations[ApplicationReceiverRouteAnnotation(s.podUID())] = string(raw)
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := newApplicationReceiverTestState(t)
			require.NoError(t, state.receiver.verifyService(t.Context()))
			modify(&state)
			state.update(t)
			require.ErrorIs(t, state.receiver.verifyService(t.Context()), ErrBinding)
			require.Equal(t, state.route, *state.receiver.route, "a running observer may not replace its frozen route")
			assertApplicationReceiverNamedReads(t, state)
		})
	}
}

func TestApplicationReceiverRejectsWrongNamedGetResponse(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	state.client.PrependReactor("get", "endpointslices", func(kubetesting.Action) (bool, runtime.Object, error) {
		wrong := state.slice.DeepCopy()
		wrong.Name = "other-generated-slice"
		return true, wrong, nil
	})
	require.ErrorIs(t, state.receiver.verifyService(t.Context()), ErrBinding)
	require.Nil(t, state.receiver.route)
	assertApplicationReceiverNamedReads(t, state)
}

func TestApplicationReceiverNonceRequiresTheActualListener(t *testing.T) {
	t.Run("fresh exact echo", func(t *testing.T) {
		state := newApplicationReceiverTestState(t)
		require.NoError(t, state.receiver.verifyService(t.Context()))
		require.NoError(t, state.receiver.verifyServiceDataPlane(t.Context()))
	})
	t.Run("API ready but Service reaches another listener", func(t *testing.T) {
		state := newApplicationReceiverTestState(t)
		require.NoError(t, state.receiver.verifyService(t.Context()))
		other, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: state.receiver.check.Port})
		require.NoError(t, err)
		defer func() { _ = other.Close() }()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { _ = other.Close() })
		defer stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			connection, err := other.AcceptTCP()
			if err != nil {
				return
			}
			defer func() { _ = connection.Close() }()
			deadline, _ := ctx.Deadline()
			_ = connection.SetDeadline(deadline)
			content, err := io.ReadAll(io.LimitReader(connection, 33))
			if err == nil {
				_, _ = connection.Write(content)
			}
		}()
		state.receiver.serviceIP = "127.0.0.2"
		require.Error(t, state.receiver.verifyServiceDataPlane(ctx))
		<-done
	})
	t.Run("unbound traffic is not a nonce", func(t *testing.T) {
		state := newApplicationReceiverTestState(t)
		require.NoError(t, state.receiver.verifyService(t.Context()))
		connection, err := net.DialTCP("tcp4", nil, state.receiver.listener.Addr().(*net.TCPAddr))
		require.NoError(t, err)
		_, err = connection.Write([]byte("not-the-private-route-nonce"))
		require.NoError(t, err)
		require.NoError(t, connection.Close())
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		require.Error(t, state.receiver.verifyServiceDataPlane(ctx))
	})
}

func TestApplicationReceiverCompletedWindowRetainsFinalRoute(t *testing.T) {
	for _, positive := range []bool{false, true} {
		name := "complete zero-connection window"
		if positive {
			name = "actual subject marker"
		}
		t.Run(name, func(t *testing.T) {
			state := newApplicationReceiverTestState(t)
			require.NoError(t, state.receiver.verifyService(t.Context()))
			require.NoError(t, state.receiver.verifyServiceDataPlane(t.Context()))
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			require.NoError(t, state.receiver.startCapture(ctx))
			if positive {
				connection, err := net.DialTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.2")},
					state.receiver.listener.Addr().(*net.TCPAddr))
				require.NoError(t, err)
				_, err = connection.Write(state.receiver.marker)
				require.NoError(t, err)
				require.NoError(t, connection.Close())
			}
			proof, content, err := state.receiver.observe(ctx)
			require.NoError(t, err)
			require.Equal(t, positive, proof.Result.Connected)
			require.Equal(t, positive, proof.Result.MarkerMatched)
			require.Equal(t, state.route, *proof.ReceiverRoute)
			require.NotContains(t, string(content), string(state.receiver.marker))
			sliceGets := 0
			for _, action := range state.client.Actions() {
				if action.GetResource().Resource == "endpointslices" {
					sliceGets++
				}
			}
			require.Equal(t, 2, sliceGets, "the route is independently read before and after the full window")
			assertApplicationReceiverNamedReads(t, state)
		})
	}
}

func TestApplicationReceiverFinalFenceRejectsMidWindowDrift(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	require.NoError(t, state.receiver.verifyService(t.Context()))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, state.receiver.startCapture(ctx))
	changed := state.slice.DeepCopy()
	changed.ResourceVersion = "18"
	updated := make(chan error, 1)
	timer := time.AfterFunc(50*time.Millisecond, func() {
		updated <- state.client.Tracker().Update(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"),
			changed, changed.Namespace)
	})
	defer timer.Stop()
	proof, content, err := state.receiver.observe(ctx)
	require.NoError(t, <-updated)
	require.ErrorIs(t, err, ErrBinding)
	require.Nil(t, proof)
	require.Nil(t, content, "route drift cannot become a negative marker observation")
}
