// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeIngressReviewClient struct {
	client.Client
	runtimeNamespace  string
	cachedReads       int
	creates           map[string]int
	deletes           map[string]client.DeleteOptions
	lostCreate        bool
	patchFailure      string
	lostPatch         bool
	lostDelete        bool
	beforeWorkerFence func(*corev1alpha1.RuntimePool)
}

func nativeIngressReviewPolicy(policy *networkingv1.NetworkPolicy) bool {
	return strings.HasSuffix(policy.Name, "external-provider-ingress") || strings.HasSuffix(policy.Name, "external-controller-ingress")
}

func (c *nativeIngressReviewClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*networkingv1.NetworkPolicy); ok && key.Namespace != c.runtimeNamespace {
		c.cachedReads++
		return apierrors.NewNotFound(schema.GroupResource{Group: networkingv1.GroupName, Resource: "networkpolicies"}, key.Name)
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *nativeIngressReviewClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	policy, isPolicy := object.(*networkingv1.NetworkPolicy)
	if isPolicy && policy.UID == "" {
		policy.UID = types.UID("policy-" + policy.Namespace + "-" + policy.Name)
	}
	if isPolicy && nativeIngressReviewPolicy(policy) {
		c.creates[policy.Name]++
	}
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if isPolicy && nativeIngressReviewPolicy(policy) && c.lostCreate {
		c.lostCreate = false
		return errors.New("accepted policy create response lost")
	}
	return nil
}

func (c *nativeIngressReviewClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	pool, isPool := object.(*corev1alpha1.RuntimePool)
	if isPool && c.beforeWorkerFence != nil && pool.Annotations[externalRuntimeEvidenceAnnotation] != "" {
		c.beforeWorkerFence(pool)
	}
	fail := isPool && c.patchFailure != "" && pool.Annotations[c.patchFailure] != ""
	if fail && !c.lostPatch {
		c.patchFailure = ""
		return errors.New("pool fence patch rejected")
	}
	if err := c.Client.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	if fail {
		c.patchFailure = ""
		return errors.New("accepted pool fence patch response lost")
	}
	return nil
}

func (c *nativeIngressReviewClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if policy, ok := object.(*networkingv1.NetworkPolicy); ok {
		c.deletes[policy.Namespace+"/"+policy.Name] = *(&client.DeleteOptions{}).ApplyOptions(options)
	}
	if err := c.Client.Delete(ctx, object, options...); err != nil {
		return err
	}
	if policy, ok := object.(*networkingv1.NetworkPolicy); ok && nativeIngressReviewPolicy(policy) && c.lostDelete {
		c.lostDelete = false
		return errors.New("accepted predecessor policy deletion response lost")
	}
	return nil
}

type nativeIngressReviewReader struct {
	client.Reader
	afterPodList func()
	listError    error
}

func (r *nativeIngressReviewReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		if r.listError != nil {
			return r.listError
		}
		if err := r.Reader.List(ctx, list, options...); err != nil {
			return err
		}
		if r.afterPodList != nil {
			r.afterPodList()
			r.afterPodList = nil
		}
		return nil
	}
	return r.Reader.List(ctx, list, options...)
}

type nativeIngressReviewTransport struct{ calls int }

func (r *nativeIngressReviewTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("fixture stops before sealed bootstrap exchange")
}

func nativeIngressReviewFixture(t *testing.T) (*externalRuntimePoolFixture, *nativeIngressReviewClient, *nativeIngressReviewTransport, corev1.Pod) {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.RuntimeNamespace = "native-runtime"
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	f.r.ControllerNamespace = "core-endpoints"
	f.r.RuntimeNamespace = pool.Spec.RuntimeNamespace
	f.r.ProviderProxy.Namespace = "proxy-endpoints"
	f.r.ProviderProxy.BaseURL = "http://provider-auth-proxy.proxy-endpoints.svc:8080"
	delegate := f.r.Client
	cl := &nativeIngressReviewClient{Client: delegate, runtimeNamespace: pool.Spec.RuntimeNamespace, creates: map[string]int{}, deletes: map[string]client.DeleteOptions{}}
	f.r.Client, f.r.APIReader = cl, delegate
	transport := &nativeIngressReviewTransport{}
	f.r.HTTPClient = &http.Client{Transport: transport}
	w, worker := f.materialize(t)
	a := w.Status.Allocation
	a.Identity.InstanceID = "native-process-instance"
	a.Startup.Identity = a.Identity
	a.Startup.Pod = nil
	a.Startup.Endpoint = "http://native-router.example:80/process"
	a.Startup.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "process", UID: a.Identity.InstanceID, Version: 1,
		Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
	worker.Labels["native.example/allocation"] = a.Identity.AllocationID
	worker.Labels["native.example/instance"] = a.Identity.InstanceID
	if err := delegate.Update(t.Context(), &worker); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	return f, cl, transport, worker
}

