/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestValidateToolsBuiltinDeclarationsFollowTheCatalog(t *testing.T) {
	known := func(string) bool { return true }
	for _, tc := range []struct {
		name string
		tool corev1alpha1.ConnectorTool
		want string
	}{
		{name: "read tool", tool: corev1alpha1.ConnectorTool{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}},
		{name: "write tool", tool: corev1alpha1.ConnectorTool{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin}},
		// A write tool declared as read would skip approval and readOnly hiding.
		{name: "write declared read", tool: corev1alpha1.ConnectorTool{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}, want: "must be declared with class write"},
		{name: "read declared write", tool: corev1alpha1.ConnectorTool{Name: "get_issue", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin}, want: "must be declared with class read"},
		// A built-in that ignores the credential must not be declared at all.
		{name: "not linked", tool: corev1alpha1.ConnectorTool{Name: "web_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}, want: "cannot use a linked account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := validateTools([]corev1alpha1.ConnectorTool{tc.tool}, known)
			switch {
			case tc.want == "" && issue != nil:
				t.Fatalf("unexpected issue: %s", issue.Message)
			case tc.want != "" && (issue == nil || !strings.Contains(issue.Message, tc.want)):
				t.Fatalf("issue = %v, want %q", issue, tc.want)
			}
		})
	}
	if names := BuiltinConnectorToolNames(); len(names) != 9 || names[0] != "check_pr_review_marker" {
		t.Fatalf("catalog names = %v", names)
	}
	if _, ok := DeclaresBuiltinTool(nil, "get_issue"); ok {
		t.Fatal("a nil provider declares nothing")
	}
	provider := &corev1alpha1.ConnectorProvider{Spec: corev1alpha1.ConnectorProviderSpec{Tools: []corev1alpha1.ConnectorTool{
		{Name: "get_issue", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP},
		{Name: "list_issues", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin},
	}}}
	if _, ok := DeclaresBuiltinTool(provider, "get_issue"); ok {
		t.Fatal("an HTTP declaration is not a built-in declaration")
	}
	if tool, ok := DeclaresBuiltinTool(provider, "list_issues"); !ok || tool.Class != corev1alpha1.ConnectorToolClassRead {
		t.Fatalf("declared = %+v %t", tool, ok)
	}
}
