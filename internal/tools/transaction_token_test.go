/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/transactiontoken"
	txtest "github.com/orka-agents/orka/internal/transactiontoken/testutil"
	"github.com/orka-agents/orka/internal/workerenv"
)

const childTransactionAudience = "child.example.test"

func TestPrepareAndCompleteChildTransactionToken(t *testing.T) {
	subjectPath := writeTestSubjectToken(t)
	issuer := newTransactionTokenIssuer(t)
	exchange := &childTokenExchange{}
	ttsServer := startChildTransactionTokenServer(t, issuer, exchange)
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSEndpoint, ttsServer.URL+"/token_endpoint")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenTTSAudience, childTransactionAudience)
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
	preparation, err := prepareChildTransactionToken(context.Background(), fc, parent, child)
	if err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}
	if preparation == nil {
		t.Fatal("prepareChildTransactionToken() returned nil preparation")
	}
	if exchange.called.Load() {
		t.Fatal("TTS exchange occurred before child task identity was assigned")
	}
	secretName := requirePreparedChildTransactionToken(t, fc, parent, child)

	child.Name = "child-task"
	child.UID = apitypes.UID("child-uid-1234")
	if err := completeChildTransactionToken(context.Background(), fc, child, preparation); err != nil {
		t.Fatalf("completeChildTransactionToken() error = %v", err)
	}
	if exchange.called.Load() {
		t.Fatal("child creation performed a one-shot exchange instead of deferring renewable setup")
	}
	requireRenewableChildTransactionSecrets(t, fc, child, secretName, "parent-tx-token")
}

func TestCompleteChildTransactionTokenDefaultsToServiceAccountSubjectToken(t *testing.T) {
	issuer := newTransactionTokenIssuer(t)
	exchange := &childTokenExchange{}
	ttsServer := startChildTransactionTokenServer(t, issuer, exchange)
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSEndpoint, ttsServer.URL+"/token_endpoint")
	t.Setenv(workerenv.ContextTokenTTSAudience, childTransactionAudience)
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)
	t.Setenv(workerenv.ContextTokenChildTokenTTL, "42s")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")

	parent := parentTask()
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: defaultNamespace,
			UID:       apitypes.UID("child-uid-1234"),
		},
		Spec: corev1alpha1.TaskSpec{
			Transaction: parent.Spec.Transaction.DeepCopy(),
		},
	}
	fc := newFakeClient()
	preparation, err := prepareChildTransactionToken(context.Background(), fc, parent, child)
	if err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}
	secretName := requirePreparedChildTransactionToken(t, fc, parent, child)
	if err := completeChildTransactionToken(context.Background(), fc, child, preparation); err != nil {
		t.Fatalf("completeChildTransactionToken() error = %v", err)
	}

	if exchange.called.Load() {
		t.Fatal("service-account child creation performed a one-shot exchange")
	}
	requireRenewableChildTransactionSecrets(t, fc, child, secretName, "service-account-token")
}

func requireRenewableChildTransactionSecrets(
	t *testing.T,
	k8sClient client.Client,
	child *corev1alpha1.Task,
	workloadName, subject string,
) {
	t.Helper()
	workload := &corev1.Secret{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: child.Namespace, Name: workloadName}, workload); err != nil {
		t.Fatalf("get child workload token Secret: %v", err)
	}
	if workload.Labels[labels.LabelPurpose] != transactiontoken.WorkloadSecretPurpose ||
		workload.Labels[labels.LabelTaskUID] != labels.SelectorValue(string(child.UID)) || len(workload.Data) != 0 {
		t.Fatal("child workload Secret contains renewal authority or invalid identity metadata")
	}
	authorities := &corev1.SecretList{}
	if err := k8sClient.List(context.Background(), authorities, client.InNamespace(child.Namespace), client.MatchingLabels{
		labels.LabelPurpose: transactiontoken.AuthoritySecretPurpose,
		labels.LabelTaskUID: labels.SelectorValue(string(child.UID)),
	}); err != nil {
		t.Fatalf("list child renewal authority: %v", err)
	}
	if len(authorities.Items) != 1 || string(authorities.Items[0].Data[transactiontoken.SubjectSecretKey]) != subject {
		t.Fatal("child controller-only renewal authority was not preserved")
	}
	for _, owner := range authorities.Items[0].OwnerReferences {
		if owner.Kind == taskKindString && owner.Name == child.Name && owner.UID == child.UID {
			return
		}
	}
	t.Fatal("child renewal authority is not owned by the child Task")
}

