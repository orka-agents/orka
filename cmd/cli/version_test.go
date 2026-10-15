/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	for _, buildVersion := range []string{"dev", "v0.3.0", "v0.3.0-rc.1"} {
		t.Run(buildVersion, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.Version = buildVersion
			cmd.SetArgs([]string{"version"})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got, want := output.String(), "orka "+buildVersion+"\n"; got != want {
				t.Fatalf("version output = %q, want %q", got, want)
			}
		})
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"version", "unexpected"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	if err := cmd.Execute(); err == nil {
		t.Fatal("version accepted a positional argument")
	}
	if output.Len() != 0 {
		t.Fatalf("failed command printed a version: %q", output.String())
	}
}
