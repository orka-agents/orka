package tools

import (
	"context"
	"encoding/json"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type publishDuringAgentRead struct {
	client.Client
	published bool
}

func (c *publishDuringAgentRead) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if agent, ok := obj.(*corev1alpha1.Agent); ok && !c.published {
		c.published = true
		published := agent.DeepCopy()
		published.Spec.Soul = &corev1alpha1.SoulSource{Inline: "reviewed persona"}
		if err := c.Update(ctx, published); err != nil {
			return err
		}
	}
	return nil
}

func TestUpdateAgentPreservesConcurrentSoulPublication(t *testing.T) {
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace}, Spec: corev1alpha1.AgentSpec{SystemPrompt: &corev1alpha1.PromptSource{Inline: "original role"}}}
	base := newFakeClient(agent)
	c := &publishDuringAgentRead{Client: base}
	ctx := WithToolContext(context.Background(), &ToolContext{Client: c, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"reviewer","systemPrompt":"changed role"}`))
	if err != nil {
		t.Fatal(err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatal(err)
	}
	if response.Success {
		t.Fatal("stale update should conflict with concurrent publication")
	}
	current := &corev1alpha1.Agent{}
	if err := base.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Soul == nil || current.Spec.Soul.Inline != "reviewed persona" || current.Spec.SystemPrompt.Inline != "original role" {
		t.Fatal("concurrent publication was overwritten")
	}
}

func TestSoulToolArgumentRequiresStructuredSource(t *testing.T) {
	if _, err := soulArgument("persona"); err == nil {
		t.Fatal("string argument bypassed the source schema")
	}
	source, err := soulArgument(map[string]any{"inline": "persona"})
	if err != nil || source.Inline != "persona" {
		t.Fatalf("nested source object was not accepted: %v", err)
	}
	if _, err := soulArgument(map[string]any{"configMapRef": map[string]any{"name": "soul", "key": "SOUL.md"}}); err == nil {
		t.Fatal("unpinned reference was accepted")
	}
}
