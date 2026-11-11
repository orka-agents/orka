package applicationexec

import (
	"context"
	"errors"
	"strings"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func receiverFixture(t *testing.T) (*fixture, *run, *corev1.Pod, pv.ApplicationServiceIdentity, pv.ApplicationContainerIdentity) {
	t.Helper()
	f, r, job, pod, imageID := readinessFixture(t, false)
	pod.Status.ContainerStatuses[0].Ready = true
	require.NoError(t, f.target.Status().Update(t.Context(), pod))
	_, receiver, ready, err := f.backend.observeJob(t.Context(), r, pv.Original, "observe-ready", job, imageID, false)
	require.NoError(t, err)
	require.True(t, ready)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fixtureServiceName("receiver"), Namespace: pod.Namespace},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.17",
			Selector: map[string]string{runLabel: string(r.object.UID), pv.ApplicationTargetLabel: "receiver"},
			Ports:    []corev1.ServicePort{{Name: "fixture", Port: 8080, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP}}}}
	object, err := f.backend.ensureObject(t.Context(), r, resourceKey(pv.Original, "fixture-service-receiver"), service)
	require.NoError(t, err)
	return f, r, pod, pv.ApplicationServiceIdentity{Namespace: service.Namespace, NamespaceUID: receiver.NamespaceUID,
		Name: service.Name, UID: string(object.GetUID()), Port: 8080, Selector: service.Spec.Selector}, receiver
}

func readyReceiverSlice(service pv.ApplicationServiceIdentity, receiver pv.ApplicationContainerIdentity) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "receiver-ready-slice", Namespace: service.Namespace, UID: "receiver-slice-uid",
		Labels: map[string]string{"kubernetes.io/service-name": service.Name, "endpointslice.kubernetes.io/managed-by": "endpointslice-controller.k8s.io"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: service.Name,
			UID: types.UID(service.UID), Controller: new(true)}}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: new("fixture"), Port: new(int32(8080)), Protocol: new(corev1.ProtocolTCP)}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.244.0.10"},
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
			TargetRef: &corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: service.Namespace,
				Name: receiver.PodName, UID: types.UID(receiver.PodUID)}}}}
}

func requireReceiverGetOnly(t *testing.T, f *fixture, service pv.ApplicationServiceIdentity, sliceName string) {
	t.Helper()
	roles := &rbacv1.RoleList{}
	require.NoError(t, f.target.List(t.Context(), roles, client.InNamespace(service.Namespace)))
	grants := 0
	for _, role := range roles.Items {
		for _, rule := range role.Rules {
			for _, resource := range rule.Resources {
				if resource != "endpointslices" {
					continue
				}
				grants++
				require.Equal(t, []string{"discovery.k8s.io"}, rule.APIGroups)
				require.Equal(t, []string{"get"}, rule.Verbs)
				require.Equal(t, []string{sliceName}, rule.ResourceNames)
			}
		}
	}
	if sliceName == "" {
		require.Zero(t, grants)
	} else {
		require.Equal(t, 1, grants)
	}
}

func TestReceiverSliceArrivalPrecedesFrozenGrantAndPublication(t *testing.T) {
	f, r, _, service, receiver := receiverFixture(t)
	key := resourceKey(pv.Original, "observe-ready")
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.False(t, ready)
	require.Empty(t, r.state.ReceiverRoutes)
	requireReceiverGetOnly(t, f, service, "")

	slice := readyReceiverSlice(service, receiver)
	require.NoError(t, f.target.Create(t.Context(), slice))
	publications := 0
	target := interceptor.NewClient(f.target.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, ok := object.(*corev1.Service); ok {
				requireReceiverGetOnly(t, f, service, slice.Name)
				binding := &rbacv1.RoleBinding{}
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: service.Namespace,
					Name: jobName("observe-ready") + "-receiver-route"}, binding))
				require.Equal(t, []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: service.Namespace, Name: "orka-observer"}}, binding.Subjects)
				require.True(t, r.state.ReceiverRoutes[key].Route.EndpointSliceUID == string(slice.UID))
				require.False(t, r.state.ReceiverRoutes[key].Published)
				publications++
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	f.backend.target = target
	ready, err = f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, 1, publications)
	require.True(t, r.state.ReceiverRoutes[key].Published)
	current := &corev1.Service{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: service.Namespace, Name: service.Name}, current))
	route, err := pv.DecodeApplicationReceiverRoute(current.Annotations[pv.ApplicationReceiverRouteAnnotation(receiver.PodUID)], service, receiver)
	require.NoError(t, err)
	require.Equal(t, r.state.ReceiverRoutes[key].Route, route)
	require.Equal(t, slice.ResourceVersion, route.EndpointSliceResourceVersion)
	before := len(f.created)
	ready, err = f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready)
	require.Len(t, f.created, before)
	require.Equal(t, 1, publications, "published input is immutable, not refreshed on resume")
}

