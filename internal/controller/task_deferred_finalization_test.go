package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/workerenv"
)

func deferredFinalizationFixture() (*corev1alpha1.Task, *batchv1.Job, *corev1.Pod, *corev1.Namespace) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cancel-output", Namespace: "default", UID: "task-uid", Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{labels.AnnotationCancelRequested: "stop"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, JobName: "output-job", JobUID: "job-uid", Attempts: 1,
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace, UID: "job-uid"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "output-pod", Namespace: task.Namespace, UID: "pod-uid", Labels: map[string]string{"job-name": job.Name}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
	return task, job, pod, namespace
}

func serveDeferredResultLogs(t *testing.T, r *TaskReconciler, logs string) *atomic.Int32 {
	t.Helper()
	reads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/namespaces/default/pods/output-pod/log" {
			http.NotFound(w, req)
			return
		}
		reads.Add(1)
		_, _ = w.Write([]byte(logs))
	}))
	t.Cleanup(server.Close)
	var err error
	r.KubeClient, err = kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	return reads
}

// Model background garbage collection, which the fake client does not perform:
// after cascading deletion there is no Pod from which a retry can read output.
func cascadeDeferredJobPod(t *testing.T, r *TaskReconciler, pod *corev1.Pod) {
	t.Helper()
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			if _, ok := object.(*batchv1.Job); ok {
				if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
			return c.Delete(ctx, object, opts...)
		},
	})
}

func TestCancellationPreservesPodOutputAcrossStorageOutageAndRestart(t *testing.T) {
	for _, sharedOutage := range []bool{false, true} {
		for _, stdout := range []bool{false, true} {
			name := "results"
			if sharedOutage {
				name = "shared-state"
			}
			if stdout {
				name += "/stdout"
			} else {
				name += "/container"
			}
			t.Run(name, func(t *testing.T) {
				task, job, pod, namespace := deferredFinalizationFixture()
				// Exceed one Secret's capacity to exercise durable chunking.
				want := bytes.Repeat([]byte("synthetic-output\n"), 80000)
				logs := string(want)
				if stdout {
					task.Spec.Type = corev1alpha1.TaskTypeAI
					task.Annotations[labels.AnnotationAgentReadOnly] = booleanTrueValue
					logs = "worker diagnostic\n" + workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString(want) + "\n"
				}
				r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
				results := r.ResultStore
				backend := results.(*sqlite.Store)
				fences := &cancellationStorageFence{FenceSource: &storageclienttest.FollowDataset{Dataset: backend}, err: store.ErrUnavailable}
				shared, err := storageclient.New(storageclient.Options{State: backend, Artifacts: backend, Embedded: backend, Fences: fences})
				require.NoError(t, err)
				if sharedOutage {
					r.ResultStore, r.ExecutionEventStore = shared, shared
					r.MessageStore, r.PlanStore = shared, shared
				} else {
					r.ResultStore = transientGetResultErrorStore{ResultStore: results, err: store.ErrUnavailable}
				}
				reads := serveDeferredResultLogs(t, r, logs)
				cascadeDeferredJobPod(t, r, pod)
				request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
				_, err = r.Reconcile(t.Context(), request)
				require.Error(t, err)
				require.True(t, errors.Is(err, store.ErrUnavailable) || errors.Is(err, store.ErrNotReady))
				require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})))
				require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
				require.EqualValues(t, 1, reads.Load())
				secrets := &corev1.SecretList{}
				require.NoError(t, r.List(t.Context(), secrets))
				require.Greater(t, len(secrets.Items), 2)
				for _, secret := range secrets.Items {
					require.True(t, metav1.IsControlledBy(&secret, task))
					require.True(t, *secret.Immutable)
					for _, data := range secret.Data {
						require.LessOrEqual(t, len(data), deferredResultChunkBytes)
					}
				}

				// A fresh reconciler has no in-memory output. Recovery first refuses
				// SaveResult, so both the intent and every output chunk must survive.
				restarted := newUnitReconciler(newTestScheme())
				restarted.Client = r.Client
				restarted.KubeClient = r.KubeClient
				restarted.ResultStore = &unacceptedResultStore{ResultStore: results, err: store.ErrOutcomeUnknown}
				_, err = restarted.Reconcile(t.Context(), request)
				require.ErrorIs(t, err, store.ErrOutcomeUnknown)
				current := &corev1alpha1.Task{}
				require.NoError(t, restarted.Get(t.Context(), request.NamespacedName, current))
				require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
				require.NoError(t, restarted.List(t.Context(), secrets))
				require.Greater(t, len(secrets.Items), 2)

				restarted.ResultStore = results
				_, err = restarted.Reconcile(t.Context(), request)
				require.NoError(t, err)
				require.NoError(t, restarted.Get(t.Context(), request.NamespacedName, current))
				require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
				require.True(t, current.Status.ResultRef.Available)
				got, err := results.GetResult(t.Context(), task.Namespace, task.Name)
				require.NoError(t, err)
				require.True(t, bytes.Equal(got, want), "the preserved result must match without exposing output on failure")
				require.EqualValues(t, 1, reads.Load(), "recovery cannot depend on the deleted Pod")
				_, err = restarted.Reconcile(t.Context(), request)
				require.NoError(t, err)
				require.NoError(t, restarted.List(t.Context(), secrets))
				require.Empty(t, secrets.Items, "terminal cleanup removes staging after result acceptance")
			})
		}
	}
}

