/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// setupTestHandlersWithArtifactStore creates Handlers backed by an in-memory
// SQLite store that implements ArtifactStore, plus any runtime objects for the
// fake k8s client.
func setupTestHandlersWithArtifactStore(objs ...runtime.Object) (*Handlers, *fiber.App, *sqlite.Store) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: fakeClient, SessionStore: ss, ResultStore: ss, ArtifactStore: ss})

	app := fiber.New()
	return handlers, app, ss
}

func testHarnessWrapperPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "harness-wrapper-pod", Namespace: "orka-system", UID: types.UID("harness-wrapper-pod-uid"),
			Labels: map[string]string{"app.kubernetes.io/component": harnessWrapperComponentLabel},
		},
		Spec:   corev1.PodSpec{ServiceAccountName: "agent-harness-wrapper"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func testHarnessWrapperUser(serviceAccount, podUID string) *UserInfo {
	const namespace = "orka-system"
	return &UserInfo{
		AuthType: AuthTypeTokenReview, Namespace: namespace,
		Username: "system:serviceaccount:" + namespace + ":" + serviceAccount,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {"harness-wrapper-pod"},
			"authentication.kubernetes.io/pod-uid":  {podUID},
		},
	}
}

func freezeBuiltInHarnessArtifactTask(task *corev1alpha1.Task, started bool, plannedAt time.Time) {
	if task.UID == "" {
		task.UID = types.UID(task.Name + "-uid")
	}
	if task.Status.Phase == "" {
		if started {
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			task.Status.Attempts = max(task.Status.Attempts, 1)
		} else {
			task.Status.Phase = corev1alpha1.TaskPhasePending
		}
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	const runtimeName = "codex"
	metadata, _ := json.Marshal(map[string]string{
		"runtime": runtimeName, "wrapper": "cli", "contractVersion": "orka.harness.v1",
	})
	task.Annotations[harnessWrapperStartedAnnotation] = fmt.Sprintf("%t", started)
	task.Annotations[harnessWrapperTurnIDAnnotation] = harnessWrapperArtifactTurnID(task, harnessWrapperArtifactAttempt(task))
	task.Annotations[harnessWrapperRuntimeAnnotation] = harnessWrapperArtifactRuntimeSessionID(task, runtimeName)
	task.Annotations[harnessWrapperCorrelationAnnotation] = string(task.UID)
	task.Annotations[harnessWrapperPlannedAtAnnotation] = plannedAt.UTC().Format(time.RFC3339Nano)
	task.Annotations[harnessWrapperMetadataAnnotation] = string(metadata)
	task.Annotations[harnessWrapperContractAnnotation] = "orka.harness.v1"
}

func setupHarnessWrapperArtifactUploadTest(
	t *testing.T,
	task *corev1alpha1.Task,
	pod *corev1.Pod,
	user *UserInfo,
) *fiber.App {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, pod).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient, APIReader: fakeClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)
	return app
}

// ---------- Internal: UploadArtifact ----------

func TestUploadArtifact(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	t.Run("success", func(t *testing.T) {
		body := []byte("hello artifact")
		req := httptest.NewRequest(http.MethodPost,
			"/internal/v1/artifacts/default/my-task/output.txt",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		// Verify data was stored
		data, ct, err := ss.GetArtifact(context.Background(), "default", "my-task", "output.txt")
		require.NoError(t, err)
		require.Equal(t, body, data)
		require.Equal(t, "text/plain", ct)
	})

	t.Run("default content type", func(t *testing.T) {
		body := []byte("binary data")
		req := httptest.NewRequest(http.MethodPost,
			"/internal/v1/artifacts/default/my-task/data.bin",
			bytes.NewReader(body))
		// No Content-Type header set

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		_, ct, err := ss.GetArtifact(context.Background(), "default", "my-task", "data.bin")
		require.NoError(t, err)
		require.Equal(t, "application/octet-stream", ct)
	})
}

func TestUploadArtifactAllowsHarnessWrapperControlPlaneUpload(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrapped-task",
			Namespace: "default",
			Annotations: map[string]string{
				harnessWrapperStartedAnnotation: "true",
				harnessWrapperTurnIDAnnotation:  "turn-1",
				harnessWrapperRuntimeAnnotation: "runtime-1",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	freezeBuiltInHarnessArtifactTask(task, true, time.Now())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, testHarnessWrapperPod()).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient})
	userInfo := testHarnessWrapperUser("agent-harness-wrapper", "harness-wrapper-pod-uid")
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	body := []byte("# threat model\n")
	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/wrapped-task/security-threat-model.md",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/markdown")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	data, contentType, err := ss.GetArtifact(context.Background(), "default", "wrapped-task", "security-threat-model.md")
	require.NoError(t, err)
	require.Equal(t, body, data)
	require.Equal(t, "text/markdown", contentType)
}

