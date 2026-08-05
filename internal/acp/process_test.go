package acp

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupTestExecHelper()
	os.Exit(code)
}

func TestBoundedBufferKeepsTail(t *testing.T) {
	buffer := newBoundedBuffer(5)
	if _, err := buffer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("defg")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "[truncated]\ncdefg" {
		t.Fatalf("buffer = %q", got)
	}
}

func TestStartProcessRequiresStrictIdentityAndPaths(t *testing.T) {
	_, err := StartProcess(ProcessConfig{Command: "relative", UID: 1000, GID: 1000})
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("error = %v", err)
	}
	_, err = StartProcess(ProcessConfig{Command: "/bin/true", Paths: SessionPaths{Workspace: "/tmp"}, UID: 0, GID: 0})
	if err == nil || !strings.Contains(err.Error(), "non-root") {
		t.Fatalf("error = %v", err)
	}
}

func TestProcessNilWaitAndStop(t *testing.T) {
	var process *Process
	if err := process.Wait(context.Background()); err == nil {
		t.Fatal("nil process Wait unexpectedly succeeded")
	}
	status, err := process.Stop(context.Background(), time.Second)
	if err != nil || !status.Proven {
		t.Fatalf("nil process Stop = %#v, %v", status, err)
	}
}

func TestProveProcessCleanupRequiresTwoZeroObservations(t *testing.T) {
	var inventories atomic.Int32
	var reaps atomic.Int32
	status := proveProcessCleanup(
		1234,
		func(uid int) {
			if uid != 1234 {
				t.Errorf("reap UID = %d", uid)
			}
			reaps.Add(1)
		},
		func(uid int) ([]int, bool) {
			if uid != 1234 {
				t.Errorf("inventory UID = %d", uid)
			}
			inventories.Add(1)
			return nil, true
		},
		func() {},
	)
	if !status.Proven || len(status.RemainingPIDs) != 0 {
		t.Fatalf("cleanup status = %#v", status)
	}
	if got := inventories.Load(); got != 2 {
		t.Fatalf("inventory observations = %d, want 2", got)
	}
	if got := reaps.Load(); got != 2 {
		t.Fatalf("reap attempts = %d, want 2", got)
	}
}

func TestProveProcessCleanupRejectsProcessAppearingBetweenObservations(t *testing.T) {
	allowAppearance := make(chan struct{})
	appeared := make(chan struct{})
	var processPresent atomic.Bool
	go func() {
		<-allowAppearance
		processPresent.Store(true)
		close(appeared)
	}()

	status := proveProcessCleanup(
		1234,
		func(int) {},
		func(int) ([]int, bool) {
			if processPresent.Load() {
				return []int{4321}, true
			}
			return nil, true
		},
		func() {
			close(allowAppearance)
			<-appeared
		},
	)
	if status.Proven {
		t.Fatalf("cleanup unexpectedly proven: %#v", status)
	}
	if len(status.RemainingPIDs) != 1 || status.RemainingPIDs[0] != 4321 {
		t.Fatalf("remaining PIDs = %v", status.RemainingPIDs)
	}
}

func TestProveProcessCleanupRejectsUnprovenSecondObservation(t *testing.T) {
	var observations atomic.Int32
	status := proveProcessCleanup(
		1234,
		func(int) {},
		func(int) ([]int, bool) {
			if observations.Add(1) == 1 {
				return nil, true
			}
			return nil, false
		},
		func() {},
	)
	if status.Proven {
		t.Fatalf("cleanup unexpectedly proven: %#v", status)
	}
}
