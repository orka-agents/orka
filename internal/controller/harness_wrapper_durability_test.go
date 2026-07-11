package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/workers/harness/cliwrapper"
)

const harnessDurabilityNotStarted = "false"

func TestHandleDeletionRetainsFinalizerWhileHarnessCancellationFails(t *testing.T) {
	var cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath()); err == nil && resource == harness.TurnResourceCancel {
			cancelCalls.Add(1)
			harness.WriteError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		harness.WriteError(w, http.StatusNotFound, "not found")
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
	task.Finalizers = []string{labels.TaskFinalizer}
	r := newUnitReconciler(newTestScheme(), task)

	result, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("handleDeletion() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want cancellation retry", result.RequeueAfter)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get Task: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updated, labels.TaskFinalizer) {
		t.Fatal("Task finalizer removed before harness cancellation was acknowledged")
	}
	if got := cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel calls = %d, want 1", got)
	}
}

func TestHandleCompletedRetriesHarnessCancellation503ThenCleansUp(t *testing.T) {
	var cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil || resource != harness.TurnResourceCancel {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		call := cancelCalls.Add(1)
		if call == 1 {
			harness.WriteError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		task := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: harness.RuntimeSessionID(task.Annotations[harnessWrapperRuntimeAnnotation]),
			TurnID:           turnID,
			CorrelationID:    task.Annotations[harnessWrapperCorrelationIDAnno],
		})
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
	task.Status.JobName = "active-harness-job"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace},
		Status:     batchv1.JobStatus{Active: 1},
	}
	r := newUnitReconciler(newTestScheme(), task, job)

	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("first handleCompleted() error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > harnessWrapperCancelMaxBackoff {
		t.Fatalf("first RequeueAfter = %v, want bounded cancellation retry", result.RequeueAfter)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("Job removed before cancellation acknowledgement: %v", err)
	}

	current := forceHarnessCancellationDue(t, r, task)
	if _, err := r.handleCompleted(context.Background(), current); err != nil {
		t.Fatalf("second handleCompleted() error = %v", err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job lookup after cancellation acknowledgement = %v, want not found", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get Task: %v", err)
	}
	state, ok := harnessWrapperCancellation(&updated)
	if !ok || state.Status != string(harnessWrapperCancelAcknowledged) {
		t.Fatalf("cancellation state = %#v, ok=%v, want acknowledged", state, ok)
	}
	if got := cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls = %d, want 2", got)
	}
}

func TestHandleRunningTimeoutWaitsForHarnessCancellationAcknowledgement(t *testing.T) {
	var cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil || resource != harness.TurnResourceCancel {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if cancelCalls.Add(1) == 1 {
			harness.WriteError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		fixture := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: harness.RuntimeSessionID(fixture.Annotations[harnessWrapperRuntimeAnnotation]),
			TurnID:           turnID,
			CorrelationID:    fixture.Annotations[harnessWrapperCorrelationIDAnno],
		})
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
	timeout := metav1.Duration{Duration: time.Millisecond}
	started := metav1.NewTime(time.Now().Add(-time.Second))
	task.Spec.Timeout = &timeout
	task.Status.StartTime = &started
	r := newUnitReconciler(newTestScheme(), task)

	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("first handleRunning() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("first RequeueAfter = %v, want cancellation retry", result.RequeueAfter)
	}
	var current corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &current); err != nil {
		t.Fatalf("Get Task after first timeout reconcile: %v", err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase before cancellation acknowledgement = %s, want Running", current.Status.Phase)
	}

	current = *forceHarnessCancellationDue(t, r, &current)
	if _, err := r.handleRunning(context.Background(), &current); err != nil {
		t.Fatalf("second handleRunning() error = %v", err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &current); err != nil {
		t.Fatalf("Get Task after cancellation acknowledgement: %v", err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.Message != "task timed out" {
		t.Fatalf("terminal status = (%s, %q), want (Failed, task timed out)", current.Status.Phase, current.Status.Message)
	}
	if got := cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls = %d, want 2", got)
	}
}

func TestHarnessCancellationTreats404And410AsDefinitiveMissing(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var cancelCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath()); err == nil && resource == harness.TurnResourceCancel {
					cancelCalls.Add(1)
					harness.WriteError(w, status, "turn missing")
					return
				}
				harness.WriteError(w, http.StatusNotFound, "not found")
			}))
			defer server.Close()
			t.Setenv(harnessWrapperEndpointEnv, server.URL)

			task := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
			r := newUnitReconciler(newTestScheme(), task)
			done, retryAfter, err := r.ensureHarnessWrapperTurnCancelled(context.Background(), task, "test missing turn")
			if err != nil {
				t.Fatalf("ensureHarnessWrapperTurnCancelled() error = %v", err)
			}
			if !done || retryAfter != 0 {
				t.Fatalf("result = (done=%v, retryAfter=%v), want definitive completion", done, retryAfter)
			}
			var updated corev1alpha1.Task
			if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
				t.Fatalf("Get Task: %v", err)
			}
			state, ok := harnessWrapperCancellation(&updated)
			if !ok || state.Status != string(harnessWrapperCancelMissing) {
				t.Fatalf("cancellation state = %#v, ok=%v, want missing", state, ok)
			}
			if got := cancelCalls.Load(); got != 1 {
				t.Fatalf("cancel calls = %d, want 1", got)
			}
		})
	}
}

