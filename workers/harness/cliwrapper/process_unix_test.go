//go:build !windows

package cliwrapper

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestDrainChildCredentialProcessesKillsLateForksAndRequiresStableEmptyScans(t *testing.T) {
	type scanResult struct {
		pids []int
		err  error
	}
	scans := []scanResult{
		{pids: []int{101}},
		{pids: []int{101, 202}},
		{pids: []int{202}},
		{},
		{},
	}
	scanIndex := 0
	scan := func() ([]int, error) {
		if scanIndex >= len(scans) {
			t.Fatal("unexpected extra child credential process scan")
		}
		result := scans[scanIndex]
		scanIndex++
		return result.pids, result.err
	}
	type sentSignal struct {
		pid    int
		signal syscall.Signal
	}
	signals := []sentSignal{}
	signal := func(pid int, value syscall.Signal) error {
		signals = append(signals, sentSignal{pid: pid, signal: value})
		return nil
	}
	sleeps := []time.Duration{}

	err := drainChildCredentialProcesses(
		25*time.Millisecond,
		len(scans),
		scan,
		signal,
		func(duration time.Duration) { sleeps = append(sleeps, duration) },
	)
	if err != nil {
		t.Fatalf("drainChildCredentialProcesses: %v", err)
	}
	wantSignals := []sentSignal{
		{pid: 101, signal: syscall.SIGTERM},
		{pid: 101, signal: syscall.SIGKILL},
		{pid: 202, signal: syscall.SIGKILL},
		{pid: 202, signal: syscall.SIGKILL},
	}
	if len(signals) != len(wantSignals) {
		t.Fatalf("signals = %#v, want %#v", signals, wantSignals)
	}
	for i := range wantSignals {
		if signals[i] != wantSignals[i] {
			t.Fatalf("signal[%d] = %#v, want %#v", i, signals[i], wantSignals[i])
		}
	}
	if scanIndex != len(scans) {
		t.Fatalf("process scans = %d, want %d", scanIndex, len(scans))
	}
	if len(sleeps) == 0 || sleeps[0] != 25*time.Millisecond {
		t.Fatalf("first cleanup sleep = %v, want grace period", sleeps)
	}
}

func TestDrainChildCredentialProcessesFailsClosedWhenScansRemainIncomplete(t *testing.T) {
	scanErr := errors.New("proc scan unavailable")
	err := drainChildCredentialProcesses(
		0,
		3,
		func() ([]int, error) { return nil, scanErr },
		func(int, syscall.Signal) error { return nil },
		func(time.Duration) {},
	)
	if err == nil || !errors.Is(err, scanErr) {
		t.Fatalf("drainChildCredentialProcesses error = %v, want scan failure", err)
	}
}

func TestDrainChildCredentialProcessesFailsClosedWhenSignalFails(t *testing.T) {
	signalErr := errors.New("signal denied")
	err := drainChildCredentialProcesses(
		0,
		childCredentialDrainRequiredEmptyScans,
		func() ([]int, error) { return []int{101}, nil },
		func(int, syscall.Signal) error { return signalErr },
		func(time.Duration) {},
	)
	if err == nil || !errors.Is(err, signalErr) {
		t.Fatalf("drainChildCredentialProcesses error = %v, want signal failure", err)
	}
}

func TestProcessStatusUIDAndZombie(t *testing.T) {
	uid, zombie, err := processStatusUIDAndZombie([]byte(
		"State:\tZ (zombie)\nUid:\t1000\t1000\t1000\t1000\n",
	))
	if err != nil || uid != 1000 || !zombie {
		t.Fatalf("processStatusUIDAndZombie() = (%d, %t, %v), want (1000, true, nil)", uid, zombie, err)
	}
}
