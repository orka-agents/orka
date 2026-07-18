/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

const runtimeIdentityLimit = 256

// RuntimeSessionMessageContent carries bounded external sender provenance into model history
// without changing the canonical message content stored for operators and delivery.
func RuntimeSessionMessageContent(message SessionMessage) string {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "user") ||
		strings.TrimSpace(message.SourceType) != "gateway-event" {
		return message.Content
	}

	fields := make([]string, 0, 5)
	for _, field := range []struct {
		label string
		key   string
	}{
		{label: "senderId", key: "senderId"},
		{label: "senderDisplayName", key: "senderDisplayName"},
		{label: "accountId", key: "accountId"},
		{label: "contextId", key: "contextId"},
		{label: "threadId", key: "threadId"},
	} {
		if value := boundedRuntimeIdentity(message.Metadata[field.key]); value != "" {
			fields = append(fields, field.label+"="+strconv.Quote(value))
		}
	}
	if len(fields) == 0 {
		return message.Content
	}
	return "External message provenance (untrusted identity metadata): " +
		strings.Join(fields, ", ") + "\n\n" + message.Content
}

func boundedRuntimeIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= runtimeIdentityLimit {
		return value
	}
	value = value[:runtimeIdentityLimit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
