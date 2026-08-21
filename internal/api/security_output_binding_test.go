package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

type outputBindingTestStore struct {
	mu        sync.Mutex
	results   map[string][]byte
	artifacts map[string][]byte
}

func newOutputBindingTestStore() *outputBindingTestStore {
	return &outputBindingTestStore{results: map[string][]byte{}, artifacts: map[string][]byte{}}
}

func (s *outputBindingTestStore) SaveResult(_ context.Context, namespace, taskName string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[namespace+"/"+taskName] = append([]byte(nil), data...)
	return nil
}
func (s *outputBindingTestStore) GetResult(_ context.Context, namespace, taskName string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.results[namespace+"/"+taskName]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}
func (s *outputBindingTestStore) DeleteResult(_ context.Context, namespace, taskName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.results, namespace+"/"+taskName)
	return nil
}
func (s *outputBindingTestStore) SaveArtifact(_ context.Context, namespace, taskName, filename, _ string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[namespace+"/"+taskName+"/"+filename] = append([]byte(nil), data...)
	return nil
}
func (s *outputBindingTestStore) GetArtifact(_ context.Context, namespace, taskName, filename string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.artifacts[namespace+"/"+taskName+"/"+filename]
	if !ok {
		return nil, "", store.ErrNotFound
	}
	return append([]byte(nil), data...), "application/octet-stream", nil
}
func (s *outputBindingTestStore) ListArtifacts(context.Context, string, string) ([]store.ArtifactMetadata, error) {
	return nil, nil
}
func (s *outputBindingTestStore) DeleteArtifacts(context.Context, string, string) error { return nil }

func (s *outputBindingTestStore) SaveBoundResult(ctx context.Context, result *store.BoundResult) error {
	return s.SaveResult(ctx, result.Namespace, result.TaskName, result.Data)
}
func (s *outputBindingTestStore) GetBoundResult(ctx context.Context, namespace, taskName, _ string, _ int64) (*store.BoundResult, error) {
	data, err := s.GetResult(ctx, namespace, taskName)
	if err != nil {
		return nil, err
	}
	return &store.BoundResult{Namespace: namespace, TaskName: taskName, Data: data}, nil
}
func (s *outputBindingTestStore) SaveBoundArtifact(ctx context.Context, artifact *store.BoundArtifact) error {
	return s.SaveArtifact(ctx, artifact.Namespace, artifact.TaskName, artifact.Filename, artifact.ContentType, artifact.Data)
}
func (s *outputBindingTestStore) GetBoundArtifact(ctx context.Context, namespace, taskName, filename, _ string, _ int64) (*store.BoundArtifact, error) {
	data, contentType, err := s.GetArtifact(ctx, namespace, taskName, filename)
	if err != nil {
		return nil, err
	}
	return &store.BoundArtifact{Namespace: namespace, TaskName: taskName, Filename: filename, ContentType: contentType, Data: data}, nil
}
func (s *outputBindingTestStore) ListBoundArtifacts(ctx context.Context, namespace, taskName, _ string, _ int64) ([]store.ArtifactMetadata, error) {
	return s.ListArtifacts(ctx, namespace, taskName)
}

func TestSecurityOutputBindingEnforcesExactCurrentWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	taskUID := types.UID("task-uid")
	jobUID := types.UID("job-uid")
	podUID := types.UID("pod-uid")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "security-review",
			Namespace: "default",
			UID:       taskUID,
			Labels:    map[string]string{labels.LabelCreatedBy: securityOutputCreatedBy},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1, JobName: "security-review-job"},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:            "security-review-job",
		Namespace:       "default",
		UID:             jobUID,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Task", Name: task.Name, UID: taskUID}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            "security-review-pod",
		Namespace:       "default",
		UID:             podUID,
		Labels:          map[string]string{labels.LabelTask: labels.SelectorValue(task.Name)},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: jobUID}},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()
	storage := newOutputBindingTestStore()
	h := NewInternalHandlers(storage, nil, nil, nil, storage, InternalHandlersConfig{
		Client:          kubeClient,
		IntegrityConfig: security.IntegrityConfig{WorkerOutputBindingMode: security.WorkerOutputBindingEnforce},
	})

	newApp := func(podName, uid string) *fiber.App {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType:  AuthTypeTokenReview,
				Username:  "system:serviceaccount:default:worker",
				Namespace: "default",
				Extra: map[string]authenticationv1.ExtraValue{
					"authentication.kubernetes.io/pod-name": {podName},
					"authentication.kubernetes.io/pod-uid":  {uid},
				},
			})
			return c.Next()
		})
		app.Post("/results/:namespace/:taskName", h.SubmitResult)
		app.Post("/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)
		return app
	}

	wrong := newApp("other-pod", "other-uid")
	resp, err := wrong.Test(httptest.NewRequest(http.MethodPost, "/results/default/security-review", bytes.NewBufferString("result")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong worker status = %d", resp.StatusCode)
	}

	correct := newApp(pod.Name, string(podUID))
	resp, err = correct.Test(httptest.NewRequest(http.MethodPost, "/results/default/security-review", bytes.NewBufferString("result")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("correct result status = %d", resp.StatusCode)
	}
	artifactReq := httptest.NewRequest(http.MethodPost, "/artifacts/default/security-review/proof.txt", bytes.NewBufferString("artifact"))
	artifactReq.Header.Set("Content-Type", "text/plain")
	resp, err = correct.Test(artifactReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("correct artifact status = %d", resp.StatusCode)
	}
}

func TestSecurityOutputBindingRejectsTerminalTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "done", Namespace: "default", UID: types.UID("uid"), Labels: map[string]string{labels.LabelCreatedBy: securityOutputCreatedBy},
	}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	storage := newOutputBindingTestStore()
	h := NewInternalHandlers(storage, nil, nil, nil, storage, InternalHandlersConfig{
		Client: kubeClient, IntegrityConfig: security.IntegrityConfig{WorkerOutputBindingMode: security.WorkerOutputBindingEnforce},
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:worker", Namespace: "default"})
		return c.Next()
	})
	app.Post("/results/:namespace/:taskName", h.SubmitResult)
	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/results/default/done", bytes.NewBufferString("late")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("terminal write status = %d", resp.StatusCode)
	}
}

