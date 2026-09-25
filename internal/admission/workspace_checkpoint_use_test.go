package admission

import (
	"context"
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type capturedCheckpointUse struct {
	capturedClassUse
	checkpoint, source string
	dataCaller         authenticationv1.UserInfo
	dataErr            error
}

func (a *capturedCheckpointUse) AuthorizeCheckpoint(_ context.Context, namespace, name string, caller authenticationv1.UserInfo) error {
	a.namespace, a.checkpoint, a.dataCaller = namespace, name, caller
	return a.dataErr
}
func (a *capturedCheckpointUse) AuthorizeCheckpointSource(_ context.Context, namespace, name string, caller authenticationv1.UserInfo) error {
	a.namespace, a.source, a.dataCaller = namespace, name, caller
	return a.dataErr
}

func TestWorkspaceCheckpointUseRequiresLiveCallerPermission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacev1alpha1.AddToScheme(scheme))
	caller := authenticationv1.UserInfo{Username: "alice", Groups: []string{"developers"}}
	for _, resource := range []workspaceClassResource{workspaceClassTask, workspaceCheckpointSource} {
		authorizer := &capturedCheckpointUse{dataErr: errors.New("denied")}
		validator := newWorkspaceClassUseValidator(scheme, authorizer, resource)
		var object runtime.Object
		if resource == workspaceClassTask {
			task := workspaceClassTaskFixture("coding")
			task.Spec.Execution.Workspace.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{Name: "checkpoint", UID: "checkpoint-uid", Digest: "sha256:fixture"}
			object = task
		} else {
			object = &workspacev1alpha1.ExecutionWorkspaceCheckpoint{
				TypeMeta:   metav1.TypeMeta{APIVersion: workspacev1alpha1.GroupVersion.String(), Kind: "ExecutionWorkspaceCheckpoint"},
				ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: admissionTestNamespace},
				Spec:       workspacev1alpha1.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspacev1alpha1.ObjectIdentityReference{Name: "source", UID: "source-uid"}},
			}
		}
		request := workspaceClassAdmissionRequest(t, admissionv1.Create, caller, object, nil, "")
		require.False(t, validator.Handle(t.Context(), request).Allowed)
		require.Equal(t, caller, authorizer.dataCaller)
		require.Equal(t, admissionTestNamespace, authorizer.namespace)
		authorizer.dataErr = nil
		require.True(t, validator.Handle(t.Context(), request).Allowed)
		if resource == workspaceClassTask {
			require.Equal(t, "checkpoint", authorizer.checkpoint)
		} else {
			require.Equal(t, "source", authorizer.source)
		}
	}
}