func TestReceiverSlicesCannotGrantAuthorityBeforeExactReadiness(t *testing.T) {
	cases := map[string]func(*discoveryv1.EndpointSlice){
		"wrong-service-owner": func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].UID = "other-service" },
		"wrong-service-name":  func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].Name = "another-service" },
		"unmanaged-slice":     func(s *discoveryv1.EndpointSlice) { s.Labels["endpointslice.kubernetes.io/managed-by"] = "unapproved" },
		"wrong-pod-uid":       func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.UID = "other-pod" },
		"wrong-pod-name":      func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.Name = "other-pod" },
		"wrong-address":       func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Addresses[0] = "10.244.0.99" },
		"wrong-port":          func(s *discoveryv1.EndpointSlice) { s.Ports[0].Port = new(int32(8081)) },
		"wrong-port-name":     func(s *discoveryv1.EndpointSlice) { s.Ports[0].Name = new("other") },
		"terminating":         func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Terminating = new(true) },
		"not-ready":           func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Ready = new(false) },
		"no-readiness":        func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Ready = nil },
		"empty":               func(s *discoveryv1.EndpointSlice) { s.Endpoints = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, r, _, service, receiver := receiverFixture(t)
			slice := readyReceiverSlice(service, receiver)
			mutate(slice)
			require.NoError(t, f.target.Create(t.Context(), slice))
			ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
			if name == "empty" || name == "not-ready" || name == "no-readiness" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.False(t, ready)
			require.Empty(t, r.state.ReceiverRoutes)
			requireReceiverGetOnly(t, f, service, "")
		})
	}
}

func TestReceiverRouteRejectsReplacementAndContentDriftWithoutRebinding(t *testing.T) {
	for _, change := range []string{"uid", "resource-version", "port", "pod-uid", "ready", "locator", "role"} {
		t.Run(change, func(t *testing.T) {
			f, r, _, service, receiver := receiverFixture(t)
			slice := readyReceiverSlice(service, receiver)
			require.NoError(t, f.target.Create(t.Context(), slice))
			ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
			require.NoError(t, err)
			require.True(t, ready)
			key := resourceKey(pv.Original, "observe-ready")
			frozen := r.state.ReceiverRoutes[key]
			mutateReceiverRoute(t, f, service, receiver, slice, change)
			before := len(f.created)
			ready, err = f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
			require.Error(t, err)
			require.False(t, ready)
			require.Len(t, f.created, before)
			require.Equal(t, frozen, r.state.ReceiverRoutes[key])
		})
	}
}

func mutateReceiverRoute(t *testing.T, f *fixture, service pv.ApplicationServiceIdentity,
	receiver pv.ApplicationContainerIdentity, slice *discoveryv1.EndpointSlice, change string) {
	t.Helper()
	switch change {
	case "uid":
		uid := slice.UID
		require.NoError(t, f.target.Delete(t.Context(), slice, client.Preconditions{UID: &uid}))
		slice.UID, slice.ResourceVersion = "replaced-slice", ""
		slice.CreationTimestamp = metav1.Time{}
		require.NoError(t, f.target.Create(t.Context(), slice))
	case "locator":
		current := &corev1.Service{}
		require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: service.Namespace, Name: service.Name}, current))
		current.Annotations[pv.ApplicationReceiverRouteAnnotation(receiver.PodUID)] = `{"unexpected":"locator"}`
		require.NoError(t, f.target.Update(t.Context(), current))
	case "role":
		current := &rbacv1.Role{}
		require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: service.Namespace,
			Name: jobName("observe-ready") + "-receiver-route"}, current))
		current.Rules[0].ResourceNames = nil
		require.NoError(t, f.target.Update(t.Context(), current))
	default:
		switch change {
		case "resource-version":
			slice.Annotations = map[string]string{"changed": "metadata"}
		case "port":
			slice.Ports[0].Port = new(int32(8081))
		case "pod-uid":
			slice.Endpoints[0].TargetRef.UID = "replacement-pod"
		case "ready":
			slice.Endpoints[0].Conditions.Ready = new(false)
		}
		require.NoError(t, f.target.Update(t.Context(), slice))
	}
}

