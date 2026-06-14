package cliwrapper

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
)

const testEchoCommand = "echo"

func TestNewRuntimeAdapterSelection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Generic.Command = testEchoCommand
	adapter, err := NewRuntimeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeAdapter(generic): %v", err)
	}
	if adapter.Name() != RuntimeGeneric {
		t.Fatalf("adapter.Name() = %s, want generic", adapter.Name())
	}

	cfg.Runtime = RuntimeCodex
	adapter, err = NewRuntimeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeAdapter(codex): %v", err)
	}
	if adapter.Name() != RuntimeCodex {
		t.Fatalf("adapter.Name() = %s, want codex", adapter.Name())
	}

	cfg.Runtime = "bogus"
	if _, err := NewRuntimeAdapter(cfg); err == nil || !strings.Contains(err.Error(), "unsupported runtime adapter") {
		t.Fatalf("NewRuntimeAdapter(bogus) error = %v, want unsupported", err)
	}
}

func TestGenericAdapterBuildCommandPromptModes(t *testing.T) {
	turn := TurnContext{Prompt: "hello", WorkDir: t.TempDir()}
	adapter := NewGenericAdapter(GenericAdapterConfig{Command: "cat", PromptMode: PromptModeStdin})
	spec, err := adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand(stdin): %v", err)
	}
	if string(spec.Stdin) != "hello" {
		t.Fatalf("stdin = %q, want prompt", string(spec.Stdin))
	}

	adapter = NewGenericAdapter(GenericAdapterConfig{
		Command:    "printenv",
		PromptMode: PromptModeEnv,
		PromptEnv:  "PROMPT_VALUE",
	})
	spec, err = adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand(env): %v", err)
	}
	if !containsEnv(spec.Env, "PROMPT_VALUE=hello") {
		t.Fatalf("env = %#v, want prompt env", spec.Env)
	}

	adapter = NewGenericAdapter(GenericAdapterConfig{Command: "cat", PromptMode: PromptModeFile, PromptEnv: "PROMPT_FILE"})
	spec, err = adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand(file): %v", err)
	}
	if len(spec.TempFiles) != 1 {
		t.Fatalf("TempFiles = %#v, want one prompt temp file", spec.TempFiles)
	}
	data, err := os.ReadFile(spec.TempFiles[0])
	if err != nil {
		t.Fatalf("read prompt temp file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("prompt file = %q, want prompt", string(data))
	}
	removeTempFiles(spec.TempFiles)
}

func TestGenericAdapterResultFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(path, []byte("file result"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    "ignored",
		ResultMode: ResultModeFile,
		ResultFile: "result.txt",
		WorkDir:    dir,
	})
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{Stdout: "stdout"})
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if result.Result != "file result" {
		t.Fatalf("Result = %q, want file result", result.Result)
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid runtime")
	}
	cfg = DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.StdoutLimitBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid stdout limit")
	}
}

func TestTurnContextFromRequestDoesNotDefaultWorkDirToRepo(t *testing.T) {
	request := validWrapperStartTurnRequest()
	turn := turnContextFromRequest(RuntimeGeneric, DefaultConfig(), request)
	if turn.WorkDir != "" {
		t.Fatalf("WorkDir = %q, want empty unless configured", turn.WorkDir)
	}
}

func validWrapperStartTurnRequest() harness.StartTurnRequest {
	return harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
		Input:            harness.TurnInput{Prompt: "hello"},
	}
}

func containsEnv(env []string, want string) bool {
	return slices.Contains(env, want)
}