func TestHarnessCancellationNetworkFailureRemainsPending(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL
	server.Close()
	t.Setenv(harnessWrapperEndpointEnv, endpoint)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
	r := newUnitReconciler(newTestScheme(), task)
	done, retryAfter, err := r.ensureHarnessWrapperTurnCancelled(context.Background(), task, "network failure")
	if err != nil {
		t.Fatalf("ensureHarnessWrapperTurnCancelled() error = %v", err)
	}
	if done || retryAfter <= 0 || retryAfter > harnessWrapperCancelMaxBackoff {
		t.Fatalf("result = (done=%v, retryAfter=%v), want bounded pending retry", done, retryAfter)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get Task: %v", err)
	}
	state, ok := harnessWrapperCancellation(&updated)
	if !ok || state.Status != string(harnessWrapperCancelPending) || state.Attempts != 1 {
		t.Fatalf("cancellation state = %#v, ok=%v, want one pending attempt", state, ok)
	}
}

func TestHarnessCancellationRecoversAfterRuntimeAuthRotation(t *testing.T) {
	var (
		mu          sync.Mutex
		authHeaders []string
		cancelCalls atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil || resource != harness.TurnResourceCancel {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		cancelCalls.Add(1)
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer new-token" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		fixture := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: harness.RuntimeSessionID(fixture.Annotations[harnessWrapperRuntimeAnnotation]),
			TurnID:           turnID,
			CorrelationID:    fixture.Annotations[harnessWrapperCorrelationIDAnno],
		})
	}))
	defer server.Close()

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL)
	token.Data["token"] = []byte("old-token")
	task.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{
		RuntimeRefName:         runtime.Name,
		RuntimeName:            runtime.Name,
		ContractVersion:        string(runtime.Spec.ContractVersion),
		Endpoint:               runtime.Spec.Deployment.Endpoint,
		RuntimeGeneration:      runtime.Generation,
		AuthRefName:            runtime.Spec.ClientAuth.BearerAuthRef.Name,
		AuthRefField:           runtime.Spec.ClientAuth.BearerAuthRef.Key,
		AuthRefResourceVersion: token.ResourceVersion,
	}
	task.Annotations[harnessWrapperRuntimeRefAnno] = runtime.Name
	r := newUnitReconciler(newTestScheme(), task, runtime, token)

	done, retryAfter, err := r.ensureHarnessWrapperTurnCancelled(context.Background(), task, "auth rotation")
	if err != nil {
		t.Fatalf("first ensureHarnessWrapperTurnCancelled() error = %v", err)
	}
	if done || retryAfter <= 0 {
		t.Fatalf("first result = (done=%v, retryAfter=%v), want pending auth retry", done, retryAfter)
	}

	var rotatedToken = &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: token.Name, Namespace: token.Namespace}, rotatedToken); err != nil {
		t.Fatalf("Get token Secret: %v", err)
	}
	rotatedToken.Data["token"] = []byte("new-token")
	if err := r.Update(context.Background(), rotatedToken); err != nil {
		t.Fatalf("rotate token Secret: %v", err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: token.Name, Namespace: token.Namespace}, rotatedToken); err != nil {
		t.Fatalf("Get rotated token Secret: %v", err)
	}
	var observedRuntime corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), types.NamespacedName{Name: runtime.Name, Namespace: runtime.Namespace}, &observedRuntime); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	observedRuntime.Status.ObservedAuthRefResourceVersion = rotatedToken.ResourceVersion
	if err := r.Status().Update(context.Background(), &observedRuntime); err != nil {
		t.Fatalf("observe rotated token Secret: %v", err)
	}

	current := forceHarnessCancellationDue(t, r, task)
	done, retryAfter, err = r.ensureHarnessWrapperTurnCancelled(context.Background(), current, "auth rotation")
	if err != nil {
		t.Fatalf("second ensureHarnessWrapperTurnCancelled() error = %v", err)
	}
	if !done || retryAfter != 0 {
		t.Fatalf("second result = (done=%v, retryAfter=%v), want acknowledged", done, retryAfter)
	}
	mu.Lock()
	gotHeaders := append([]string(nil), authHeaders...)
	mu.Unlock()
	if len(gotHeaders) != 2 || gotHeaders[0] != "Bearer old-token" || gotHeaders[1] != "Bearer new-token" {
		t.Fatalf("Authorization headers = %#v, want old then rotated token", gotHeaders)
	}
	if got := cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls = %d, want 2", got)
	}
}

