package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	e2eFaultTestNamespace        = "default"
	e2eFaultTestRuntimeNamespace = "orka-runtimes"
	e2eFaultTestTaskUID          = "task-uid"
	e2eFaultTestPromptID         = "prompt-1"
	e2eFaultTestRuntimeOne       = "runtime-1"
	e2eFaultTestSessionUID       = "session-uid"
)

func TestACPE2EPromptWriteFaultRecordSurvivesRuntimeAndServerReplacement(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	poolUID := types.UID("pool-uid")
	secretUID := types.UID("pool-auth-secret-uid")
	secretName := "pool-auth-e1-" + strings.Repeat("a", 24)
	profileDigest := harnessv2.ProfileDigest("sha256:" + strings.Repeat("b", 64))
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: e2eFaultTestNamespace, Name: "direct-workspace-pool", UID: poolUID, Generation: 1,
			Annotations: map[string]string{runtimePoolPrivateAuthBindingAPI + "1": secretName + "/" + string(secretUID)},
		},
		Spec: corev1alpha1.RuntimePoolSpec{
			ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: corev1alpha1.WorkspaceProviderAgentSandbox},
		},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: e2eFaultTestActiveInstance(e2eFaultTestRuntimeOne, "boot-1", profileDigest)},
	}
	immutable := true
	controllerToken := strings.Repeat("t", 32)
	capabilitySecret := []byte(strings.Repeat("s", 32))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: e2eFaultTestRuntimeNamespace, Name: secretName, UID: secretUID,
			Labels: map[string]string{
				runtimePoolAuthLabelAPI:            runtimePoolAuthLabelValueAPI,
				runtimePoolUIDLabelAPI:             string(poolUID),
				runtimePoolCredentialEpochLabelAPI: "1",
			},
		},
		Immutable: &immutable,
		Data: map[string][]byte{
			runtimePoolControllerTokenKeyAPI:  []byte(controllerToken),
			runtimePoolCapabilitySecretKeyAPI: capabilitySecret,
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: "ambiguous-task", UID: e2eFaultTestTaskUID},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSubmitting, Attempt: 1, PromptID: e2eFaultTestPromptID,
			RuntimeInstanceID: e2eFaultTestRuntimeOne, RuntimeSessionUID: e2eFaultTestSessionUID, RuntimeSessionGeneration: 1,
		}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, secret, task).Build()
	operationID := harnessv2.OperationID("start-prompt-retry-1-prompt-1")

	firstApp := fiber.New()
	firstServer := &Server{
		app: firstApp, client: kubeClient,
		config: ServerConfig{E2EPromptFaultEnabled: true},
	}
	firstServer.installACPE2EPromptWriteFaultRecorder()
	performE2EFaultRecordRequest(
		t, firstApp, pool, e2eFaultTestActiveInstance(e2eFaultTestRuntimeOne, "boot-1", profileDigest),
		controllerToken, capabilitySecret, operationID, 2, http.StatusForbidden,
	)
	var records corev1.ConfigMapList
	if err := kubeClient.List(t.Context(), &records, client.InNamespace(pool.Namespace), client.MatchingLabels{e2ePromptWriteFaultLabel: e2ePromptWriteFaultLabelValue}); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 0 {
		t.Fatalf("fault records after stale attempt = %d, want 0", len(records.Items))
	}
	first := performE2EFaultRecordRequest(
		t, firstApp, pool, e2eFaultTestActiveInstance(e2eFaultTestRuntimeOne, "boot-1", profileDigest),
		controllerToken, capabilitySecret, operationID, 1, http.StatusOK,
	)
	if !first.Inject {
		t.Fatal("first runtime did not consume the prompt-write fault")
	}

	records = corev1.ConfigMapList{}
	if err := kubeClient.List(t.Context(), &records, client.InNamespace(pool.Namespace), client.MatchingLabels{e2ePromptWriteFaultLabel: e2ePromptWriteFaultLabelValue}); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 1 {
		t.Fatalf("fault records = %d, want 1", len(records.Items))
	}
	operationDigest := sha256.Sum256([]byte(operationID))
	if records.Items[0].Data["operationDigest"] != "sha256:"+hex.EncodeToString(operationDigest[:]) ||
		strings.Contains(records.Items[0].Name, string(operationID)) {
		t.Fatalf("fault record = name %q data %#v", records.Items[0].Name, records.Items[0].Data)
	}

	currentPool := &corev1alpha1.RuntimePool{}
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(pool), currentPool); err != nil {
		t.Fatal(err)
	}
	replacement := e2eFaultTestActiveInstance("runtime-2", "boot-2", profileDigest)
	currentPool.Status.ActiveInstance = replacement
	if err := kubeClient.Update(t.Context(), currentPool); err != nil {
		t.Fatal(err)
	}
	currentTask := &corev1alpha1.Task{}
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(task), currentTask); err != nil {
		t.Fatal(err)
	}
	currentTask.Status.Execution.RuntimeInstanceID = replacement.RuntimeInstanceID
	if err := kubeClient.Update(t.Context(), currentTask); err != nil {
		t.Fatal(err)
	}

	secondApp := fiber.New()
	secondServer := &Server{
		app: secondApp, client: kubeClient,
		config: ServerConfig{E2EPromptFaultEnabled: true},
	}
	secondServer.installACPE2EPromptWriteFaultRecorder()
	second := performE2EFaultRecordRequest(
		t, secondApp, currentPool, replacement, controllerToken, capabilitySecret, operationID, 1, http.StatusOK,
	)
	if second.Inject {
		t.Fatal("replacement runtime re-armed the consumed prompt-write fault")
	}
	records = corev1.ConfigMapList{}
	if err := kubeClient.List(t.Context(), &records, client.InNamespace(pool.Namespace), client.MatchingLabels{e2ePromptWriteFaultLabel: e2ePromptWriteFaultLabelValue}); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 1 {
		t.Fatalf("fault records after replacement = %d, want 1", len(records.Items))
	}
}

