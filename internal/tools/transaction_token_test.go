/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aramase/kontxt/pkg/keys"
	kontxttoken "github.com/aramase/kontxt/pkg/token"
	sdkverify "github.com/aramase/kontxt/sdk/verify"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/workerenv"
)

func TestPrepareChildTransactionToken(t *testing.T) {
	subjectPath := writeTestSubjectToken(t)
	keyManager := newKontxtKeyManager(t)
	jwksServer := httptest.NewServer(keyManager.JWKSHandler())
	defer jwksServer.Close()
	childToken := newChildTransactionToken(t, keyManager)

	var exchange childTokenExchange
	ttsServer := startChildTransactionTokenServer(t, childToken, &exchange)
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenTTSAudience, "child.example.test")
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectPath)
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)
	t.Setenv(workerenv.ContextTokenChildTokenTTL, "42s")

	parent := parentTask()
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Transaction: parent.Spec.Transaction.DeepCopy(),
		},
	}
	fc := newFakeClient()
	if err := prepareChildTransactionToken(context.Background(), fc, parent, child, "delegateTask", testResearcherAgentName); err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}

	requireChildTokenExchange(t, exchange)
	secretName := requirePreparedChildTransactionToken(t, fc, parent, child, childToken, jwksServer.URL)

	child.Name = "child-task"
	child.UID = apitypes.UID("child-uid-1234")
	if err := adoptChildTransactionTokenSecret(context.Background(), fc, child); err != nil {
		t.Fatalf("adoptChildTransactionTokenSecret() error = %v", err)
	}
	requireAdoptedChildTransactionTokenSecret(t, fc, child, secretName)
}

type childTokenExchange struct {
	requestDetails     map[string]any
	audience           string
	scope              string
	subjectToken       string
	subjectTokenTyp    string
	requestedExpiresIn string
}

func writeTestSubjectToken(t *testing.T) string {
	t.Helper()

	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("parent-tx-token"), 0600); err != nil {
		t.Fatalf("failed to write subject token: %v", err)
	}
	return subjectPath
}

func newKontxtKeyManager(t *testing.T) *keys.Manager {
	t.Helper()

	keyManager, err := keys.NewManager(2048, time.Hour)
	if err != nil {
		t.Fatalf("failed to create kontxt key manager: %v", err)
	}
	return keyManager
}

func newChildTransactionToken(t *testing.T, keyManager *keys.Manager) string {
	t.Helper()

	signingKey, kid := keyManager.SigningKey()
	childToken, err := kontxttoken.New(kontxttoken.Claims{
		Issuer:             "https://tts.example.test",
		Audience:           "child.example.test",
		TransactionID:      parentTransactionID,
		Subject:            "spiffe://example.test/ns/default/sa/child",
		Scope:              childTransactionScope,
		RequestingWorkload: "spiffe://example.test/ns/default/sa/orka-worker",
		TransactionContext: map[string]any{
			"operation": "delegateTask",
			"agent":     testResearcherAgentName,
		},
	}, signingKey, kid, time.Minute)
	if err != nil {
		t.Fatalf("failed to create child TxToken: %v", err)
	}
	return childToken
}

func startChildTransactionTokenServer(t *testing.T, childToken string, exchange *childTokenExchange) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token_endpoint" {
			t.Fatalf("path = %q, want /token_endpoint", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		exchange.subjectToken = r.FormValue("subject_token")
		exchange.audience = r.FormValue("audience")
		exchange.scope = r.FormValue("scope")
		exchange.subjectTokenTyp = r.FormValue("subject_token_type")
		exchange.requestedExpiresIn = r.FormValue("requested_expires_in")
		if err := json.Unmarshal([]byte(r.FormValue("request_details")), &exchange.requestDetails); err != nil {
			t.Fatalf("request_details JSON error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":      childToken,
			"issued_token_type": "urn:ietf:params:oauth:token-type:txn_token",
			"token_type":        "N_A",
		})
	}))
}

func requireChildTokenExchange(t *testing.T, exchange childTokenExchange) {
	t.Helper()

	if exchange.subjectToken != "parent-tx-token" {
		t.Fatalf("subject_token = %q, want parent-tx-token", exchange.subjectToken)
	}
	if exchange.scope != childTransactionScope {
		t.Fatalf("scope = %q, want %q", exchange.scope, childTransactionScope)
	}
	if exchange.audience != "child.example.test" {
		t.Fatalf("audience = %q, want child.example.test", exchange.audience)
	}
	if exchange.subjectTokenTyp != kontxttoken.SubjectTokenTypeTxnToken {
		t.Fatalf("subject_token_type = %q", exchange.subjectTokenTyp)
	}
	if exchange.requestedExpiresIn != "42" {
		t.Fatalf("requested_expires_in = %q, want 42", exchange.requestedExpiresIn)
	}
	if exchange.requestDetails["operation"] != "delegateTask" || exchange.requestDetails["agent"] != testResearcherAgentName || exchange.requestDetails["txn"] != parentTransactionID {
		t.Fatalf("request_details = %#v", exchange.requestDetails)
	}
}