func TestUploadArtifactAllowsPlannedHarnessWrapperControlPlaneUpload(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "planned-task",
			Namespace: "default",
			Annotations: map[string]string{
				harnessWrapperTurnIDAnnotation:    "turn-1",
				harnessWrapperRuntimeAnnotation:   "runtime-1",
				harnessWrapperPlannedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	freezeBuiltInHarnessArtifactTask(task, false, time.Now())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, testHarnessWrapperPod()).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, testHarnessWrapperUser("agent-harness-wrapper", "harness-wrapper-pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/planned-task/output.txt",
		bytes.NewReader([]byte("artifact")))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestUploadArtifactAllowsPendingHarnessWrapperRetryUpload(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")

	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "retry-task", Namespace: "default"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhasePending, Attempts: 2,
				},
			}
			freezeBuiltInHarnessArtifactTask(task, started, time.Now())
			pod := testHarnessWrapperPod()
			user := testHarnessWrapperUser("agent-harness-wrapper", string(pod.UID))
			app := setupHarnessWrapperArtifactUploadTest(t, task, pod, user)
			req := httptest.NewRequest(
				http.MethodPost,
				"/internal/v1/artifacts/default/retry-task/output.txt",
				bytes.NewReader([]byte("artifact")),
			)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
		})
	}
}

func TestUploadArtifactRejectsCompletedHarnessWrapperUpload(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "completed-wrapper-task",
			Namespace: "default",
			Annotations: map[string]string{
				harnessWrapperStartedAnnotation: "true",
				harnessWrapperTurnIDAnnotation:  "turn-1",
				harnessWrapperRuntimeAnnotation: "runtime-1",
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	freezeBuiltInHarnessArtifactTask(task, true, time.Now())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, testHarnessWrapperPod()).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, testHarnessWrapperUser("agent-harness-wrapper", "harness-wrapper-pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/completed-wrapper-task/output.txt",
		bytes.NewReader([]byte("artifact")))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestUploadArtifactRejectsCrossNamespaceNonWrapperTask(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "job-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status:     corev1alpha1.TaskStatus{JobName: "job-task-worker"},
	}
	freezeBuiltInHarnessArtifactTask(task, true, time.Now())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, testHarnessWrapperPod()).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, testHarnessWrapperUser("agent-harness-wrapper", "harness-wrapper-pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/job-task/output.txt",
		bytes.NewReader([]byte("artifact")))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestUploadArtifactRejectsWrongControlPlaneServiceAccount(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrapped-task",
			Namespace: "default",
			Annotations: map[string]string{
				harnessWrapperStartedAnnotation: "true",
				harnessWrapperTurnIDAnnotation:  "turn-1",
				harnessWrapperRuntimeAnnotation: "runtime-1",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	freezeBuiltInHarnessArtifactTask(task, true, time.Now())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, testHarnessWrapperPod()).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, testHarnessWrapperUser("default", "harness-wrapper-pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/wrapped-task/output.txt",
		bytes.NewReader([]byte("artifact")))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestUploadArtifactRejectsFuturePlannedHarnessWrapperUpload(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "future-planned-task",
			Namespace: "default",
			Annotations: map[string]string{
				harnessWrapperTurnIDAnnotation:    "turn-1",
				harnessWrapperRuntimeAnnotation:   "runtime-1",
				harnessWrapperPlannedAtAnnotation: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	freezeBuiltInHarnessArtifactTask(task, false, time.Now().Add(time.Hour))
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, testHarnessWrapperPod()).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{Client: fakeClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, testHarnessWrapperUser("agent-harness-wrapper", "harness-wrapper-pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)
	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/future-planned-task/output.txt",
		bytes.NewReader([]byte("artifact")))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestUploadArtifactRejectsUnboundHarnessWrapperTurn(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")

	tests := []struct {
		name   string
		mutate func(*corev1alpha1.Task, *corev1.Pod, *UserInfo)
	}{
		{
			name: "recreated Task UID",
			mutate: func(task *corev1alpha1.Task, _ *corev1.Pod, _ *UserInfo) {
				task.UID = types.UID("recreated-task-uid")
			},
		},
		{
			name: "runtime session mismatch",
			mutate: func(task *corev1alpha1.Task, _ *corev1.Pod, _ *UserInfo) {
				task.Annotations[harnessWrapperRuntimeAnnotation] = "attacker-controlled"
			},
		},
		{
			name: "external frozen runtime",
			mutate: func(task *corev1alpha1.Task, _ *corev1.Pod, _ *UserInfo) {
				task.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{RuntimeRefName: "external-runtime"}
				task.Annotations[harnessWrapperRuntimeRefAnnotation] = "external-runtime"
			},
		},
		{
			name: "recreated wrapper Pod UID",
			mutate: func(_ *corev1alpha1.Task, _ *corev1.Pod, user *UserInfo) {
				user.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"stale-pod-uid"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "wrapped-task", Namespace: "default"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
			}
			freezeBuiltInHarnessArtifactTask(task, true, time.Now())
			pod := testHarnessWrapperPod()
			user := testHarnessWrapperUser("agent-harness-wrapper", string(pod.UID))
			tt.mutate(task, pod, user)
			app := setupHarnessWrapperArtifactUploadTest(t, task, pod, user)
			req := httptest.NewRequest(
				http.MethodPost,
				"/internal/v1/artifacts/default/wrapped-task/output.txt",
				bytes.NewReader([]byte("artifact")),
			)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
		})
	}
}

func TestUploadArtifactTooLarge(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()
	// Use a custom app with a large enough body limit so the handler's own
	// size check is exercised rather than Fiber's built-in limit.
	app := fiber.New(fiber.Config{BodyLimit: 20 << 20})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	body := make([]byte, 10<<20+1) // 10MB + 1 byte
	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/my-task/big.bin",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestUploadArtifactEmptyBody(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/my-task/empty.txt",
		bytes.NewReader([]byte{}))
	req.Header.Set("Content-Type", "text/plain")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadArtifactMissingParams(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	tests := []struct {
		name string
		path string
	}{
		{"missing filename", "/internal/v1/artifacts/:namespace/:taskName"},
		{"missing taskName", "/internal/v1/artifacts/:namespace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, &UserInfo{
					Username: "system:serviceaccount:default:worker",
				})
				return c.Next()
			})
			app.Post(tc.path, h.UploadArtifact)

			url := strings.ReplaceAll(tc.path, ":namespace", "default")
			url = strings.ReplaceAll(url, ":taskName", "my-task")
			req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("data")))
			req.Header.Set("Content-Type", "text/plain")

			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestUploadArtifactStoreNotEnabled(t *testing.T) {
	baseHandlers, app, ss := setupTestInternalHandlers()
	h := NewInternalHandlers(ss, ss, ss, ss, nil, InternalHandlersConfig{
		Client: baseHandlers.k8sClient, APIReader: baseHandlers.apiReader,
	}) // nil artifact store

	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/my-task/file.txt",
		bytes.NewReader([]byte("data")))
	req.Header.Set("Content-Type", "text/plain")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

