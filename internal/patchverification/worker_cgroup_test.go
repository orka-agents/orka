package patchverification

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerCgroupBoundsAreReadForEveryWorker(t *testing.T) {
	values := map[string]string{
		"/sys/fs/cgroup/memory.max":      "536870912",
		"/sys/fs/cgroup/pids.max":        "512",
		"/sys/fs/cgroup/memory.swap.max": "0",
		"/sys/fs/cgroup/cpu.max":         "100000 100000",
	}
	reads := map[string]int{}
	read := func(name string) ([]byte, error) {
		reads[name]++
		value, ok := values[name]
		if !ok {
			return nil, errors.New("missing kernel control")
		}
		return []byte(value), nil
	}
	require.NoError(t, checkWorkerCgroupBounds(read))
	for name := range values {
		require.Equal(t, 1, reads[name])
	}
	values["/sys/fs/cgroup/pids.max"] = "max"
	require.Error(t, checkWorkerCgroupBounds(read), "an earlier qualified worker must not authorize another unbounded worker")
	require.Equal(t, 2, reads["/sys/fs/cgroup/pids.max"])
	values["/sys/fs/cgroup/pids.max"] = "512"
	for _, name := range []string{"memory.max", "pids.max", "memory.swap.max", "cpu.max"} {
		path := "/sys/fs/cgroup/" + name
		original := values[path]
		delete(values, path)
		require.Error(t, checkWorkerCgroupBounds(read))
		values[path] = original
	}
	for path, value := range map[string]string{
		"/sys/fs/cgroup/memory.max":      "536870913",
		"/sys/fs/cgroup/pids.max":        "513",
		"/sys/fs/cgroup/memory.swap.max": "max",
		"/sys/fs/cgroup/cpu.max":         "max 100000",
	} {
		original := values[path]
		values[path] = value
		require.Error(t, checkWorkerCgroupBounds(read))
		values[path] = original
	}
}

func TestWorkerCgroupValuesRejectUnlimitedOrInvalidBounds(t *testing.T) {
	for _, value := range []string{"", "max", "-1", "0", "536870913", "18446744073709551616", "1024 1024"} {
		require.False(t, boundedCgroupValue(value, 512<<20), value)
	}
	for _, value := range []string{"1", "536870912\n", " 1024 "} {
		require.True(t, boundedCgroupValue(value, 512<<20), value)
	}
	for _, value := range []string{"", "max 100000", "0 100000", "100000 0", "100001 100000", "1", "1 1 1"} {
		require.False(t, boundedCgroupCPU(value), value)
	}
	for _, value := range []string{"100000 100000\n", "50000 100000", "1 1"} {
		require.True(t, boundedCgroupCPU(value), value)
	}
}
