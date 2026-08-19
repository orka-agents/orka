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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

func TestPublisherCredentialBrokerUsesTargetReadBindingForContinuation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publisherBearer := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-broker-auth", []byte(publisherBearer))
	taskUID := types.UID("continuation-credential-task-uid")
	plannedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "continuation", UID: taskUID},
		Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{
			ReadCredentialRef:            &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"},
			PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "target-read"},
		}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStatePlanned, Attempt: 1, PromptID: "prompt",
			ReadCredentialResourceVersion: "7", PublicationReadCredentialResourceVersion: "9",
		}},
	}
	const targetMaterial = "target-test-value"
	targetK8sObject := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "target-read", UID: "target-object-uid", ResourceVersion: "9"},
		Data:       map[string][]byte{defaultWorkspaceCredentialKey: []byte(targetMaterial)},
	}
	attempt := &corev1alpha1.PromptAttempt{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"}, Spec: corev1alpha1.PromptAttemptSpec{
		TaskUID: string(taskUID), Attempt: 1, PromptID: "prompt",
		CredentialBindings: []corev1alpha1.PromptCredentialBinding{
			{Role: "SourceRead", Namespace: "default", SecretName: "source-read", SecretKey: defaultWorkspaceCredentialKey, SecretUID: "source-object-uid", ResourceVersion: "7"},
			{Role: "TargetRead", Namespace: "default", SecretName: "target-read", SecretKey: defaultWorkspaceCredentialKey, SecretUID: "target-object-uid", ResourceVersion: "9"},
		},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		plannedTask, targetK8sObject, attempt,
		publisherEffectForTest("workspace-continuation-credential-effect", "workspace.prepare", string(taskUID), "workspace-prepare-prompt"),
	).Build()
	request := publisherservice.CredentialMaterialRequest{
		ParentOperation: publisherservice.OperationWorkspacePrepare,
		Metadata:        publisherservice.OperationMetadata{Namespace: "default", TaskID: string(taskUID), OperationID: "workspace-prepare-prompt"},
		Reference: publisherservice.CredentialReference{
			Name: "target-read", Kind: publisherservice.CredentialHTTPExtraHeader, Role: publisherservice.CredentialRoleTargetRead,
		},
	}
	response := callPublisherCredentialBroker(t, kubeClient, publisherBearer, request)
	expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+targetMaterial))
	if response.Material != expected || response.ResourceVersion != "9" {
		t.Fatalf("credential response = %#v", response)
	}
}

func TestPublisherCredentialBrokerUsesTargetReadForContinuationPublicationPrepare(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publisherBearer := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-broker-auth", []byte(publisherBearer))
	taskUID := types.UID("continuation-publication-task-uid")
	plannedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "continuation-publication", UID: taskUID},
		Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{
			ReadCredentialRef:            &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"},
			PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "target-read"},
		}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Attempt: 1, PromptID: "prompt",
			ReadCredentialResourceVersion: "7", PublicationReadCredentialResourceVersion: "9",
		}},
	}
	publicationID := controller.ACPPublicationIDForTask(plannedTask)
	publication := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "continuation-publication-record"},
		Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(taskUID)},
		Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState("Preparing")},
	}
	const targetMaterial = "target-test-value"
	targetK8sObject := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "target-read", UID: "target-object-uid", ResourceVersion: "9"},
		Data:       map[string][]byte{defaultWorkspaceCredentialKey: []byte(targetMaterial)},
	}
	attempt := &corev1alpha1.PromptAttempt{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication-attempt"}, Spec: corev1alpha1.PromptAttemptSpec{
		TaskUID: string(taskUID), Attempt: 1, PromptID: "prompt",
		CredentialBindings: []corev1alpha1.PromptCredentialBinding{
			{Role: "SourceRead", Namespace: "default", SecretName: "source-read", SecretKey: defaultWorkspaceCredentialKey, SecretUID: "source-object-uid", ResourceVersion: "7"},
			{Role: "TargetRead", Namespace: "default", SecretName: "target-read", SecretKey: defaultWorkspaceCredentialKey, SecretUID: "target-object-uid", ResourceVersion: "9"},
		},
	}}
	operationID := controller.ACPPublicationOperationID("prepare", plannedTask)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		plannedTask, publication, targetK8sObject, attempt,
		publisherEffectForTest("publication-continuation-credential-effect", "publisher.prepare", publicationID, operationID),
	).Build()
	request := publisherservice.CredentialMaterialRequest{
		ParentOperation: publisherservice.OperationPublicationPrepare,
		Metadata:        publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: operationID},
		Reference: publisherservice.CredentialReference{
			Name: "target-read", Kind: publisherservice.CredentialHTTPExtraHeader, Role: publisherservice.CredentialRoleTargetRead,
		},
	}
	response := callPublisherCredentialBroker(t, kubeClient, publisherBearer, request)
	expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+targetMaterial))
	if response.Material != expected || response.ResourceVersion != "9" {
		t.Fatalf("credential response = %#v", response)
	}
}

func TestPublisherCredentialBrokerUsesUncachedReaderForCredentialVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publisherBearer := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-broker-auth", []byte(publisherBearer))
	taskUID := types.UID("rotated-credential-task-uid")
	plannedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rotated", UID: taskUID},
		Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{
			ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "workspace-read"},
		}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStatePlanned, Attempt: 1, PromptID: "prompt", ReadCredentialResourceVersion: "7",
		}},
	}
	staleK8sObject := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "workspace-read", UID: "workspace-object-uid", ResourceVersion: "7"},
		Data:       map[string][]byte{defaultWorkspaceCredentialKey: []byte("stale-test-value")},
	}
	rotatedK8sObject := staleK8sObject.DeepCopy()
	rotatedK8sObject.ResourceVersion = "8"
	rotatedK8sObject.Data[defaultWorkspaceCredentialKey] = []byte("rotated-test-value")
	attempt := &corev1alpha1.PromptAttempt{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rotated-attempt"}, Spec: corev1alpha1.PromptAttemptSpec{
		TaskUID: string(taskUID), Attempt: 1, PromptID: "prompt",
		CredentialBindings: []corev1alpha1.PromptCredentialBinding{
			{Role: "SourceRead", Namespace: "default", SecretName: "workspace-read", SecretKey: defaultWorkspaceCredentialKey, SecretUID: "workspace-object-uid", ResourceVersion: "7"},
		},
	}}
	effect := publisherEffectForTest(
		"workspace-rotated-credential-effect", "workspace.prepare", string(taskUID), "workspace-prepare-prompt",
	)
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		plannedTask, staleK8sObject, attempt, effect,
	).Build()
	uncachedReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		plannedTask.DeepCopy(), rotatedK8sObject, attempt.DeepCopy(), effect.DeepCopy(),
	).Build()
	request := publisherservice.CredentialMaterialRequest{
		ParentOperation: publisherservice.OperationWorkspacePrepare,
		Metadata:        publisherservice.OperationMetadata{Namespace: "default", TaskID: string(taskUID), OperationID: "workspace-prepare-prompt"},
		Reference: publisherservice.CredentialReference{
			Name: "workspace-read", Kind: publisherservice.CredentialHTTPExtraHeader, Role: publisherservice.CredentialRoleSourceRead,
		},
	}

	app := fiber.New()
	server := &Server{
		app: app, client: cachedClient,
		config: ServerConfig{APIReader: uncachedReader, ControllerEpochs: publisherEpochSourceForTest()},
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
	if httpResponse.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want %d", httpResponse.StatusCode, http.StatusGone)
	}
}