// ---------- Public: ListTaskArtifacts ----------

func TestListTaskArtifacts(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	h, app, ss := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts", h.ListTaskArtifacts)

	// Pre-populate artifacts
	ctx := context.Background()
	require.NoError(t, ss.SaveArtifact(ctx, "default", "my-task", "output.txt", "text/plain", []byte("hello")))
	require.NoError(t, ss.SaveArtifact(ctx, "default", "my-task", "report.json", "application/json", []byte(`{"ok":true}`)))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Artifacts []store.ArtifactMetadata `json:"artifacts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Len(t, result.Artifacts, 2)

	names := map[string]bool{}
	for _, a := range result.Artifacts {
		names[a.Filename] = true
	}
	require.True(t, names["output.txt"])
	require.True(t, names["report.json"])
}

func TestListTaskArtifactsEmpty(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-task", Namespace: "default"},
	}
	h, app, _ := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts", h.ListTaskArtifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/empty-task/artifacts", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Artifacts []store.ArtifactMetadata `json:"artifacts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Empty(t, result.Artifacts)
}

func TestListTaskArtifactsTaskNotFound(t *testing.T) {
	h, app, _ := setupTestHandlersWithArtifactStore() // no task objects
	app.Get("/api/v1/tasks/:id/artifacts", h.ListTaskArtifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nonexistent/artifacts", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---------- Public: DownloadTaskArtifact ----------

func TestDownloadTaskArtifact(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	h, app, ss := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	ctx := context.Background()
	data := []byte("artifact content here")
	require.NoError(t, ss.SaveArtifact(ctx, "default", "my-task", "output.txt", "text/plain", data))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts/output.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/plain", resp.Header.Get("Content-Type"))
	require.Contains(t, resp.Header.Get("Content-Disposition"), `filename="output.txt"`)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, data, body)
}

func TestDownloadTaskArtifactNotFound(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	h, app, _ := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts/nonexistent.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadTaskArtifactTaskNotFound(t *testing.T) {
	h, app, _ := setupTestHandlersWithArtifactStore() // no task objects
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nonexistent/artifacts/file.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadTaskArtifactStoreNotConfigured(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()

	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	h := NewHandlers(HandlersConfig{Client: fakeClient, SessionStore: ss, ResultStore: ss}) // nil artifact store

	app := fiber.New()
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts/file.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHarnessWrapperArtifactIdentityFencesAttemptWithStableRuntimeSession(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-task", Namespace: "default", UID: types.UID("retry-task-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1},
	}
	const runtimeName = "codex"
	firstTurn := harnessWrapperArtifactTurnID(task, 1)
	firstSession := harnessWrapperArtifactRuntimeSessionID(task, runtimeName)
	task.Status.Attempts = 2
	secondTurn := harnessWrapperArtifactTurnID(task, 2)
	secondSession := harnessWrapperArtifactRuntimeSessionID(task, runtimeName)
	require.NotEqual(t, firstTurn, secondTurn)
	require.Equal(t, firstSession, secondSession)
}
