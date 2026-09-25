package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type harnessArtifactFixture struct {
	handlers      *InternalHandlers
	kube          client.Client
	data          *sqlite.Store
	app           *fiber.App
	task          *corev1alpha1.Task
	pod           *corev1.Pod
	secret        *corev1.Secret
	attempt       store.HarnessV1Attempt
	fence         store.ControllerEpochFence
	upload        harness.ArtifactUpload
	authorization artifactcap.Authorization
}

func newHarnessArtifactFixture(t *testing.T) *harnessArtifactFixture {
	t.Helper()
	ctx := context.Background()
	bearer := strings.Repeat("wrapper-fixture-", 3)
	bindingDigest := artifactcap.DigestBytes([]byte("binding"))
	snapshotDigest := artifactcap.DigestBytes([]byte("snapshot"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "task-a", UID: "task-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				SchemaVersion: 1, ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
				Backend: corev1alpha1.AgentExecutionBackendHarnessWrapper, BindingDigest: bindingDigest,
				Task:     corev1alpha1.AgentExecutionBindingTaskRef{UID: "task-uid", NamespaceUID: "namespace-uid"},
				Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: snapshotDigest},
			},
		},
	}
	selector := map[string]string{"app": "wrapper"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "wrapper", UID: "deployment-uid"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector}},
		},
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "wrapper-rs", UID: "rs-uid",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "wrapper", UID: "service-uid"},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "wrapper-pod", UID: "wrapper-pod-uid",
			Labels:          selector,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
		Spec: corev1.PodSpec{ServiceAccountName: "wrapper"}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "wrapper-auth", UID: "auth-uid", ResourceVersion: "1"},
		Data:       map[string][]byte{"token": []byte(bearer)},
	}
	kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(task, pod, secret, deployment, replicaSet, service).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	data := sqlite.NewStore(db, ":memory:")
	epoch, err := data.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
		NewEpoch: 1, HolderID: "controller", RequestDigest: artifactcap.DigestBytes([]byte("epoch")), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	f := &harnessArtifactFixture{
		kube: kube, data: data, task: task, pod: pod, secret: secret,
		fence: store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID},
		attempt: store.HarnessV1Attempt{
			Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: 1,
			BindingDigest: bindingDigest, SnapshotDigest: snapshotDigest, RequestDigest: artifactcap.DigestBytes([]byte("request")),
			TurnID: "turn-1", Backend: string(corev1alpha1.AgentExecutionBackendHarnessWrapper),
			BackendEndpoint:     "https://wrapper.control.svc:8080",
			AuthSecretNamespace: secret.Namespace, AuthSecretName: secret.Name, AuthSecretKey: "token",
			AuthSecretUID: string(secret.UID), AuthSecretResourceVersion: secret.ResourceVersion,
			State: store.HarnessV1AttemptPrepared, RetryClass: store.HarnessV1RetryClassNone,
		},
		upload: harness.ArtifactUpload{
			Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), TurnID: "turn-1", BindingDigest: bindingDigest,
			Filename: "output.txt", ContentType: "text/plain", Data: []byte("artifact"),
		},
	}
	require.NoError(t, data.CreateHarnessV1Attempt(ctx, &f.attempt, f.fence))
	f.advanceAttempt(t, store.HarnessV1AttemptSubmitting)
	f.authorization, err = harness.SignArtifactUpload(bearer, f.upload, time.Now().UTC())
	require.NoError(t, err)
	h := NewInternalHandlers(data, data, data, data, data, InternalHandlersConfig{Client: kube, APIReader: kube})
	f.handlers = h
	user := internalCallerAuthWorkerUser(pod.Name, string(pod.UID))
	user.Namespace, user.Username = pod.Namespace, "system:serviceaccount:control:wrapper"
	f.app = newTaskScopedInternalApp(h, user)
	return f
}