func TestHarnessDurableAnnotationPatchesRejectConcurrentTaskChanges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		phase      corev1alpha1.TaskPhase
		annotation string
		patch      func(context.Context, *TaskReconciler, *corev1alpha1.Task) error
	}{
		{
			name:       "cancellation",
			phase:      corev1alpha1.TaskPhaseRunning,
			annotation: harnessWrapperCancellationAnno,
			patch: func(ctx context.Context, r *TaskReconciler, task *corev1alpha1.Task) error {
				return r.patchHarnessWrapperCancellation(ctx, task, harnessWrapperCancellationState{
					TurnID: task.Annotations[harnessWrapperTurnIDAnnotation],
					Status: string(harnessWrapperCancelPending),
				})
			},
		},
		{
			name:       "outcome unknown",
			phase:      corev1alpha1.TaskPhasePending,
			annotation: labels.AnnotationHarnessTurnOutcomeUnknown,
			patch: func(ctx context.Context, r *TaskReconciler, task *corev1alpha1.Task) error {
				_, err := r.patchHarnessWrapperTurnOutcomeUnknown(
					ctx,
					task,
					harness.HarnessTurnID(task.Annotations[harnessWrapperTurnIDAnnotation]),
				)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := harnessDurabilityTask(tc.phase)
			r := newUnitReconciler(newTestScheme(), task)
			base := r.Client
			baseWithWatch, ok := base.(ctrlclient.WithWatch)
			if !ok {
				t.Fatal("fake client does not implement client.WithWatch")
			}
			injected := false
			r.Client = interceptor.NewClient(baseWithWatch, interceptor.Funcs{
				Patch: func(
					ctx context.Context,
					c ctrlclient.WithWatch,
					obj ctrlclient.Object,
					patch ctrlclient.Patch,
					opts ...ctrlclient.PatchOption,
				) error {
					if !injected {
						injected = true
						var latest corev1alpha1.Task
						if err := c.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &latest); err != nil {
							return err
						}
						if latest.Annotations == nil {
							latest.Annotations = map[string]string{}
						}
						latest.Annotations["test.orka.ai/concurrent-write"] = scheduledRunLabelValue
						if err := c.Update(ctx, &latest); err != nil {
							return err
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			err := tc.patch(context.Background(), r, task)
			if !apierrors.IsConflict(err) {
				t.Fatalf("patch error = %v, want optimistic-lock conflict", err)
			}
			var updated corev1alpha1.Task
			if err := base.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
				t.Fatalf("Get Task: %v", err)
			}
			if updated.Annotations[tc.annotation] != "" {
				t.Fatalf("stale durable annotation %q was applied: %#v", tc.annotation, updated.Annotations)
			}
			if updated.Annotations["test.orka.ai/concurrent-write"] != scheduledRunLabelValue {
				t.Fatalf("concurrent write missing: %#v", updated.Annotations)
			}
		})
	}
}

func TestHarnessOutcomeUnknownPatchDoesNotFenceRunningOwner(t *testing.T) {
	task := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
	r := newUnitReconciler(newTestScheme(), task)
	marked, err := r.patchHarnessWrapperTurnOutcomeUnknown(
		context.Background(),
		task,
		harness.HarnessTurnID(task.Annotations[harnessWrapperTurnIDAnnotation]),
	)
	if err != nil {
		t.Fatalf("patchHarnessWrapperTurnOutcomeUnknown() error = %v", err)
	}
	if marked {
		t.Fatal("Running turn owner was fenced as outcome unknown")
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get Task: %v", err)
	}
	if got := updated.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown]; got != "" {
		t.Fatalf("Running turn outcome-unknown fence = %q, want empty", got)
	}
}

func TestHandleDeletionSkipsCancellationForCompletedOutcomeUnknownTurn(t *testing.T) {
	t.Setenv(harnessWrapperEndpointEnv, "")
	task := harnessDurabilityTask(corev1alpha1.TaskPhaseFailed)
	task.Finalizers = []string{labels.TaskFinalizer}
	task.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown] = task.Annotations[harnessWrapperTurnIDAnnotation]
	r := newUnitReconciler(newTestScheme(), task)

	if _, err := r.handleDeletion(context.Background(), task); err != nil {
		t.Fatalf("handleDeletion() error = %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get Task: %v", err)
	}
	if controllerutil.ContainsFinalizer(&updated, labels.TaskFinalizer) {
		t.Fatal("Task finalizer retained for a turn already proven completed")
	}
	if taskHasHarnessWrapperCancellationState(&updated) {
		t.Fatalf("completed outcome-unknown turn created cancellation state: %#v", updated.Annotations)
	}
}

