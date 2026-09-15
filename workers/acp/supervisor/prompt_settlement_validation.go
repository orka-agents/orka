package supervisor

import (
	"context"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// awaitPromptTerminalValidation joins the stream owner's authoritative terminal
// validation, not the native provider's completion tombstone. That tombstone
// precedes queued identity/size validation and may therefore be unsuccessful at
// the supervisor boundary. No supervisor mutex is held while waiting.
//
// A deadline leaves the prompt unmodified: the cancellation operation returns a
// replayable, retryable error rather than freezing an unchecked success or an
// artificial terminal failure. A later operation may observe validated success.
func (s *Server) awaitPromptTerminalValidation(ctx context.Context, prompt *promptState) (harnessv2.PromptSettlement, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultDuration(s.cfg.CancelGrace, acp.DefaultStopGrace)*2)
	defer cancel()

	s.mu.Lock()
	if prompt.settlement != nil {
		settlement := *prompt.settlement
		s.mu.Unlock()
		return settlement, nil
	}
	done := prompt.terminalValidationDone
	s.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return harnessv2.PromptSettlement{}, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return *prompt.settlement, nil
}
