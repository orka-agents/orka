package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type backendConformanceFixture struct {
	r       *AgentRuntimeReconciler
	runtime *corev1alpha1.AgentRuntime
	config  conformancetest.Config
	server  *conformancetest.Server
	pod     *corev1.Pod
	service *corev1.Service
	slice   *discoveryv1.EndpointSlice
}

func newBackendConformanceFixture(t *testing.T) *backendConformanceFixture {
	t.Helper()
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "backend-instance", SupervisorBootID: "backend-boot", RuntimePoolUID: "backend-pool",
		ControllerEpoch: 1, Profile: profile, Limits: limits, WorkspaceGovernance: claims, SupportsDrain: true,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	runtime, secret := testAgentRuntimeAndSecret(t, "http://runtime.default.svc.cluster.local:8080", config)
	_, _, pod, service, slice := runtimeRecoveryObjects(t, runtime, server.URL())
	service.Spec.Ports[0].TargetPort = intstr.FromString("control")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "control", ContainerPort: *slice.Ports[0].Port, Protocol: corev1.ProtocolTCP}}
	r := newAgentRuntimeUnitReconciler(t, runtime, secret, pod, service, slice)
	if runtime.Spec.Deployment.KubernetesRecovery != nil {
		t.Fatal("fixture unexpectedly enrolled Kubernetes recovery")
	}
	return &backendConformanceFixture{r: r, runtime: runtime, config: config, server: server, pod: pod, service: service, slice: slice}
}

func (f *backendConformanceFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, f.r, f.runtime)
	f.runtime = &updated
}

func (f *backendConformanceFixture) replacePod(t *testing.T) {
	t.Helper()
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.UID = "replacement-pod-uid"
	f.pod.ResourceVersion = ""
	if err := f.r.Create(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.slice), f.slice); err != nil {
		t.Fatal(err)
	}
	f.slice.Endpoints[0].TargetRef.UID = f.pod.UID
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRuntimeServiceBackendReplacementRequiresDeepConformance(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(*testing.T, *backendConformanceFixture)
	}{
		{name: "new Pod at same address", replace: func(t *testing.T, f *backendConformanceFixture) { f.replacePod(t) }},
		{name: "container restart in same Pod", replace: func(t *testing.T, f *backendConformanceFixture) {
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
				t.Fatal(err)
			}
			f.pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement-container"
			f.pod.Status.ContainerStatuses[0].RestartCount++
			if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "recreated Service", replace: func(t *testing.T, f *backendConformanceFixture) {
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.service), f.service); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Delete(t.Context(), f.service); err != nil {
				t.Fatal(err)
			}
			f.service.UID = "replacement-service-uid"
			f.service.ResourceVersion = ""
			if err := f.r.Create(t.Context(), f.service); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBackendConformanceFixture(t)
			f.reconcile(t)
			if !f.runtime.Status.Ready || f.server.Counts().PromptStarts != 2 {
				t.Fatalf("initial deep conformance failed: ready=%v, message=%s", f.runtime.Status.Ready, f.runtime.Status.Message)
			}
			before := f.server.Counts()
			f.reconcile(t)
			if !f.runtime.Status.Ready || f.server.Counts() != before {
				t.Fatal("unchanged backend did not use shallow conformance")
			}
			originalPins, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime)
			if err != nil || len(originalPins) != 1 {
				t.Fatalf("conformed backend admission failed: %v", err)
			}
			test.replace(t, f)
			if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err == nil {
				t.Fatal("replacement admitted through stale Ready status before reconciliation")
			}
			cleanupPins, err := f.r.AgentRuntimeServiceBackendPins(t.Context(), f.runtime)
			if err != nil || !slices.Equal(cleanupPins, originalPins) {
				t.Fatalf("physical conformance change altered cleanup pinning: %v", err)
			}
			f.reconcile(t)
			if !f.runtime.Status.Ready {
				t.Fatalf("conformant replacement rejected: %s", f.runtime.Status.Message)
			}
			if got := f.server.Counts().PromptStarts - before.PromptStarts; got != 2 {
				t.Fatalf("same-fence physical replacement ran %d lifecycle prompts, want 2 before readiness", got)
			}
			if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err != nil {
				t.Fatalf("successfully conformed replacement not admitted: %v", err)
			}
		})
	}
}

