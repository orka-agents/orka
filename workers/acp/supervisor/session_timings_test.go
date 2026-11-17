/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
)

func TestParseCPUStatReadsCgroupV2AndV1(t *testing.T) {
	v2, ok := parseCPUStat([]byte("usage_usec 100\nnr_periods 40\nnr_throttled 7\nthrottled_usec 2500\n"))
	if !ok || v2.throttledPeriods != 7 || v2.throttled != 2500*time.Microsecond {
		t.Fatalf("cgroup v2 sample = %+v, %v", v2, ok)
	}
	v1, ok := parseCPUStat([]byte("nr_periods 40\nnr_throttled 3\nthrottled_time 4000000\n"))
	if !ok || v1.throttledPeriods != 3 || v1.throttled != 4*time.Millisecond {
		t.Fatalf("cgroup v1 sample = %+v, %v", v1, ok)
	}
	if _, ok := parseCPUStat([]byte("usage_usec 100\nnr_periods 40\n")); ok {
		t.Fatal("cpu.stat without throttling counters was accepted")
	}
}

// Throttled times always fit a Duration: a count whose Duration would overflow
// saturates, and one beyond a signed 64-bit count is not reported.
func TestParseCPUStatBoundsThrottledTime(t *testing.T) {
	saturated, ok := parseCPUStat([]byte("nr_throttled 18446744073709551615\nthrottled_usec 9223372036854775807\n"))
	if !ok || saturated.throttledPeriods != math.MaxUint64 || saturated.throttled != time.Duration(math.MaxInt64) {
		t.Fatalf("saturated sample = %+v, %v", saturated, ok)
	}
	exact, ok := parseCPUStat([]byte("nr_throttled 1\nthrottled_time 9223372036854775807\n"))
	if !ok || exact.throttled != time.Duration(math.MaxInt64) {
		t.Fatalf("largest v1 sample = %+v, %v", exact, ok)
	}
	for _, stat := range []string{
		"nr_throttled 1\nthrottled_time 9223372036854775808\n",
		"nr_throttled 1\nthrottled_usec 18446744073709551615\n",
		"nr_throttled 1\nthrottled_usec -5\n",
	} {
		if sample, ok := parseCPUStat([]byte(stat)); ok {
			t.Fatalf("parseCPUStat(%q) = %+v, want no sample", stat, sample)
		}
	}
}

