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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

type recordingMessageStore struct {
	messages []store.Message
}

func (s *recordingMessageStore) SendMessage(_ context.Context, message *store.Message) error {
	s.messages = append(s.messages, *message)
	return nil
}

func (*recordingMessageStore) GetMessages(context.Context, string, string, string, bool) ([]store.Message, error) {
	return nil, nil
}

func (*recordingMessageStore) DeleteTaskMessages(context.Context, string, string) error {
	return nil
}

func (*recordingMessageStore) DeleteParentMessages(context.Context, string, string) error {
	return nil
}

func TestSendMessageTool_Name(t *testing.T) {
	tool := NewSendMessageTool()
	if got := tool.Name(); got != sendMessageToolName {
		t.Errorf("Name() = %v, want %v", got, sendMessageToolName)
	}
}

func TestSendMessageTool_Description(t *testing.T) {
	tool := NewSendMessageTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestSendMessageTool_Parameters(t *testing.T) {
	tool := NewSendMessageTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props["to_task"]; !ok {
		t.Error("missing to_task property")
	}
	if _, ok := props["content"]; !ok {
		t.Error("missing content property")
	}
}

func TestSendMessageTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       SendMessageArgs
		envVars    map[string]string
		serverCode int
		wantErr    bool
		wantMsg    string
	}{
		{
			name: "send to specific sibling",
			args: SendMessageArgs{
				ToTask:  testSiblingTaskName,
				Content: "found a bug in auth module",
			},
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusNoContent,
			wantMsg:    "Message sent to sibling-1",
		},
		{
			name: "broadcast to all siblings",
			args: SendMessageArgs{
				ToTask:  "*",
				Content: "phase 1 complete",
			},
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusNoContent,
			wantMsg:    "Message sent to all siblings",
		},
		{
			name: "missing to_task",
			args: SendMessageArgs{
				Content: testHelloText,
			},
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
				envOrkaControllerURL: localhostURL,
			},
			wantErr: true,
		},
		{
			name: "missing content",
			args: SendMessageArgs{
				ToTask: testSiblingTaskName,
			},
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
				envOrkaControllerURL: localhostURL,
			},
			wantErr: true,
		},
		{
			name: "missing env vars",
			args: SendMessageArgs{
				ToTask:  testSiblingTaskName,
				Content: testHelloText,
			},
			envVars: map[string]string{},
			wantErr: true,
		},
		{
			name: serverErrorMessage,
			args: SendMessageArgs{
				ToTask:  testSiblingTaskName,
				Content: testHelloText,
			},
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusInternalServerError,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear env vars first
			for _, k := range []string{envOrkaTaskName, envOrkaTaskNamespace, envOrkaParentTask, envOrkaControllerURL} {
				t.Setenv(k, "")
			}

			var server *httptest.Server
			if tt.serverCode != 0 {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost {
						t.Errorf("expected POST, got %s", r.Method)
					}
					w.WriteHeader(tt.serverCode)
				}))
				defer server.Close()
				tt.envVars[envOrkaControllerURL] = server.URL
			}

			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			tool := NewSendMessageTool()
			argsJSON, _ := json.Marshal(tt.args)
			result, err := tool.Execute(context.Background(), argsJSON)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result != tt.wantMsg {
				t.Errorf("result = %q, want %q", result, tt.wantMsg)
			}
		})
	}
}

func TestSendMessageTool_BrokeredSiblingScope(t *testing.T) {
	const (
		parentTask      = "coordinator-a"
		senderTask      = "worker-a"
		siblingTask     = "worker-b"
		unrelatedTask   = "worker-other"
		unrelatedParent = "coordinator-b"
	)
	sibling := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: siblingTask, Namespace: defaultNamespace,
		Labels:      map[string]string{labels.LabelParentTask: labels.SelectorValue(parentTask)},
		Annotations: map[string]string{labels.AnnotationParentTaskName: parentTask},
	}}
	unrelated := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: unrelatedTask, Namespace: defaultNamespace,
		Labels:      map[string]string{labels.LabelParentTask: labels.SelectorValue(unrelatedParent)},
		Annotations: map[string]string{labels.AnnotationParentTaskName: unrelatedParent},
	}}

	tests := []struct {
		name         string
		target       string
		withClient   bool
		wantErr      string
		wantMessages int
	}{
		{name: "sibling", target: siblingTask, withClient: true, wantMessages: 1},
		{name: "broadcast", target: "*", wantMessages: 1},
		{name: "unrelated coordinator family", target: unrelatedTask, withClient: true, wantErr: "not a sibling"},
		{name: "missing target", target: "missing", withClient: true, wantErr: "validate sibling task"},
		{name: "missing broker client", target: siblingTask, wantErr: "requires a Kubernetes client"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messageStore := &recordingMessageStore{}
			toolContext := &ToolContext{
				Brokered: true, Namespace: defaultNamespace, TaskID: senderTask,
				ParentTaskID: parentTask, MessageStore: messageStore,
			}
			if test.withClient {
				toolContext.Client = newFakeClient(sibling, unrelated)
			}
			ctx := WithToolContext(context.Background(), toolContext)
			arguments, err := json.Marshal(SendMessageArgs{ToTask: test.target, Content: testHelloText})
			if err != nil {
				t.Fatal(err)
			}
			result, err := NewSendMessageTool().Execute(ctx, arguments)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Execute() error = %v, want substring %q", err, test.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
				if result == "" {
					t.Fatal("Execute() returned an empty success result")
				}
			}
			if len(messageStore.messages) != test.wantMessages {
				t.Fatalf("stored messages = %d, want %d", len(messageStore.messages), test.wantMessages)
			}
			if test.wantMessages == 1 {
				message := messageStore.messages[0]
				if message.FromTask != senderTask || message.ToTask != test.target || message.ParentTask != parentTask {
					t.Fatalf("stored message scope = %#v", message)
				}
			}
		})
	}
}