func TestAgentRuntimeServiceBackendBrokenReplacementCannotInheritReady(t *testing.T) {
	f := newBackendConformanceFixture(t)
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("initial deep conformance failed: %s", f.runtime.Status.Message)
	}
	originalAuthority, err := f.r.agentRuntimeCleanupSecret(t.Context(), f.runtime)
	if err != nil || originalAuthority == nil {
		t.Fatalf("initial cleanup authority missing: %v", err)
	}
	endpoint, err := url.Parse(f.server.URL())
	if err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	f.config.ListenAddress = endpoint.Host
	f.config.BreakDuplicateSafeMutations = true
	f.server, err = conformancetest.NewServer(f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.Close)
	f.replacePod(t)
	f.reconcile(t)
	if f.runtime.Status.Ready {
		t.Fatal("same-fence broken replacement inherited readiness without deep conformance")
	}
	if f.server.Counts().SessionCreates == 0 {
		t.Fatal("replacement was not exercised by deep conformance")
	}
	if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err == nil {
		t.Fatal("failed replacement admitted work")
	}
	retainedAuthority, err := f.r.agentRuntimeCleanupSecret(t.Context(), f.runtime)
	if err != nil || retainedAuthority == nil || !bytes.Equal(
		originalAuthority.Data[agentRuntimeCleanupSecretAuthorityKey], retainedAuthority.Data[agentRuntimeCleanupSecretAuthorityKey],
	) {
		t.Fatalf("failed replacement overwrote original cleanup authority: %v", err)
	}
}

func TestAgentRuntimeServiceBackendNativeSidecarRestartRequiresConformance(t *testing.T) {
	f := newBackendConformanceFixture(t)
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.Spec.InitContainers = []corev1.Container{{Name: "runtime-proxy", Image: "example/proxy", RestartPolicy: new(corev1.ContainerRestartPolicyAlways)}}
	if err := f.r.Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	proxyStatus := f.pod.Status.ContainerStatuses[0]
	proxyStatus.Name, proxyStatus.ContainerID = "runtime-proxy", "containerd://proxy-1"
	f.pod.Status.InitContainerStatuses = []corev1.ContainerStatus{proxyStatus}
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("initial conformance failed: %s", f.runtime.Status.Message)
	}
	before := f.server.Counts()
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.Status.InitContainerStatuses[0].ContainerID = "containerd://proxy-2"
	f.pod.Status.InitContainerStatuses[0].RestartCount++
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err == nil {
		t.Fatal("native sidecar replacement inherited the old Pod conformance")
	}
	f.reconcile(t)
	if !f.runtime.Status.Ready || f.server.Counts().PromptStarts-before.PromptStarts != 2 {
		t.Fatal("native sidecar replacement did not receive deep conformance")
	}
}

