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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	parentTaskName        = "parent-task"
	parentTransactionID   = "txn-parent"
	parentTransactionHash = "sha256:parent-context"
	childTransactionScope = "orka:agents:run"
)

func researcherAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testResearcherAgentName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{},
	}
}

func parentTask() *corev1alpha1.Task {
	priority := int32(500)
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      parentTaskName,
			Namespace: defaultNamespace,
			UID:       apitypes.UID("parent-uid-1234"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			Priority: &priority,
			RequestedBy: &corev1alpha1.RequestedBy{
				Subject: "parent-subject",
				Issuer:  "https://issuer.example.test",
				Roles:   []string{"orka:agents:delegate", childTransactionScope},
			},
			Transaction: &corev1alpha1.TaskTransaction{
				Profile:            "kontxt",
				ID:                 parentTransactionID,
				Issuer:             "https://issuer.example.test",
				Subject:            "parent-subject",
				RequestingWorkload: "spiffe://example.test/ns/default/sa/parent",
				Scope:              "orka:agents:delegate orka:agents:run",
				Scopes:             []string{"orka:agents:delegate", childTransactionScope},
				ContextDigest:      parentTransactionHash,
			},
		},
	}
}

func expectInheritedTaskProvenance(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	if task.Spec.RequestedBy == nil || task.Spec.RequestedBy.Subject != "parent-subject" {
		t.Fatalf("spec.requestedBy = %#v, want parent requester", task.Spec.RequestedBy)
	}
	if task.Spec.Transaction == nil || task.Spec.Transaction.ID != parentTransactionID {
		t.Fatalf("spec.transaction = %#v, want parent transaction", task.Spec.Transaction)
	}
	if task.Labels[labels.LabelTransactionID] != labels.SelectorValue(parentTransactionID) {
		t.Fatalf("transaction label = %q, want %q", task.Labels[labels.LabelTransactionID], labels.SelectorValue(parentTransactionID))
	}
	if task.Annotations[labels.AnnotationTransactionContextDigest] != parentTransactionHash {
		t.Fatalf("transaction context digest annotation = %q, want %q", task.Annotations[labels.AnnotationTransactionContextDigest], parentTransactionHash)
	}
}

func TestDelegateTaskTool_Name(t *testing.T) {
	tool := NewDelegateTaskTool(newFakeClient())
	if got := tool.Name(); got != delegateTaskToolName {
		t.Errorf("Name() = %v, want %v", got, delegateTaskToolName)
	}
}