func TestReceiverGrantFailureCannotPublishOrStartObservation(t *testing.T) {
	f, r, _, service, receiver := receiverFixture(t)
	slice := readyReceiverSlice(service, receiver)
	require.NoError(t, f.target.Create(t.Context(), slice))
	f.createHook = func(_ context.Context, _ client.WithWatch, object client.Object) error {
		if _, ok := object.(*rbacv1.Role); ok && strings.HasSuffix(object.GetName(), "-receiver-route") {
			return apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "roles"},
				object.GetName(), errors.New("fixture grant denied"))
		}
		return nil
	}
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.ErrorIs(t, err, contract.ErrConflict)
	require.False(t, ready)
	receipt := r.state.ReceiverRoutes[resourceKey(pv.Original, "observe-ready")]
	require.False(t, receipt.Published)
	require.Equal(t, string(slice.UID), receipt.Route.EndpointSliceUID)
	current := &corev1.Service{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: service.Namespace, Name: service.Name}, current))
	require.Empty(t, current.Annotations[pv.ApplicationReceiverRouteAnnotation(receiver.PodUID)])
}

func TestReceiverGrantIsRevokedWithoutDeletingItsRetainedInput(t *testing.T) {
	f, r, _, service, receiver := receiverFixture(t)
	slice := readyReceiverSlice(service, receiver)
	require.NoError(t, f.target.Create(t.Context(), slice))
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready)
	for range 2 {
		_, err := f.backend.stopReceiverAccess(t.Context(), r, pv.Original, "observe-ready")
		require.NoError(t, err)
	}
	requireReceiverGetOnly(t, f, service, "")
	require.True(t, r.state.ReceiverRoutes[resourceKey(pv.Original, "observe-ready")].Published)
}

func TestCompletedReceiverPreservesFinalReceiptAfterEndpointWithdrawal(t *testing.T) {
	f, r, pod, service, receiver := receiverFixture(t)
	slice := readyReceiverSlice(service, receiver)
	require.NoError(t, f.target.Create(t.Context(), slice))
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready)
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses[0].Ready = false
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	require.NoError(t, f.target.Status().Update(t.Context(), pod))
	uid := slice.UID
	require.NoError(t, f.target.Delete(t.Context(), slice, client.Preconditions{UID: &uid}))
	ready, err = f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready, "the validated final worker frame is still required by observerTranscript")
}

func TestReceiverWaitsForSliceReadinessWithoutFreezingEarly(t *testing.T) {
	f, r, _, service, receiver := receiverFixture(t)
	slice := readyReceiverSlice(service, receiver)
	slice.Endpoints[0].Conditions.Ready = new(false)
	require.NoError(t, f.target.Create(t.Context(), slice))
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.False(t, ready)
	require.Empty(t, r.state.ReceiverRoutes)
	requireReceiverGetOnly(t, f, service, "")
	slice.Endpoints[0].Conditions.Ready = new(true)
	require.NoError(t, f.target.Update(t.Context(), slice))
	ready, err = f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, slice.ResourceVersion, r.state.ReceiverRoutes[resourceKey(pv.Original, "observe-ready")].Route.EndpointSliceResourceVersion)
}

func TestReceiverRouteRequiresLivePodReadiness(t *testing.T) {
	f, r, pod, service, receiver := receiverFixture(t)
	require.NoError(t, f.target.Create(t.Context(), readyReceiverSlice(service, receiver)))
	pod.Status.ContainerStatuses[0].Ready = false
	require.NoError(t, f.target.Status().Update(t.Context(), pod))
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.False(t, ready)
	requireReceiverGetOnly(t, f, service, "")
}