func TestDeferredFinalizationStagingFailureRetainsJobAndOutput(t *testing.T) {
	for _, failure := range []string{"output chunk", "intent"} {
		t.Run(failure, func(t *testing.T) {
			task, job, pod, namespace := deferredFinalizationFixture()
			r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
			r.ResultStore = transientGetResultErrorStore{ResultStore: r.ResultStore, err: store.ErrUnavailable}
			serveDeferredResultLogs(t, r, "synthetic output")
			blocked := true
			injected := apierrors.NewServiceUnavailable("staging temporarily unavailable")
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if secret, ok := obj.(*corev1.Secret); ok && blocked {
						_, intent := secret.Data["intent.json"]
						if intent == (failure == "intent") {
							return injected
						}
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
			_, err := r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, injected)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{}))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
			blocked = false
			_, err = r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, store.ErrUnavailable)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})))
		})
	}
}

func TestDeferredFinalizationRejectsForeignSecretsAndTaskNameReuse(t *testing.T) {
	task, job, pod, namespace := deferredFinalizationFixture()
	r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
	serveDeferredResultLogs(t, r, "synthetic output")
	intent := deferredTaskFinalization{Phase: corev1alpha1.TaskPhaseCancelled, OutcomePhase: corev1alpha1.TaskPhaseCancelled, Message: "stop"}
	require.NoError(t, r.stageDeferredTaskFinalization(t.Context(), task, intent))
	replacement := task.DeepCopy()
	replacement.UID = "replacement-task-uid"
	pending, err := r.loadDeferredTaskFinalization(t.Context(), replacement)
	require.NoError(t, err)
	require.Nil(t, pending, "a same-name Task cannot resume the previous UID's intent")

	secret := &corev1.Secret{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: deferredFinalizationName(task)}, secret))
	// Model a same-name foreign object at the deterministic intent name.
	require.NoError(t, r.Delete(t.Context(), secret))
	secret.ResourceVersion, secret.UID = "", ""
	secret.OwnerReferences[0].UID = "foreign-task-uid"
	require.NoError(t, r.Create(t.Context(), secret))
	_, err = r.loadDeferredTaskFinalization(t.Context(), task)
	require.ErrorContains(t, err, "identity is invalid")
	foreign := secret.DeepCopy()
	require.NoError(t, r.cleanupDeferredTaskFinalization(t.Context(), task))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(foreign), &corev1.Secret{}), "cleanup must retain foreign Secrets")
}

