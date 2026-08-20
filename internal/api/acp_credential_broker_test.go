package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

func TestPublisherCredentialBrokerReturnsOnlyFrozenOperationCredential(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-token", []byte(publisherToken))
	taskUID := types.UID("credential-task-uid")
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent:                   corev1alpha1.WorkspaceIntentWrite,
		ReadCredentialRef:        &corev1alpha1.WorkspaceCredentialReference{Name: "git-read", Key: "token"},
		PublicationCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "git-publish", Key: "token"},
	}
	plannedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "planned", UID: taskUID},
		Spec:       corev1alpha1.TaskSpec{Workspace: workspace},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStatePlanned, Attempt: 1, PromptID: "prompt", ReadCredentialResourceVersion: "7",
		}},
	}
	readSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "git-read", UID: "read-secret-uid", ResourceVersion: "7"}, Data: map[string][]byte{"token": []byte("read-canary-token")}}
	attempt := &corev1alpha1.PromptAttempt{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"}, Spec: corev1alpha1.PromptAttemptSpec{
		TaskUID: string(taskUID), Attempt: 1, PromptID: "prompt",
		CredentialBindings: []corev1alpha1.PromptCredentialBinding{{Role: "SourceRead", Namespace: "default", SecretName: "git-read", SecretKey: "token", SecretUID: "read-secret-uid", ResourceVersion: "7"}},
	}}
	effect := publisherEffectForTest("workspace-credential-effect", "workspace.prepare", string(taskUID), "workspace-prepare-prompt")
	request := publisherservice.CredentialMaterialRequest{
		ParentOperation: publisherservice.OperationWorkspacePrepare,
		Metadata:        publisherservice.OperationMetadata{Namespace: "default", TaskID: string(taskUID), OperationID: "workspace-prepare-prompt"},
		Reference:       publisherservice.CredentialReference{Name: "git-read", Kind: publisherservice.CredentialHTTPExtraHeader},
	}
	expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:read-canary-token"))
	for _, state := range []corev1alpha1.TaskExecutionState{
		corev1alpha1.TaskExecutionStateQueued,
		corev1alpha1.TaskExecutionStateReserved,
		corev1alpha1.TaskExecutionStatePlanned,
		corev1alpha1.TaskExecutionStateSessionStarting,
	} {
		t.Run(string(state), func(t *testing.T) {
			task := plannedTask.DeepCopy()
			task.Status.Execution.State = state
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, readSecret.DeepCopy(), attempt.DeepCopy(), effect.DeepCopy()).Build()
			response := callPublisherCredentialBroker(t, kubeClient, publisherToken, request, effect)
			if response.Material != expected || response.ResourceVersion != "7" {
				t.Fatalf("credential response = %#v", response)
			}
		})
	}
}

func TestPublisherCredentialBrokerBindsPublicationAndForgePurpose(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-token", []byte(publisherToken))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "settled", UID: types.UID("settled-task-uid")},
		Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{
			Intent:                   corev1alpha1.WorkspaceIntentWrite,
			PublicationCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "git-publish"},
			ForgeCredentialRef:       &corev1alpha1.WorkspaceCredentialReference{Name: "git-publish"},
		}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			Attempt: 1, PromptID: "prompt", PublicationCredentialResourceVersion: "11", ForgeCredentialResourceVersion: "11",
		}},
	}
	publicationID := controller.ACPPublicationIDForTask(task)
	publication := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication"},
		Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(task.UID)},
		Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState("Verifying")},
	}
	publishSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "git-publish", UID: "publish-secret-uid", ResourceVersion: "11"}, Data: map[string][]byte{"token": []byte("forge-canary-token")}}
	attempt := &corev1alpha1.PromptAttempt{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"}, Spec: corev1alpha1.PromptAttemptSpec{
		TaskUID: string(task.UID), Attempt: 1, PromptID: "prompt",
		CredentialBindings: []corev1alpha1.PromptCredentialBinding{{Role: "Forge", Namespace: "default", SecretName: "git-publish", SecretKey: "token", SecretUID: "publish-secret-uid", ResourceVersion: "11"}},
	}}
	prOperation := controller.ACPPublicationOperationID("pr-reconcile", task)
	effect := publisherEffectForTest("forge-credential-effect", "publisher.pull-request", publicationID, prOperation)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, publication, publishSecret, attempt, effect).Build()
	request := publisherservice.CredentialMaterialRequest{
		ParentOperation: publisherservice.OperationPullRequestReconcile,
		Metadata:        publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: prOperation},
		Reference:       publisherservice.CredentialReference{Name: "git-publish", Kind: publisherservice.CredentialForgeToken},
	}
	response := callPublisherCredentialBroker(t, kubeClient, publisherToken, request, effect)
	if response.Material != "forge-canary-token" || response.ResourceVersion != "11" {
		t.Fatalf("credential response = %#v", response)
	}
}

