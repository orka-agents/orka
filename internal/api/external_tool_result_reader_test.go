package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskmeta"
)

type recordingResultReader struct {
	keys []store.ResultKey
}

func (r *recordingResultReader) GetResult(_ context.Context, key store.ResultKey) ([]byte, error) {
	r.keys = append(r.keys, key)
	return []byte("result"), nil
}

// The external tool result reader serves a key only for the data of the Task
// that holds the name now: its own UID, or the source UID of a Task a restore
// recreated and Orka settled on its source execution.
func TestExternalToolResultReaderMatchesTheTaskDataUID(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	plain := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "default", UID: "plain-uid"}}
	restored := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "restored", Namespace: "default", UID: "restored-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
				Backend:         corev1alpha1.AgentExecutionBackendRuntimePool,
				Task:            corev1alpha1.AgentExecutionBindingTaskRef{UID: "source-uid"},
			},
			Execution: &corev1alpha1.TaskExecutionStatus{
				State:   corev1alpha1.TaskExecutionStateSucceeded,
				Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Reason:  taskmeta.RestoreIdentityChangedReason,
			},
		},
	}
	backing := &recordingResultReader{}
	reader := &externalToolResultReader{
		reader: backing,
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(plain, restored).Build(),
	}
	for _, test := range []struct {
		task, uid string
		served    bool
	}{
		{"plain", "plain-uid", true},
		{"plain", "earlier-plain-uid", false},
		{"restored", "source-uid", true},
		{"restored", "restored-uid", false},
	} {
		key := store.ResultKey{Namespace: "default", TaskName: test.task, TaskUID: test.uid}
		data, err := reader.GetResult(t.Context(), key)
		if !test.served {
			require.ErrorIs(t, err, store.ErrNotFound, "%+v", key)
			continue
		}
		require.NoError(t, err, "%+v", key)
		require.Equal(t, "result", string(data))
	}
	require.Equal(t, []store.ResultKey{
		{Namespace: "default", TaskName: "plain", TaskUID: "plain-uid"},
		{Namespace: "default", TaskName: "restored", TaskUID: "source-uid"},
	}, backing.keys)
}
