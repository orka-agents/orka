/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

type ExecutionWorkspaceRequest struct {
	TemplateName               string
	TemplateNamespace          string
	TemplateUID                string
	TemplateSuspendUnsupported bool

	SubstrateBootstrapSecretName string
	SubstrateBootstrapSecretKey  string
}

func deterministicSubstratePoolActorID(prefix string, ordinal int) string {
	return fmt.Sprintf("%s-%05d", strings.Trim(strings.TrimSpace(prefix), "-"), ordinal)
}

func deterministicSubstratePoolActorOrdinal(target int32, parts ...string) int {
	if target <= 0 {
		return 0
	}
	trimmedParts := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmedParts = append(trimmedParts, strings.TrimSpace(part))
	}
	sum := sha256.Sum256([]byte(strings.Join(trimmedParts, "\x00")))
	ordinal := 0
	for _, b := range sum {
		ordinal = (ordinal*256 + int(b)) % int(target)
	}
	return ordinal
}
