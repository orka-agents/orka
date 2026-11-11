package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPreparedIdentityReadOutageDoesNotTerminateSubmission(t *testing.T) {
	for _, role := range []string{"preparation", "parent"} {
		for _, failure := range []error{
			apierrors.NewTimeoutError("synthetic", 1),
			apierrors.NewTooManyRequests("synthetic", 1),
			apierrors.NewInternalError(errors.New("synthetic")),
			apierrors.NewServiceUnavailable("synthetic"),
		} {
			t.Run(role+"/"+string(apierrors.ReasonForError(failure)), func(t *testing.T) {
				s, store := testService(t, true)
				task, input, report := preparedFixture(t, s)
				submission, err := s.SubmitPrepared(t.Context(), task, input, report)
				require.NoError(t, err)
				name := task.Name
				if role == "parent" {
					name = input.ParentTaskName
				}
				fail := true
				s.reader = interceptor.NewClient(s.client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if key.Name == name && fail {
							fail = false
							return fmt.Errorf("read owner: %w", failure)
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
				err = s.Advance(t.Context(), submission.Namespace, submission.RequestID)
				require.Error(t, err)
				require.True(t, IsTransientError(err))
				require.False(t, errors.Is(err, pv.ErrBinding))
				current, err := store.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
				require.NoError(t, err)
				require.Equal(t, pv.SubmissionPreparing, current.State)
				require.Empty(t, current.Failure)
				require.NoError(t, s.Advance(t.Context(), submission.Namespace, submission.RequestID))
				current, err = store.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
				require.NoError(t, err)
				require.Equal(t, pv.SubmissionRunning, current.State)
				require.NotNil(t, current.Binding)
			})
		}
	}
}

func TestTransientPodLogFailurePreservesObservedDispatch(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, store := testService(t, true)
			submission := boundSubmission(t, s, pv.ValidateReport)
			check := submission.Manifest.Checks[0]
			task := &corev1alpha1.Task{}
			require.NoError(t, s.reader.Get(t.Context(), client.ObjectKey{Namespace: submission.Namespace, Name: submission.OriginalTaskName}, task))
			dispatch := pv.KubernetesDispatch{RunID: submission.RunID, Side: pv.Original, CheckID: check.ID,
				JobName: "log-fixture-job", SpecDigest: pv.Digest([]byte("fixture")), State: pv.DispatchPlanned}
			require.NoError(t, store.CreateKubernetesValidationDispatch(t.Context(), dispatch))
			dispatch.JobUID, dispatch.PodUID, dispatch.ContainerID = "job-fixture", "pod-fixture", "container-fixture"
			require.NoError(t, store.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, dispatch.JobUID))
			require.NoError(t, store.MarkKubernetesValidationDispatchObserved(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, dispatch.JobUID, dispatch.PodUID, dispatch.ContainerID))
			dispatch.State = pv.DispatchObserved
			output := []byte(check.Failure.Stdout)
			now := time.Now().UTC()
			report := pv.PodReport{Version: pv.PodProtocolVersion, TaskUID: string(task.UID), PodUID: dispatch.PodUID,
				Evidence: pv.ExecutionEvidence{Observation: pv.Observation{
					RunID: submission.RunID, AttemptID: submission.AttemptID, TaskID: string(task.UID), ManifestDigest: submission.Binding.ManifestDigest,
					Side: pv.Original, CheckID: check.ID, SourceTree: submission.Manifest.Sources.Original.Tree, ImageID: submission.Manifest.Environment.ImageID,
					Origin: "runner", StartedAt: now, FinishedAt: now.Add(time.Millisecond), Executed: true, ExitCode: new(0),
					StdoutDigest: pv.Digest(output), StdoutBytes: len(output), StderrDigest: pv.Digest(nil),
				}, Blobs: map[string][]byte{pv.Digest(output): output, pv.Digest(nil): {}}}}
			var fail atomic.Bool
			fail.Store(true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "validation", r.URL.Query().Get("container"))
				w.Header().Set("Content-Type", "application/json")
				if fail.Load() {
					w.WriteHeader(status)
					require.NoError(t, json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
						Status: "Failure", Code: int32(status), Message: "synthetic outage", Reason: metav1.StatusReasonUnknown}))
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(report))
			}))
			t.Cleanup(server.Close)
			var err error
			s.kubeClient, err = kubernetes.NewForConfig(&rest.Config{Host: server.URL})
			require.NoError(t, err)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "log-fixture", UID: "pod-fixture"}}
			container := corev1.ContainerStatus{ContainerID: dispatch.ContainerID}
			var result ctrl.Result
			result, err = s.recordPodEvidence(t.Context(), submission, task, check, &dispatch, pod, container)
			require.Error(t, err)
			require.True(t, IsTransientError(err), "%v", err)
			require.Zero(t, result)
			current, err := store.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, pv.DispatchObserved, current.State)
			require.Empty(t, current.Failure)
			fail.Store(false)
			_, err = s.recordPodEvidence(t.Context(), submission, task, check, &dispatch, pod, container)
			require.NoError(t, err)
			current, err = store.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, pv.DispatchRecorded, current.State)
		})
	}
}
