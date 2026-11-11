package patchverification

import (
	"os"
	"strconv"
	"strings"
)

type workerCgroupFailure uint8

const (
	workerCgroupMemory workerCgroupFailure = iota + 1
	workerCgroupPIDs
	workerCgroupSwap
	workerCgroupCPU
)

func (e workerCgroupFailure) Error() string {
	switch e {
	case workerCgroupMemory:
		return "worker requires a finite cgroup-v2 memory.max bound"
	case workerCgroupPIDs:
		return "worker requires a finite cgroup-v2 pids.max bound"
	case workerCgroupSwap:
		return "worker requires disabled cgroup-v2 swap"
	case workerCgroupCPU:
		return "worker requires finite cgroup-v2 CPU bandwidth"
	default:
		return "worker cgroup bounds are unavailable"
	}
}

func (e workerCgroupFailure) PreparationIsolationReason() string {
	switch e {
	case workerCgroupMemory:
		return "cgroup-memory"
	case workerCgroupPIDs:
		return "cgroup-pids"
	case workerCgroupSwap:
		return "cgroup-swap"
	case workerCgroupCPU:
		return "cgroup-cpu"
	default:
		return ""
	}
}

// CheckWorkerCgroupBounds reads the executing container's kernel controls on
// every invocation. RLIMIT_NPROC counts the real UID across containers, so it
// cannot replace this per-container descendant limit or be a safe low ceiling.
func CheckWorkerCgroupBounds() error {
	return checkWorkerCgroupBounds(os.ReadFile)
}

func checkWorkerCgroupBounds(readFile func(string) ([]byte, error)) error {
	for _, bound := range []struct {
		name    string
		maximum uint64
		failure workerCgroupFailure
	}{
		{"memory.max", 512 << 20, workerCgroupMemory},
		{"pids.max", 512, workerCgroupPIDs},
	} {
		value, err := readFile("/sys/fs/cgroup/" + bound.name)
		if err != nil || !boundedCgroupValue(string(value), bound.maximum) {
			return bound.failure
		}
	}
	swap, err := readFile("/sys/fs/cgroup/memory.swap.max")
	if err != nil || strings.TrimSpace(string(swap)) != "0" {
		return workerCgroupSwap
	}
	cpu, err := readFile("/sys/fs/cgroup/cpu.max")
	if err != nil || !boundedCgroupCPU(string(cpu)) {
		return workerCgroupCPU
	}
	return nil
}

func boundedCgroupValue(value string, maximum uint64) bool {
	number, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	return err == nil && number > 0 && number <= maximum
}

func boundedCgroupCPU(value string) bool {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return false
	}
	quota, qErr := strconv.ParseUint(fields[0], 10, 64)
	period, pErr := strconv.ParseUint(fields[1], 10, 64)
	return qErr == nil && pErr == nil && quota > 0 && period > 0 && quota <= period
}
