/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestRemovedRuntimeFieldsAreRejectedAfterCRDPruning(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	namespace := admissionNamespace(string(executionmode.HarnessV2))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()
	decoder := ctrladmission.NewDecoder(scheme)
	handlers := map[string]ctrladmission.Handler{
		"Task": &TaskExecutionAuthorityValidator{
			decoder: decoder,
			reader:  reader,
			config:  ExecutionModeConfig{ControllerUsernames: []string{trustedControllerUser}},
		},
		"Agent":        &AgentContractValidator{decoder: decoder, reader: reader},
		"AgentRuntime": &AgentRuntimeContractValidator{decoder: decoder, reader: reader},
	}

	tests := []struct {
		kind        string
		resource    string
		path        string
		subresource string
	}{
		{kind: "Task", resource: "tasks", path: "spec.agentRuntime.workspace"},
		{kind: "Task", resource: "tasks", path: "status.harnessRuntime", subresource: statusSubresource},
		{kind: "Agent", resource: "agents", path: "spec.runtime.secretRef"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.clientAuth.bearerTokenSecretRef"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.capabilities.toolExecutionModes"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.capabilities.brokeredToolClasses"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.capabilities.supportsCancel"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.capabilities.supportsRuntimeSessions"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.capabilities.supportsContinuation"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "spec.capabilities.supportsArtifacts"},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "status.observedCapabilities.runtimeName", subresource: statusSubresource},
		{kind: "AgentRuntime", resource: "agentruntimes", path: "status.observedCapabilities.supportsContinuation", subresource: statusSubresource},
	}
	for _, test := range tests {
		t.Run(test.kind+"/"+test.path, func(t *testing.T) {
			structural := generatedRuntimeSchema(t, test.resource)
			for _, value := range []any{nil, "removed-value", map[string]any{"name": "removed-secret"}} {
				object := runtimeAdmissionObject(test.kind, namespace.Name)
				oldRaw, err := json.Marshal(object)
				require.NoError(t, err)
				fields := strings.Split(test.path, ".")
				parent := object
				for _, field := range fields[:len(fields)-1] {
					next, ok := parent[field].(map[string]any)
					if !ok {
						next = map[string]any{}
						parent[field] = next
					}
					parent = next
				}
				parent[fields[len(fields)-1]] = value
				pruning.Prune(object, structural, true)
				raw, err := json.Marshal(object)
				require.NoError(t, err)
				request := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
					Operation:   admissionv1.Create,
					Namespace:   namespace.Name,
					Object:      runtime.RawExtension{Raw: raw},
					SubResource: test.subresource,
				}}
				if test.subresource != "" {
					request.Operation = admissionv1.Update
					request.OldObject = runtime.RawExtension{Raw: oldRaw}
				}
				request.UserInfo.Username = trustedControllerUser
				response := handlers[test.kind].Handle(context.Background(), request)
				require.False(t, response.Allowed, "removed field %s must survive pruning and be rejected", test.path)
				require.Contains(t, response.Result.Message, test.path+" is no longer supported")
			}
		})
	}
}

func TestUnknownRuntimeKeysCannotBypassSchemaValidation(t *testing.T) {
	validator := newTestTaskExecutionAuthorityValidator(t)
	structural := generatedRuntimeSchema(t, "tasks")
	for _, key := range []string{"MaxTurns", "unexpectedCredential"} {
		t.Run(key, func(t *testing.T) {
			object := runtimeAdmissionObject("Task", "default")
			object["spec"].(map[string]any)["agentRuntime"] = map[string]any{key: int64(1001)}
			pruning.Prune(object, structural, true)
			raw, err := json.Marshal(object)
			require.NoError(t, err)
			response := validator.Handle(t.Context(), ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			}})
			require.False(t, response.Allowed)
			require.Contains(t, response.Result.Message, "spec.agentRuntime."+key+" is not supported")
		})
	}
}

func runtimeAdmissionObject(kind, namespace string) map[string]any {
	var spec map[string]any
	switch kind {
	case "Task":
		spec = map[string]any{"type": "agent", "agentRef": map[string]any{"name": "coder"}, "prompt": "Inspect the workspace"}
	case "Agent":
		spec = map[string]any{
			"model":   map[string]any{"name": "test-model"},
			"runtime": map[string]any{"type": "codex", "contractVersion": "orka.harness.v2"},
		}
	case "AgentRuntime":
		spec = map[string]any{
			"contractVersion": "orka.harness.v2",
			"deployment":      map[string]any{"mode": "external-endpoint", "endpoint": "https://runtime.example.test"},
			"clientAuth": map[string]any{
				"controllerBearerTokenSecretRef": map[string]any{"name": "runtime-auth", "key": "controller"},
				"operationCapabilitySecretRef":   map[string]any{"name": "runtime-auth", "key": "capability"},
			},
			"capabilities": map[string]any{"runtimeInstanceID": "runtime-1"},
		}
	}
	return map[string]any{
		"apiVersion": corev1alpha1.GroupVersion.String(), "kind": kind,
		"metadata": map[string]any{"name": "runtime-rejection", "namespace": namespace},
		"spec":     spec,
	}
}

func generatedRuntimeSchema(t *testing.T, resource string) *structuralschema.Structural {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_"+resource+".yaml"))
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))
	var schema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &schema, nil))
	structural, err := structuralschema.NewStructural(&schema)
	require.NoError(t, err)
	return structural
}