type childTokenExchange struct {
	called             atomic.Bool
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

func newTransactionTokenIssuer(t *testing.T) *txtest.Issuer {
	t.Helper()
	return txtest.NewIssuer(t)
}

func startChildTransactionTokenServer(t *testing.T, issuer *txtest.Issuer, exchange *childTokenExchange) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token_endpoint" {
			t.Errorf("path = %q, want /token_endpoint", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		exchange.called.Store(true)
		exchange.subjectToken = r.FormValue("subject_token")
		exchange.audience = r.FormValue("audience")
		exchange.scope = r.FormValue("scope")
		exchange.subjectTokenTyp = r.FormValue("subject_token_type")
		exchange.requestedExpiresIn = r.FormValue("requested_expires_in")
		if err := json.Unmarshal([]byte(r.FormValue("request_details")), &exchange.requestDetails); err != nil {
			t.Errorf("request_details JSON error = %v", err)
			http.Error(w, "invalid request details", http.StatusBadRequest)
			return
		}
		childToken, err := issuer.SignClaims(transactiontoken.Claims{
			Issuer:             "https://tts.example.test",
			Audience:           childTransactionAudience,
			TransactionID:      parentTransactionID,
			Subject:            "spiffe://example.test/ns/default/sa/child",
			Scope:              childTransactionScope,
			RequestingWorkload: "spiffe://example.test/ns/default/sa/orka-worker",
			TransactionContext: exchange.requestDetails,
		}, time.Minute)
		if err != nil {
			t.Errorf("sign child transaction token: %v", err)
			http.Error(w, "signing failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":      childToken,
			"issued_token_type": "urn:ietf:params:oauth:token-type:txn_token",
			"token_type":        "N_A",
		})
	}))
}

func requirePreparedChildTransactionToken(
	t *testing.T,
	fc client.Client,
	parent *corev1alpha1.Task,
	child *corev1alpha1.Task,
) string {
	t.Helper()
	secretName := child.Annotations[labels.AnnotationTransactionTokenSecret]
	if secretName == "" {
		t.Fatal("expected child transaction token secret annotation")
	}
	secret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: secretName, Namespace: child.Namespace}, secret); err != nil {
		t.Fatalf("failed to get child transaction token placeholder: %v", err)
	}
	if len(secret.Data) != 0 || len(secret.OwnerReferences) != 0 {
		t.Fatalf("placeholder data/owners = %#v/%#v, want empty and ownerless", secret.Data, secret.OwnerReferences)
	}
	expectedLabels, expectedAnnotations := childTransactionTokenPlaceholderMetadata(parent)
	if !maps.Equal(secret.Labels, expectedLabels) || !maps.Equal(secret.Annotations, expectedAnnotations) {
		t.Fatalf("placeholder metadata = %#v/%#v, want %#v/%#v", secret.Labels, secret.Annotations, expectedLabels, expectedAnnotations)
	}
	if got := child.Annotations[transactiontoken.PlaceholderUIDAnnotation]; got != string(secret.UID) {
		t.Fatalf("placeholder UID annotation = %q, want %q", got, secret.UID)
	}
	if child.Spec.Transaction.Scope != childTransactionScope {
		t.Fatalf("child transaction scope = %q, want %q", child.Spec.Transaction.Scope, childTransactionScope)
	}
	if got, want := child.Spec.Transaction.Scopes, []string{childTransactionScope}; !slices.Equal(got, want) {
		t.Fatalf("child transaction scopes = %#v, want %#v", got, want)
	}
	return secretName
}

func TestPrepareChildTransactionTokenRequiresParentUID(t *testing.T) {
	var called atomic.Bool
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		http.Error(w, "unexpected TTS call", http.StatusInternalServerError)
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSEndpoint, ttsServer.URL+"/token_endpoint")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, writeTestSubjectToken(t))
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)

	parent := parentTask()
	parent.UID = ""
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Transaction: parent.Spec.Transaction.DeepCopy(),
		},
	}
	k8sClient := newFakeClient()

	_, err := prepareChildTransactionToken(context.Background(), k8sClient, parent, child)
	if err == nil || !strings.Contains(err.Error(), "parent task identity is required") {
		t.Fatalf("prepareChildTransactionToken() error = %v, want parent UID error", err)
	}
	if called.Load() {
		t.Fatal("TTS was called despite missing parent UID")
	}
	if child.Annotations[labels.AnnotationTransactionTokenSecret] != "" {
		t.Fatalf("unexpected child transaction token secret annotation: %#v", child.Annotations)
	}
	secrets := &corev1.SecretList{}
	if err := k8sClient.List(context.Background(), secrets, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatalf("failed to list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("unexpected secrets created without parent UID: %#v", secrets.Items)
	}
}

