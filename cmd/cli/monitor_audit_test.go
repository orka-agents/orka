package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintMonitorAuditTableShowsOneRowPerDecision(t *testing.T) {
	result := map[string]any{
		"items": []any{
			map[string]any{
				"createdAt": "2026-11-26T01:00:00Z", "decision": "refused", "eventType": "command_rejected",
				"itemKind": "issue", "itemNumber": float64(44), "reason": "actor_permission_not_allowed",
				"policy": "spec.triggers.github.labels.requireActorPermission=maintain", "actor": "github-webhook",
				"command": map[string]any{"id": "cmd-1", "author": "octo\x1b[31mcat"},
			},
			map[string]any{
				"createdAt": "2026-11-26T00:30:00Z", "decision": "acted", "eventType": "repair_succeeded",
				"itemKind": "pull_request", "itemNumber": float64(12), "actor": "controller",
				"task":      map[string]any{"name": "repair-task"},
				"mutations": []any{map[string]any{"id": "ghmut-1", "operation": "push", "status": "succeeded"}},
			},
		},
		"metadata": map[string]any{"continue": "20"},
	}
	var out bytes.Buffer
	if err := printMonitorAuditTable(&out, result); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "TIME") || !strings.Contains(lines[0], "POLICY") || !strings.Contains(lines[0], "MUTATION") {
		t.Fatalf("table = %q, want a header and one row per decision", out.String())
	}
	refused, acted := lines[1], lines[2]
	for _, want := range []string{"refused", "command_rejected", "#44", "actor_permission_not_allowed", "requireActorPermission=maintain", "octo[31mcat"} {
		if !strings.Contains(refused, want) {
			t.Errorf("refused row %q lacks %q", refused, want)
		}
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("table kept a terminal escape: %q", out.String())
	}
	for _, want := range []string{"acted", "PR #12", "controller", "repair-task", "push succeeded"} {
		if !strings.Contains(acted, want) {
			t.Errorf("acted row %q lacks %q", acted, want)
		}
	}
	if !strings.Contains(out.String(), "--continue 20") {
		t.Errorf("table = %q, want the continue token", out.String())
	}

	out.Reset()
	if err := printMonitorAuditTable(&out, map[string]any{"items": []any{}}); err != nil || strings.TrimSpace(out.String()) != "No decisions found." {
		t.Fatalf("empty table = %q err=%v", out.String(), err)
	}
}