func (f *harnessArtifactFixture) advanceAttempt(t *testing.T, state store.HarnessV1AttemptState) {
	t.Helper()
	key := store.HarnessV1AttemptKey{Namespace: f.attempt.Namespace, TaskUID: f.attempt.TaskUID, Attempt: f.attempt.Attempt}
	current, err := f.data.GetHarnessV1Attempt(context.Background(), key)
	require.NoError(t, err)
	transition := store.HarnessV1AttemptTransition{
		Key: key, ExpectedVersion: current.Version, ExpectedState: current.State, TargetState: state, Fence: f.fence,
		OperationID: "transition-" + string(state), OperationDigest: artifactcap.DigestBytes([]byte(state)),
	}
	if store.IsTerminalHarnessV1AttemptState(state) {
		digest := artifactcap.DigestBytes([]byte("terminal"))
		transition.Updates.TerminalReceiptDigest = &digest
	}
	_, err = f.data.TransitionHarnessV1Attempt(context.Background(), transition)
	require.NoError(t, err)
}

func TestHarnessArtifactUploadRequiresCurrentTurnCapability(t *testing.T) {
	cases := []struct {
		name                string
		change              func(*testing.T, *harnessArtifactFixture)
		duringAuthorization func(*testing.T, *harnessArtifactFixture)
		allowed             bool
	}{
		{name: "active Deployment Pod with capability", allowed: true},
		{name: "escaped filename", allowed: true, change: func(t *testing.T, f *harnessArtifactFixture) {
			f.upload.Filename = url.PathEscape("report + résumé %.txt")
			var err error
			f.authorization, err = harness.SignArtifactUpload(string(f.secret.Data["token"]), f.upload, time.Now().UTC())
			require.NoError(t, err)
		}},
		{name: "ServiceAccount token alone", change: func(_ *testing.T, f *harnessArtifactFixture) { f.authorization = artifactcap.Authorization{} }},
		{name: "Job Pod with wrapper credentials and labels", change: func(t *testing.T, f *harnessArtifactFixture) {
			f.pod.OwnerReferences[0].APIVersion = "batch/v1"
			f.pod.OwnerReferences[0].Kind = "Job"
			require.NoError(t, f.kube.Update(t.Context(), f.pod))
		}},
		{name: "unrelated Deployment", change: func(t *testing.T, f *harnessArtifactFixture) {
			service := &corev1.Service{}
			require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "control", Name: "wrapper"}, service))
			service.Spec.Selector = map[string]string{"app": "different-wrapper"}
			require.NoError(t, f.kube.Update(t.Context(), service))
		}},
		{name: "replaced ReplicaSet", change: func(t *testing.T, f *harnessArtifactFixture) {
			f.pod.OwnerReferences[0].UID = "other-rs-uid"
			require.NoError(t, f.kube.Update(t.Context(), f.pod))
		}},
		{name: "replaced Deployment", change: func(t *testing.T, f *harnessArtifactFixture) {
			rs := &appsv1.ReplicaSet{}
			require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "control", Name: "wrapper-rs"}, rs))
			rs.OwnerReferences[0].UID = "other-deployment-uid"
			require.NoError(t, f.kube.Update(t.Context(), rs))
		}},
		{name: "different filename", change: func(_ *testing.T, f *harnessArtifactFixture) { f.upload.Filename = "other.txt" }},
		{name: "different content", change: func(_ *testing.T, f *harnessArtifactFixture) { f.upload.Data = []byte("forged") }},
		{name: "recreated Task", change: func(t *testing.T, f *harnessArtifactFixture) {
			require.NoError(t, f.kube.Delete(context.Background(), f.task))
			replacement := f.task.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "new-task-uid", ""
			replacement.Status.AgentExecutionBinding.Task.UID = replacement.UID
			require.NoError(t, f.kube.Create(context.Background(), replacement))
		}},
		{name: "completed Task", change: func(t *testing.T, f *harnessArtifactFixture) {
			current := &corev1alpha1.Task{}
			require.NoError(t, f.kube.Get(context.Background(), client.ObjectKeyFromObject(f.task), current))
			current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			require.NoError(t, f.kube.Update(context.Background(), current))
		}},
		{name: "Task cancelled during Kubernetes authorization", duringAuthorization: func(t *testing.T, f *harnessArtifactFixture) {
			current := &corev1alpha1.Task{}
			require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
			current.Status.Phase = corev1alpha1.TaskPhaseCancelled
			require.NoError(t, f.kube.Update(t.Context(), current))
		}},
		{name: "Task deleting during Kubernetes authorization", duringAuthorization: func(t *testing.T, f *harnessArtifactFixture) {
			current := &corev1alpha1.Task{}
			require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
			current.Finalizers = []string{"test/retain"}
			require.NoError(t, f.kube.Update(t.Context(), current))
			require.NoError(t, f.kube.Delete(t.Context(), current))
		}},
		{name: "Task replaced during Kubernetes authorization", duringAuthorization: func(t *testing.T, f *harnessArtifactFixture) {
			require.NoError(t, f.kube.Delete(t.Context(), f.task))
			replacement := f.task.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "replacement-task-uid", ""
			replacement.Status.AgentExecutionBinding.Task.UID = replacement.UID
			require.NoError(t, f.kube.Create(t.Context(), replacement))
		}},
		{name: "terminal attempt before Task projection", change: func(t *testing.T, f *harnessArtifactFixture) {
			f.advanceAttempt(t, store.HarnessV1AttemptAccepted)
			f.advanceAttempt(t, store.HarnessV1AttemptSucceeded)
		}},
		{name: "terminal attempt during Kubernetes authorization", change: func(t *testing.T, f *harnessArtifactFixture) {
			f.advanceAttempt(t, store.HarnessV1AttemptAccepted)
			f.handlers.apiReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1.Secret); ok {
						f.advanceAttempt(t, store.HarnessV1AttemptSucceeded)
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
		}},
		{name: "older turn", change: func(t *testing.T, f *harnessArtifactFixture) {
			next := f.attempt
			next.Attempt, next.TurnID = 2, "turn-2"
			require.NoError(t, f.data.CreateHarnessV1Attempt(context.Background(), &next, f.fence))
			f.attempt = next
			f.advanceAttempt(t, store.HarnessV1AttemptSubmitting)
		}},
		{name: "rotated auth Secret", change: func(t *testing.T, f *harnessArtifactFixture) {
			current := &corev1.Secret{}
			require.NoError(t, f.kube.Get(context.Background(), client.ObjectKeyFromObject(f.secret), current))
			current.Labels = map[string]string{"changed": "true"}
			require.NoError(t, f.kube.Update(context.Background(), current))
		}},
		{name: "terminated wrapper Pod", change: func(t *testing.T, f *harnessArtifactFixture) {
			current := &corev1.Pod{}
			require.NoError(t, f.kube.Get(context.Background(), client.ObjectKeyFromObject(f.pod), current))
			current.Status.Phase = corev1.PodSucceeded
			require.NoError(t, f.kube.Status().Update(context.Background(), current))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHarnessArtifactFixture(t)
			if tc.change != nil {
				tc.change(t, f)
			}
			if tc.duringAuthorization != nil {
				f.handlers.apiReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if _, ok := object.(*corev1.Secret); ok {
							tc.duringAuthorization(t, f)
						}
						return c.Get(ctx, key, object, opts...)
					},
				})
			}
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/artifacts/tenant/task-a/"+f.upload.Filename, bytes.NewReader(f.upload.Data))
			request.Header.Set("Content-Type", f.upload.ContentType)
			request.Header.Set(artifactcap.CapabilityHeader, f.authorization.Capability)
			request.Header.Set(artifactcap.RequestDigestHeader, f.authorization.RequestDigest)
			response, err := f.app.Test(request)
			require.NoError(t, err)
			t.Cleanup(func() { _ = response.Body.Close() })
			if tc.allowed {
				require.Equal(t, http.StatusCreated, response.StatusCode)
				stored, _, err := f.data.GetArtifact(context.Background(), "tenant", "task-a", f.upload.Filename)
				require.NoError(t, err)
				require.Equal(t, f.upload.Data, stored)
			} else {
				require.Equal(t, http.StatusForbidden, response.StatusCode)
				_, _, err := f.data.GetArtifact(context.Background(), "tenant", "task-a", f.upload.Filename)
				require.ErrorIs(t, err, store.ErrNotFound)
			}
		})
	}
}