func TestCompleteChildTransactionTokenRequiresChildUID(t *testing.T) {
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child-task", Namespace: defaultNamespace}}
	err := completeChildTransactionToken(context.Background(), newFakeClient(), child, &childTransactionTokenPreparation{
		subjectToken: "subject", subjectTokenType: transactiontoken.SubjectTokenTypeTransactionToken,
	})
	if err == nil || !strings.Contains(err.Error(), "child task identity is required") {
		t.Fatalf("completeChildTransactionToken() error = %v, want child identity error", err)
	}
}

func TestCleanupChildTransactionTokenSecretOnlyDeletesValidatedPlaceholder(t *testing.T) {
	parent := parentTask()
	prep := &childTransactionTokenPreparation{
		parentName: parent.Name, parentNamespace: parent.Namespace, parentUID: string(parent.UID),
		placeholderUID: apitypes.UID("prepared-secret-uid"),
	}
	placeholderLabels, placeholderAnnotations := childTransactionTokenPlaceholderMetadata(parent)
	placeholder := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "prepared-child-token-secret", Namespace: defaultNamespace, UID: apitypes.UID("prepared-secret-uid"),
		Labels: placeholderLabels, Annotations: placeholderAnnotations,
	}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: defaultNamespace}}
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: defaultNamespace, Annotations: map[string]string{
		labels.AnnotationTransactionTokenSecret: placeholder.Name,
	}}}
	fc := newFakeClient(placeholder, unrelated)
	cleanupChildTaskAfterTokenAdoptionFailure(context.Background(), fc, child, prep)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(placeholder), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("validated placeholder still exists: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(unrelated), &corev1.Secret{}); err != nil {
		t.Fatalf("unrelated Secret was deleted: %v", err)
	}
}

func TestCleanupChildTaskAfterTokenAdoptionFailureAttemptsSecretCleanupWhenTaskDeleteFails(t *testing.T) {
	parent := parentTask()
	prep := &childTransactionTokenPreparation{
		parentName: parent.Name, parentNamespace: parent.Namespace, parentUID: string(parent.UID),
		placeholderUID: apitypes.UID("secret-uid"),
	}
	placeholderLabels, placeholderAnnotations := childTransactionTokenPlaceholderMetadata(parent)
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "child-task", Namespace: defaultNamespace, UID: apitypes.UID("child-uid"),
		Annotations: map[string]string{labels.AnnotationTransactionTokenSecret: "child-token-secret"},
	}}
	placeholder := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "child-token-secret", Namespace: defaultNamespace, UID: apitypes.UID("secret-uid"),
		Labels: placeholderLabels, Annotations: placeholderAnnotations,
	}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	fc := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok {
				return errors.New("forced task delete failure")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, child, placeholder)
	cleanupChildTaskAfterTokenAdoptionFailure(context.Background(), fc, child, prep)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(placeholder), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("validated placeholder still exists: %v", err)
	}
}

func TestCompleteChildTransactionTokenFailsClosedOnTTSExchangeError(t *testing.T) {
	t.Setenv(workerenv.ContextTokenTTSEndpoint, "https://transactions.example.test/token")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, writeTestSubjectToken(t))
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)
	parent := parentTask()
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child-task", Namespace: defaultNamespace, UID: apitypes.UID("child-uid-1234")}, Spec: corev1alpha1.TaskSpec{Transaction: parent.Spec.Transaction.DeepCopy()}}
	k8sClient := newFakeClient()
	preparation, err := prepareChildTransactionToken(context.Background(), k8sClient, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	secretName := requirePreparedChildTransactionToken(t, k8sClient, parent, child)
	if err := completeChildTransactionToken(context.Background(), k8sClient, child, preparation); err != nil {
		t.Fatal(err)
	}
	requireRenewableChildTransactionSecrets(t, k8sClient, child, secretName, "parent-tx-token")
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
	t.Setenv(workerenv.ContextTokenTTSEndpoint, ttsServer.URL+"/token_endpoint")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectPath)
	t.Setenv(workerenv.ContextTokenChildScope, "orka:admin")

	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace}}
	_, err := prepareChildTransactionToken(context.Background(), newFakeClient(), parentTask(), child)
	if err == nil || !strings.Contains(err.Error(), "not present in parent") {
		t.Fatalf("prepareChildTransactionToken() error = %v, want scope expansion error", err)
	}
}

