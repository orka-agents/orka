package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/workerenv"
)

// A stdout result is decoded only when it can fit the result limit, and a
// larger one is refused whole.
func TestExtractStdoutTaskResultRefusesAResultAboveTheLimit(t *testing.T) {
	limit := bytes.Repeat([]byte("r"), store.MaxResultBytes)
	got, ok, err := extractStdoutTaskResult(workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString(limit))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, bytes.Equal(limit, got))

	got, ok, err = extractStdoutTaskResult(workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString(append(limit, 'r')))
	require.ErrorIs(t, err, store.ErrTooLarge)
	require.True(t, ok)
	require.Nil(t, got)

	// A payload too long for the limit is refused before it is decoded, so
	// its content is never examined.
	tooLong := strings.Repeat("!", base64.StdEncoding.EncodedLen(store.MaxResultBytes)+4)
	got, ok, err = extractStdoutTaskResult(workerenv.ResultStdoutPrefix + tooLong)
	require.ErrorIs(t, err, store.ErrTooLarge)
	require.True(t, ok)
	require.Nil(t, got)
}

// A result above the limit is never stored, so a Task with one is neither
// reported as succeeded without it nor left retrying it: it ends with the
// reason recorded.
func TestTaskCompletionEndsWithAResultAboveTheLimit(t *testing.T) {
	t.Run("stdout result of a successful workload", func(t *testing.T) {
		task, job, pod, namespace := deferredFinalizationFixture()
		task.Annotations = map[string]string{labels.AnnotationAgentReadOnly: booleanTrueValue}
		task.Spec.Type = corev1alpha1.TaskTypeAI
		job.Status = batchv1.JobStatus{Succeeded: 1}
		r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
		oversized := bytes.Repeat([]byte("r"), store.MaxResultBytes+1)
		serveDeferredResultLogs(t, r, workerenv.ResultStdoutPrefix+base64.StdEncoding.EncodeToString(oversized)+"\n")

		_, err := r.completeExecutedTask(t.Context(), task.DeepCopy(), corev1alpha1.TaskPhaseSucceeded, "completed")
		require.NoError(t, err)
		current := &corev1alpha1.Task{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
		require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase, "a result that was not stored is not a success")
		require.Equal(t, "completed; task result exceeds the 10485760 byte limit and was not stored", current.Status.Message)
		require.Nil(t, current.Status.ResultRef)
		require.NotNil(t, current.Status.ExecutionOutcome)
		require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.ExecutionOutcome.Phase, "the workload itself succeeded")
		require.Equal(t, "completed", current.Status.ExecutionOutcome.Message)
		_, err = r.ResultStore.GetResult(t.Context(), task.Namespace, task.Name)
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("deferred result the store refuses", func(t *testing.T) {
		// A controller without the stdout bound could stage a result above
		// the limit before stopping the work. The store refuses it.
		task, _, _, namespace := deferredFinalizationFixture()
		r := newUnitReconciler(newTestScheme(), task, namespace)
		oversized := bytes.Repeat([]byte("r"), store.MaxResultBytes+1)
		digest := sha256.Sum256(oversized)
		intent := deferredTaskFinalization{
			Phase: corev1alpha1.TaskPhaseCancelled, OutcomePhase: corev1alpha1.TaskPhaseCancelled, Message: "stop",
			ResultBytes: len(oversized), ResultDigest: hex.EncodeToString(digest[:]),
			ResultChunks: (len(oversized) + deferredResultChunkBytes - 1) / deferredResultChunkBytes,
		}
		for i := range intent.ResultChunks {
			chunk := oversized[i*deferredResultChunkBytes : min((i+1)*deferredResultChunkBytes, len(oversized))]
			require.NoError(t, r.createDeferredFinalizationSecret(t.Context(), task, deferredResultChunkName(task, &intent, i), "result", chunk))
		}
		data, err := json.Marshal(intent)
		require.NoError(t, err)
		require.NoError(t, r.createDeferredFinalizationSecret(t.Context(), task, deferredFinalizationName(task), "intent.json", data))

		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
		_, err = r.Reconcile(t.Context(), request)
		require.NoError(t, err)
		current := &corev1alpha1.Task{}
		require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
		require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
		require.Equal(t, "stop; task result exceeds the 10485760 byte limit and was not stored", current.Status.Message)
		require.Nil(t, current.Status.ResultRef)
		_, err = r.ResultStore.GetResult(t.Context(), task.Namespace, task.Name)
		require.ErrorIs(t, err, store.ErrNotFound)
		_, err = r.Reconcile(t.Context(), request)
		require.NoError(t, err)
		secrets := &corev1.SecretList{}
		require.NoError(t, r.List(t.Context(), secrets))
		require.Empty(t, secrets.Items, "terminal cleanup releases the staging")
	})

	t.Run("cancellation retried after a session outage", func(t *testing.T) {
		// The staged intent keeps the original decision, so the retried
		// completion records the reason once.
		task, job, pod, namespace := deferredFinalizationFixture()
		task.Spec.Type = corev1alpha1.TaskTypeAI
		task.Spec.Prompt = "summarize the release"
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "result-session", Append: true}
		task.Annotations[labels.AnnotationAgentReadOnly] = booleanTrueValue
		r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
		backend := r.SessionManager.store.(*sqlite.Store)
		r.SessionManager = NewSessionManager(&lossySessionStore{Store: backend, appendErrs: []error{
			fmt.Errorf("append: %w", store.ErrNotReady),
		}})
		require.NoError(t, backend.CreateSession(t.Context(), &store.SessionRecord{
			Namespace: task.Namespace, Name: "result-session", SessionType: "task",
			ActiveTask: task.Name, ActiveTaskUID: string(task.UID),
		}))
		oversized := bytes.Repeat([]byte("r"), store.MaxResultBytes+1)
		serveDeferredResultLogs(t, r, workerenv.ResultStdoutPrefix+base64.StdEncoding.EncodeToString(oversized)+"\n")
		cascadeDeferredJobPod(t, r, pod)

		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
		_, err := r.Reconcile(t.Context(), request)
		require.ErrorIs(t, err, store.ErrNotReady)
		require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})))
		intent, err := r.loadDeferredTaskFinalization(t.Context(), task)
		require.NoError(t, err)
		require.NotNil(t, intent)
		require.Equal(t, "stop", intent.Message)

		_, err = r.Reconcile(t.Context(), request)
		require.NoError(t, err)
		current := &corev1alpha1.Task{}
		require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
		require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
		require.Equal(t, "stop; task result exceeds the 10485760 byte limit and was not stored", current.Status.Message)
	})
}