func TestDelegateTaskTool_Description(t *testing.T) {
	tool := NewDelegateTaskTool(newFakeClient())
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestDelegateTaskTool_Parameters(t *testing.T) {
	tool := NewDelegateTaskTool(newFakeClient())
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestDelegateTaskTool_Execute(t *testing.T) {
	tests := []struct {
		name        string
		envVars     map[string]string
		args        json.RawMessage
		wantErr     bool
		wantErrMsg  string
		checkResult bool
		wantStatus  string
	}{
		{
			name: "successful delegation",
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "0",
				envOrkaCoordinationAllowedAgents: "researcher,coder",
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:        json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  GitHubPullRequestStatusCreated,
		},
		{
			name: "agent not allowed",
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "0",
				envOrkaCoordinationAllowedAgents: "researcher,coder",
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:       json.RawMessage(`{"agent": "unauthorized-agent", "prompt": "Do something"}`),
			wantErr:    true,
			wantErrMsg: "not in the allowed agents list",
		},
		{
			name: "depth exceeded",
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "3",
				envOrkaCoordinationAllowedAgents: testResearcherAgentName,
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:       json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`),
			wantErr:    true,
			wantErrMsg: "coordination depth exceeded",
		},
		{
			name: "missing agent arg",
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "0",
				envOrkaCoordinationAllowedAgents: testResearcherAgentName,
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:       json.RawMessage(`{"prompt": "Research the topic"}`),
			wantErr:    true,
			wantErrMsg: "agent is required",
		},
		{
			name: "missing prompt arg",
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "0",
				envOrkaCoordinationAllowedAgents: testResearcherAgentName,
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:       json.RawMessage(`{"agent": "researcher"}`),
			wantErr:    true,
			wantErrMsg: "prompt is required",
		},
		{
			name: invalidJSONArgsCaseName,
			envVars: map[string]string{
				envOrkaTaskName:      parentTaskName,
				envOrkaTaskNamespace: defaultNamespace,
			},
			args:       json.RawMessage(invalidJSONText),
			wantErr:    true,
			wantErrMsg: invalidArgumentsMessage,
		},
		{
			name: "custom priority",
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "0",
				envOrkaCoordinationAllowedAgents: testResearcherAgentName,
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:        json.RawMessage(`{"agent": "researcher", "prompt": "Research", "priority": 800}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  GitHubPullRequestStatusCreated,
		},
		{
			name: testCustomNamespaceCaseName,
			envVars: map[string]string{
				envOrkaTaskName:                  parentTaskName,
				envOrkaTaskNamespace:             defaultNamespace,
				envOrkaCoordinationDepth:         "0",
				envOrkaCoordinationAllowedAgents: testResearcherAgentName,
				envOrkaCoordinationMaxDepth:      "3",
			},
			args:    json.RawMessage(`{"agent": "researcher", "prompt": "Research", "namespace": "other-ns"}`),
			wantErr: true, // parent task not found in other-ns
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variables
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			// Create fake client with parent task and agent
			k8sClient := newFakeClient(parentTask(), researcherAgent())
			tool := NewDelegateTaskTool(k8sClient)

			result, err := tool.Execute(context.Background(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.wantErrMsg != "" {
				if err == nil || !contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("Execute() error = %v, want error containing %q", err, tt.wantErrMsg)
				}
				return
			}

			if tt.checkResult {
				var delegateResult DelegateTaskResult
				if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if delegateResult.Status != tt.wantStatus {
					t.Errorf("Execute() status = %q, want %q", delegateResult.Status, tt.wantStatus)
				}
				if delegateResult.TaskName == "" {
					t.Error("Execute() returned empty task name")
				}
			}
		})
	}
}

func TestDelegateTaskTool_Execute_WithTTSChildToken(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("parent-tx-token"), 0600); err != nil {
		t.Fatalf("failed to write subject token fixture: %v", err)
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
		Scope:              childTransactionScope,
		RequestingWorkload: "spiffe://example.test/ns/default/sa/orka-worker",
	}, signingKey, kid, time.Minute)
	if err != nil {
		t.Fatalf("failed to create child TxToken: %v", err)
	}

	var requestDetails map[string]any
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.FormValue("subject_token"); got != "parent-tx-token" {
			t.Fatalf("subject_token = %q, want parent-tx-token", got)
		}
		if got := r.FormValue("scope"); got != childTransactionScope {
			t.Fatalf("scope = %q, want orka:agents:run", got)
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
	t.Setenv(workerenv.ContextTokenChildScope, childTransactionScope)

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"agent":"researcher","prompt":"Research with token"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if delegateResult.TaskName == "" {
		t.Fatal("Execute() returned empty task name")
	}
	if requestDetails["operation"] != "delegateTask" || requestDetails["agent"] != testResearcherAgentName || requestDetails["txn"] != parentTransactionID {
		t.Fatalf("request_details = %#v", requestDetails)
	}

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{Name: delegateResult.TaskName, Namespace: defaultNamespace}, childTask); err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}
	expectInheritedTaskProvenance(t, childTask)
	secretName := childTask.Annotations[labels.AnnotationTransactionTokenSecret]
	if secretName == "" {
		t.Fatal("expected child transaction token secret annotation")
	}
	secret := &corev1.Secret{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{Name: secretName, Namespace: defaultNamespace}, secret); err != nil {
		t.Fatalf("failed to get child token secret: %v", err)
	}
	claims, err := sdkverify.New(jwksServer.URL, "child.example.test").Verify(context.Background(), string(secret.Data["token"]))
	if err != nil {
		t.Fatalf("failed to verify child TxToken from secret: %v", err)
	}
	if claims.TransactionID != parentTransactionID {
		t.Fatalf("child token txn = %q, want %q", claims.TransactionID, parentTransactionID)
	}
	if claims.Scope != childTransactionScope {
		t.Fatalf("child token scope = %q, want orka:agents:run", claims.Scope)
	}
}

