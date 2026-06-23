/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package approvals

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApprovalTargetDigestCanonicalizesJSONObject(t *testing.T) {
	left, err := TargetArgsDigest(json.RawMessage(`{ "b" : 2, "a" : { "z" : true, "m" : "ok" } }`))
	if err != nil {
		t.Fatalf("TargetArgsDigest(left) error = %v", err)
	}
	right, err := TargetArgsDigest(json.RawMessage(`{"a":{"m":"ok","z":true},"b":2}`))
	if err != nil {
		t.Fatalf("TargetArgsDigest(right) error = %v", err)
	}
	if left != right {
		t.Fatalf("digest changed with map order/whitespace: %s != %s", left, right)
	}
}

func TestApprovalIDChangesByTaskToolOrArgs(t *testing.T) {
	argsA, err := TargetArgsDigest(json.RawMessage(`{"incident":"inc-1"}`))
	if err != nil {
		t.Fatalf("digest argsA: %v", err)
	}
	argsB, err := TargetArgsDigest(json.RawMessage(`{"incident":"inc-2"}`))
	if err != nil {
		t.Fatalf("digest argsB: %v", err)
	}
	base := ApprovalID("default", "incident-task", "task-uid-1", "dispatch_work_order", argsA)
	for name, got := range map[string]string{
		"namespace": ApprovalID("other", "incident-task", "task-uid-1", "dispatch_work_order", argsA),
		"task":      ApprovalID("default", "other-task", "task-uid-1", "dispatch_work_order", argsA),
		"taskUID":   ApprovalID("default", "incident-task", "task-uid-2", "dispatch_work_order", argsA),
		"tool":      ApprovalID("default", "incident-task", "task-uid-1", "escalate_incident", argsA),
		"args":      ApprovalID("default", "incident-task", "task-uid-1", "dispatch_work_order", argsB),
	} {
		if got == base {
			t.Fatalf("ApprovalID did not change when %s changed", name)
		}
	}
}

func TestApprovalTargetPayloadDoesNotPersistRawSensitiveArgs(t *testing.T) {
	sensitiveKey := "api" + "Token"
	sensitiveValue := "sensitive" + "-value"
	rawArgs, err := json.Marshal(map[string]string{sensitiveKey: sensitiveValue, "safe": "ok"})
	if err != nil {
		t.Fatalf("marshal raw args: %v", err)
	}
	target, err := NewApprovalTarget("default", "task-1", "task-uid-1", "dispatch_work_order", rawArgs, "Dispatch work order", "Needs human", "warning")
	if err != nil {
		t.Fatalf("NewApprovalTarget() error = %v", err)
	}
	payload, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}
	if strings.Contains(string(payload), sensitiveValue) {
		t.Fatalf("target payload leaked raw args: %s", payload)
	}
	if !strings.Contains(string(payload), `"safe":"ok"`) {
		t.Fatalf("target payload did not include sanitized safe argument preview: %s", payload)
	}
	if target.TargetArgsDigest == "" || target.ApprovalID == "" {
		t.Fatalf("target missing digest/id: %#v", target)
	}
}

func TestApprovalTargetArgsPreviewIsBounded(t *testing.T) {
	large := strings.Repeat("x", maxApprovalTargetArgsPreviewBytes+1024)
	target, err := NewApprovalTarget(
		"default",
		"task-1",
		"task-uid-1",
		"dispatch_work_order",
		json.RawMessage(`{"note":"`+large+`"}`),
		"Dispatch work order",
		"Needs human",
		"warning",
	)
	if err != nil {
		t.Fatalf("NewApprovalTarget() error = %v", err)
	}
	if len(target.TargetArgsPreview) > maxApprovalTargetArgsPreviewBytes {
		t.Fatalf("TargetArgsPreview length = %d, want <= %d", len(target.TargetArgsPreview), maxApprovalTargetArgsPreviewBytes)
	}
	if !strings.Contains(string(target.TargetArgsPreview), "truncated") {
		t.Fatalf("TargetArgsPreview = %s, want truncation marker", target.TargetArgsPreview)
	}
}