func TestDeferredFinalizationReadFailureCannotCompleteWithoutResult(t *testing.T) {
	task, job, pod, namespace := deferredFinalizationFixture()
	r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
	results := r.ResultStore
	serveDeferredResultLogs(t, r, "synthetic output")
	r.ResultStore = transientGetResultErrorStore{ResultStore: results, err: store.ErrUnavailable}
	cascadeDeferredJobPod(t, r, pod)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
	_, err := r.Reconcile(t.Context(), request)
	require.ErrorIs(t, err, store.ErrUnavailable)
	r.ResultStore = results
	readErr := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "missing-chunk")
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if _, ok := object.(*corev1.Secret); ok && strings.HasPrefix(key.Name, deferredFinalizationName(task)+"-") {
				return readErr
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	_, err = r.Reconcile(t.Context(), request)
	require.ErrorIs(t, err, readErr)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
	require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
	require.Nil(t, current.Status.ResultRef)
}

func TestDeferredFinalizationWithoutStartedPodNeedsNoLogsAfterRecovery(t *testing.T) {
	for _, completedInit := range []bool{false, true} {
		name := "no containers started"
		if completedInit {
			name = "init completed but main never started"
		}
		t.Run(name, func(t *testing.T) {
			task, job, pod, namespace := deferredFinalizationFixture()
			pod.Status.Phase = corev1.PodPending
			pod.Spec.Containers = []corev1.Container{{Name: "worker"}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: "missing Secret"},
			}}}
			if completedInit {
				pod.Spec.InitContainers = []corev1.Container{{Name: "setup"}}
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "setup", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
				}}}
			}
			r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
			results := r.ResultStore
			r.ResultStore = transientGetResultErrorStore{ResultStore: results, err: store.ErrUnavailable}
			reads := serveDeferredResultLogs(t, r, "no output from an unstarted main container")
			cascadeDeferredJobPod(t, r, pod)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
			_, err := r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, store.ErrUnavailable)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})))
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
			r.ResultStore = results
			_, err = r.Reconcile(t.Context(), request)
			require.NoError(t, err)
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
			require.Nil(t, current.Status.ResultRef)
			require.Zero(t, reads.Load(), "an unstarted main container has no output to fetch or restore")
		})
	}
}

func TestDeferredFailureStopsJobAfterRestartDuringUsageStorageOutage(t *testing.T) {
	task, job, pod, namespace := deferredFinalizationFixture()
	task.Annotations = nil // The saved failure must work without a cancellation request.
	r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
	results := r.ResultStore
	reads := serveDeferredResultLogs(t, r, "preserved failure output")
	intent := deferredTaskFinalization{
		Phase: corev1alpha1.TaskPhaseFailed, OutcomePhase: corev1alpha1.TaskPhaseFailed,
		Message: "pod never became ready", RecordOutcome: true,
	}
	// Model a crash immediately after publication, before the Job delete.
	require.NoError(t, r.stageDeferredTaskFinalization(t.Context(), task, intent))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{}))
	cascadeDeferredJobPod(t, r, pod)
	backend := results.(*sqlite.Store)
	fences := &cancellationStorageFence{FenceSource: &storageclienttest.FollowDataset{Dataset: backend}, err: store.ErrUnavailable}
	shared, err := storageclient.New(storageclient.Options{State: backend, Artifacts: backend, Embedded: backend, Fences: fences})
	require.NoError(t, err)
	restarted := newUnitReconciler(newTestScheme())
	restarted.Client, restarted.KubeClient = r.Client, r.KubeClient
	restarted.ResultStore, restarted.ExecutionEventStore = shared, shared
	restarted.MessageStore, restarted.PlanStore = shared, shared
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
	_, err = restarted.Reconcile(t.Context(), request)
	require.Error(t, err)
	require.True(t, errors.Is(err, store.ErrUnavailable) || errors.Is(err, store.ErrNotReady))
	require.True(t, apierrors.IsNotFound(restarted.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})),
		"the saved failure decision must stop active work despite usage retention failure")
	current := &corev1alpha1.Task{}
	require.NoError(t, restarted.Get(t.Context(), request.NamespacedName, current))
	require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
	require.Nil(t, current.Status.CompletionTime)
	restarted.ResultStore, restarted.ExecutionEventStore = results, backend
	restarted.MessageStore, restarted.PlanStore = backend, backend
	_, err = restarted.Reconcile(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, restarted.Get(t.Context(), request.NamespacedName, current))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
	require.Equal(t, intent.Message, current.Status.Message)
	require.NotNil(t, current.Status.ExecutionOutcome)
	got, err := results.GetResult(t.Context(), task.Namespace, task.Name)
	require.NoError(t, err)
	require.Equal(t, "preserved failure output", string(got))
	require.EqualValues(t, 1, reads.Load(), "replay uses the saved output")
}