func TestReadCPUThrottleUsesFirstReadableFile(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "cpu.stat")
	if err := os.WriteFile(stat, []byte("nr_throttled 2\nthrottled_usec 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sample, ok := readCPUThrottle([]string{filepath.Join(dir, "missing"), stat})
	if !ok || sample.throttledPeriods != 2 {
		t.Fatalf("sample = %+v, %v", sample, ok)
	}
	if _, ok := readCPUThrottle([]string{filepath.Join(dir, "missing")}); ok {
		t.Fatal("missing cpu.stat was reported readable")
	}
}

// fakeClock advances by each step it is given.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestSessionStartTimingsRecordStagesAdapterAndThrottling(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	readings := []cpuThrottleSample{{throttledPeriods: 5, throttled: time.Second}, {throttledPeriods: 9, throttled: 3 * time.Second}}
	timings := newSessionStartTimingsWith(clock.Now, func() (cpuThrottleSample, bool) {
		sample := readings[0]
		readings = readings[1:]
		return sample, true
	})
	clock.now = clock.now.Add(120 * time.Millisecond)
	timings.mark("workspaceMaterialization")
	clock.now = clock.now.Add(3 * time.Second)
	timings.mark("adapterStartup")
	timings.recordAdapter(acp.StartupTimings{ProcessStart: 10 * time.Millisecond, Initialize: 2 * time.Second, ProviderSession: 900 * time.Millisecond})
	timings.finish()

	var buffer bytes.Buffer
	slog.New(slog.NewJSONHandler(&buffer, nil)).Info("created", timings.logAttrs()...)
	var line struct {
		TotalMs  int64            `json:"totalMs"`
		Stages   map[string]int64 `json:"stagesMs"`
		Adapter  map[string]int64 `json:"adapterMs"`
		Periods  uint64           `json:"cpuThrottledPeriods"`
		Throttle int64            `json:"cpuThrottledMs"`
	}
	if err := json.Unmarshal(buffer.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line.TotalMs != 3120 || line.Stages["workspaceMaterialization"] != 120 || line.Stages["adapterStartup"] != 3000 {
		t.Fatalf("log line = %s", buffer.String())
	}
	if line.Adapter["initialize"] != 2000 || line.Adapter["providerSession"] != 900 || line.Adapter["authenticate"] != 0 {
		t.Fatalf("adapter breakdown = %v", line.Adapter)
	}
	if line.Periods != 4 || line.Throttle != 2000 {
		t.Fatalf("throttling = %d periods, %d ms", line.Periods, line.Throttle)
	}

	// The span reports the same window: finish read the counters once.
	attrs := map[string]int64{}
	for _, attr := range timings.spanAttributes() {
		attrs[string(attr.Key)] = attr.Value.AsInt64()
	}
	if attrs["orka.acp.session_start.workspace_materialization_ms"] != 120 ||
		attrs["orka.acp.session_start.adapter.initialize_ms"] != 2000 ||
		attrs["orka.acp.session_start.cpu_throttled_periods"] != 4 {
		t.Fatalf("span attributes = %v", attrs)
	}
}

func TestSessionStartTimingsOmitUnknownThrottling(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	calls := 0
	timings := newSessionStartTimingsWith(clock.Now, func() (cpuThrottleSample, bool) {
		calls++
		if calls == 1 {
			return cpuThrottleSample{throttledPeriods: 9}, true
		}
		// A lower reading means a different cgroup: report nothing.
		return cpuThrottleSample{throttledPeriods: 1}, true
	})
	timings.finish()
	if _, _, ok := timings.throttleDelta(); ok {
		t.Fatal("backwards throttling counters were reported")
	}
	var buffer bytes.Buffer
	slog.New(slog.NewJSONHandler(&buffer, nil)).Info("created", timings.logAttrs()...)
	if bytes.Contains(buffer.Bytes(), []byte("cpuThrottled")) || bytes.Contains(buffer.Bytes(), []byte("adapterMs")) {
		t.Fatalf("log line = %s, want no throttling or adapter fields", buffer.String())
	}
}

// A creation that fails partway reports its total through finish, so a slow
// failing stage that never reached its mark still counts, with or without
// readable throttling counters.
func TestSessionStartTimingsTotalCoversFailedStage(t *testing.T) {
	for _, readable := range []bool{true, false} {
		clock := &fakeClock{now: time.Unix(1000, 0)}
		timings := newSessionStartTimingsWith(clock.Now, func() (cpuThrottleSample, bool) {
			return cpuThrottleSample{}, readable
		})
		clock.now = clock.now.Add(40 * time.Millisecond)
		timings.mark("sessionPaths")
		// Workspace materialization runs for five seconds and fails.
		clock.now = clock.now.Add(5 * time.Second)
		timings.finish()
		// Anything after finish is outside the window.
		clock.now = clock.now.Add(time.Hour)

		var buffer bytes.Buffer
		slog.New(slog.NewJSONHandler(&buffer, nil)).Info("failed", timings.logAttrs()...)
		var line struct {
			TotalMs int64            `json:"totalMs"`
			Stages  map[string]int64 `json:"stagesMs"`
		}
		if err := json.Unmarshal(buffer.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.TotalMs != 5040 || line.Stages["sessionPaths"] != 40 || len(line.Stages) != 1 {
			t.Fatalf("readable=%v log line = %s", readable, buffer.String())
		}
		for _, attr := range timings.spanAttributes() {
			if attr.Key == "orka.acp.session_start.total_ms" && attr.Value.AsInt64() != 5040 {
				t.Fatalf("readable=%v span total = %d, want 5040", readable, attr.Value.AsInt64())
			}
		}
	}
}

func TestSessionStartTimingsNilReceiverIsSafe(t *testing.T) {
	var timings *sessionStartTimings
	timings.mark("stage")
	timings.recordAdapter(acp.StartupTimings{})
	timings.finish()
	if timings.logAttrs() != nil || timings.spanAttributes() != nil {
		t.Fatal("nil timings produced attributes")
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{"sessionPaths": "session_paths", "adapterStartup": "adapter_startup", "durableCommit": "durable_commit"} {
		if got := snakeCase(in); got != want {
			t.Fatalf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// A real create logs one line with its stage breakdown and the adapter's own
// startup steps, and no request content.
func TestSupervisorCreateLogsSessionStartTimings(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	var line struct {
		Msg     string           `json:"msg"`
		Stages  map[string]int64 `json:"stagesMs"`
		Adapter map[string]int64 `json:"adapterMs"`
	}
	found := false
	for raw := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		if json.Unmarshal(raw, &line) == nil && line.Msg == "ACP runtime session created" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no session-created line in logs:\n%s", logs.String())
	}
	for _, stage := range []string{"sessionPaths", "workspaceMaterialization", "baselineCapture", "proxySetup", "providerConfiguration", "ownershipAndEnvironment", "adapterStartup"} {
		if _, ok := line.Stages[stage]; !ok {
			t.Fatalf("stages = %v, missing %s", line.Stages, stage)
		}
	}
	if _, ok := line.Adapter["initialize"]; !ok {
		t.Fatalf("adapter breakdown = %v", line.Adapter)
	}
}

// A create whose adapter handshake fails still logs the adapter stage and the
// handshake steps it completed, so a slow failed startup is diagnosable.
func TestSupervisorCreateFailureLogsAdapterTimings(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server, cfg, profile := newTestServer(t, adapterSessionNewFailureMode)
	create := testCreateSessionRequest(t, cfg, profile)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if response.Code == http.StatusCreated {
		t.Fatalf("create succeeded although session/new was rejected: %s", response.Body.String())
	}
	var line struct {
		Msg     string           `json:"msg"`
		Stage   string           `json:"stage"`
		TotalMs *int64           `json:"totalMs"`
		Stages  map[string]int64 `json:"stagesMs"`
		Adapter map[string]int64 `json:"adapterMs"`
	}
	found := false
	for raw := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		if json.Unmarshal(raw, &line) == nil && line.Msg == "ACP runtime session creation failed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no session-creation-failed line in logs:\n%s", logs.String())
	}
	if line.Stage != "provider adapter initialization" || line.TotalMs == nil {
		t.Fatalf("failure line = %+v", line)
	}
	if _, ok := line.Stages["adapterStartup"]; !ok {
		t.Fatalf("stages = %v, missing adapterStartup", line.Stages)
	}
	for _, step := range []string{"processStart", "initialize", "providerSession"} {
		if _, ok := line.Adapter[step]; !ok {
			t.Fatalf("adapter breakdown = %v, missing %s", line.Adapter, step)
		}
	}
}