func TestExternalNativeIngressRejectsUnboundOrAmbiguousWorker(t *testing.T) {
	for _, mode := range []string{"missing allocation", "missing instance", "sibling", "deleting sibling", "replacement after inventory", "inventory outage"} {
		t.Run(mode, func(t *testing.T) {
			f, cl, transport, worker := nativeIngressReviewFixture(t)
			reader := &nativeIngressReviewReader{Reader: cl.Client}
			f.r.APIReader = reader
			switch mode {
			case "missing allocation", "missing instance":
				delete(worker.Labels, "native.example/"+strings.TrimPrefix(mode, "missing "))
				if err := cl.Update(t.Context(), &worker); err != nil {
					t.Fatal(err)
				}
			case "sibling", "deleting sibling":
				sibling := worker.DeepCopy()
				sibling.Name, sibling.UID, sibling.ResourceVersion = "unattested-sibling", "sibling-uid", ""
				if mode == "deleting sibling" {
					sibling.Finalizers = []string{"native.example/retain"}
				}
				if err := cl.Client.Create(t.Context(), sibling); err != nil {
					t.Fatal(err)
				}
				if mode == "deleting sibling" {
					if err := cl.Client.Delete(t.Context(), sibling); err != nil {
						t.Fatal(err)
					}
				}
			case "replacement after inventory":
				reader.afterPodList = func() {
					if err := cl.Client.Delete(t.Context(), &worker); err != nil {
						t.Fatal(err)
					}
					replacement := worker.DeepCopy()
					replacement.UID, replacement.ResourceVersion = "replacement-worker", ""
					if err := cl.Client.Create(t.Context(), replacement); err != nil {
						t.Fatal(err)
					}
				}
			case "inventory outage":
				reader.listError = errors.New("authoritative Pod inventory unavailable")
			}
			_, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)})
			if err == nil || (mode != "inventory outage" && !errors.Is(err, workspaceprovider.ErrStaleIdentity)) {
				t.Fatalf("ambiguous worker accepted: %v", err)
			}
			if len(cl.creates) != 0 || transport.calls != 0 || f.supervisor.probeCalls != 0 {
				t.Fatal("unattested worker obtained endpoint grants or bootstrap/probe calls")
			}
		})
	}
}

