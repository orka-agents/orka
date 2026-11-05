//go:build !windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The Task's code can put anything under /tmp. A FIFO named like Chromium's
// port file must not hold up discovery, or stopping the live view would wait
// for it and the worker would never report the Task's result.
func TestLiveViewStopsWithAFIFOForAPortFile(t *testing.T) {
	setUpLiveView(t, "9", "http://127.0.0.1:1")
	fifo := filepath.Join(filepath.Dir(filepath.Dir(liveViewProfiles)), "fifo-profile")
	if err := os.Mkdir(fifo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fifo, "DevToolsActivePort"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := make(chan bool, 1)
	go func() {
		_, ok := devToolsPort(filepath.Join(fifo, "DevToolsActivePort"))
		read <- ok
	}()
	select {
	case ok := <-read:
		if ok {
			t.Fatal("read a port from a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO port file blocked")
	}

	stop := StartLiveView(t.Context())
	time.Sleep(5 * liveViewScanInterval)
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping the live view waited on a FIFO")
	}
}

func TestLiveViewReadsOnlyARegularPortFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(file, []byte("41234\n/devtools/browser/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if port, ok := devToolsPort(file); !ok || port != 41234 {
		t.Fatalf("devToolsPort = %d, %t; want 41234, true", port, ok)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	if port, ok := devToolsPort(link); ok {
		t.Fatalf("read port %d from a device", port)
	}
}