func TestDeferredCancellationStopsJobDuringValidationClassificationOutage(t *testing.T) {
	for _, sharedOutage := range []bool{false, true} {
		name := "classification only"
		if sharedOutage {
			name = "shared storage outage"
		}
		t.Run(name, func(t *testing.T) {
			task, job, pod, namespace := deferredFinalizationFixture()
			task.Name += "-validation" // No metadata proves whether output is safe.
			r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
			backend := r.ResultStore.(*sqlite.Store)
			r.RepositoryValidationBindings = repositoryMonitorValidationBindingErrorStore{err: store.ErrUnavailable}
			if sharedOutage {
				fences := &cancellationStorageFence{FenceSource: &storageclienttest.FollowDataset{Dataset: backend}, err: store.ErrUnavailable}
				shared, err := storageclient.New(storageclient.Options{State: backend, Artifacts: backend, Embedded: backend, Fences: fences})
				require.NoError(t, err)
				r.ResultStore, r.ExecutionEventStore = shared, shared
				r.MessageStore, r.PlanStore = shared, shared
			}
			reads := serveDeferredResultLogs(t, r, "possible validation output must not be read")
			cascadeDeferredJobPod(t, r, pod)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
			_, err := r.Reconcile(t.Context(), request)
			require.Error(t, err)
			require.True(t, errors.Is(err, errRepositoryMonitorValidationClassificationUnavailable) ||
				errors.Is(err, store.ErrUnavailable) || errors.Is(err, store.ErrNotReady))
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})),
				"uncertain output classification must not block workload cancellation")
			require.Zero(t, reads.Load())
			intent, err := r.loadDeferredTaskFinalization(t.Context(), task)
			require.NoError(t, err)
			require.NotNil(t, intent)
			require.Zero(t, intent.ResultChunks)
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
			require.Nil(t, current.Status.CompletionTime)
			// Usage storage recovery alone cannot bypass output classification.
			r.ResultStore, r.ExecutionEventStore = backend, backend
			r.MessageStore, r.PlanStore = backend, backend
			_, err = r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, errRepositoryMonitorValidationClassificationUnavailable)
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
			r.RepositoryValidationBindings = backend
			_, err = r.Reconcile(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
			require.Nil(t, current.Status.ResultRef)
			require.Zero(t, reads.Load())
		})
	}
}