func TestDelegateTaskTool_Execute_ChildTaskFields(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "1")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "5")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Investigate this"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Fetch the created child task to verify fields
	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	// Find the child task (not the parent)
	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != parentTaskName {
			childTask = &taskList.Items[i]
			break
		}
	}

	if childTask == nil {
		t.Fatal("child task not found")
	}

	// Verify labels
	if childTask.Labels[labels.LabelParentTask] != labels.SelectorValue(parentTaskName) {
		t.Errorf("label orka.ai/parent-task = %q, want %q", childTask.Labels[labels.LabelParentTask], labels.SelectorValue(parentTaskName))
	}
	if childTask.Labels[labels.LabelCoordinator] != trueStr {
		t.Errorf("label orka.ai/coordinator = %q, want %q", childTask.Labels[labels.LabelCoordinator], trueStr)
	}
	if childTask.Labels[labels.LabelDelegatedAgent] != testResearcherAgentName {
		t.Errorf("label orka.ai/delegated-agent = %q, want %q", childTask.Labels[labels.LabelDelegatedAgent], testResearcherAgentName)
	}

	// Verify annotations
	if childTask.Annotations[labels.AnnotationParentTaskName] != parentTaskName {
		t.Errorf("annotation orka.ai/parent-task-name = %q, want %q", childTask.Annotations[labels.AnnotationParentTaskName], parentTaskName)
	}
	if childTask.Annotations[labels.AnnotationCoordinationDepth] != "2" {
		t.Errorf("annotation orka.ai/coordination-depth = %q, want %q", childTask.Annotations[labels.AnnotationCoordinationDepth], "2")
	}

	// Verify spec
	if childTask.Spec.Type != corev1alpha1.TaskTypeAI {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAI)
	}
	if childTask.Spec.AgentRef == nil || childTask.Spec.AgentRef.Name != testResearcherAgentName {
		t.Errorf("spec.agentRef.name = %v, want %q", childTask.Spec.AgentRef, testResearcherAgentName)
	}
	if childTask.Spec.Prompt != "Investigate this" {
		t.Errorf("spec.prompt = %q, want %q", childTask.Spec.Prompt, "Investigate this")
	}
	expectInheritedTaskProvenance(t, childTask)

	// Verify owner reference
	if len(childTask.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(childTask.OwnerReferences))
	}
	ownerRef := childTask.OwnerReferences[0]
	if ownerRef.Name != parentTaskName {
		t.Errorf("ownerRef.Name = %q, want %q", ownerRef.Name, parentTaskName)
	}
	if ownerRef.UID != apitypes.UID("parent-uid-1234") {
		t.Errorf("ownerRef.UID = %q, want %q", ownerRef.UID, "parent-uid-1234")
	}
	if ownerRef.Controller == nil || !*ownerRef.Controller {
		t.Error("ownerRef.Controller should be true")
	}
	if ownerRef.BlockOwnerDeletion == nil || !*ownerRef.BlockOwnerDeletion {
		t.Error("ownerRef.BlockOwnerDeletion should be true")
	}

	// Verify priority inherited from parent
	if childTask.Spec.Priority == nil || *childTask.Spec.Priority != 500 {
		t.Errorf("spec.priority = %v, want 500", childTask.Spec.Priority)
	}
}