func TestAgentRuntimeServiceBackendLegacyProofRequiresDeepConformance(t *testing.T) {
	f := newBackendConformanceFixture(t)
	f.reconcile(t)
	secret, err := f.r.agentRuntimeCleanupSecret(t.Context(), f.runtime)
	if err != nil || secret == nil {
		t.Fatalf("cleanup authority missing: %v", err)
	}
	snapshot, err := agentRuntimeDeletionSnapshotFromSecret(f.runtime, secret)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ServiceBackendDigest = ""
	secret.Data[agentRuntimeCleanupSecretAuthorityKey], err = harnessv2.CanonicalValue(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeAgentRuntimeDeletionSnapshot(f.runtime, secret); err != nil {
		t.Fatalf("legacy snapshot lost cleanup authority: %v", err)
	}
	if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err == nil {
		t.Fatal("legacy snapshot certified an unrecorded physical backend")
	}
	before := f.server.Counts()
	f.reconcile(t)
	if !f.runtime.Status.Ready || f.server.Counts().PromptStarts-before.PromptStarts != 2 {
		t.Fatal("legacy snapshot was not upgraded by deep conformance")
	}
	if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err != nil {
		t.Fatalf("new physical conformance proof was not usable: %v", err)
	}
}

func TestAgentRuntimeServiceBackendPublicationUsesProbedIdentity(t *testing.T) {
	f := newBackendConformanceFixture(t)
	observed, ready, controllerVersion, capabilityVersion, backendDigest, message := f.r.probeAgentRuntime(t.Context(), f.runtime)
	if !ready || backendDigest == "" {
		t.Fatalf("initial conformance failed: %s", message)
	}
	f.replacePod(t)
	if err := f.r.persistAgentRuntimeDeletionSnapshot(t.Context(), f.runtime, observed, controllerVersion, capabilityVersion, backendDigest); err == nil {
		t.Fatal("publication certified an unprobed replacement")
	}
	if secret, err := f.r.agentRuntimeCleanupSecret(t.Context(), f.runtime); err != nil || secret != nil {
		t.Fatalf("replacement acquired conformance authority: %v", err)
	}
}

type backendConformanceChangeReader struct {
	client.Reader
	afterRead func(client.Object)
}

func (r *backendConformanceChangeReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	r.afterRead(object)
	return nil
}

func TestAgentRuntimeServiceBackendChangeDuringConformanceCannotPublishReady(t *testing.T) {
	f := newBackendConformanceFixture(t)
	changed := false
	f.r.APIReader = &backendConformanceChangeReader{Reader: f.r.APIReader, afterRead: func(object client.Object) {
		if _, serviceRead := object.(*corev1.Service); serviceRead && !changed && f.server.Counts().SessionDeletes == 2 {
			changed = true
			f.replacePod(t)
		}
	}}
	f.reconcile(t)
	if !changed {
		t.Fatal("replacement at final conformance identity check was not exercised")
	}
	if f.runtime.Status.Ready {
		t.Fatal("backend changed during conformance but became Ready")
	}
	if secret, err := f.r.agentRuntimeCleanupSecret(t.Context(), f.runtime); err != nil || secret != nil {
		t.Fatalf("replacement acquired conformance authority: %v", err)
	}
}

func TestAgentRuntimeServiceBackendConformanceCoversEveryPin(t *testing.T) {
	for _, broken := range []bool{false, true} {
		name := "conformant replacement set"
		if broken {
			name = "one broken new backend"
		}
		t.Run(name, func(t *testing.T) {
			f := newBackendConformanceFixture(t)
			f.reconcile(t)
			before := f.server.Counts()
			config := f.config
			config.BreakDuplicateSafeMutations = broken
			other, err := conformancetest.NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(other.Close)
			pod := f.pod.DeepCopy()
			pod.Name, pod.UID, pod.ResourceVersion = "other-pod", "other-pod-uid", ""
			pod.Spec.Containers[0].Ports[0].ContainerPort = runtimeRecoveryServerPort(t, other.URL())
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://other-container"
			if err := f.r.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			slice := f.slice.DeepCopy()
			slice.Name, slice.UID, slice.ResourceVersion = "other-endpoints", "other-slice-uid", ""
			slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, other.URL()))
			slice.Endpoints[0].TargetRef.Name, slice.Endpoints[0].TargetRef.UID = pod.Name, pod.UID
			if err := f.r.Create(t.Context(), slice); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err == nil {
				t.Fatal("added backend admitted before conformance")
			}
			f.reconcile(t)
			if broken {
				if f.runtime.Status.Ready || other.Counts().SessionCreates == 0 {
					t.Fatal("broken backend did not fail deep conformance")
				}
				return
			}
			if !f.runtime.Status.Ready || other.Counts().PromptStarts != 2 || f.server.Counts().PromptStarts-before.PromptStarts != 2 {
				t.Fatalf("every backend was not deeply conformed: %s", f.runtime.Status.Message)
			}
			if pins, err := f.r.AgentRuntimeConformedServiceBackendPins(t.Context(), f.runtime); err != nil || len(pins) != 2 {
				t.Fatalf("conformed backend set not admitted: %v", err)
			}
		})
	}
}