// Output above the result limit can never be stored, so it is not staged:
// cancellation still stops the work during a storage outage, and recovery ends
// the Task with the reason and releases the staging instead of retrying.
func TestDeferredStdoutResultAboveTheLimitFinalizesWithoutIt(t *testing.T) {
	task, job, pod, namespace := deferredFinalizationFixture()
	task.Spec.Type = corev1alpha1.TaskTypeAI
	task.Annotations[labels.AnnotationAgentReadOnly] = booleanTrueValue
	oversized := bytes.Repeat([]byte("r"), store.MaxResultBytes+1)
	logs := "worker diagnostic\n" + workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString(oversized) + "\n"
	r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
	results := r.ResultStore
	r.ResultStore = transientGetResultErrorStore{ResultStore: results, err: store.ErrUnavailable}
	reads := serveDeferredResultLogs(t, r, logs)
	cascadeDeferredJobPod(t, r, pod)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
	_, err := r.Reconcile(t.Context(), request)
	require.ErrorIs(t, err, store.ErrUnavailable)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})),
		"output above the limit must not keep cancelled work running")
	intent, err := r.loadDeferredTaskFinalization(t.Context(), task)
	require.NoError(t, err)
	require.NotNil(t, intent)
	require.True(t, intent.ResultTooLarge)
	require.Zero(t, intent.ResultChunks)
	secrets := &corev1.SecretList{}
	require.NoError(t, r.List(t.Context(), secrets))
	require.Len(t, secrets.Items, 1, "output that can never be stored is not staged")

	r.ResultStore = results
	_, err = r.Reconcile(t.Context(), request)
	require.NoError(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
	require.Equal(t, "stop; task result exceeds the 10485760 byte limit and was not stored", current.Status.Message)
	require.Nil(t, current.Status.ResultRef)
	_, err = results.GetResult(t.Context(), task.Namespace, task.Name)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.EqualValues(t, 1, reads.Load())
	_, err = r.Reconcile(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, r.List(t.Context(), secrets))
	require.Empty(t, secrets.Items, "terminal cleanup releases the staging")
}

// An intent that records refused output stages none of it.
func TestDeferredFinalizationRejectsARefusedResultWithStagedOutput(t *testing.T) {
	task, job, pod, namespace := deferredFinalizationFixture()
	r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
	data, err := json.Marshal(deferredTaskFinalization{
		Phase: corev1alpha1.TaskPhaseCancelled, OutcomePhase: corev1alpha1.TaskPhaseCancelled, Message: "stop",
		ResultTooLarge: true, ResultChunks: 1, ResultBytes: 1, ResultDigest: strings.Repeat("0", 64),
	})
	require.NoError(t, err)
	require.NoError(t, r.createDeferredFinalizationSecret(t.Context(), task, deferredFinalizationName(task), "intent.json", data))
	_, err = r.loadDeferredTaskFinalization(t.Context(), task)
	require.ErrorContains(t, err, "intent is invalid")
}

// TestDeferredFinalizationStopsTheJobBeforeItsChildren covers a stop already
// decided whose Job delete failed: once storage recovers, the reconcile that
// records it deletes the Job before it cancels the Task's children.
func TestDeferredFinalizationStopsTheJobBeforeItsChildren(t *testing.T) {
	task, job, pod, namespace := deferredFinalizationFixture()
	controller := true
	owner := []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: task.Name, UID: task.UID, Controller: &controller,
	}}
	job.OwnerReferences = owner
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "delegated", Namespace: task.Namespace, OwnerReferences: owner,
			Labels: map[string]string{labels.LabelParentTask: task.Name},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(newTestScheme(), task, job, pod, namespace, child)
	serveDeferredResultLogs(t, r, "output")
	// The stop was decided and saved, but its Job delete never happened.
	require.NoError(t, r.stageDeferredTaskFinalization(t.Context(), task, deferredTaskFinalization{
		Phase: corev1alpha1.TaskPhaseCancelled, OutcomePhase: corev1alpha1.TaskPhaseCancelled, Message: "stop",
	}))
	watched, ok := r.Client.(client.WithWatch)
	require.True(t, ok)
	var order []string
	r.Client = interceptor.NewClient(watched, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob {
				order = append(order, "job deleted")
			}
			return c.Delete(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == child.Name {
				order = append(order, "child cancelled")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	require.NoError(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
	require.Equal(t, []string{"job deleted", "child cancelled"}, order)
}