func TestDelegateTaskTool_Execute_AgentType(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testClaudeCoderName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	maxTurns := int32(100)
	agentTask := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClaudeCoderName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             runtimeTypeClaude,
				DefaultMaxTurns:  &maxTurns,
				DefaultAllowBash: new(true),
			},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentTask)
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{
		"agent": "claude-coder",
		"prompt": "Fix the auth module",
		"workspace": {
			"gitRepo": "https://github.com/myorg/myrepo.git",
			"branch": "main"
		},
		"timeout": "20m",
		"maxTurns": 50,
		"allowBash": true
	}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if delegateResult.Status != GitHubPullRequestStatusCreated {
		t.Errorf("status = %q, want %q", delegateResult.Status, GitHubPullRequestStatusCreated)
	}

	// Fetch the child task
	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != parentTaskName {
			childTask = &taskList.Items[i]
			break
		}
	}
	if childTask == nil {
		t.Fatal("child task not found")
	}

	// Verify task type is agent
	if childTask.Spec.Type != corev1alpha1.TaskTypeAgent {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAgent)
	}

	// Verify agent runtime config
	if childTask.Spec.AgentRuntime == nil {
		t.Fatal("spec.agentRuntime is nil")
	}
	if childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("spec.agentRuntime.workspace is nil")
	}
	if childTask.Spec.AgentRuntime.Workspace.GitRepo != "https://github.com/myorg/myrepo.git" {
		t.Errorf("workspace.gitRepo = %q, want %q", childTask.Spec.AgentRuntime.Workspace.GitRepo, "https://github.com/myorg/myrepo.git")
	}
	if childTask.Spec.AgentRuntime.Workspace.Branch != testBranch {
		t.Errorf("workspace.branch = %q, want %q", childTask.Spec.AgentRuntime.Workspace.Branch, testBranch)
	}
	if childTask.Spec.AgentRuntime.MaxTurns == nil || *childTask.Spec.AgentRuntime.MaxTurns != 50 {
		t.Errorf("agentRuntime.maxTurns = %v, want 50", childTask.Spec.AgentRuntime.MaxTurns)
	}
	if childTask.Spec.AgentRuntime.AllowBash == nil || !*childTask.Spec.AgentRuntime.AllowBash {
		t.Error("agentRuntime.allowBash should be true")
	}
	if childTask.Spec.Timeout == nil || childTask.Spec.Timeout.Duration != 20*time.Minute {
		t.Errorf("spec.timeout = %v, want 20m", childTask.Spec.Timeout)
	}
}

func TestDelegateTaskTool_Execute_InvalidTimeout(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testClaudeCoderName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	agentTask := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClaudeCoderName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtimeTypeClaude},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentTask)
	tool := NewDelegateTaskTool(k8sClient)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"agent": "claude-coder",
		"prompt": "Fix the auth module",
		"timeout": "eventually"
	}`))
	if err == nil {
		t.Fatal("Execute() expected error for invalid timeout")
	}
	if !contains(err.Error(), invalidTimeoutCaseName) {
		t.Errorf("Execute() error = %v, want error containing %q", err, invalidTimeoutCaseName)
	}
}

func TestDelegateTaskTool_Execute_AgentNotFound(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, "nonexistent-agent")
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	// No agent registered in the fake client
	k8sClient := newFakeClient(parentTask())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "nonexistent-agent", "prompt": "Do something"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute() expected error for nonexistent agent")
	}
	if !contains(err.Error(), "failed to get agent") {
		t.Errorf("Execute() error = %v, want error containing %q", err, "failed to get agent")
	}
}

func TestDelegateTaskTool_Execute_AgentTypeNoWorkspace(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testClaudeCoderName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	agentWithRuntime := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClaudeCoderName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: runtimeTypeClaude,
			},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentWithRuntime)
	tool := NewDelegateTaskTool(k8sClient)

	// No workspace, maxTurns, or allowBash args
	args := json.RawMessage(`{"agent": "claude-coder", "prompt": "Fix bugs"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != parentTaskName {
			childTask = &taskList.Items[i]
			break
		}
	}
	if childTask == nil {
		t.Fatal("child task not found")
	}

	if childTask.Spec.Type != corev1alpha1.TaskTypeAgent {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAgent)
	}
	if childTask.Spec.AgentRuntime == nil {
		t.Fatal("spec.agentRuntime should not be nil for agent-type tasks")
	}
	if childTask.Spec.AgentRuntime.Workspace != nil {
		t.Error("spec.agentRuntime.workspace should be nil when not provided")
	}
	if childTask.Spec.AgentRuntime.MaxTurns != nil {
		t.Error("spec.agentRuntime.maxTurns should be nil when not provided")
	}
	if childTask.Spec.AgentRuntime.AllowBash != nil {
		t.Error("spec.agentRuntime.allowBash should be nil when not provided")
	}
}