func TestReceiverReceiptMustMatchPublishedControllerBinding(t *testing.T) {
	f, r, _, service, receiver := receiverFixture(t)
	require.NoError(t, f.target.Create(t.Context(), readyReceiverSlice(service, receiver)))
	ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
	require.NoError(t, err)
	require.True(t, ready)
	key := resourceKey(pv.Original, "observe-ready")
	route := r.state.ReceiverRoutes[key].Route
	require.NoError(t, f.backend.validateReceiverReceipt(t.Context(), r, service, receiver, route))
	for _, mutate := range []func(*pv.ApplicationReceiverRoute){
		func(value *pv.ApplicationReceiverRoute) { value.EndpointSliceUID = "other-slice" },
		func(value *pv.ApplicationReceiverRoute) { value.EndpointSliceName = "another-name" },
		func(value *pv.ApplicationReceiverRoute) { value.EndpointSliceResourceVersion = "another-version" },
		func(value *pv.ApplicationReceiverRoute) { value.PodUID = "other-pod" },
		func(value *pv.ApplicationReceiverRoute) { value.ServiceUID = "other-service" },
		func(value *pv.ApplicationReceiverRoute) { value.Namespace = "outside" },
	} {
		changed := route
		mutate(&changed)
		require.ErrorIs(t, f.backend.validateReceiverReceipt(t.Context(), r, service, receiver, changed), contract.ErrConflict)
	}
	receipt := r.state.ReceiverRoutes[key]
	receipt.Published = false
	r.state.ReceiverRoutes[key] = receipt
	require.ErrorIs(t, f.backend.validateReceiverReceipt(t.Context(), r, service, receiver, route), contract.ErrConflict)
}

func TestReceiverBootstrapStartsListenerBeforeFreezingRouteInput(t *testing.T) {
	f := newFixture(t)
	f.autoPods = false
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false))
	namespace := r.effect(resourceKey(pv.Original, "namespace"))
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fixtureServiceName("receiver"), Namespace: namespace.Name},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.17",
			Selector: map[string]string{runLabel: string(r.object.UID), pv.ApplicationTargetLabel: "receiver"},
			Ports:    []corev1.ServicePort{{Name: "fixture", Port: 8080, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP}}}}
	created, err := f.backend.ensureObject(t.Context(), r, resourceKey(pv.Original, "fixture-service-receiver"), service)
	require.NoError(t, err)
	ref := pv.ApplicationServiceIdentity{Namespace: namespace.Name, NamespaceUID: string(namespace.UID),
		Name: service.Name, UID: string(created.GetUID()), Port: 8080, Selector: service.Spec.Selector}
	input := pv.ApplicationWorkerInput{Version: pv.ApplicationProtocolVersion, TaskUID: string(r.state.Task.UID),
		Side: pv.Original, CheckID: "bootstrap", ReceiverService: &ref}
	options := jobOptions{role: "observer", mode: "observe", image: f.config.WorkerImage,
		taskUID: input.TaskUID, serviceAccount: "observer", token: true, receiverPort: 8080, target: "receiver", deadline: 30}
	var listener *corev1.Pod
	f.createHook = func(ctx context.Context, c client.WithWatch, object client.Object) error {
		job, ok := object.(*batchv1.Job)
		if !ok {
			return nil
		}
		require.Empty(t, r.state.ReceiverRoutes)
		listener = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, UID: "bootstrap-pod",
			Labels:          map[string]string{runLabel: string(r.object.UID), pv.ApplicationTargetLabel: "receiver", "job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}},
			Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.10",
				ContainerStatuses: []corev1.ContainerStatus{{Name: "observe", ImageID: f.config.WorkerImageID,
					ContainerID: "containerd://bootstrap", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
		listener.Spec.NodeName = r.state.Target.NodeName
		f.reports.Store(listener.Namespace+"/"+listener.Name, []byte{})
		return c.Create(ctx, listener)
	}
	discoveries := 0
	f.backend.targetReader = interceptor.NewClient(f.target.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
				require.NotNil(t, listener, "receiver is the observer Pod, not a pre-existing endpoint")
				require.True(t, listener.Status.ContainerStatuses[0].Ready)
				discoveries++
			}
			return c.List(ctx, list, options...)
		},
	})
	slot := pv.Original + ":bootstrap"
	done, err := f.backend.runObserver(t.Context(), r, pv.Original, "bootstrap", slot, input, options)
	require.NoError(t, err)
	require.False(t, done)
	require.NotNil(t, listener)
	require.Zero(t, discoveries)
	require.Empty(t, r.state.ReceiverRoutes)
	baseInput := &corev1.ConfigMap{}
	inputKey := client.ObjectKey{Namespace: namespace.Name, Name: jobName("bootstrap") + "-input"}
	require.NoError(t, f.target.Get(t.Context(), inputKey, baseInput))
	listener.Status.ContainerStatuses[0].Ready = true
	require.NoError(t, f.target.Status().Update(t.Context(), listener))

	done, err = f.backend.runObserver(t.Context(), r, pv.Original, "bootstrap", slot, input, options)
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, 1, discoveries)
	require.Empty(t, r.state.ReceiverRoutes, "zero slices cannot produce an observation input or grant")
	requireReceiverGetOnly(t, f, ref, "")
	receiver := r.state.Pods[resourceKey(pv.Original, "bootstrap")].Identity
	slice := readyReceiverSlice(ref, receiver)
	require.NoError(t, f.target.Create(t.Context(), slice))

	done, err = f.backend.runObserver(t.Context(), r, pv.Original, "bootstrap", slot, input, options)
	require.NoError(t, err)
	require.False(t, done, "publication alone is not a completed observation or an actor receipt")
	require.True(t, r.state.ReceiverRoutes[resourceKey(pv.Original, "bootstrap")].Published)
	requireReceiverGetOnly(t, f, ref, slice.Name)
	currentInput := &corev1.ConfigMap{}
	require.NoError(t, f.target.Get(t.Context(), inputKey, currentInput))
	require.Equal(t, baseInput.UID, currentInput.UID)
	require.Equal(t, baseInput.BinaryData, currentInput.BinaryData, "bootstrap does not rewrite the frozen base worker input")
	require.Empty(t, r.state.Actions)
	require.Empty(t, r.state.Observed)
}