func TestHandleDeletionRemovesFinalizerOnlyAfterHarnessCancellationAcknowledgement(t *testing.T) {
	var cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil || resource != harness.TurnResourceCancel {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if cancelCalls.Add(1) == 1 {
			harness.WriteError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		fixture := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: harness.RuntimeSessionID(fixture.Annotations[harnessWrapperRuntimeAnnotation]),
			TurnID:           turnID,
			CorrelationID:    fixture.Annotations[harnessWrapperCorrelationIDAnno],
		})
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
	task.Finalizers = []string{labels.TaskFinalizer}
	r := newUnitReconciler(newTestScheme(), task)

	result, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("first handleDeletion() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("first RequeueAfter = %v, want cancellation retry", result.RequeueAfter)
	}
	current := forceHarnessCancellationDue(t, r, task)
	if _, err := r.handleDeletion(context.Background(), current); err != nil {
		t.Fatalf("second handleDeletion() error = %v", err)
	}

	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get Task: %v", err)
	}
	if controllerutil.ContainsFinalizer(&updated, labels.TaskFinalizer) {
		t.Fatal("Task finalizer retained after harness cancellation acknowledgement")
	}
	if got := cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls = %d, want 2", got)
	}
}

func TestHarnessCancellationPendingStateSurvivesControllerRestart(t *testing.T) {
	var cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil || resource != harness.TurnResourceCancel {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if cancelCalls.Add(1) == 1 {
			harness.WriteError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		fixture := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: harness.RuntimeSessionID(fixture.Annotations[harnessWrapperRuntimeAnnotation]),
			TurnID:           turnID,
			CorrelationID:    fixture.Annotations[harnessWrapperCorrelationIDAnno],
		})
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseCancelled)
	first := newUnitReconciler(newTestScheme(), task)
	done, retryAfter, err := first.ensureHarnessWrapperTurnCancelled(context.Background(), task, "restart durability")
	if err != nil {
		t.Fatalf("first ensureHarnessWrapperTurnCancelled() error = %v", err)
	}
	if done || retryAfter <= 0 {
		t.Fatalf("first result = (done=%v, retryAfter=%v), want pending", done, retryAfter)
	}
	var persisted corev1alpha1.Task
	if err := first.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &persisted); err != nil {
		t.Fatalf("Get persisted Task: %v", err)
	}

	// A new reconciler has no process-local retry state; the Task annotation is the owner.
	restarted := newUnitReconciler(newTestScheme(), persisted.DeepCopy())
	done, retryAfter, err = restarted.ensureHarnessWrapperTurnCancelled(context.Background(), &persisted, "restart durability")
	if err != nil {
		t.Fatalf("restarted ensureHarnessWrapperTurnCancelled() error = %v", err)
	}
	if done || retryAfter <= 0 {
		t.Fatalf("restarted result = (done=%v, retryAfter=%v), want persisted backoff", done, retryAfter)
	}
	if got := cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel calls before persisted retry is due = %d, want 1", got)
	}

	current := forceHarnessCancellationDue(t, restarted, &persisted)
	done, retryAfter, err = restarted.ensureHarnessWrapperTurnCancelled(context.Background(), current, "restart durability")
	if err != nil {
		t.Fatalf("due ensureHarnessWrapperTurnCancelled() error = %v", err)
	}
	if !done || retryAfter != 0 {
		t.Fatalf("due result = (done=%v, retryAfter=%v), want acknowledged", done, retryAfter)
	}
	if got := cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls after restart recovery = %d, want 2", got)
	}
}