func TestExternalNativeIngressUsesUncachedFrozenTargetsAndRecoversLostCreate(t *testing.T) {
	f, cl, transport, worker := nativeIngressReviewFixture(t)
	cl.lostCreate = true
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err == nil {
		t.Fatal("lost accepted Create response was not surfaced")
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	targets, err := externalSavedIngressTargets(&pool)
	if err != nil || len(targets) != 2 || pool.Annotations[externalRuntimeEvidenceAnnotation] == "" || transport.calls != 0 {
		t.Fatalf("pre-create target/worker fences missing or credentials used: %v", err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if cl.cachedReads != 0 || transport.calls != 1 || len(cl.creates) != 2 {
		t.Fatalf("out-of-cache namespace recovery failed: cached=%d creates=%v bootstrap=%d", cl.cachedReads, cl.creates, transport.calls)
	}
	for _, target := range targets {
		policy := &networkingv1.NetworkPolicy{}
		if err := cl.Client.Get(t.Context(), types.NamespacedName{Namespace: target.Namespace, Name: target.Name}, policy); err != nil {
			t.Fatal(err)
		}
		if cl.creates[target.Name] != 1 || !reflect.DeepEqual(policy.Spec, externalIngressPolicySpec(target, worker.Namespace, worker.Labels)) {
			t.Fatal("recovery replayed Create or changed the attested worker selector")
		}
	}
}

func TestExternalNativeIngressPoolFenceFailuresNeverSeedOrCreate(t *testing.T) {
	for _, test := range []struct {
		name, annotation string
		lost             bool
	}{
		{"target fence rejected", externalProcessIngressTargetsAnnotation, false},
		{"target fence accepted response lost", externalProcessIngressTargetsAnnotation, true},
		{"worker fence rejected", externalRuntimeEvidenceAnnotation, false},
		{"worker fence accepted response lost", externalRuntimeEvidenceAnnotation, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, cl, transport, _ := nativeIngressReviewFixture(t)
			cl.patchFailure, cl.lostPatch = test.annotation, test.lost
			if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err == nil {
				t.Fatal("fence failure was not surfaced")
			}
			if len(cl.creates) != 0 || transport.calls != 0 {
				t.Fatal("failed durable fence allowed policy creation or credential bootstrap")
			}
			runtimePoolReconcile(t, f.r, f.pool)
			if len(cl.creates) != 2 || transport.calls != 1 {
				t.Fatalf("exact fence retry failed: creates=%v bootstrap=%d", cl.creates, transport.calls)
			}
		})
	}
}

func nativeIngressReviewDeletePool(t *testing.T, f *externalRuntimePoolFixture) {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.Delete(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	w := f.currentWorkspace(t)
	if w.Spec.Retirement == nil || w.Spec.Retirement.Identity != w.Status.Allocation.Identity || w.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementDelete {
		t.Fatal("Core did not authorize exact native retirement")
	}
	w.Status.Allocation.State, w.Status.Allocation.Startup = workspacev1alpha1.AllocationDeleted, nil
	w.Status.Allocation.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked,
		EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionDeleted, PersistentVolumes: workspacev1alpha1.DispositionDeleted,
		Checkpoints: workspacev1alpha1.DispositionDeleted, ProviderResources: workspacev1alpha1.DispositionDeleted}
	w.Status.ObservedGeneration = w.Generation
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
}

func TestExternalNativeIngressCleanupIgnoresForeignLabelsAndUsesFrozenNamespaces(t *testing.T) {
	for _, spoofKey := range []bool{false, true} {
		name := "wrong key"
		if spoofKey {
			name = "matching key"
		}
		t.Run(name, func(t *testing.T) {
			f, cl, _, worker := nativeIngressReviewFixture(t)
			runtimePoolReconcile(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			targets, err := externalFrozenIngressTargets(&pool, f.currentWorkspace(t).Spec.Workload.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := f.r.runtimePoolConfig(&pool)
			if err != nil {
				t.Fatal(err)
			}
			foreign := []*networkingv1.NetworkPolicy{
				{ObjectMeta: metav1.ObjectMeta{Namespace: "foreign", Name: targets[0].Name, UID: "foreign-native-target", Labels: cloneStringMap(cfg.labels)}, Spec: externalIngressPolicySpec(targets[0], worker.Namespace, worker.Labels)},
				{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: "foreign-name", UID: "foreign-runtime-policy", Labels: cloneStringMap(cfg.labels)}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{}}}},
			}
			for _, policy := range foreign {
				if !spoofKey {
					policy.Labels[runtimePoolKeyLabel] = "foreign-pool-key"
				}
				if err := cl.Client.Create(t.Context(), policy); err != nil {
					t.Fatal(err)
				}
			}
			// Current operator namespaces are not authority for older grants.
			f.r.ControllerNamespace, f.r.ProviderProxy.Namespace = "new-core", "new-proxy"
			f.r.ProviderProxy.BaseURL = "http://provider-auth-proxy.new-proxy.svc:8080"
			nativeIngressReviewDeletePool(t, f)
			for range 4 {
				runtimePoolReconcile(t, f.r, f.pool)
				if err := cl.Client.Get(t.Context(), client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
					break
				}
			}
			if err := cl.Client.Get(t.Context(), client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
				t.Fatalf("foreign policy blocked normal exact retirement: %v", err)
			}
			for _, policy := range foreign {
				kept := &networkingv1.NetworkPolicy{}
				if err := cl.Client.Get(t.Context(), client.ObjectKeyFromObject(policy), kept); err != nil || kept.UID != policy.UID {
					t.Fatalf("visible pool labels authorized foreign policy deletion: %v", err)
				}
			}
			for _, target := range targets {
				options, ok := cl.deletes[target.Namespace+"/"+target.Name]
				if !ok || options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
					t.Fatal("native policy deletion lost its exact UID/resourceVersion fence")
				}
			}
			if err := cl.Client.Get(t.Context(), client.ObjectKeyFromObject(&worker), &corev1.Pod{}); err != nil {
				t.Fatalf("Core deleted native infrastructure worker: %v", err)
			}
		})
	}
}

func TestExternalNativeIngressPolicyReadsUseAuthoritativeAPIReader(t *testing.T) {
	f, cl, transport, _ := nativeIngressReviewFixture(t)
	for range 2 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if cl.cachedReads != 0 || transport.calls != 2 {
		t.Fatalf("native policies outside cache scope could not be reconciled: cached=%d bootstrap=%d", cl.cachedReads, transport.calls)
	}
	for _, creates := range cl.creates {
		if creates != 1 {
			t.Fatal("out-of-cache native policy was created repeatedly")
		}
	}
}