// The conformance server supplies real capability/status and admission routes.
// This proxy supplies the two existing-prompt responses needed to exercise the
// dispatcher's installed before-mutation callback through the public v2 client.
func newBackendExistingWorkFixture(t *testing.T) (*backendConformanceFixture, *ACPDispatcher, harnessv2.MCPPolicyConfiguration, *atomic.Int32) {
	t.Helper()
	f := newBackendConformanceFixture(t)
	target, err := url.Parse(f.server.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response any
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/lease"):
			var request harnessv2.RenewPromptLeaseRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode lease renewal: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			response = harnessv2.PromptLeaseResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				Lease: request.Lease,
			}
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/permissions/"):
			var request harnessv2.ResolvePermissionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode permission decision: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			response = harnessv2.PermissionResolutionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				State: harnessv2.PermissionResolutionApplied, Decision: request.Decision, ResolvedAt: time.Now().UTC(),
			}
		default:
			proxy.ServeHTTP(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode existing-work response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.slice), f.slice); err != nil {
		t.Fatal(err)
	}
	f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, server.URL))
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.Spec.Containers[0].Ports[0].ContainerPort = *f.slice.Ports[0].Port
	if err := f.r.Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("initial deep conformance failed: %s", f.runtime.Status.Message)
	}
	configuration, err := buildAgentRuntimeMCPConfigurationWithRegistry(t.Context(), f.r.Client, f.runtime, f.config.Profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &ACPDispatcher{Client: f.r.Client, APIReader: f.r.APIReader, Epochs: f.r.ControllerEpochManager}
	return f, d, configuration, calls
}

func backendExistingWorkMutation(t *testing.T, runtimeClient *harnessv2.Client, fence harnessv2.Fence, profile harnessv2.RuntimeProfile, configuration harnessv2.MCPPolicyConfiguration, operation string) error {
	t.Helper()
	fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "existing-session", 1
	now := time.Now().UTC()
	metadata := harnessv2.MutationMetadata{
		Fence: fence, TaskUID: "existing-task", TaskAttempt: 1, PromptID: "existing-prompt", OperationID: harnessv2.OperationID(operation),
		RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion, ExpiresAt: now.Add(30 * time.Second),
	}
	if operation == "renew_prompt_lease" {
		lease := harnessv2.PromptLease{Generation: 2, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
		authorization, err := buildPromptMCPAuthorization(configuration, fence, profile, metadata, lease, metadata.ExpiresAt)
		if err != nil {
			t.Fatal(err)
		}
		request := harnessv2.RenewPromptLeaseRequest{
			Protocol: harnessv2.ProtocolVersion, Metadata: metadata, ExpectedLeaseGeneration: 1, Lease: lease, MCPAuthorization: authorization,
		}
		if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
			t.Fatal(err)
		}
		_, err = runtimeClient.RenewPromptLease(t.Context(), "existing-session", request)
		return err
	}
	request := harnessv2.ResolvePermissionRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: metadata, RequestID: "existing-permission",
		Decision: harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow-once"},
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		t.Fatal(err)
	}
	_, err := runtimeClient.ResolvePermission(t.Context(), "existing-session", request)
	return err
}

