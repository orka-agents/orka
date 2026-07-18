/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"strings"
	"testing"
)

func TestRuntimeSessionMessageContentIncludesGatewayProvenance(t *testing.T) {
	message := SessionMessage{
		Role: "user", Content: "please investigate", SourceType: "gateway-event",
		Metadata: map[string]string{
			"senderId": "user-1", "senderDisplayName": "User One", "accountId": "acct",
			"contextId": "room", "threadId": "thread-1",
		},
	}
	got := RuntimeSessionMessageContent(message)
	for _, want := range []string{
		`senderId="user-1"`, `senderDisplayName="User One"`, `accountId="acct"`,
		`contextId="room"`, `threadId="thread-1"`, "please investigate",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("RuntimeSessionMessageContent() = %q, want %q", got, want)
		}
	}
}

func TestRuntimeSessionMessageContentLeavesNonGatewayMessagesUnchanged(t *testing.T) {
	message := SessionMessage{Role: "user", Content: "plain", Metadata: map[string]string{"senderId": "ignored"}}
	if got := RuntimeSessionMessageContent(message); got != message.Content {
		t.Fatalf("RuntimeSessionMessageContent() = %q, want %q", got, message.Content)
	}
}