func TestExternalNativeIngressRejectsMalformedOrMissingFrozenIntent(t *testing.T) {
	for _, mode := range []string{"malformed saved targets", "wrong saved target name", "missing frozen network policy"} {
		t.Run(mode, func(t *testing.T) {
			f, cl, transport, _ := nativeIngressReviewFixture(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			switch mode {
			case "malformed saved targets":
				pool.Annotations[externalProcessIngressTargetsAnnotation] = "not-json"
			case "wrong saved target name":
				pool.Annotations[externalProcessIngressTargetsAnnotation] = `[{"namespace":"proxy-endpoints","name":"foreign-policy","selector":{"app":"proxy"},"port":8080},{"namespace":"core-endpoints","name":"foreign-policy","selector":{"app":"controller"},"port":8080}]`
			case "missing frozen network policy":
				w := f.currentWorkspace(t)
				w.Spec.Workload.Runtime.NetworkPolicy = nil
				var err error
				w.Spec.Workload.Revision, err = workspacev1alpha1.WorkloadRevision(*w.Spec.Workload)
				if err != nil {
					t.Fatal(err)
				}
				status := w.Status.DeepCopy()
				status.Allocation.Identity.RequestRevision = w.Spec.Workload.Revision
				status.Allocation.Startup.Identity = status.Allocation.Identity
				if err := cl.Update(t.Context(), w); err != nil {
					t.Fatal(err)
				}
				w.Status = *status
				if err := f.r.Status().Update(t.Context(), w); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "missing frozen network policy" {
				if err := cl.Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
				t.Fatalf("unsupported native target intent admitted: %v", err)
			}
			if len(cl.creates) != 0 || transport.calls != 0 {
				t.Fatal("missing or malformed frozen intent allowed endpoint policies/credentials")
			}
		})
	}
}

func TestExternalNativeIngressLegacyCleanupRequiresValidFrozenRequest(t *testing.T) {
	for _, valid := range []bool{false, true} {
		name := "changed revision"
		if valid {
			name = "valid immutable request"
		}
		t.Run(name, func(t *testing.T) {
			f, cl, _, _ := nativeIngressReviewFixture(t)
			runtimePoolReconcile(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			delete(pool.Annotations, externalProcessIngressTargetsAnnotation)
			if err := cl.Update(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			nativeIngressReviewDeletePool(t, f)
			if !valid {
				w := f.currentWorkspace(t)
				w.Spec.Workload.Runtime.NetworkPolicy.Egress[0].To[0].NamespaceSelector.MatchLabels[corev1.LabelMetadataName] = "unadmitted-target"
				if err := cl.Update(t.Context(), w); err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				runtimePoolReconcile(t, f.r, f.pool)
				if err := cl.Client.Get(t.Context(), client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
					break
				}
			}
			err := cl.Client.Get(t.Context(), client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{})
			if apierrors.IsNotFound(err) != valid || (!valid && len(cl.deletes) != 0) {
				t.Fatalf("legacy cleanup did not retain exact frozen-intent authority: valid=%v deletion=%v err=%v", valid, cl.deletes, err)
			}
		})
	}
}

func TestExternalNativeIngressColdResumeRetiresPredecessorBeforeBindingAndRecovers(t *testing.T) {
	f, cl, transport, worker := nativeIngressReviewFixture(t)
	runtimePoolReconcile(t, f.r, f.pool)
	w := f.currentWorkspace(t)
	oldIdentity, oldSequence := w.Status.Allocation.Identity, w.Spec.Workload.Sequence
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	targets, err := externalSavedIngressTargets(&pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.Annotations["orka.ai/external-runtime-retirement-requested"] = booleanTrueValue
	if err := cl.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	w = f.currentWorkspace(t)
	if w.Spec.Retirement == nil || w.Spec.Retirement.Identity != oldIdentity {
		t.Fatal("predecessor did not receive Core's exact retirement authority")
	}
	w.Status.Allocation.State, w.Status.Allocation.Startup = workspacev1alpha1.AllocationStopped, nil
	w.Status.ObservedGeneration = w.Generation
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		runtimePoolReconcile(t, f.r, f.pool)
		w = f.currentWorkspace(t)
		if w.Spec.Workload.Sequence == oldSequence+1 {
			break
		}
	}
	if w.Spec.Workload.Sequence != oldSequence+1 || w.Spec.Workload.PreviousInstance == nil || *w.Spec.Workload.PreviousInstance != oldIdentity {
		t.Fatal("new workload did not freeze the independently terminated predecessor")
	}
	nextWorker := worker.DeepCopy()
	nextWorker.Namespace = "cold-resume-infrastructure"
	nextWorker.Name, nextWorker.UID, nextWorker.ResourceVersion = "cold-resume-worker", "cold-resume-worker-uid", ""
	nextIdentity := workspacev1alpha1.InstanceIdentity{AllocationID: "next-allocation", InstanceID: "next-native-instance", RequestRevision: w.Spec.Workload.Revision}
	nextWorker.Labels["native.example/allocation"], nextWorker.Labels["native.example/instance"] = nextIdentity.AllocationID, nextIdentity.InstanceID
	if err := cl.Client.Create(t.Context(), nextWorker); err != nil {
		t.Fatal(err)
	}
	w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Key: w.Spec.Workload.Key, Sequence: w.Spec.Workload.Sequence, Identity: nextIdentity, State: workspacev1alpha1.AllocationReady,
		Startup: &workspacev1alpha1.StartupEvidence{ContractVersion: workspacev1alpha1.LifecycleContractV1, Identity: nextIdentity, Endpoint: "http://native-router.example:80/replacement",
			Process: &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "replacement", UID: nextIdentity.InstanceID, Version: 1,
				Worker: workspacev1alpha1.PodReference{Namespace: nextWorker.Namespace, Name: nextWorker.Name, UID: nextWorker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("b", 64)}}}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	cl.beforeWorkerFence = func(candidate *corev1alpha1.RuntimePool) {
		binding, err := externalRuntimeEvidence(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if binding.Sequence != oldSequence+1 {
			return
		}
		for _, target := range targets {
			policy := &networkingv1.NetworkPolicy{}
			if err := cl.Client.Get(t.Context(), types.NamespacedName{Namespace: target.Namespace, Name: target.Name}, policy); !apierrors.IsNotFound(err) {
				t.Fatal("current evidence replaced predecessor before its exact ingress disappeared")
			}
		}
		cl.beforeWorkerFence = nil
	}
	cl.lostDelete = true
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err == nil {
		t.Fatal("lost predecessor delete response was not surfaced")
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	binding, err := externalRuntimeEvidence(&pool)
	if err != nil || binding.Identity != oldIdentity || transport.calls != 1 {
		t.Fatal("interrupted predecessor cleanup changed evidence or used credentials")
	}
	cl.lostCreate = true
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err == nil {
		t.Fatal("lost current Create response was not surfaced")
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	binding, err = externalRuntimeEvidence(&pool)
	if err != nil || binding.Identity != nextIdentity || transport.calls != 1 {
		t.Fatal("current Create lacked saved exact evidence or seeded before recovery")
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if transport.calls != 2 {
		t.Fatal("exact current recovery never reached sealed bootstrap")
	}
	for _, target := range targets {
		policy := &networkingv1.NetworkPolicy{}
		if err := cl.Client.Get(t.Context(), types.NamespacedName{Namespace: target.Namespace, Name: target.Name}, policy); err != nil || cl.creates[target.Name] != 2 || !reflect.DeepEqual(policy.Spec, externalIngressPolicySpec(target, nextWorker.Namespace, nextWorker.Labels)) {
			t.Fatalf("replacement ingress recovery changed worker fence or replayed Create: %v", err)
		}
	}
}

func TestExternalNativeIngressRejectsChangedExactPolicyBeforeBootstrapAndDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		name := "startup"
		if deleting {
			name = "retirement"
		}
		t.Run(name, func(t *testing.T) {
			f, cl, transport, _ := nativeIngressReviewFixture(t)
			runtimePoolReconcile(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			targets, err := externalSavedIngressTargets(&pool)
			if err != nil {
				t.Fatal(err)
			}
			key := types.NamespacedName{Namespace: targets[0].Namespace, Name: targets[0].Name}
			policy := &networkingv1.NetworkPolicy{}
			if err := cl.Client.Get(t.Context(), key, policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.Ingress = append(policy.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{})
			if err := cl.Update(t.Context(), policy); err != nil {
				t.Fatal(err)
			}
			if deleting {
				nativeIngressReviewDeletePool(t, f)
			}
			if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
				t.Fatalf("changed exact policy was accepted: %v", err)
			}
			if transport.calls != 1 || cl.deletes[key.String()].Preconditions != nil {
				t.Fatal("ambiguous exact policy was seeded or deleted")
			}
			if err := cl.Client.Get(t.Context(), key, &networkingv1.NetworkPolicy{}); err != nil {
				t.Fatalf("ambiguous policy disappeared: %v", err)
			}
		})
	}
}
