/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/workerenv"
)

func TestPrepareChildTransactionToken(t *testing.T) {
	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("parent-tx-token"), 0600); err != nil {
		t.Fatalf("failed to write subject token: %v", err)
	}

	var requestDetails map[string]any
	var gotScope string
	var gotSubjectToken string
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token_endpoint" {
			t.Fatalf("path = %q, want /token_endpoint", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotSubjectToken = r.FormValue("subject_token")
		gotScope = r.FormValue("scope")
		if got := r.FormValue("subject_token_type"); got != kontxttoken.SubjectTokenTypeTxnToken {
			t.Fatalf("subject_token_type = %q", got)
		}
		if err := json.Unmarshal([]byte(r.FormValue("request_details")), &requestDetails); err != nil {
			t.Fatalf("request_details JSON error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"child-tx-token","issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"N_A"}`))
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectPath)
	t.Setenv(workerenv.ContextTokenChildScope, "orka:agents:run")

	parent := parentTask()
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace}}
	fc := newFakeClient()
	if err := prepareChildTransactionToken(context.Background(), fc, parent, child, "delegateTask", testResearcherAgentName); err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}

	if gotSubjectToken != "parent-tx-token" {
		t.Fatalf("subject_token = %q, want parent-tx-token", gotSubjectToken)
	}
	if gotScope != "orka:agents:run" {
		t.Fatalf("scope = %q, want orka:agents:run", gotScope)
	}
	if requestDetails["operation"] != "delegateTask" || requestDetails["agent"] != testResearcherAgentName || requestDetails["txn"] != parentTransactionID {
		t.Fatalf("request_details = %#v", requestDetails)
	}

	secretName := child.Annotations[labels.AnnotationTransactionTokenSecret]
	if secretName == "" {
		t.Fatal("expected child transaction token secret annotation")
	}
	secret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: secretName, Namespace: defaultNamespace}, secret); err != nil {
		t.Fatalf("failed to get child transaction token secret: %v", err)
	}
	if string(secret.Data["token"]) != "child-tx-token" {
		t.Fatalf("secret token = %q, want child-tx-token", string(secret.Data["token"]))
	}
}

func TestPrepareChildTransactionTokenDisabledWithoutTTSURL(t *testing.T) {
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace}}
	if err := prepareChildTransactionToken(context.Background(), newFakeClient(), parentTask(), child, "delegateTask", testResearcherAgentName); err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}
	if child.Annotations[labels.AnnotationTransactionTokenSecret] != "" {
		t.Fatalf("unexpected transaction token secret annotation: %#v", child.Annotations)
	}
}