func TestMatchesE2EPromptOperationID(t *testing.T) {
	promptID := harnessv2.PromptID("prompt-1")
	for operationID, want := range map[harnessv2.OperationID]bool{
		"start-prompt-prompt-1":          true,
		"start-prompt-retry-1-prompt-1":  true,
		"start-prompt-retry-12-prompt-1": true,
		"start-prompt-retry-0-prompt-1":  false,
		"start-prompt-retry-01-prompt-1": false,
		"start-prompt-retry-1-prompt-2":  false,
		"start-prompt-retry-prompt-1":    false,
	} {
		if got := matchesE2EPromptOperationID(operationID, promptID); got != want {
			t.Fatalf("matchesE2EPromptOperationID(%q, %q) = %t, want %t", operationID, promptID, got, want)
		}
	}
}

func e2eFaultTestActiveInstance(
	runtimeInstanceID, bootID string,
	profileDigest harnessv2.ProfileDigest,
) *corev1alpha1.RuntimePoolActiveInstanceStatus {
	return &corev1alpha1.RuntimePoolActiveInstanceStatus{
		PodNamespace: e2eFaultTestRuntimeNamespace, RuntimeInstanceID: runtimeInstanceID, BootID: bootID, ControllerEpoch: 1,
		ProfileDigest: string(profileDigest), ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
	}
}

func performE2EFaultRecordRequest(
	t *testing.T,
	app *fiber.App,
	pool *corev1alpha1.RuntimePool,
	active *corev1alpha1.RuntimePoolActiveInstanceStatus,
	controllerToken string,
	capabilitySecret []byte,
	promptOperationID harnessv2.OperationID,
	taskAttempt uint32,
	wantStatus int,
) harnessv2.E2EPromptWriteAmbiguityRecordResponse {
	t.Helper()
	request := harnessv2.E2EPromptWriteAmbiguityRecordRequest{
		Namespace: pool.Namespace,
		Metadata: harnessv2.MutationMetadata{
			Fence: harnessv2.Fence{
				RuntimeInstanceID: harnessv2.RuntimeInstanceID(active.RuntimeInstanceID),
				SupervisorBootID:  harnessv2.SupervisorBootID(active.BootID),
				ControllerEpoch:   uint64(active.ControllerEpoch),
				RuntimePoolUID:    harnessv2.RuntimePoolUID(pool.UID), RuntimePoolGeneration: uint64(pool.Generation),
				RuntimeSessionUID: e2eFaultTestSessionUID, RuntimeSessionGeneration: 1,
				RuntimeProfileDigest:       harnessv2.ProfileDigest(active.ProfileDigest),
				ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
			},
			TaskUID: e2eFaultTestTaskUID, TaskAttempt: taskAttempt, PromptID: e2eFaultTestPromptID,
			OperationID:                "record-e2e-prompt-write-ambiguity",
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
			ExpiresAt:                  time.Now().UTC().Add(time.Minute),
		},
		PromptOperationID: promptOperationID,
	}
	digest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.RequestDigest = digest
	capability, err := harnessv2.SignOperationCapability(capabilitySecret, harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, harnessv2.E2EPromptWriteAmbiguityRecordPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+controllerToken)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolNamespaceHeader, pool.Namespace)
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolUIDHeader, string(pool.UID))
	response, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != wantStatus {
		t.Fatalf("fault record status = %d, want %d", response.StatusCode, wantStatus)
	}
	if wantStatus != http.StatusOK {
		return harnessv2.E2EPromptWriteAmbiguityRecordResponse{}
	}
	var decoded harnessv2.E2EPromptWriteAmbiguityRecordResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
