/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import "testing"

func TestNewAuditCmd(t *testing.T) {
	cmd := newAuditCmd()
	if cmd.Use != "audit" {
		t.Fatalf("Use = %q, want audit", cmd.Use)
	}
	if len(cmd.Commands()) != 1 || cmd.Commands()[0].Use != "trace <transaction-id>" {
		t.Fatalf("audit subcommands = %#v, want trace", cmd.Commands())
	}
}

func TestNewAuditTraceCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"audit", "trace", "--server", srv.URL, "txn-123"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}
