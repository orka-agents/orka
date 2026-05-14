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
	"strings"
	"testing"
	"time"

	"github.com/aramase/kontxt/pkg/keys"
	kontxttoken "github.com/aramase/kontxt/pkg/token"
	sdkverify "github.com/aramase/kontxt/sdk/verify"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
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

	keyManager, err := keys.NewManager(2048, time.Hour)
	if err != nil {
		t.Fatalf("failed to create kontxt key manager: %v", err)
	}
	jwksServer := httptest.NewServer(keyManager.JWKSHandler())
	defer jwksServer.Close()
	signingKey, kid := keyManager.SigningKey()
	childToken, err := kontxttoken.New(kontxttoken.Claims{
		Issuer:             "https://tts.example.test",
		Audience:           "child.example.test",
		TransactionID:      parentTransactionID,
		Subject:            "spiffe://example.test/ns/default/sa/child",
		Scope:              "orka:agents:run",
		RequestingWorkload: "spiffe://example.test/ns/default/sa/orka-worker",
		TransactionContext: map[string]any{
			"operation": "delegateTask",
			"agent":     testResearcherAgentName,
		},
	}, signingKey, kid, time.Minute)
	if err != nil {
		t.Fatalf("failed to create child TxToken: %v", err)
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
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":      childToken,
			"issued_token_type": "urn:ietf:params:oauth:token-type:txn_token",
			"token_type":        "N_A",
		})
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
	secretToken := string(secret.Data["token"])
	if secretToken != childToken {
		t.Fatalf("secret token did not contain child TxToken returned by TTS")
	}
	claims, err := sdkverify.New(jwksServer.URL, "child.example.test").Verify(context.Background(), secretToken)
	if err != nil {
		t.Fatalf("failed to verify child TxToken from secret: %v", err)
	}
	if claims.TransactionID != parentTransactionID {
		t.Fatalf("child token txn = %q, want %q", claims.TransactionID, parentTransactionID)
	}
	if claims.Scope != "orka:agents:run" {
		t.Fatalf("child token scope = %q, want orka:agents:run", claims.Scope)
	}
	if len(secret.OwnerReferences) != 0 {
		t.Fatalf("ownerReferences = %#v, want no owner before child task adoption", secret.OwnerReferences)
	}

	child.Name = "child-task"
	child.UID = apitypes.UID("child-uid-1234")
	if err := adoptChildTransactionTokenSecret(context.Background(), fc, child); err != nil {
		t.Fatalf("adoptChildTransactionTokenSecret() error = %v", err)
	}
	adoptedSecret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: secretName, Namespace: defaultNamespace}, adoptedSecret); err != nil {
		t.Fatalf("failed to get adopted child transaction token secret: %v", err)
	}
	if len(adoptedSecret.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %#v, want child task owner", adoptedSecret.OwnerReferences)
	}
	owner := adoptedSecret.OwnerReferences[0]
	if owner.Name != child.Name || owner.UID != child.UID {
		t.Fatalf("ownerReference = %#v, want child task name %q uid %q", owner, child.Name, child.UID)
	}
	if owner.Name == parentTaskName {
		t.Fatalf("ownerReference = %#v, want child task owner not parent task", owner)
	}
}

func TestPrepareChildTransactionTokenFailsClosedOnTTSExchangeError(t *testing.T) {
	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("parent-tx-token"), 0600); err != nil {
		t.Fatalf("failed to write subject token: %v", err)
	}
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable","error_description":"maintenance"}`))
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectPath)
	t.Setenv(workerenv.ContextTokenChildScope, "orka:agents:run")

	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace}}
	err := prepareChildTransactionToken(context.Background(), newFakeClient(), parentTask(), child, "delegateTask", testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), "exchanging child transaction token") || !strings.Contains(err.Error(), "temporarily_unavailable") {
		t.Fatalf("prepareChildTransactionToken() error = %v, want TTS exchange failure", err)
	}
	if child.Annotations[labels.AnnotationTransactionTokenSecret] != "" {
		t.Fatalf("unexpected child transaction token secret annotation after failed exchange: %#v", child.Annotations)
	}
}

func TestPrepareChildTransactionTokenRejectsScopeExpansion(t *testing.T) {
	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("parent-tx-token"), 0600); err != nil {
		t.Fatalf("failed to write subject token: %v", err)
	}
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("TTS should not be called when child scope exceeds parent")
	}))
	defer ttsServer.Close()
	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectPath)
	t.Setenv(workerenv.ContextTokenChildScope, "orka:admin")

	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace}}
	err := prepareChildTransactionToken(context.Background(), newFakeClient(), parentTask(), child, "delegateTask", testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), "not present in parent") {
		t.Fatalf("prepareChildTransactionToken() error = %v, want scope expansion error", err)
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