func TestPrepareChildTransactionTokenDisabledWithoutTTSURL(t *testing.T) {
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace}}
	preparation, err := prepareChildTransactionToken(context.Background(), newFakeClient(), parentTask(), child)
	if err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}
	if preparation != nil {
		t.Fatalf("prepareChildTransactionToken() = %#v, want nil when TTS is disabled", preparation)
	}
	if child.Annotations[labels.AnnotationTransactionTokenSecret] != "" {
		t.Fatalf("unexpected transaction token secret annotation: %#v", child.Annotations)
	}
}

func TestPrepareChildTransactionTokenDisabledForNonTransactionalParent(t *testing.T) {
	var called atomic.Bool
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		http.Error(w, "unexpected TTS call", http.StatusInternalServerError)
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSEndpoint, ttsServer.URL+"/token_endpoint")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)

	parent := parentTask()
	parent.Spec.Transaction = nil
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: defaultNamespace,
		},
	}
	k8sClient := newFakeClient()

	preparation, err := prepareChildTransactionToken(context.Background(), k8sClient, parent, child)
	if err != nil {
		t.Fatalf("prepareChildTransactionToken() error = %v", err)
	}
	if preparation != nil {
		t.Fatalf("prepareChildTransactionToken() = %#v, want nil for non-transactional parent", preparation)
	}
	if called.Load() {
		t.Fatal("TTS was called for non-transactional parent task")
	}
	if child.Annotations[labels.AnnotationTransactionTokenSecret] != "" {
		t.Fatalf("unexpected child transaction token secret annotation: %#v", child.Annotations)
	}
	secrets := &corev1.SecretList{}
	if err := k8sClient.List(context.Background(), secrets, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatalf("failed to list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("unexpected child transaction token secrets: %#v", secrets.Items)
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

func TestCrossNamespaceChildTransactionTokenPlaceholderAdoption(t *testing.T) {
	t.Setenv(workerenv.ContextTokenTTSEndpoint, "https://transactions.example.test/token")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, writeTestSubjectToken(t))
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)
	parent := parentTask()
	parent.Namespace = "parents"
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "children", UID: apitypes.UID("cross-child-uid")}, Spec: corev1alpha1.TaskSpec{
		Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "worker"}, Transaction: parent.Spec.Transaction.DeepCopy(),
	}}
	fc := newFakeClient()
	prep, err := prepareChildTransactionToken(context.Background(), fc, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	name := requirePreparedChildTransactionToken(t, fc, parent, child)
	if err := completeChildTransactionToken(context.Background(), fc, child, prep); err != nil {
		t.Fatal(err)
	}
	requireRenewableChildTransactionSecrets(t, fc, child, name, "parent-tx-token")
	authorities := &corev1.SecretList{}
	if err := fc.List(context.Background(), authorities, client.InNamespace(child.Namespace), client.MatchingLabels{labels.LabelPurpose: transactiontoken.AuthoritySecretPurpose}); err != nil {
		t.Fatal(err)
	}
	if got := string(authorities.Items[0].Data[transactiontoken.SubjectTokenTypeSecretKey]); got != transactiontoken.SubjectTokenTypeTransactionToken {
		t.Fatalf("persisted subject type = %q", got)
	}
	var details map[string]any
	if err := json.Unmarshal(authorities.Items[0].Data[transactiontoken.RequestDetailsSecretKey], &details); err != nil || details["parentTask"] != parent.Name || details["taskUID"] != string(child.UID) {
		t.Fatalf("persisted request details = %#v, err=%v", details, err)
	}
}

func TestChildTransactionTokenPlaceholderReplacementRaceFailsClosed(t *testing.T) {
	t.Setenv(workerenv.ContextTokenTTSEndpoint, "https://transactions.example.test/token")
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, writeTestSubjectToken(t))
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)
	parent := parentTask()
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "children", UID: apitypes.UID("race-child-uid")}, Spec: corev1alpha1.TaskSpec{Transaction: parent.Spec.Transaction.DeepCopy()}}
	fc := newFakeClient()
	prep, err := prepareChildTransactionToken(context.Background(), fc, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	name := child.Annotations[labels.AnnotationTransactionTokenSecret]
	original := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: child.Namespace, Name: name}, original); err != nil {
		t.Fatal(err)
	}
	if err := fc.Delete(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	replacement := original.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = apitypes.UID("replacement-placeholder-uid")
	replacement.OwnerReferences = nil
	if err := fc.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := completeChildTransactionToken(context.Background(), fc, child, prep); err == nil || !strings.Contains(err.Error(), "placeholder identity is invalid") {
		t.Fatalf("complete error = %v", err)
	}
	cleanupChildTaskAfterTokenAdoptionFailure(context.Background(), fc, child, prep)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(replacement), &corev1.Secret{}); err != nil {
		t.Fatalf("replacement Secret was deleted: %v", err)
	}
}