func TestHarnessWrapperCompletedTombstoneWithoutFramesFailsOutcomeUnknown(t *testing.T) {
	var startCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             string(corev1alpha1.AgentRuntimeCodex),
				ProviderKind:            harness.ProviderKindKubernetesService,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			startCalls.Add(1)
			harness.WriteError(w, http.StatusConflict, "turn already completed")
		default:
			harness.WriteError(w, http.StatusNotFound, "turn not found")
		}
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 3}
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("plan handlePending() error = %v", err)
	}
	var planned corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &planned); err != nil {
		t.Fatalf("Get planned Task: %v", err)
	}
	if _, err := r.handlePending(context.Background(), &planned); err != nil {
		t.Fatalf("tombstone handlePending() error = %v", err)
	}

	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get tombstoned Task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "outcome_unknown") {
		t.Fatalf("message = %q, want outcome_unknown", updated.Status.Message)
	}
	turnID := planned.Annotations[harnessWrapperTurnIDAnnotation]
	if got := updated.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown]; got != turnID {
		t.Fatalf("outcome-unknown fence = %q, want turn ID %q", got, turnID)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("StartTurn calls = %d, want 1 tombstone observation", got)
	}

	if _, err := r.handleCompleted(context.Background(), &updated); err != nil {
		t.Fatalf("handleCompleted() error = %v", err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("StartTurn calls after terminal reconcile = %d, want no second StartTurn", got)
	}
}

func TestHarnessWrapperOutcomeUnknownFenceSurvivesControllerRestart(t *testing.T) {
	var startCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath {
			startCalls.Add(1)
			harness.WriteError(w, http.StatusInternalServerError, "StartTurn must remain fenced")
			return
		}
		harness.WriteError(w, http.StatusNotFound, "not found")
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	turnID := harnessWrapperTurnID(task, 1)
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:             string(turnID),
		harnessWrapperRuntimeAnnotation:            string(harnessWrapperRuntimeSessionID(task, string(agent.Spec.Runtime.Type))),
		harnessWrapperCorrelationIDAnno:            string(task.UID),
		harnessWrapperLastFrameSeqAnno:             "0",
		harnessWrapperStartedAnno:                  harnessDurabilityNotStarted,
		harnessWrapperPlannedAtAnno:                time.Now().UTC().Format(time.RFC3339Nano),
		harnessWrapperMetadataAnno:                 `{"runtime":"codex","wrapper":"cli","contractVersion":"orka.harness.v1"}`,
		labels.AnnotationHarnessTurnOutcomeUnknown: string(turnID),
	}
	// A fresh reconciler with only persisted Task state simulates controller restart
	// after the fence patch succeeded but terminal status persistence did not.
	r := newUnitReconciler(newTestScheme(), task, agent, secret)

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("restarted handlePending() error = %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get restarted Task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(updated.Status.Message, "outcome_unknown") {
		t.Fatalf("restarted status = (%s, %q), want failed outcome_unknown", updated.Status.Phase, updated.Status.Message)
	}
	if got := startCalls.Load(); got != 0 {
		t.Fatalf("StartTurn calls after restart = %d, want 0", got)
	}
}

