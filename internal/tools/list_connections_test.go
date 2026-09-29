/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

func TestListConnectionsTool(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	accepted := func(name, display string, tools ...corev1alpha1.ConnectorTool) *corev1alpha1.ConnectorProvider {
		return &corev1alpha1.ConnectorProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant", Generation: 1},
			Spec:       corev1alpha1.ConnectorProviderSpec{DisplayName: display, Tools: tools},
			Status: corev1alpha1.ConnectorProviderStatus{Conditions: []metav1.Condition{
				{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: 1},
				{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, Reason: "Resolved", ObservedGeneration: 1},
			}},
		}
	}
	github := accepted("github", "GitHub",
		corev1alpha1.ConnectorTool{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin},
		corev1alpha1.ConnectorTool{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin})
	slack := accepted("slack", "")
	pending := accepted("jira", "Jira")
	pending.Status.Conditions = nil
	linked := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: connectors.ConnectionName("github", requester.Issuer, requester.Subject), Namespace: "tenant", Generation: 2,
			Labels: map[string]string{connectors.ConnectionSubjectLabel: connectors.ConnectionSubjectLabelValue(requester.Issuer, requester.Subject)}},
		Spec: corev1alpha1.ConnectionSpec{Subject: corev1alpha1.ConnectionSubject{Issuer: requester.Issuer, Subject: requester.Subject},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: corev1alpha1.ConnectionModeReadWrite},
		Status: corev1alpha1.ConnectionStatus{State: "Ready", GrantSequence: 1, LinkedAt: &metav1.Time{Time: metav1.Now().Time}, Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonScopesGranted, ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonProviderResolved, ObservedGeneration: 2},
		}},
	}
	// Somebody else's Connection under the name this requester would get
	// (a collision) is never listed as theirs.
	foreign := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: connectors.ConnectionName("slack", requester.Issuer, requester.Subject), Namespace: "tenant",
			Labels: map[string]string{connectors.ConnectionSubjectLabel: connectors.ConnectionSubjectLabelValue(requester.Issuer, requester.Subject)}},
		Spec: corev1alpha1.ConnectionSpec{Subject: corev1alpha1.ConnectionSubject{Issuer: requester.Issuer, Subject: "bob"},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "slack"}},
	}
	// A link whose provider was removed is still the person's, and still listed.
	retained := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: connectors.ConnectionName("gone", requester.Issuer, requester.Subject), Namespace: "tenant",
			Labels: map[string]string{connectors.ConnectionSubjectLabel: connectors.ConnectionSubjectLabelValue(requester.Issuer, requester.Subject)}},
		Spec: corev1alpha1.ConnectionSpec{Subject: corev1alpha1.ConnectionSubject{Issuer: requester.Issuer, Subject: requester.Subject},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "gone"}, Mode: corev1alpha1.ConnectionModeReadWrite},
		Status: corev1alpha1.ConnectionStatus{State: "Ready", GrantSequence: 1},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(github, slack, pending, linked, foreign, retained).Build()
	tool := &ListConnectionsTool{}
	if tool.Name() != ListConnectionsToolName || !strings.Contains(tool.Description(), ConnectorSettingsPath) {
		t.Fatalf("name = %q description = %q", tool.Name(), tool.Description())
	}

	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: "tenant", PolicyReader: reader, Requester: requester})
	out, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data ListConnectionsResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	result := envelope.Data
	if len(result.Connections) != 2 || result.Connections[0].Provider != "github" || result.Connections[0].DisplayName != "GitHub" ||
		result.Connections[0].Mode != corev1alpha1.ConnectionModeReadWrite || !result.Connections[0].Ready || result.Connections[0].LinkedAt == "" ||
		strings.Join(result.Connections[0].Tools, ",") != "list_pull_requests (read),create_pull_request (write)" {
		t.Fatalf("connections = %+v", result.Connections)
	}
	// The link to the removed provider is reported, unusable, with no tools.
	if gone := result.Connections[1]; gone.Provider != "gone" || !gone.ProviderMissing || gone.Ready || len(gone.Tools) != 0 ||
		!strings.Contains(gone.Message, "no longer configured") {
		t.Fatalf("retained = %+v", gone)
	}
	if len(result.Available) != 2 || result.Available[0].Provider != "jira" || result.Available[0].Ready || result.Available[1].Provider != "slack" ||
		result.Available[1].DisplayName != "slack" || !result.Available[1].Ready {
		t.Fatalf("available = %+v", result.Available)
	}
	if result.SettingsPath != ConnectorSettingsPath || strings.Contains(out, "token") {
		t.Fatalf("result = %s", out)
	}

	// No verified person: an explicit error result, never a listing.
	anonymous := WithToolContext(context.Background(), &ToolContext{Namespace: "tenant", PolicyReader: reader})
	out, err = tool.Execute(anonymous, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "no_identity") {
		t.Fatalf("anonymous = %s err = %v", out, err)
	}
	if out, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err != nil || !strings.Contains(out, "missing tool context") {
		t.Fatalf("no context = %s err = %v", out, err)
	}
}

func TestListConnectionsToolHonoursConnectorReadGate(t *testing.T) {
	tool := &ListConnectionsTool{}
	denied := &ToolContext{
		Namespace: "tenant", Requester: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"},
		AuthorizeConnectorRead: func() *ChatToolError {
			return &ChatToolError{Type: "unauthorized_tool", Message: "token lacks orka:connectors:read", Suggestion: "use a wider token"}
		},
	}
	out, err := tool.Execute(WithToolContext(context.Background(), denied), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unauthorized_tool") || !strings.Contains(out, "orka:connectors:read") || strings.Contains(out, "connections\":[") {
		t.Fatalf("denied output = %s", out)
	}
}