func requirePreparedChildTransactionToken(
	t *testing.T,
	fc client.Client,
	parent *corev1alpha1.Task,
	child *corev1alpha1.Task,
	childToken string,
	jwksURL string,
) string {
	t.Helper()

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
	claims, err := sdkverify.New(jwksURL, "child.example.test").Verify(context.Background(), secretToken)
	if err != nil {
		t.Fatalf("failed to verify child TxToken from secret: %v", err)
	}
	if claims.TransactionID != parentTransactionID {
		t.Fatalf("child token txn = %q, want %q", claims.TransactionID, parentTransactionID)
	}
	if claims.Scope != childTransactionScope {
		t.Fatalf("child token scope = %q, want %q", claims.Scope, childTransactionScope)
	}
	if child.Spec.Transaction.Scope != childTransactionScope {
		t.Fatalf("child transaction scope = %q, want %q", child.Spec.Transaction.Scope, childTransactionScope)
	}
	if got, want := child.Spec.Transaction.Scopes, []string{childTransactionScope}; !slices.Equal(got, want) {
		t.Fatalf("child transaction scopes = %#v, want %#v", got, want)
	}
	if got := child.Annotations[labels.AnnotationTransactionScope]; got != childTransactionScope {
		t.Fatalf("transaction scope annotation = %q, want %q", got, childTransactionScope)
	}
	if len(secret.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %#v, want parent task owner before child task adoption", secret.OwnerReferences)
	}
	preAdoptionOwner := secret.OwnerReferences[0]
	if preAdoptionOwner.Name != parent.Name || preAdoptionOwner.UID != parent.UID {
		t.Fatalf("ownerReference = %#v, want parent task name %q uid %q", preAdoptionOwner, parent.Name, parent.UID)
	}
	if preAdoptionOwner.BlockOwnerDeletion != nil {
		t.Fatalf("ownerReference BlockOwnerDeletion = %#v, want nil", preAdoptionOwner.BlockOwnerDeletion)
	}
	return secretName
}

func requireAdoptedChildTransactionTokenSecret(t *testing.T, fc client.Client, child *corev1alpha1.Task, secretName string) {
	t.Helper()

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
	if owner.BlockOwnerDeletion != nil {
		t.Fatalf("ownerReference BlockOwnerDeletion = %#v, want nil", owner.BlockOwnerDeletion)
	}
}

func TestCleanupChildTaskAfterTokenAdoptionFailureAttemptsSecretCleanupWhenTaskDeleteFails(t *testing.T) {
	forcedErr := errors.New("forced task delete failure")
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: defaultNamespace,
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "child-token-secret",
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-token-secret",
			Namespace: defaultNamespace,
		},
	}
	k8sClient := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok {
				return forcedErr
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, child, secret)

	cleanupChildTaskAfterTokenAdoptionFailure(context.Background(), k8sClient, child)

	gotSecret := &corev1.Secret{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: secret.Name, Namespace: secret.Namespace}, gotSecret); err == nil {
		t.Fatalf("expected child transaction token secret to be deleted despite task delete failure")
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
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)

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

func TestChildTransactionTokenSecretNameExtremeParentNames(t *testing.T) {
	tests := []struct {
		name       string
		parentName string
	}{
		{
			name:       "very long",
			parentName: strings.Repeat("parent-task-name-", 20) + "tail",
		},
		{
			name:       "sixty plus hyphens",
			parentName: strings.Repeat("-", 64),
		},
		{
			name:       "hyphen heavy",
			parentName: "----" + strings.Repeat("parent-", 40) + "----",
		},
		{
			name:       "all hyphen",
			parentName: strings.Repeat("-", 120),
		},
		{
			name:       "invalid chars uppercase unicode",
			parentName: "Parent_Task 日本語 ☃ WITH/slashes.and spaces",
		},
		{
			name:       "mixed long hyphen suffixed",
			parentName: strings.Repeat("Parent_TASK---with.invalid_chars-", 8) + strings.Repeat("-", 24),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := childTransactionTokenSecretName(tt.parentName)
			if err != nil {
				t.Fatalf("childTransactionTokenSecretName(%q) error = %v", tt.parentName, err)
			}
			if got == "" {
				t.Fatalf("childTransactionTokenSecretName(%q) returned an empty name", tt.parentName)
			}
			if len(got) > 63 {
				t.Fatalf("childTransactionTokenSecretName(%q) = %q, length %d > 63", tt.parentName, got, len(got))
			}
			if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
				t.Fatalf("childTransactionTokenSecretName(%q) = %q, not DNS-1123 label: %v", tt.parentName, got, errs)
			}
			if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
				t.Fatalf("childTransactionTokenSecretName(%q) = %q, has leading or trailing hyphen", tt.parentName, got)
			}
		})
	}
}
