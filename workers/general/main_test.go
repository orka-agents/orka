/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestRun_Success(t *testing.T) {
	os.Args = []string{"worker", "echo", "hello"}
	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_NoCommand(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Unsetenv("MERCAN_COMMAND")
	err := run()
	if err == nil {
		t.Error("run() should return error when no command specified")
	}
}

func TestRun_CommandFromEnv(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Setenv("MERCAN_COMMAND", "echo hello")
	defer os.Unsetenv("MERCAN_COMMAND")

	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_CommandNotFound(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	// run() calls os.Exit for exec failures, so we test the underlying exec
	os.Args = []string{"worker", "nonexistent_command_12345"}
	err := run()
	if err == nil {
		t.Error("run() should return error for nonexistent command")
	}
	if _, ok := err.(*exec.Error); !ok {
		t.Errorf("expected *exec.Error, got %T", err)
	}
}