func TestPublisherCredentialBrokerUsesFreshSucceededTaskAndPromptAttemptState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publisherBearer := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-broker-auth", []byte(publisherBearer))

	taskUID := types.UID("fresh-credential-task-uid")
	freshTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-task", UID: taskUID},
		Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{
			PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "git-publish-read"},
		}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			Attempt: 1, PromptID: "fresh-prompt", PublicationReadCredentialResourceVersion: "13",
		}},
	}
	staleTask := freshTask.DeepCopy()
	staleTask.Status.Execution.State = corev1alpha1.TaskExecutionStateRunning

	credentialValue := "fixture-value"
	publicationID := controller.ACPPublicationIDForTask(freshTask)
	operationID := controller.ACPPublicationOperationID("preflight", freshTask)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "git-publish-read", UID: "credential-object-uid", ResourceVersion: "13",
		},
		Data: map[string][]byte{"token": []byte(credentialValue)},
	}
	attempt := &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fresh-attempt"},
		Spec: corev1alpha1.PromptAttemptSpec{
			TaskUID: string(taskUID), Attempt: 1, PromptID: "fresh-prompt",
			CredentialBindings: []corev1alpha1.PromptCredentialBinding{{
				Role: "TargetRead", Namespace: "default", SecretName: "git-publish-read", SecretKey: "token",
				SecretUID: "credential-object-uid", ResourceVersion: "13",
			}},
		},
	}
	effect := publisherEffectForTest(
		"fresh-preflight-credential-effect", "publisher.preflight", publicationID, operationID,
	)
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		staleTask, secret.DeepCopy(), effect.DeepCopy(),
	).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		freshTask, secret, attempt, effect,
	).Build()
	request := publisherservice.CredentialMaterialRequest{
		ParentOperation: publisherservice.OperationPublicationPreflight,
		Metadata: publisherservice.OperationMetadata{
			Namespace: "default", PublicationID: publicationID, OperationID: operationID,
		},
		Reference: publisherservice.CredentialReference{
			Name: "git-publish-read", Kind: publisherservice.CredentialHTTPExtraHeader,
		},
	}

	app := fiber.New()
	server := &Server{
		app: app, client: cachedClient,
		config: ServerConfig{
			APIReader: apiReader, ControllerEpochs: publisherEpochSourceForTest(),
			ExternalEffects: publisherEffectReaderForTest(effect),
		},
	}
	server.installACPArtifactAuthorizationBroker()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, publisherservice.CredentialBrokerPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+publisherBearer)
	httpResponse, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", httpResponse.StatusCode, http.StatusOK)
	}
	var response publisherservice.CredentialMaterialResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+credentialValue))
	if response.Material != expected || response.ResourceVersion != "13" {
		t.Fatalf("credential response = %#v", response)
	}
}

func callPublisherCredentialBroker(
	t *testing.T,
	kubeClient client.Client,
	publisherToken string,
	request publisherservice.CredentialMaterialRequest,
	effect *corev1alpha1.ExternalEffect,
) publisherservice.CredentialMaterialResponse {
	t.Helper()
	app := fiber.New()
	server := &Server{
		app: app, client: kubeClient,
		config: ServerConfig{
			ControllerEpochs: publisherEpochSourceForTest(), ExternalEffects: publisherEffectReaderForTest(effect),
		},
	}
	server.installACPArtifactAuthorizationBroker()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, publisherservice.CredentialBrokerPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+publisherToken)
	httpResponse, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", httpResponse.StatusCode)
	}
	var response publisherservice.CredentialMaterialResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestFormatPublisherCredentialForGitAndForge(t *testing.T) {
	basicValue := base64.StdEncoding.EncodeToString([]byte("custom-user:custom-token"))
	tests := []struct {
		name    string
		kind    publisherservice.CredentialKind
		value   string
		want    string
		wantErr bool
	}{
		{name: "raw git token uses x-access-token basic auth", kind: publisherservice.CredentialHTTPExtraHeader, value: "raw-token", want: "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:raw-token"))},
		{name: "explicit bearer header is preserved", kind: publisherservice.CredentialHTTPExtraHeader, value: "Authorization: Bearer explicit-token", want: "Authorization: Bearer explicit-token"},
		{name: "explicit basic header is preserved", kind: publisherservice.CredentialHTTPExtraHeader, value: "Authorization: Basic " + basicValue, want: "Authorization: Basic " + basicValue},
		{name: "raw forge token stays raw", kind: publisherservice.CredentialForgeToken, value: "forge-token", want: "forge-token"},
		{name: "bearer forge header is unwrapped", kind: publisherservice.CredentialForgeToken, value: "Authorization: Bearer forge-token", want: "forge-token"},
		{name: "basic forge header is rejected", kind: publisherservice.CredentialForgeToken, value: "Authorization: Basic " + basicValue, wantErr: true},
		{name: "malformed basic header is rejected", kind: publisherservice.CredentialHTTPExtraHeader, value: "Authorization: Basic not-base64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := formatPublisherCredential(tt.kind, []byte(tt.value))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("formatPublisherCredential() = %q, want error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("formatPublisherCredential() = %q, %v, want %q, nil", got, err, tt.want)
			}
		})
	}
}
