package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/tools"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeAgentReadClient struct {
	client.Client
	foreignAgentReads int
}

func (c *nativeAgentReadClient) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption,
) error {
	if _, agent := object.(*corev1alpha1.Agent); agent && key.Namespace != "team" {
		c.foreignAgentReads++
	}
	// The underlying fake permits foreign Agent reads, unlike the worker RBAC.
	return c.Client.Get(ctx, key, object, options...)
}

func TestNativeRemoteMCPAgentNamespaceBoundary(t *testing.T) {
	for _, namespace := range []string{"", "team", "other"} {
		t.Run(namespace, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			f.task.Spec.AgentRef.Namespace = namespace
			if err := f.client.Update(t.Context(), f.task); err != nil {
				t.Fatal(err)
			}
			if namespace == "other" {
				foreign := f.agent.DeepCopy()
				foreign.Namespace = namespace
				foreign.ResourceVersion = ""
				if err := f.client.Create(t.Context(), foreign); err != nil {
					t.Fatal(err)
				}
			}
			reader := &nativeAgentReadClient{Client: f.client}
			f.client = reader
			ctx, loaded, err := f.prepare(t)
			if namespace == "other" {
				if err == nil || err.Error() != "remote MCP requires the Agent and Task to share a namespace" {
					t.Error("foreign Agent did not fail with the remote namespace support boundary")
				}
				if reader.foreignAgentReads != 0 || f.requests.Load() != 0 {
					t.Errorf("foreign selection made %d Agent reads and %d protocol requests",
						reader.foreignAgentReads, f.requests.Load())
				}
				if ctx.Value(nativeRemoteToolsKey{}) != nil {
					t.Error("foreign selection became an exposed remote capability")
				}
				return
			}
			if err != nil {
				t.Fatalf("same-namespace preparation failed: %v", err)
			}
			toolContext := &tools.ToolContext{Client: f.client, Namespace: "team", TaskID: "task", TaskUID: "task-uid"}
			result, err := executeNativeRemoteTool(ctx, toolContext, loaded["health"], json.RawMessage(`{}`))
			if err != nil || !strings.Contains(result, "healthy") || f.calls.Load() != 1 {
				t.Fatal("same-namespace Agent did not retain remote execution")
			}
		})
	}
}

func TestNativeRemoteMCPNamespaceBoundaryLeavesLegacyStartupUnchanged(t *testing.T) {
	for _, backend := range []string{"HTTP", "managed MCP", "builtin"} {
		t.Run(backend, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			f.task.Spec.AgentRef.Namespace = "other"
			if err := f.client.Update(t.Context(), f.task); err != nil {
				t.Fatal(err)
			}
			f.agent.Namespace = "other"
			tool := f.tool.DeepCopy()
			tool.Spec.MCP.Remote = nil
			switch backend {
			case "HTTP":
				tool.Spec.MCP = nil
				tool.Spec.HTTP.URL = "https://example.com/health"
			case "managed MCP":
				tool.Spec.MCP.Workspace = &corev1alpha1.MCPWorkspace{
					ClassRef: corev1alpha1.WorkspaceClassReference{Name: "mcp"}, Port: 8080,
				}
			case "builtin":
				tool = nil
			}
			if err := aitools.ValidateRemoteMCPSelection(f.task, f.agent, tool); err != nil {
				t.Fatal("remote namespace restriction reached legacy selection")
			}
			name := "file_read"
			loaded := map[string]*corev1alpha1.Tool{}
			if tool != nil {
				name = tool.Name
				loaded[name] = tool
			}
			reader := &nativeAgentReadClient{Client: f.client}
			if _, err := prepareNativeRemoteTools(
				t.Context(), reader, "team", "task", []string{name}, loaded, f.executor,
			); err != nil {
				t.Fatalf("remote namespace restriction reached legacy startup: %v", err)
			}
			if reader.foreignAgentReads != 0 || f.requests.Load() != 0 {
				t.Fatal("legacy startup entered remote verification")
			}
		})
	}
}