func TestDelegateTaskTool_Execute_AITypeNoRuntime(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != parentTaskName {
			childTask = &taskList.Items[i]
			break
		}
	}
	if childTask == nil {
		t.Fatal("child task not found")
	}

	if childTask.Spec.Type != corev1alpha1.TaskTypeAI {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAI)
	}
	if childTask.Spec.AgentRuntime != nil {
		t.Error("spec.agentRuntime should be nil for AI-type tasks")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestDelegateTaskTool_Execute_PriorTask(t *testing.T) {
	// Create a prior task in the fake client
	prior := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPriorTaskName,
			Namespace: defaultNamespace,
			UID:       "prior-uid",
			Labels: map[string]string{
				labels.LabelIteration:      "1",
				labels.LabelIterationGroup: "group-abc",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "original prompt",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					Branch:  testBranch,
				},
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	parent := parentTask()
	agent := researcherAgent()

	fakeClient := newFakeClient(parent, agent, prior)
	tool := NewDelegateTaskTool(fakeClient)

	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)

	args, _ := json.Marshal(map[string]any{
		"agent": testResearcherAgentName, promptField: "fix the bug", priorTaskField: testPriorTaskName, "feedback": "Add error handling",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if delegateResult.Status != GitHubPullRequestStatusCreated {
		t.Errorf("expected status 'created', got %q", delegateResult.Status)
	}

	// Verify the child task was created with PriorTaskRef
	childTask := &corev1alpha1.Task{}
	if err := fakeClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	if childTask.Spec.PriorTaskRef == nil {
		t.Fatal("expected PriorTaskRef to be set")
	}
	if childTask.Spec.PriorTaskRef.Name != testPriorTaskName {
		t.Errorf("expected PriorTaskRef.Name 'prior-task-1', got %q", childTask.Spec.PriorTaskRef.Name)
	}

	// Verify feedback was prepended to prompt
	if !strings.Contains(childTask.Spec.Prompt, "FEEDBACK FROM REVIEW") {
		t.Errorf("expected prompt to contain feedback, got %q", childTask.Spec.Prompt)
	}
	if !strings.Contains(childTask.Spec.Prompt, "Add error handling") {
		t.Errorf("expected prompt to contain feedback text")
	}

	// Verify iteration labels
	if childTask.Labels[labels.LabelIteration] != "2" {
		t.Errorf("expected iteration=2, got %q", childTask.Labels[labels.LabelIteration])
	}
	if childTask.Labels[labels.LabelIterationGroup] != "group-abc" {
		t.Errorf("expected iteration-group=group-abc, got %q", childTask.Labels[labels.LabelIterationGroup])
	}

	// Verify workspace was copied from prior task
	if childTask.Spec.AgentRuntime == nil || childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be copied from prior task")
	}
	if childTask.Spec.AgentRuntime.Workspace.GitRepo != "https://github.com/example/repo" {
		t.Errorf("expected git repo from prior task, got %q", childTask.Spec.AgentRuntime.Workspace.GitRepo)
	}
}

func TestDelegateTaskTool_Execute_FeedbackOnly(t *testing.T) {
	parent := parentTask()
	agent := researcherAgent()

	fakeClient := newFakeClient(parent, agent)
	tool := NewDelegateTaskTool(fakeClient)

	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)

	args, _ := json.Marshal(map[string]any{
		"agent": testResearcherAgentName, promptField: "implement feature", "feedback": "Use dependency injection",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var delegateResult DelegateTaskResult
	_ = json.Unmarshal([]byte(result), &delegateResult)

	// Verify the child task was created with feedback in prompt
	childTask := &corev1alpha1.Task{}
	_ = fakeClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask)

	if !strings.Contains(childTask.Spec.Prompt, "FEEDBACK FROM REVIEW") {
		t.Errorf("expected feedback in prompt, got %q", childTask.Spec.Prompt)
	}
	// PriorTaskRef should NOT be set
	if childTask.Spec.PriorTaskRef != nil {
		t.Errorf("expected PriorTaskRef to be nil when prior_task not specified")
	}
}

func TestDelegateTaskTool_Execute_PushBranch(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testClaudeCoderName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	agentWithRuntime := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClaudeCoderName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: runtimeTypeClaude,
			},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentWithRuntime)
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{
		"agent": "claude-coder",
		"prompt": "Implement feature",
		"workspace": {
			"gitRepo": "https://github.com/sozercan/ayna",
			"branch": "main",
			"gitSecretRef": "git-credentials",
			"pushBranch": "feature/edit-message"
		}
	}`)

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	_ = json.Unmarshal([]byte(result), &delegateResult)

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask); err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	if childTask.Spec.AgentRuntime == nil || childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be set")
	}
	ws := childTask.Spec.AgentRuntime.Workspace
	if ws.PushBranch != "feature/edit-message" {
		t.Errorf("pushBranch = %q, want %q", ws.PushBranch, "feature/edit-message")
	}
	if ws.GitRepo != testSozercanAynaRepoURL {
		t.Errorf("gitRepo = %q, want %q", ws.GitRepo, testSozercanAynaRepoURL)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != "git-credentials" {
		t.Errorf("gitSecretRef = %v, want git-credentials", ws.GitSecretRef)
	}
}

func TestDelegateTaskTool_Execute_AutoDiscoversGitSecretRef(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, "copilot-coder")
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	agentWithRuntime := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "copilot-coder",
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot},
			SecretRef: &corev1.LocalObjectReference{Name: testCustomCopilotSecretName},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentWithRuntime)
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{
		"agent": "copilot-coder",
		"prompt": "Implement feature",
		"workspace": {
			"gitRepo": "https://github.com/sozercan/ayna",
			"branch": "main",
			"pushBranch": "feature/auto-secret"
		}
	}`)

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	_ = json.Unmarshal([]byte(result), &delegateResult)

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask); err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	if childTask.Spec.AgentRuntime == nil || childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be set")
	}
	if childTask.Spec.AgentRuntime.Workspace.GitSecretRef == nil {
		t.Fatal("expected gitSecretRef to be auto-populated")
	}
	if childTask.Spec.AgentRuntime.Workspace.GitSecretRef.Name != testCustomCopilotSecretName {
		t.Errorf("gitSecretRef = %q, want %q", childTask.Spec.AgentRuntime.Workspace.GitSecretRef.Name, testCustomCopilotSecretName)
	}
}

