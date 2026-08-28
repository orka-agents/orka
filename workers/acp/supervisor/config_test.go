package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type acceptingE2EPromptWriteFaultRecorder struct{}

func (acceptingE2EPromptWriteFaultRecorder) Consume(context.Context, harnessv2.MutationMetadata) (bool, error) {
	return true, nil
}

func TestConfigValidateRequiresExternalAmbiguityRecorderForDirectPool(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.E2EPromptWriteAmbiguityMarker = testE2EPromptWriteAmbiguityMarker
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "fault recorder is required") {
		t.Fatalf("Validate error = %v, want missing direct-pool recorder rejection", err)
	}
	cfg.E2EPromptWriteFaultRecorder = acceptingE2EPromptWriteFaultRecorder{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with direct-pool recorder: %v", err)
	}
}

func TestConfigValidateRejectsSessionBaseInsideDurableWorkspace(t *testing.T) {
	base, _ := newSessionIdentityTestConfig(t)
	durableRoot := t.TempDir()
	tests := []struct {
		name       string
		sessionDir string
	}{
		{name: "same directory", sessionDir: durableRoot},
		{name: "nested directory", sessionDir: filepath.Join(durableRoot, "sessions")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.DurableWorkspaceDir = durableRoot
			cfg.SessionBaseDir = tt.sessionDir
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "must not equal or be beneath") {
				t.Fatalf("Validate error = %v, want overlapping-directory rejection", err)
			}
		})
	}
}
