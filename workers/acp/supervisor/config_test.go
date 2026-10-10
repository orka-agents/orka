package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
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

func TestConfigValidateStableWorkspaceKeyRequiresDedicatedPool(t *testing.T) {
	base, _ := newSessionIdentityTestConfig(t)
	base.DurableWorkspaceDir, base.DurableWorkspaceKey = t.TempDir(), "workspace"
	base.Capabilities.Limits.MaxResidentSessions = 1
	base.Capabilities.Limits.MaxConcurrentPrompts = 1
	if err := base.Validate(); err != nil {
		t.Fatalf("dedicated workspace config: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"without durable storage", func(cfg *Config) { cfg.DurableWorkspaceDir = "" }},
		{"unsafe directory key", func(cfg *Config) { cfg.DurableWorkspaceKey = "../another-workspace" }},
		{"multiple resident sessions", func(cfg *Config) { cfg.Capabilities.Limits.MaxResidentSessions = 2 }},
		{"multiple running prompts", func(cfg *Config) { cfg.Capabilities.Limits.MaxConcurrentPrompts = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.change(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid stable workspace config was accepted")
			}
		})
	}
}

func TestConfigValidateSandboxExecDeviceGroup(t *testing.T) {
	base, _ := newSessionIdentityTestConfig(t)
	allocator, err := acp.NewUIDAllocator(20000, 29999, 40000)
	if err != nil {
		t.Fatal(err)
	}
	base.UIDAllocator = allocator
	// Allocated identities stay forbidden too: a live session may own one.
	if _, _, err := allocator.AllocateAboveReserve(0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		gid       uint32
		wantError bool
	}{
		{"no supplementary group", 0, false},
		{"UID but not GID", 20000, false},
		{"below session range", 39999, false},
		{"first allocated group", 40000, true},
		{"future group", 45000, true},
		{"last group", 49999, true},
		{"above session range", 50000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.SandboxExec = &SandboxExecConfig{DeviceGID: tc.gid}
			err := cfg.Validate()
			if (err != nil) != tc.wantError || (tc.wantError && !strings.Contains(err.Error(), "session primary GID range")) {
				t.Fatalf("Validate() = %v, want overlap rejection = %t", err, tc.wantError)
			}
		})
	}
	base.SandboxExec = nil
	if err := base.Validate(); err != nil {
		t.Fatalf("sandbox disabled: %v", err)
	}
}

func TestNewRejectsSandboxExecDeviceGroupBeforeIdentitySetup(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	_, _, firstGID, _ := cfg.UIDAllocator.Range()
	cfg.SandboxExec = &SandboxExecConfig{DeviceGID: uint32(firstGID)}
	cfg.SessionBaseDir = filepath.Join(t.TempDir(), "uncreated-sessions")
	server, err := New(cfg)
	if err == nil || server != nil || !strings.Contains(err.Error(), "session primary GID range") {
		t.Fatalf("New() = %v, %v, want configuration rejection", server, err)
	}
	if _, err := os.Stat(cfg.SessionBaseDir); !os.IsNotExist(err) {
		t.Fatalf("invalid startup prepared identity directories: %v", err)
	}
}