func TestDelegateTaskTool_Execute_AutoRetry(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Do the work", "auto_retry": true, "max_retries": 3}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Fetch child task and verify annotations
	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	if childTask.Annotations[labels.AnnotationAutoRetry] != trueStr {
		t.Errorf("expected auto-retry=true, got %q", childTask.Annotations[labels.AnnotationAutoRetry])
	}
	if childTask.Annotations[labels.AnnotationMaxRetries] != "3" {
		t.Errorf("expected max-retries=3, got %q", childTask.Annotations[labels.AnnotationMaxRetries])
	}
	if childTask.Annotations[labels.AnnotationRetryCount] != "0" {
		t.Errorf("expected retry-count=0, got %q", childTask.Annotations[labels.AnnotationRetryCount])
	}
	if childTask.Annotations[labels.AnnotationOriginalPrompt] != "Do the work" {
		t.Errorf("expected original-prompt stored, got %q", childTask.Annotations[labels.AnnotationOriginalPrompt])
	}
}

func TestDelegateTaskTool_Execute_AutoRetryDefault(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	// auto_retry without max_retries should default to 2
	args := json.RawMessage(`{"agent": "researcher", "prompt": "Do work", "auto_retry": true}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	if childTask.Annotations[labels.AnnotationMaxRetries] != "2" {
		t.Errorf("expected default max-retries=2, got %q", childTask.Annotations[labels.AnnotationMaxRetries])
	}
}

func TestDelegateTaskTool_Execute_NoAutoRetry(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Do work"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: defaultNamespace,
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	// When auto_retry is not set, no retry annotations should be present
	if _, ok := childTask.Annotations[labels.AnnotationAutoRetry]; ok {
		t.Error("expected no auto-retry annotation when auto_retry is false")
	}
}