func TestHarnessWrapperCompletedTombstoneSuppressesSecondEffect(t *testing.T) {
	adapter := &countingHarnessAdapter{
		FakeAdapter: &cliwrapper.FakeAdapter{
			Behavior:    cliwrapper.FakeBehaviorSuccess,
			RuntimeName: string(corev1alpha1.AgentRuntimeCodex),
		},
		done: make(chan struct{}),
	}
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.TurnRetention = 10 * time.Millisecond
	wrapper, err := cliwrapper.NewServer(cfg, adapter)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var startCalls atomic.Int32
	handler := wrapper.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath {
			startCalls.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 3}
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("plan handlePending() error = %v", err)
	}
	var planned corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &planned); err != nil {
		t.Fatalf("Get planned Task: %v", err)
	}
	request, err := r.plannedHarnessWrapperStartTurnRequest(context.Background(), &planned, agent, time.Now())
	if err != nil {
		t.Fatalf("plannedHarnessWrapperStartTurnRequest() error = %v", err)
	}
	client, err := harness.NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("pre-accept StartTurn() error = %v", err)
	}
	select {
	case <-adapter.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pre-accepted turn effect")
	}
	// The wrapper evicts completed turns after retention and keeps a consumed-ID tombstone.
	time.Sleep(5 * cfg.TurnRetention)

	if _, err := r.handlePending(context.Background(), &planned); err != nil {
		t.Fatalf("tombstone handlePending() error = %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get tombstoned Task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(updated.Status.Message, "outcome_unknown") {
		t.Fatalf("tombstoned status = (%s, %q), want failed outcome_unknown", updated.Status.Phase, updated.Status.Message)
	}
	if got := adapter.runs.Load(); got != 1 {
		t.Fatalf("runtime effects = %d, want exactly 1", got)
	}
	if got := startCalls.Load(); got != 2 {
		t.Fatalf("StartTurn calls = %d, want original acceptance plus one tombstone observation", got)
	}

	if _, err := r.handleCompleted(context.Background(), &updated); err != nil {
		t.Fatalf("handleCompleted() error = %v", err)
	}
	if got := adapter.runs.Load(); got != 1 {
		t.Fatalf("runtime effects after terminal reconcile = %d, want no second effect", got)
	}
	if got := startCalls.Load(); got != 2 {
		t.Fatalf("StartTurn calls after terminal reconcile = %d, want no second controller StartTurn", got)
	}
}

func TestHarnessWrapperActiveAlreadyExistsRetainsRecovery(t *testing.T) {
	var (
		mu      sync.Mutex
		started harness.StartTurnRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             string(corev1alpha1.AgentRuntimeCodex),
				ProviderKind:            harness.ProviderKindKubernetesService,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			var request harness.StartTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				harness.WriteError(w, http.StatusBadRequest, "invalid request")
				return
			}
			mu.Lock()
			started = request
			mu.Unlock()
			harness.WriteError(w, http.StatusConflict, "turn already exists")
		default:
			mu.Lock()
			request := started
			mu.Unlock()
			eventsPath, _ := harness.EventStreamPath(request.TurnID)
			if r.Method != http.MethodGet || r.URL.Path != eventsPath {
				harness.WriteError(w, http.StatusNotFound, "not found")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
				Version:          harness.ProtocolVersion,
				Type:             harness.FrameTurnCompleted,
				RuntimeSessionID: request.RuntimeSessionID,
				TurnID:           request.TurnID,
				CorrelationID:    request.CorrelationID,
				Seq:              1,
				CreatedAt:        time.Now().UTC(),
				Completed:        &harness.TurnCompleted{Result: "recovered"},
			})
			_ = harness.WriteSSEDone(w)
		}
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	running := runHarnessWrapperTaskToRunning(t, r, task)
	if got := running.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown]; got != "" {
		t.Fatalf("active turn fenced as outcome unknown: %q", got)
	}
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning() error = %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get completed Task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessWrapperNeverAcceptedStartRetriesSameTurn(t *testing.T) {
	var (
		startCalls atomic.Int32
		mu         sync.Mutex
		turnIDs    []harness.HarnessTurnID
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             string(corev1alpha1.AgentRuntimeCodex),
				ProviderKind:            harness.ProviderKindKubernetesService,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			var request harness.StartTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				harness.WriteError(w, http.StatusBadRequest, "invalid request")
				return
			}
			mu.Lock()
			turnIDs = append(turnIDs, request.TurnID)
			mu.Unlock()
			if startCalls.Add(1) == 1 {
				harness.WriteError(w, http.StatusServiceUnavailable, "not accepted")
				return
			}
			eventsPath, _ := harness.EventStreamPath(request.TurnID)
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: request.RuntimeSessionID,
				TurnID:           request.TurnID,
				CorrelationID:    request.CorrelationID,
				EventStreamPath:  eventsPath,
			})
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("plan handlePending() error = %v", err)
	}
	var planned corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &planned); err != nil {
		t.Fatalf("Get planned Task: %v", err)
	}
	if _, err := r.handlePending(context.Background(), &planned); err != nil {
		t.Fatalf("first StartTurn handlePending() error = %v", err)
	}
	var retrying corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &retrying); err != nil {
		t.Fatalf("Get retrying Task: %v", err)
	}
	if retrying.Status.Phase != corev1alpha1.TaskPhasePending || taskHasHarnessWrapperTurn(&retrying) {
		t.Fatalf("retrying state = phase %s annotations %#v, want planned Pending", retrying.Status.Phase, retrying.Annotations)
	}
	if _, err := r.handlePending(context.Background(), &retrying); err != nil {
		t.Fatalf("second StartTurn handlePending() error = %v", err)
	}
	var running corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &running); err != nil {
		t.Fatalf("Get running Task: %v", err)
	}
	if running.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running", running.Status.Phase)
	}
	mu.Lock()
	gotTurnIDs := append([]harness.HarnessTurnID(nil), turnIDs...)
	mu.Unlock()
	if len(gotTurnIDs) != 2 || gotTurnIDs[0] != gotTurnIDs[1] {
		t.Fatalf("StartTurn IDs = %#v, want same planned turn retried", gotTurnIDs)
	}
	if got := running.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown]; got != "" {
		t.Fatalf("never-accepted turn fenced as outcome unknown: %q", got)
	}
}