type rotatingTaskReader struct {
	client.Reader
	mu       sync.Mutex
	taskGets int
}

func (r *rotatingTaskReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if task, ok := obj.(*corev1alpha1.Task); ok {
		r.mu.Lock()
		r.taskGets++
		getNumber := r.taskGets
		r.mu.Unlock()
		if err := r.Reader.Get(ctx, key, task, opts...); err != nil {
			return err
		}
		if getNumber >= 2 {
			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		}
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestSecurityOutputBindingRevalidatesBeforeCommit(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "security-review", Namespace: "default", UID: types.UID("task-uid"),
		Labels: map[string]string{labels.LabelCreatedBy: securityOutputCreatedBy},
	}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1, JobName: "job"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default", UID: types.UID("job-uid"),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Task", Name: task.Name, UID: task.UID}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "default", UID: types.UID("pod-uid"),
		Labels:          map[string]string{labels.LabelTask: labels.SelectorValue(task.Name)},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: job.UID}}}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()
	reader := &rotatingTaskReader{Reader: base}
	storage := newOutputBindingTestStore()
	h := NewInternalHandlers(storage, nil, nil, nil, storage, InternalHandlersConfig{
		Client: base, APIReader: reader,
		IntegrityConfig: security.IntegrityConfig{WorkerOutputBindingMode: security.WorkerOutputBindingEnforce},
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:worker", Namespace: "default",
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {pod.Name},
				"authentication.kubernetes.io/pod-uid":  {string(pod.UID)},
			},
		})
		return c.Next()
	})
	app.Post("/results/:namespace/:taskName", h.SubmitResult)
	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/results/default/security-review", bytes.NewBufferString("late")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("late write status = %d", resp.StatusCode)
	}
	if _, err := storage.GetResult(context.Background(), "default", task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("late write was committed: %v", err)
	}
}

func TestTaskBoundOutputAttemptUsesFrozenHarnessAttempt(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{harnessWrapperAttemptAnnotationAPI: "2"}},
		Status:     corev1alpha1.TaskStatus{Attempts: 0},
	}
	if got := taskBoundOutputAttempt(task); got != 2 {
		t.Fatalf("taskBoundOutputAttempt() = %d, want 2", got)
	}
	delete(task.Annotations, harnessWrapperAttemptAnnotationAPI)
	task.Status.Attempts = 3
	if got := taskBoundOutputAttempt(task); got != 3 {
		t.Fatalf("taskBoundOutputAttempt(status) = %d, want 3", got)
	}
	task.Annotations[harnessWrapperAttemptAnnotationAPI] = "2"
	if got := taskBoundOutputAttempt(task); got != 3 {
		t.Fatalf("taskBoundOutputAttempt(stale annotation) = %d, want 3", got)
	}
}

func TestHasActiveSecurityScanPipelineTaskIgnoresUnownedTask(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid")},
	}
	forged := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "forged", Namespace: "ns", Labels: map[string]string{
			labels.LabelSecurityTarget: "repo", labels.LabelSecurityStage: security.StageReview,
		}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, forged).Build()
	handlers := &Handlers{client: kubeClient}
	active, err := handlers.hasActiveSecurityScanPipelineTask(context.Background(), scan)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("unowned label-only task blocked RepositoryScan")
	}
}
