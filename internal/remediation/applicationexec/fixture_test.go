package applicationexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Protocol fixtures exercise real binding, lifecycle, SQLite evidence validation
// and sealing. These fake API/worker reports are not runtime qualification; the
// deployed worker and network/storage boundary require separate live proof.
type fixture struct {
	t          *testing.T
	backend    *Backend
	control    client.Client
	target     client.Client
	store      *sqlite.Store
	request    contract.ExecutionRequest
	scheme     *runtime.Scheme
	config     Config
	sequence   atomic.Int64
	reports    sync.Map
	created    []client.Object
	deleted    []types.UID
	autoPods   bool
	reclaim    bool
	createHook func(context.Context, client.WithWatch, client.Object) error
	reportHook func(string, any)
}

func testCA(t *testing.T) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "application test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, scheme: runtime.NewScheme(), autoPods: true, reclaim: true}
	require.NoError(t, clientgoscheme.AddToScheme(f.scheme))
	require.NoError(t, corev1alpha1.AddToScheme(f.scheme))
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f.store = sqlite.NewStore(db, ":memory:")
	require.NoError(t, f.store.InitializeKubernetesValidationStore(t.Context()))
	f.config = Config{Enabled: true, Namespace: "control", WorkerImage: "example/worker@" + pv.Digest([]byte("worker")),
		WorkerImageID: pv.Digest([]byte("worker-config")), ToolchainImage: "example/go@" + pv.Digest([]byte("go")),
		ToolchainImageID: pv.Digest([]byte("go-config")), GoVersion: "go1.27.2", Platform: "linux/amd64", StorageClass: "standard",
		NodeSelectorKey: "orka.ai/application-node", NodeSelectorValue: "dedicated", TargetClusterUID: "target-cluster", ControlClusterUID: "control-cluster",
		TargetAPIServer: "https://172.18.0.2:6443", TargetCAData: testCA(t), CanaryHost: "192.0.2.10", CanaryPort: 8081,
		SourceEndpoints:   []SourceEndpoint{{Host: "github.com", Address: "192.0.2.20"}},
		ReconcileInterval: time.Millisecond, RunTimeout: time.Minute, CleanupTimeout: time.Second}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "parent", UID: "parent-uid"}}
	f.control = fake.NewClientBuilder().WithScheme(f.scheme).
		WithObjects(parent, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "control-cluster"}}).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if object.GetUID() == "" {
				object.SetUID(types.UID(fmt.Sprintf("control-%d", f.sequence.Add(1))))
			}
			object.SetCreationTimestamp(metav1.Now())
			return c.Create(ctx, object, options...)
		}}).Build()
	f.target = fake.NewClientBuilder().WithScheme(f.scheme).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "target-cluster"}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "target-node", UID: "node-uid",
				Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64", f.config.NodeSelectorKey: f.config.NodeSelectorValue}},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
			&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "storage-uid"}, Provisioner: "example.test/provisioner",
				ReclaimPolicy: new(corev1.PersistentVolumeReclaimDelete), VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer)}).
		WithStatusSubresource(&corev1.Pod{}, &batchv1.Job{}, &corev1.PersistentVolumeClaim{}, &networkingv1.Ingress{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: f.createTarget,
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
				opts := (&client.DeleteOptions{}).ApplyOptions(options)
				require.NotNil(t, opts.Preconditions)
				require.NotNil(t, opts.Preconditions.UID)
				require.Equal(t, object.GetUID(), *opts.Preconditions.UID)
				f.deleted = append(f.deleted, *opts.Preconditions.UID)
				if pvc, ok := object.(*corev1.PersistentVolumeClaim); ok && f.reclaim && pvc.Spec.VolumeName != "" {
					volume := &corev1.PersistentVolume{}
					require.NoError(t, c.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, volume))
					require.NoError(t, c.Delete(ctx, volume))
				}
				return c.Delete(ctx, object, options...)
			},
		}).Build()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		parts := strings.Split(request.URL.Path, "/")
		if len(parts) != 8 || parts[7] != "log" {
			http.NotFound(writer, request)
			return
		}
		content, ok := f.reports.Load(parts[4] + "/" + parts[6])
		if !ok {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(content.([]byte))
	}))
	t.Cleanup(server.Close)
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: 1000, Burst: 1000})
	require.NoError(t, err)
	f.backend, err = New(f.control, f.control, f.target, f.target, kube, f.store, f.scheme, f.config)
	require.NoError(t, err)
	f.request = applicationRequest(t, f.backend)
	return f
}

