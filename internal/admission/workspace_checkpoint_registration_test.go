/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

type registeredCheckpointAuthorizer struct {
	calls     int
	namespace string
	workspace string
	username  string
	err       error
}

func (*registeredCheckpointAuthorizer) Authorize(context.Context, string, string, authenticationv1.UserInfo) error {
	return errors.New("checkpoint admission must authorize its source workspace")
}

func (*registeredCheckpointAuthorizer) AuthorizeCheckpoint(
	context.Context, string, string, authenticationv1.UserInfo,
) error {
	return errors.New("checkpoint admission must authorize its source workspace")
}

func (a *registeredCheckpointAuthorizer) AuthorizeCheckpointSource(
	_ context.Context, namespace, workspace string, caller authenticationv1.UserInfo,
) error {
	a.calls++
	a.namespace, a.workspace, a.username = namespace, workspace, caller.Username
	return a.err
}

func TestRegisteredCheckpointAdmissionAuthorizesDecodedSource(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	authorizer := &registeredCheckpointAuthorizer{}
	server := webhook.NewServer(webhook.Options{})
	RegisterWorkspaceClassUseWebhooks(server, scheme, authorizer)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID: "checkpoint-create", Namespace: "tenant-a", Operation: admissionv1.Create,
			UserInfo: authenticationv1.UserInfo{Username: "alice"},
			Object: runtime.RawExtension{Raw: []byte(`{
				"apiVersion":"workspace.orka.ai/v1alpha1", "kind":"ExecutionWorkspaceCheckpoint",
				"metadata":{"name":"saved","namespace":"tenant-a"}, "spec":{"workspaceRef":{"name":"source"}}
			}`)},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []bool{false, true} {
		if denied {
			authorizer.err = errors.New("source access denied")
		}
		request := httptest.NewRequest(http.MethodPost, CheckpointSourceUseWebhookPath, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.WebhookMux().ServeHTTP(response, request)
		var result admissionv1.AdmissionReview
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Response == nil || result.Response.Allowed == denied || result.Response.UID != review.Request.UID {
			t.Fatalf("denied=%t: admission response %s", denied, response.Body.String())
		}
	}
	if authorizer.calls != 2 || authorizer.namespace != "tenant-a" ||
		authorizer.workspace != "source" || authorizer.username != "alice" {
		t.Fatalf("checkpoint source authorization did not receive the decoded request: %+v", authorizer)
	}
}