func TestReceiverCompletionBetweenPodGetAndSliceList(t *testing.T) {
	for _, transition := range []string{"success", "failed", "replaced-pod", "still-running", "unpublished"} {
		t.Run(transition, func(t *testing.T) {
			f, r, pod, service, receiver := receiverFixture(t)
			slice := readyReceiverSlice(service, receiver)
			require.NoError(t, f.target.Create(t.Context(), slice))
			ready, err := f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
			require.NoError(t, err)
			require.True(t, ready)
			key := resourceKey(pv.Original, "observe-ready")
			frozen := r.state.ReceiverRoutes[key]
			if transition == "unpublished" {
				frozen.Published = false
				r.state.ReceiverRoutes[key] = frozen
				require.NoError(t, f.backend.save(t.Context(), r))
			}
			withdrawn, podReads := false, 0
			f.backend.targetReader = interceptor.NewClient(f.target.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if _, ok := object.(*corev1.Pod); ok {
						podReads++
					}
					return c.Get(ctx, key, object, options...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
					if _, ok := list.(*discoveryv1.EndpointSliceList); ok && !withdrawn {
						withdrawn = true
						require.Positive(t, podReads, "initial read must see the running receiver")
						uid := slice.UID
						require.NoError(t, f.target.Delete(ctx, slice, client.Preconditions{UID: &uid}))
						switch transition {
						case "success", "unpublished", "failed":
							exit := int32(0)
							pod.Status.Phase = corev1.PodSucceeded
							if transition == "failed" {
								exit, pod.Status.Phase = 1, corev1.PodFailed
							}
							pod.Status.ContainerStatuses[0].Ready = false
							pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}}
							require.NoError(t, f.target.Status().Update(ctx, pod))
						case "replaced-pod":
							uid := pod.UID
							require.NoError(t, f.target.Delete(ctx, pod, client.Preconditions{UID: &uid}))
							pod.UID, pod.ResourceVersion = "replacement", ""
							pod.CreationTimestamp = metav1.Time{}
							require.NoError(t, f.target.Create(ctx, pod))
						}
					}
					return c.List(ctx, list, options...)
				},
			})
			ready, err = f.backend.ensureReceiverRoute(t.Context(), r, pv.Original, "observe-ready", service, receiver)
			if transition == "success" || transition == "unpublished" {
				require.NoError(t, err)
				require.True(t, ready)
				frozen.Published = true
			} else {
				require.ErrorIs(t, err, contract.ErrConflict)
				require.False(t, ready)
			}
			require.True(t, withdrawn)
			require.GreaterOrEqual(t, podReads, 2)
			require.Equal(t, frozen, r.state.ReceiverRoutes[key])
		})
	}
}
