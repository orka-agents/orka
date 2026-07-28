/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type capturedClassUse struct {
	namespace string
	className string
	caller    authenticationv1.UserInfo
	err       error
	calls     int
}

func (a *capturedClassUse) Authorize(
	_ context.Context,
	namespace string,
	className string,
	caller authenticationv1.UserInfo,
) error {
	a.namespace = namespace
	a.className = className
	a.caller = caller
	a.calls++
	return a.err
}

func TestWorkspaceClassUseValidatorTaskSelection(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	authorizer := &capturedClassUse{}
	validator := newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTask)

	task := workspaceClassTaskFixture("coding-v1")
	caller := authenticationv1.UserInfo{
		Username: "alice",
		Groups:   []string{"developers"},
		Extra:    map[string]authenticationv1.ExtraValue{"tenant": {"tenant-a"}},
	}
	response := validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t, admissionv1.Create, caller, task, nil, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, 1, authorizer.calls)
	require.Equal(t, admissionTestNamespace, authorizer.namespace)
	require.Equal(t, "coding-v1", authorizer.className)
	require.Equal(t, caller, authorizer.caller)

	unchanged := task.DeepCopy()
	unchanged.Spec.Image = "alpine:latest"
	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t, admissionv1.Update, caller, unchanged, task, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, 2, authorizer.calls, "mutable updates retaining a class must be reauthorized")

	changed := task.DeepCopy()
	changed.Spec.Execution.Workspace.ClassRef.Name = "coding-v2"
	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t, admissionv1.Update, caller, changed, task, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, 3, authorizer.calls)
	require.Equal(t, "coding-v2", authorizer.className)

	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t, admissionv1.Update, caller, changed, task, "status",
	))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, 3, authorizer.calls, "status writes must not invoke class authorization")
}

func TestWorkspaceClassUseValidatorFailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	authorizer := &capturedClassUse{err: errors.New("subject access review unavailable")}
	validator := newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTask)

	response := validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t,
		admissionv1.Create,
		authenticationv1.UserInfo{Username: "alice"},
		workspaceClassTaskFixture("coding-v1"),
		nil,
		"",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "subject access review unavailable")

	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t,
		admissionv1.Create,
		authenticationv1.UserInfo{Username: "alice"},
		workspaceClassTaskFixture(""),
		nil,
		"",
	))
	require.True(t, response.Allowed)
}

func TestWorkspaceClassUseValidatorToolSelection(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	authorizer := &capturedClassUse{}
	validator := newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTool)
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: admissionTestNamespace},
		Spec: corev1alpha1.ToolSpec{
			Description: "tool",
			MCP: &corev1alpha1.MCPToolServer{Workspace: &corev1alpha1.MCPWorkspace{
				ClassRef: corev1alpha1.WorkspaceClassReference{Name: "service-v1"},
				Port:     8080,
			}},
		},
	}
	response := validator.Handle(context.Background(), workspaceClassAdmissionRequest(
		t,
		admissionv1.Create,
		authenticationv1.UserInfo{Username: "bob"},
		tool,
		nil,
		"",
	))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, "service-v1", authorizer.className)
	require.Equal(t, admissionTestNamespace, authorizer.namespace)
}

func workspaceClassTaskFixture(className string) *corev1alpha1.Task {
	task := newAdmissionTestTask()
	if className == "" {
		return task
	}
	task.Spec.Execution = &corev1alpha1.ExecutionSpec{
		Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
			ClassRef: &corev1alpha1.WorkspaceClassReference{Name: className},
		},
	}
	return task
}

func workspaceClassAdmissionRequest(
	t *testing.T,
	operation admissionv1.Operation,
	caller authenticationv1.UserInfo,
	object runtime.Object,
	oldObject runtime.Object,
	subresource string,
) ctrladmission.Request {
	t.Helper()
	request := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation:   operation,
		Namespace:   admissionTestNamespace,
		SubResource: subresource,
		UserInfo:    caller,
		Object:      runtime.RawExtension{Raw: mustMarshalWorkspaceClassObject(t, object)},
	}}
	if oldObject != nil {
		request.OldObject = runtime.RawExtension{Raw: mustMarshalWorkspaceClassObject(t, oldObject)}
	}
	return request
}

func mustMarshalWorkspaceClassObject(t *testing.T, object runtime.Object) []byte {
	t.Helper()
	switch value := object.(type) {
	case *corev1alpha1.Task:
		copy := value.DeepCopy()
		copy.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task"}
		object = copy
	case *corev1alpha1.Tool:
		copy := value.DeepCopy()
		copy.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Tool"}
		object = copy
	}
	data, err := json.Marshal(object)
	require.NoError(t, err)
	return data
}
