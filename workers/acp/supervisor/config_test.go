package supervisor

import (
	"path/filepath"
	"strings"
	"testing"
)

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