func applicationRequest(t *testing.T, backend *Backend) contract.ExecutionRequest {
	t.Helper()
	healthy, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{Exists: true, Found: true, Value: json.RawMessage(`[{"ip":"198.51.100.10"}]`)})
	require.NoError(t, err)
	failure, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{Exists: true, Found: true, Value: json.RawMessage(`[]`)})
	require.NoError(t, err)
	plan := backend.ApplicationPlanDefaults()
	plan.Build.Package = "."
	for _, name := range []string{"case", "control"} {
		plan.Resources = append(plan.Resources, json.RawMessage(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"`+name+`"},"spec":{"defaultBackend":{"service":{"name":"backend","port":{"number":8080}}}}}`))
	}
	plan.Permissions = []pv.ApplicationPermission{{APIGroup: "networking.k8s.io", Resources: []string{"ingresses/status"}, Verbs: []string{"get", "patch", "update"}}}
	request := contract.ExecutionRequest{ID: "generic-experiment", Namespace: "control", ParentTaskName: "parent", ParentTaskUID: "parent-uid",
		Action: pv.ValidateReport, Problem: "generic status reconciliation", EvidenceScope: contract.ApplicationRuntime,
		Deadline: new(time.Now().UTC().Add(time.Minute)),
		Source:   contract.Source{Repository: "https://github.com/example/upstream", Commit: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)},
		Checks:   contract.Checks{Scope: []string{"status reconciliation"}, Application: &plan}}
	for index, name := range []string{"case", "control"} {
		kind := pv.Reproduction
		if index == 1 {
			kind = pv.Normal
		}
		kubernetesCheck := &pv.ApplicationKubernetesCheck{APIVersion: "networking.k8s.io/v1",
			Resource: "ingresses", Name: name, Pointer: "/status/loadBalancer/ingress"}
		if kind == pv.Normal {
			kubernetesCheck.NormalValue = json.RawMessage(`[{"ip":"198.51.100.10"}]`)
		}
		request.Checks.Checks = append(request.Checks.Checks, pv.Check{ID: name, Kind: kind, TimeoutSeconds: 5, Healthy: healthy, Failure: failure,
			Application: &pv.ApplicationCheck{Version: pv.ApplicationProtocolVersion, Kubernetes: kubernetesCheck}})
	}
	return request
}

func (f *fixture) createTarget(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
	if object.GetUID() == "" {
		object.SetUID(types.UID(fmt.Sprintf("target-%d", f.sequence.Add(1))))
	}
	object.SetCreationTimestamp(metav1.Now())
	f.created = append(f.created, object.DeepCopyObject().(client.Object))
	if f.createHook != nil {
		if err := f.createHook(ctx, c, object); err != nil {
			return err
		}
	}
	if pvc, ok := object.(*corev1.PersistentVolumeClaim); ok {
		pvc.Spec.VolumeName, pvc.Status.Phase = "pv-"+string(pvc.UID), corev1.ClaimBound
		volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName, UID: types.UID("volume-" + string(pvc.UID))},
			Spec: corev1.PersistentVolumeSpec{StorageClassName: f.config.StorageClass, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				ClaimRef: &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID}}}
		if err := c.Create(ctx, volume); err != nil {
			return err
		}
	}
	if err := c.Create(ctx, object, options...); err != nil {
		return err
	}
	if job, ok := object.(*batchv1.Job); ok && f.autoPods {
		f.createWorkerPod(ctx, c, job)
	}
	return nil
}

func (f *fixture) createWorkerPod(ctx context.Context, c client.WithWatch, job *batchv1.Job) {
	f.t.Helper()
	container := job.Spec.Template.Spec.Containers[0]
	imageID := f.config.WorkerImageID
	switch container.Name {
	case "build":
		imageID = f.config.ToolchainImageID
	case "subject":
		imageID = f.backend.config.RuntimeImageID
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: job.Name + "-pod",
		UID: types.UID("pod-" + string(job.UID)), Labels: map[string]string{runLabel: job.Labels[runLabel], "job-name": job.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}},
		Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodSucceeded, PodIP: "10.244.0.10",
			ContainerStatuses: []corev1.ContainerStatus{{Name: container.Name, Image: container.Image, ImageID: imageID,
				ContainerID: "containerd://container-" + string(job.UID),
				State:       corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}}}
	pod.Spec.NodeName = "target-node"
	if container.Name == "subject" || container.Name == "marker" {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	for _, init := range pod.Spec.InitContainers {
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{
			Name: init.Name, ImageID: f.config.WorkerImageID, ContainerID: "containerd://init-" + string(job.UID),
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}})
	}
	require.NoError(f.t, c.Create(ctx, pod))
	if container.Name == "subject" || container.Name == "marker" {
		return
	}
	config := &corev1.ConfigMap{}
	require.NoError(f.t, c.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: job.Name + "-input"}, config))
	var report any
	switch container.Name {
	case "boundary":
		var input pv.ApplicationBoundaryInput
		require.NoError(f.t, json.Unmarshal(config.BinaryData["input.json"], &input))
		digest, err := pv.ApplicationBoundaryInputDigest(input)
		require.NoError(f.t, err)
		account := &corev1.ServiceAccount{}
		require.NoError(f.t, c.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: pod.Spec.ServiceAccountName}, account))
		value := &pv.ApplicationBoundaryReport{Version: pv.ApplicationProtocolVersion, InputDigest: digest, TaskUID: input.TaskUID, PodUID: string(pod.UID),
			UID: 65532, GID: 65532, PID: 1, NoNewPrivileges: true, SeccompMode: 2, ReadOnlyRoot: true,
			MemoryMax: 256 << 20, PIDsMax: 256, CPUQuota: 100000, CPUPeriod: 100000, CanaryReachable: input.PositiveControl,
			APIUserUID: string(account.UID), APIStatuses: map[string]int{"self-identity": 201, "foreign-namespace": 403, "secrets": 403, "token-request": 403, "cluster-list": 403},
			Capabilities: map[string]string{"CapEff": "0000000000000000", "CapPrm": "0000000000000000", "CapBnd": "0000000000000000", "CapInh": "0000000000000000", "CapAmb": "0000000000000000"}}
		fileSize, err := pv.ApplicationFileSizeLimit(input.FileSizeRole)
		require.NoError(f.t, err)
		value.FileSizeSoft, value.FileSizeHard = fileSize, fileSize
		if input.PositiveControl {
			value.APIStatuses["self-pod"] = 200
		}
		if input.APIAddress == "" {
			value.APIStatuses, value.APIUserUID = nil, ""
		}
		if input.SourceHost != "" && input.PositiveControl {
			value.SourceHTTPSVerified, value.SourceHTTPStatus = true, 200
		}
		report = value
	case "prepare":
		var input pv.ApplicationPreparationInput
		require.NoError(f.t, json.Unmarshal(config.BinaryData["input.json"], &input))
		digest, err := pv.PreparationInputDigest(input)
		require.NoError(f.t, err)
		original := []byte("fixture archive-original")
		manifest := input.Manifest
		manifest.Sources.Original.ArchiveDigest = pv.Digest(original)
		metadata := &pv.ApplicationSourceMetadata{Version: pv.ApplicationProtocolVersion,
			Original: pv.ApplicationArchiveMetadata{ArchiveDigest: pv.Digest(original), ArchiveBytes: 2048, ContentBytes: int64(len(original)), Entries: 1}}
		provenance := make(map[string][]byte)
		if manifest.Action == pv.VerifyPatch {
			patched, difference := []byte("fixture archive-patched"), []byte("fixture source difference")
			manifest.Sources.Patched = pv.SourceIdentity{Tree: strings.Repeat("d", 40), ArchiveDigest: pv.Digest(patched)}
			manifest.Sources.PatchDigest, manifest.Sources.DiffDigest = pv.Digest(input.Patch), pv.Digest(difference)
			metadata.Patched = &pv.ApplicationArchiveMetadata{ArchiveDigest: pv.Digest(patched), ArchiveBytes: 2048, ContentBytes: int64(len(patched)), Entries: 1}
			provenance[pv.Digest(input.Patch)], provenance[pv.Digest(difference)] = input.Patch, difference
		}
		manifest.ApplicationSourceMetadata = metadata
		content, err := json.Marshal(metadata)
		require.NoError(f.t, err)
		provenance[pv.Digest(content)] = content
		report = &pv.ApplicationPreparationReport{Version: pv.PreparationProtocolVersion, TaskUID: input.TaskUID,
			PodUID: string(pod.UID), InputDigest: digest, Manifest: manifest, Provenance: provenance}
	case "build":
		var input pv.ApplicationWorkerInput
		require.NoError(f.t, json.Unmarshal(config.BinaryData["input.json"], &input))
		partial := input.Worker
		partial.PodName, partial.PodUID = pod.Name, string(pod.UID)
		artifact, err := pv.NewApplicationArtifact(input, pv.Digest([]byte("fixture-binary-"+input.Side)), 64, partial)
		require.NoError(f.t, err)
		report = &pv.ApplicationBuildReport{Version: pv.ApplicationProtocolVersion, TaskUID: input.TaskUID, PodUID: string(pod.UID), Artifact: artifact}
	case "observe":
		var input pv.ApplicationWorkerInput
		require.NoError(f.t, json.Unmarshal(config.BinaryData["input.json"], &input))
		check := input.Manifest.Checks[0]
		for _, candidate := range input.Manifest.Checks {
			if candidate.ID == input.CheckID {
				check = candidate
			}
		}
		expected := check.Healthy
		if input.Side == pv.Original && check.Kind == pv.Reproduction {
			expected = check.Failure
		}
		var value pv.ApplicationAPIResult
		require.NoError(f.t, json.Unmarshal([]byte(expected.Stdout), &value))
		object := &networkingv1.Ingress{}
		require.NoError(f.t, c.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: check.Application.Kubernetes.Name}, object))
		if check.Kind == pv.Normal {
			object.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "198.51.100.10"}}
			require.NoError(f.t, c.Status().Update(ctx, object))
		}
		source, taskUID := input.Manifest.Sources.Original, input.Binding.OriginalTaskID
		if input.Side == pv.Patched {
			source, taskUID = input.Manifest.Sources.Patched, input.Binding.PatchedTaskID
		}
		now := time.Now().UTC()
		observation := pv.Observation{RunID: input.Binding.RunID, AttemptID: input.Binding.AttemptID, TaskID: taskUID,
			ManifestDigest: input.Binding.ManifestDigest, Side: input.Side, CheckID: check.ID, SourceTree: source.Tree,
			ImageID: imageID, ContainerID: pod.Status.ContainerStatuses[0].ContainerID, JobUID: string(job.UID), PodUID: string(pod.UID),
			Origin: "application-observer", StartedAt: now, FinishedAt: now.Add(time.Millisecond), Executed: true,
			ExitCode: new(0), StdoutDigest: pv.Digest([]byte(expected.Stdout)), StdoutBytes: len(expected.Stdout), StderrDigest: pv.Digest(nil),
			Application: &pv.ApplicationObservation{Version: pv.ApplicationProtocolVersion, Completed: true,
				PlanDigest: input.Artifact.PlanDigest, Target: input.Target, Artifact: *input.Artifact, Subject: *input.Subject,
				APIBaseline: input.APIBaseline,
				Kubernetes: &pv.ApplicationAPIIdentity{APIVersion: check.Application.Kubernetes.APIVersion,
					Resource: check.Application.Kubernetes.Resource, Name: object.Name, Pointer: check.Application.Kubernetes.Pointer,
					UID: string(object.UID), ResourceVersion: object.ResourceVersion, Result: value}}}
		report = &pv.ApplicationReport{Version: pv.ApplicationProtocolVersion, TaskUID: input.TaskUID, PodUID: string(pod.UID),
			Evidence: pv.ExecutionEvidence{Observation: observation,
				Blobs: map[string][]byte{observation.StdoutDigest: []byte(expected.Stdout), observation.StderrDigest: {}}}}
	default:
		require.FailNow(f.t, "unexpected worker mode", container.Name)
	}
	if f.reportHook != nil {
		f.reportHook(container.Name, report)
	}
	if container.Name == "observe" {
		var input pv.ApplicationWorkerInput
		require.NoError(f.t, json.Unmarshal(config.BinaryData["input.json"], &input))
		digest, err := pv.ApplicationWorkerInputDigest(input)
		require.NoError(f.t, err)
		report = &pv.ApplicationStreamFrame{Version: pv.ApplicationProtocolVersion, Kind: "final",
			TaskUID: input.TaskUID, PodUID: string(pod.UID), InputDigest: digest, Report: report.(*pv.ApplicationReport)}
	}
	content, err := json.Marshal(report)
	require.NoError(f.t, err)
	f.reports.Store(pod.Namespace+"/"+pod.Name, append(content, '\n'))
}

func (f *fixture) newRun() *run {
	f.t.Helper()
	f.backend.qualified.Store(true)
	request, digest, err := f.backend.requestInput(f.request)
	require.NoError(f.t, err)
	r, err := f.backend.createRun(f.t.Context(), request, digest, false)
	require.NoError(f.t, err)
	require.NoError(f.t, f.backend.ensureTask(f.t.Context(), r, ""))
	return r
}