func TestACPDispatcherExternalServiceExistingWorkDoesNotRequireAdmissionConformance(t *testing.T) {
	for _, operation := range []string{"renew_prompt_lease", "resolve_permission"} {
		for _, constructAfterChange := range []bool{false, true} {
			stage := "existing client"
			if constructAfterChange {
				stage = "non-admission client construction"
			}
			for _, change := range []string{"readiness lost", "same-address backend replacement"} {
				t.Run(operation+"/"+stage+"/"+change, func(t *testing.T) {
					f, d, configuration, calls := newBackendExistingWorkFixture(t)
					runtimeClient, fence, profile, _, err := d.externalRuntimeClient(t.Context(), f.runtime, configuration, true)
					if err != nil {
						t.Fatal(err)
					}
					if change == "readiness lost" {
						f.runtime.Status.Ready = false
						if err := f.r.Status().Update(t.Context(), f.runtime); err != nil {
							t.Fatal(err)
						}
					} else {
						f.replacePod(t)
					}
					if constructAfterChange {
						runtimeClient, fence, profile, _, err = d.externalRuntimeClient(t.Context(), f.runtime, configuration, false)
						if err != nil {
							t.Fatalf("construct existing-work client: %v", err)
						}
					}
					if err := backendExistingWorkMutation(t, runtimeClient, fence, profile, configuration, operation); err != nil {
						t.Fatalf("existing work blocked by admission-only conformance: %v", err)
					}
					if calls.Load() != 1 {
						t.Fatalf("existing-work mutations = %d, want 1", calls.Load())
					}
					if _, _, _, _, err := d.externalRuntimeClient(t.Context(), f.runtime, configuration, true); err == nil {
						t.Fatal("new admission client accepted unconformed runtime")
					}
					// Even a client constructed for existing work must recheck
					// admission conformance when asked to create a new session.
					task := &corev1alpha1.Task{}
					task.UID = "new-task"
					task.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "new-prompt"}
					_, workspace, err := emptyRuntimeWorkspace(task, "")
					if err != nil {
						t.Fatal(err)
					}
					metadata := mutationMetadata(fence, task, "create", false, time.Now().UTC().Add(30*time.Second))
					request := harnessv2.CreateRuntimeSessionRequest{
						Protocol: harnessv2.ProtocolVersion, Metadata: metadata, RuntimeSessionID: harnessv2.RuntimeSessionID(runtimeSessionID(metadata.Fence)),
						Profile: profile, MCPConfiguration: configuration, Workspace: workspace,
					}
					if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
						t.Fatal(err)
					}
					before := f.server.Counts()
					if _, err := runtimeClient.CreateRuntimeSession(t.Context(), request); err == nil ||
						!strings.Contains(err.Error(), "conformance") {
						t.Fatalf("admission after conformance change: %v", err)
					}
					if f.server.Counts() != before {
						t.Fatal("new admission reached the runtime")
					}
				})
			}
		}
	}
}

func TestACPDispatcherExternalServiceExistingWorkStillRejectsAuthorityDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *backendConformanceFixture)
		want   string
	}{
		{
			name: "registration", want: "registration or observed authority changed",
			change: func(t *testing.T, f *backendConformanceFixture) {
				f.runtime.Generation++
				if err := f.r.Update(t.Context(), f.runtime); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "observed boot", want: "registration or observed authority changed",
			change: func(t *testing.T, f *backendConformanceFixture) {
				f.runtime.Status.ObservedCapabilities.SupervisorBootID = "replacement-boot"
				if err := f.r.Status().Update(t.Context(), f.runtime); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "backend address", want: "verified backend set changed",
			change: func(t *testing.T, f *backendConformanceFixture) {
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
					t.Fatal(err)
				}
				f.pod.Spec.Containers[0].Ports[0].ContainerPort = 1
				if err := f.r.Update(t.Context(), f.pod); err != nil {
					t.Fatal(err)
				}
				f.slice.Ports[0].Port = new(int32(1))
				if err := f.r.Update(t.Context(), f.slice); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "credentials", want: "authentication authority changed",
			change: func(t *testing.T, f *backendConformanceFixture) {
				secret := &corev1.Secret{}
				ref := f.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef
				if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: f.runtime.Namespace, Name: ref.Name}, secret); err != nil {
					t.Fatal(err)
				}
				secret.Data[ref.Key] = bytes.Repeat([]byte("r"), 32)
				if err := f.r.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "controller epoch", want: "authenticated status fence drifted",
			change: func(t *testing.T, f *backendConformanceFixture) {
				setAgentRuntimeTestControllerEpoch(f.r.ControllerEpochManager, 2)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, d, configuration, calls := newBackendExistingWorkFixture(t)
			runtimeClient, fence, profile, _, err := d.externalRuntimeClient(t.Context(), f.runtime, configuration, true)
			if err != nil {
				t.Fatal(err)
			}
			f.runtime.Status.Ready = false
			if err := f.r.Status().Update(t.Context(), f.runtime); err != nil {
				t.Fatal(err)
			}
			test.change(t, f)
			for _, operation := range []string{"renew_prompt_lease", "resolve_permission"} {
				if err := backendExistingWorkMutation(t, runtimeClient, fence, profile, configuration, operation); err == nil ||
					!strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "zero_bytes_written") {
					t.Fatalf("%s authority rejection: %v", operation, err)
				}
			}
			if calls.Load() != 0 {
				t.Fatalf("%d existing-work mutations reached the runtime after authority drift", calls.Load())
			}
		})
	}
}
