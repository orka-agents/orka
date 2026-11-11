//go:build linux

package main

import (
	"bytes"
	"slices"
	"strings"
	"sync"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func TestCaptureBoundariesAndReadiness(t *testing.T) {
	notReady := newCapture("ready\n", nil)
	_, _ = notReady.Write([]byte("not-ready\n"))
	select {
	case <-notReady.ready:
		t.Fatal("a substring of unrelated output attested readiness")
	default:
	}
	overflows := 0
	capture := newCapture("ready\n", func() { overflows++ })
	_, _ = capture.Write([]byte("rea"))
	select {
	case <-capture.ready:
		t.Fatal("partial readiness was accepted")
	default:
	}
	_, _ = capture.Write([]byte("dy\n"))
	select {
	case <-capture.ready:
	default:
		t.Fatal("readiness split across reads was lost")
	}
	_, _ = capture.Write(bytes.Repeat([]byte("x"), pv.MaxOutputBytes-6))
	if _, truncated := capture.snapshot(); truncated {
		t.Fatal("exact output limit was truncated")
	}
	for range 3 {
		if count, err := capture.Write([]byte("extra")); count != 5 || err != nil {
			t.Fatal("overflow stopped draining the child stream")
		}
	}
	content, truncated := capture.snapshot()
	if !truncated || len(content) != pv.MaxOutputBytes || overflows != 1 {
		t.Fatal("capture is not bounded or signaled overflow repeatedly")
	}
}

func TestCaptureConcurrentSnapshots(t *testing.T) {
	capture := newCapture("", nil)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 1000 {
				_, _ = capture.Write([]byte("x"))
				_, _ = capture.snapshot()
			}
		})
	}
	workers.Wait()
	content, truncated := capture.snapshot()
	if len(content) != 4000 || truncated {
		t.Fatal("concurrent capture lost output")
	}
}

func TestChildEnvironmentAllowlist(t *testing.T) {
	t.Setenv("ORKA_VALIDATION_TASK_UID", "must-not-be-inherited")
	t.Setenv("LD_LIBRARY_PATH", "/uncontrolled")
	values, err := childEnvironment(map[string]string{"GOMAXPROCS": "2"}, "/private/role", false)
	if err != nil || !slices.IsSorted(values) {
		t.Fatalf("environment could not be frozen: %v", err)
	}
	joined := strings.Join(values, "\n")
	for _, forbidden := range []string{"must-not-be-inherited", "/uncontrolled", "ORKA_LISTEN_FD", "LD_LIBRARY_PATH"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("uncontrolled environment was inherited")
		}
	}
	for _, expected := range []string{"TMPDIR=/private/role", "HOME=/private/role", "GOTOOLCHAIN=local",
		"GOPROXY=off", "GOSUMDB=off", "GOCACHE=/private/role/go-build", "GOMAXPROCS=2"} {
		if !slices.Contains(values, expected) {
			t.Fatalf("missing allowlisted setting %q", expected)
		}
	}
	fixture, err := childEnvironment(nil, "/private/fixture", true)
	if err != nil || !slices.Contains(fixture, "ORKA_LISTEN_FD=3") {
		t.Fatal("fixture listener environment is missing")
	}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "ORKA_LISTEN_FD", "LD_PRELOAD"} {
		if _, err := childEnvironment(map[string]string{name: "override"}, "/private/role", false); err == nil {
			t.Fatal("uncontrolled environment override was accepted")
		}
	}
	if _, err := childEnvironment(map[string]string{"LANG": "x\x00y"}, "/private/role", false); err == nil {
		t.Fatal("NUL environment value was accepted")
	}
}
