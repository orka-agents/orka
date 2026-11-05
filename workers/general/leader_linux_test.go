//go:build linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"os/exec"
	"testing"
	"time"
)

func TestLeaderAliveDistinguishesZombies(t *testing.T) {
	running := exec.Command("sleep", "30")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Process.Kill(); _ = running.Wait() })
	if !leaderAlive(running.Process.Pid) {
		t.Fatalf("a running leader was reported dead")
	}

	// Exited but not reaped: Wait has not been called, so the process is a zombie.
	exited := exec.Command("true")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for leaderAlive(exited.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaderAlive(exited.Process.Pid) {
		t.Fatalf("an exited, unreaped leader was reported alive")
	}
	if err := exited.Wait(); err != nil {
		t.Fatal(err)
	}
	if leaderAlive(exited.Process.Pid) {
		t.Fatalf("a reaped leader was reported alive")
	}
}
