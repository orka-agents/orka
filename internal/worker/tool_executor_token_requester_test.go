/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestToolExecutorTokenRequester(t *testing.T) {
	t.Setenv(workerenv.ServiceAccountName, " orka-ai-worker ")
	if got := toolExecutorTokenRequester("tenant"); got != "tenant/orka-ai-worker" {
		t.Fatalf("toolExecutorTokenRequester() = %q, want tenant/orka-ai-worker", got)
	}
	if got := toolExecutorTokenRequester(""); got != "" {
		t.Fatalf("toolExecutorTokenRequester() without a namespace = %q, want empty", got)
	}
	t.Setenv(workerenv.ServiceAccountName, "")
	if got := toolExecutorTokenRequester("tenant"); got != "" {
		t.Fatalf("toolExecutorTokenRequester() without a ServiceAccount = %q, want empty", got)
	}
}
