package workspaceprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	types "k8s.io/apimachinery/pkg/types"
)

// InteractiveWorkspaceIdentity returns the deterministic identity input for an
// interactive workspace. sessionUID selects Session reuse; otherwise taskUID is
// used for a fresh per-Task workspace.
func InteractiveWorkspaceIdentity(namespace string, sessionUID, taskUID, classUID types.UID, slot string) string {
	if sessionUID == "" {
		return workspaceIdentity(namespace, "task", string(taskUID), string(classUID), "")
	}
	return workspaceIdentity(namespace, "session", string(sessionUID), string(classUID), slot)
}

// ServiceWorkspaceIdentity returns the deterministic identity input for a Tool-owned service workspace.
func ServiceWorkspaceIdentity(namespace string, toolUID, classUID types.UID) string {
	return workspaceIdentity(namespace, "tool", string(toolUID), string(classUID), "service")
}

// WorkspaceName converts a deterministic identity into a DNS-label-safe name.
func WorkspaceName(prefix, identity string) string {
	prefix = sanitizeDNSLabel(prefix)
	if prefix == "" {
		prefix = "workspace"
	}
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	sum := sha256.Sum256([]byte(identity))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}

func sanitizeDNSLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastHyphen := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			builder.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen && builder.Len() > 0 {
			builder.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func workspaceIdentity(namespace, ownerKind, ownerUID, classUID, slot string) string {
	return strings.Join([]string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(ownerKind),
		strings.TrimSpace(ownerUID),
		strings.TrimSpace(classUID),
		slot,
	}, "\x00")
}
