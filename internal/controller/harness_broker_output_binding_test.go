package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/security"
	toolspkg "github.com/orka-agents/orka/internal/tools"
)

type captureWorkerOutputBindingModeTool struct {
	mode security.WorkerOutputBindingMode
}

func (t *captureWorkerOutputBindingModeTool) Name() string { return "capture_mode" }

func (t *captureWorkerOutputBindingModeTool) Description() string {
	return "captures tool context mode"
}

func (t *captureWorkerOutputBindingModeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func (t *captureWorkerOutputBindingModeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	toolCtx := toolspkg.GetToolContext(ctx)
	if toolCtx == nil {
		return "", fmt.Errorf("missing tool context")
	}
	t.mode = toolCtx.WorkerOutputBindingMode
	return `{}`, nil
}

func TestExecuteHarnessBrokeredCoordinationToolPropagatesWorkerOutputBindingMode(t *testing.T) {
	tool := &captureWorkerOutputBindingModeTool{}
	reconciler := &TaskReconciler{SecurityIntegrityConfig: security.IntegrityConfig{
		WorkerOutputBindingMode: security.WorkerOutputBindingAudit,
	}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"}}

	if _, err := reconciler.executeHarnessBrokeredCoordinationTool(
		context.Background(), task, nil, harness.HarnessEventFrame{ToolCallID: "call-1"}, tool,
	); err != nil {
		t.Fatal(err)
	}
	if tool.mode != security.WorkerOutputBindingAudit {
		t.Fatalf("WorkerOutputBindingMode = %q, want audit", tool.mode)
	}
}