func TestHarnessWrapperMissingNeverAcceptedTurnRetainsAutomaticRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		harness.WriteError(w, http.StatusNotFound, "turn not found")
	}))
	defer server.Close()
	t.Setenv(harnessWrapperEndpointEnv, server.URL)

	task := harnessDurabilityTask(corev1alpha1.TaskPhaseRunning)
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 2}
	r := newUnitReconciler(newTestScheme(), task)
	if _, err := r.handleRunning(context.Background(), task); err != nil {
		t.Fatalf("handleRunning() error = %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("Get retried Task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending retry", updated.Status.Phase)
	}
	if taskHasPlannedHarnessWrapperTurn(&updated) {
		t.Fatalf("missing never-accepted turn identity was not cleared: %#v", updated.Annotations)
	}
	if got := updated.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown]; got != "" {
		t.Fatalf("missing never-accepted turn fenced as outcome unknown: %q", got)
	}
}

type countingHarnessAdapter struct {
	*cliwrapper.FakeAdapter
	runs atomic.Int32
	done chan struct{}
	once sync.Once
}

func (a *countingHarnessAdapter) RunTurn(
	ctx context.Context,
	turn cliwrapper.TurnContext,
	emit func(harness.HarnessEventFrame) error,
) (cliwrapper.TurnResult, error) {
	a.runs.Add(1)
	result, err := a.FakeAdapter.RunTurn(ctx, turn, emit)
	a.once.Do(func() { close(a.done) })
	return result, err
}

func harnessDurabilityTask(phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness-durability-task",
			Namespace: defaultNS,
			UID:       types.UID("uid-harness-durability-task"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    phase,
			Attempts: 1,
		},
	}
	turnID := harnessWrapperTurnID(task, 1)
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(turnID),
		harnessWrapperRuntimeAnnotation: string(harnessWrapperRuntimeSessionID(task, "codex")),
		harnessWrapperCorrelationIDAnno: string(task.UID),
		harnessWrapperStartedAnno:       scheduledRunLabelValue,
		harnessWrapperLastFrameSeqAnno:  "0",
		harnessWrapperPlannedAtAnno:     time.Now().UTC().Format(time.RFC3339Nano),
		harnessWrapperMetadataAnno:      `{"runtime":"codex","wrapper":"cli"}`,
	}
	return task
}

func forceHarnessCancellationDue(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	var current corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &current); err != nil {
		t.Fatalf("Get Task before forcing cancellation retry: %v", err)
	}
	state, ok := harnessWrapperCancellation(&current)
	if !ok {
		t.Fatalf("missing cancellation state on Task: %#v", current.Annotations)
	}
	state.RetryAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	if err := r.patchHarnessWrapperCancellation(context.Background(), &current, state); err != nil {
		t.Fatalf("patch cancellation retry due: %v", err)
	}
	return &current
}
