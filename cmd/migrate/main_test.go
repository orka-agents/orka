/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestExtractTaskNameFromResult(t *testing.T) {
	tests := []struct {
		name   string
		cmName string
		want   string
	}{
		{name: "standard result suffix", cmName: "my-task-result", want: "my-task"},
		{name: "no suffix", cmName: "my-task", want: "my-task"},
		{name: "empty string", cmName: "", want: ""},
		{name: "only suffix", cmName: "-result", want: ""},
		{name: "double suffix", cmName: "task-result-result", want: "task-result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTaskNameFromResult(tt.cmName)
			if got != tt.want {
				t.Errorf("extractTaskNameFromResult(%q) = %q, want %q", tt.cmName, got, tt.want)
			}
		})
	}
}

func TestExtractSessionName(t *testing.T) {
	tests := []struct {
		name   string
		cmName string
		want   string
	}{
		{name: "task session", cmName: "session-my-task", want: "my-task"},
		{name: "chat session", cmName: "chat-session-abc123", want: "abc123"},
		{name: "no prefix", cmName: "unknown-cm", want: "unknown-cm"},
		{name: "empty string", cmName: "", want: ""},
		{name: "only session prefix", cmName: "session-", want: ""},
		{name: "only chat-session prefix", cmName: "chat-session-", want: ""},
		{name: "session without hyphen after", cmName: "sessiondata", want: "sessiondata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSessionName(tt.cmName)
			if got != tt.want {
				t.Errorf("extractSessionName(%q) = %q, want %q", tt.cmName, got, tt.want)
			}
		})
	}
}

func TestParseJSONL(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    int // expected message count
		wantErr bool
	}{
		{
			name:    "empty string",
			data:    "",
			want:    0,
			wantErr: false,
		},
		{
			name:    "single message",
			data:    `{"role":"user","content":"hello","ts":"2024-01-01T00:00:00Z"}`,
			want:    1,
			wantErr: false,
		},
		{
			name: "multiple messages",
			data: `{"role":"user","content":"hello","ts":"2024-01-01T00:00:00Z"}
{"role":"assistant","content":"hi","ts":"2024-01-01T00:00:01Z"}`,
			want:    2,
			wantErr: false,
		},
		{
			name: "blank lines skipped",
			data: `{"role":"user","content":"hello","ts":"2024-01-01T00:00:00Z"}

{"role":"assistant","content":"hi","ts":"2024-01-01T00:00:01Z"}
`,
			want:    2,
			wantErr: false,
		},
		{
			name:    "whitespace-only lines skipped",
			data:    "   \n\t\n",
			want:    0,
			wantErr: false,
		},
		{
			name:    "invalid JSON",
			data:    `{invalid json}`,
			want:    0,
			wantErr: true,
		},
		{
			name: "one valid one invalid",
			data: `{"role":"user","content":"hello","ts":"2024-01-01T00:00:00Z"}
not-json`,
			want:    0,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := parseJSONL(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseJSONL() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(msgs) != tt.want {
				t.Errorf("parseJSONL() returned %d messages, want %d", len(msgs), tt.want)
			}
		})
	}
}

func TestParseJSONL_MessageContent(t *testing.T) {
	data := `{"role":"user","content":"hello world","ts":"2024-01-01T00:00:00Z"}`
	msgs, err := parseJSONL(data)
	if err != nil {
		t.Fatalf("parseJSONL() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("role = %q, want %q", msgs[0].Role, "user")
	}
	if msgs[0].Content != "hello world" {
		t.Errorf("content = %q, want %q", msgs[0].Content, "hello world")
	}
}

func TestListConfigMaps_Namespaced(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-task-result",
			Namespace: "test-ns",
			Labels:    map[string]string{"orka.ai/result": "true"},
		},
		Data: map[string]string{"result": "data"},
	}

	clientset := fake.NewSimpleClientset(cm) //nolint:staticcheck

	cms, err := listConfigMaps(context.Background(), clientset, "test-ns", "orka.ai/result=true")
	if err != nil {
		t.Fatalf("listConfigMaps() error = %v", err)
	}
	if len(cms) != 1 {
		t.Fatalf("expected 1 ConfigMap, got %d", len(cms))
	}
	if cms[0].Name != "my-task-result" {
		t.Errorf("name = %q, want %q", cms[0].Name, "my-task-result")
	}
}

func TestListConfigMaps_AllNamespaces(t *testing.T) {
	cm1 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task1-result",
			Namespace: "ns1",
			Labels:    map[string]string{"orka.ai/result": "true"},
		},
	}
	cm2 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task2-result",
			Namespace: "ns2",
			Labels:    map[string]string{"orka.ai/result": "true"},
		},
	}

	clientset := fake.NewSimpleClientset(cm1, cm2) //nolint:staticcheck

	cms, err := listConfigMaps(context.Background(), clientset, "", "orka.ai/result=true")
	if err != nil {
		t.Fatalf("listConfigMaps() error = %v", err)
	}
	if len(cms) != 2 {
		t.Fatalf("expected 2 ConfigMaps, got %d", len(cms))
	}
}

func TestListConfigMaps_Empty(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck

	cms, err := listConfigMaps(context.Background(), clientset, "test-ns", "orka.ai/result=true")
	if err != nil {
		t.Fatalf("listConfigMaps() error = %v", err)
	}
	if len(cms) != 0 {
		t.Errorf("expected 0 ConfigMaps, got %d", len(cms))
	}
}

func TestDeleteConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "to-delete",
			Namespace: "test-ns",
		},
	}

	clientset := fake.NewSimpleClientset(cm) //nolint:staticcheck

	err := deleteConfigMap(context.Background(), clientset, "test-ns", "to-delete")
	if err != nil {
		t.Fatalf("deleteConfigMap() error = %v", err)
	}

	// Verify it's gone
	cms, err := listConfigMaps(context.Background(), clientset, "test-ns", "")
	if err != nil {
		t.Fatalf("listConfigMaps() error = %v", err)
	}
	if len(cms) != 0 {
		t.Errorf("expected 0 ConfigMaps after delete, got %d", len(cms))
	}
}

func TestDeleteConfigMap_NotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck

	err := deleteConfigMap(context.Background(), clientset, "test-ns", "nonexistent")
	if err == nil {
		t.Error("deleteConfigMap() expected error for nonexistent ConfigMap")
	}
}

// Verify parseJSONL returns correct store.SessionMessage types
func TestParseJSONL_WithOptionalFields(t *testing.T) {
	data := `{"role":"assistant","content":"response","name":"bot","toolCallID":"tc-1","ts":"2024-01-01T00:00:00Z"}`
	msgs, err := parseJSONL(data)
	if err != nil {
		t.Fatalf("parseJSONL() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0]
	if msg.Name != "bot" {
		t.Errorf("name = %q, want %q", msg.Name, "bot")
	}
	if msg.ToolCallID != "tc-1" {
		t.Errorf("toolCallID = %q, want %q", msg.ToolCallID, "tc-1")
	}
	// Verify it's the correct type
	var _ = msg
}
