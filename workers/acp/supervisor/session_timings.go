/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/orka-agents/orka/internal/acp"
)

// cgroupCPUStatPaths are the runtime container's CPU statistics: cgroup v2
// first, then the two common cgroup v1 mounts.
var cgroupCPUStatPaths = []string{
	"/sys/fs/cgroup/cpu.stat",
	"/sys/fs/cgroup/cpu,cpuacct/cpu.stat",
	"/sys/fs/cgroup/cpu/cpu.stat",
}

// cpuThrottleSample is one reading of the container's cumulative CPU
// throttling counters.
type cpuThrottleSample struct {
	throttledPeriods uint64
	throttled        time.Duration
}

// readCPUThrottle reads the first available cgroup cpu.stat. It reports false
// when none is readable, as on a runtime that is not in a cgroup with a CPU
// limit, so callers omit throttling rather than report zero.
func readCPUThrottle(paths []string) (cpuThrottleSample, bool) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if sample, ok := parseCPUStat(data); ok {
			return sample, true
		}
	}
	return cpuThrottleSample{}, false
}

// parseCPUStat reads nr_throttled and the throttled time from cpu.stat:
// throttled_usec under cgroup v2, throttled_time in nanoseconds under v1.
func parseCPUStat(data []byte) (cpuThrottleSample, bool) {
	var sample cpuThrottleSample
	periods, throttled := false, false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !found {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "nr_throttled":
			sample.throttledPeriods, periods = parsed, true
		case "throttled_usec":
			sample.throttled, throttled = time.Duration(parsed)*time.Microsecond, true
		case "throttled_time":
			sample.throttled, throttled = time.Duration(parsed), true
		}
	}
	return sample, periods && throttled
}

// sessionStartStage is one completed stage of a RuntimeSession creation.
type sessionStartStage struct {
	name     string
	duration time.Duration
}

// sessionStartTimings records how long each stage of one RuntimeSession
// creation takes, and the CPU throttling the runtime container saw over the
// same window. It holds stage names, durations and throttling counters only:
// never paths, credentials or content.
type sessionStartTimings struct {
	now          func() time.Time
	readThrottle func() (cpuThrottleSample, bool)

	started      time.Time
	last         time.Time
	stages       []sessionStartStage
	adapter      acp.StartupTimings
	adapterKnown bool

	throttleStart   cpuThrottleSample
	throttleStarted bool
	throttleEnd     cpuThrottleSample
	throttleEnded   bool
}

func newSessionStartTimings() *sessionStartTimings {
	return newSessionStartTimingsWith(time.Now, func() (cpuThrottleSample, bool) {
		return readCPUThrottle(cgroupCPUStatPaths)
	})
}

func newSessionStartTimingsWith(now func() time.Time, readThrottle func() (cpuThrottleSample, bool)) *sessionStartTimings {
	t := &sessionStartTimings{now: now, readThrottle: readThrottle}
	t.started = now()
	t.last = t.started
	t.throttleStart, t.throttleStarted = readThrottle()
	return t
}

// mark records the stage that just finished as the time since the previous
// mark. A nil receiver records nothing.
func (t *sessionStartTimings) mark(stage string) {
	if t == nil {
		return
	}
	now := t.now()
	t.stages = append(t.stages, sessionStartStage{name: stage, duration: now.Sub(t.last)})
	t.last = now
}

// recordAdapter keeps the adapter's own breakdown of its startup stage.
func (t *sessionStartTimings) recordAdapter(timings acp.StartupTimings) {
	if t == nil {
		return
	}
	t.adapter, t.adapterKnown = timings, true
}

// finish reads the throttling counters once more, so the log line and the
// span report the same window. A nil receiver records nothing.
func (t *sessionStartTimings) finish() {
	if t == nil || !t.throttleStarted {
		return
	}
	t.throttleEnd, t.throttleEnded = t.readThrottle()
}

func milliseconds(duration time.Duration) int64 { return duration.Milliseconds() }

// logAttrs returns the timings as slog attributes: the total, each stage in
// order, the adapter's breakdown when known, and the throttling delta when the
// container's cgroup was readable at both ends.
func (t *sessionStartTimings) logAttrs() []any {
	if t == nil {
		return nil
	}
	attrs := []any{slog.Int64("totalMs", milliseconds(t.last.Sub(t.started)))}
	stages := make([]any, 0, len(t.stages))
	for _, stage := range t.stages {
		stages = append(stages, slog.Int64(stage.name, milliseconds(stage.duration)))
	}
	attrs = append(attrs, slog.Group("stagesMs", stages...))
	if t.adapterKnown {
		attrs = append(attrs, slog.Group("adapterMs",
			slog.Int64("processStart", milliseconds(t.adapter.ProcessStart)),
			slog.Int64("initialize", milliseconds(t.adapter.Initialize)),
			slog.Int64("authenticate", milliseconds(t.adapter.Authenticate)),
			slog.Int64("providerSession", milliseconds(t.adapter.ProviderSession)),
		))
	}
	if periods, throttled, ok := t.throttleDelta(); ok {
		attrs = append(attrs, slog.Uint64("cpuThrottledPeriods", periods), slog.Int64("cpuThrottledMs", milliseconds(throttled)))
	}
	return attrs
}

// spanAttributes returns the same timings as span attributes.
func (t *sessionStartTimings) spanAttributes() []attribute.KeyValue {
	if t == nil {
		return nil
	}
	attrs := []attribute.KeyValue{attribute.Int64("orka.acp.session_start.total_ms", milliseconds(t.last.Sub(t.started)))}
	for _, stage := range t.stages {
		attrs = append(attrs, attribute.Int64("orka.acp.session_start."+snakeCase(stage.name)+"_ms", milliseconds(stage.duration)))
	}
	if t.adapterKnown {
		attrs = append(attrs,
			attribute.Int64("orka.acp.session_start.adapter.process_start_ms", milliseconds(t.adapter.ProcessStart)),
			attribute.Int64("orka.acp.session_start.adapter.initialize_ms", milliseconds(t.adapter.Initialize)),
			attribute.Int64("orka.acp.session_start.adapter.authenticate_ms", milliseconds(t.adapter.Authenticate)),
			attribute.Int64("orka.acp.session_start.adapter.provider_session_ms", milliseconds(t.adapter.ProviderSession)),
		)
	}
	if periods, throttled, ok := t.throttleDelta(); ok {
		attrs = append(attrs,
			attribute.Int64("orka.acp.session_start.cpu_throttled_periods", int64(min(periods, uint64(1<<62)))),
			attribute.Int64("orka.acp.session_start.cpu_throttled_ms", milliseconds(throttled)),
		)
	}
	return attrs
}

// throttleDelta returns the change in the throttling counters between the
// start and finish. Unread or backwards counters (a different cgroup) are
// reported as unknown.
func (t *sessionStartTimings) throttleDelta() (uint64, time.Duration, bool) {
	if !t.throttleStarted || !t.throttleEnded {
		return 0, 0, false
	}
	end := t.throttleEnd
	if end.throttledPeriods < t.throttleStart.throttledPeriods || end.throttled < t.throttleStart.throttled {
		return 0, 0, false
	}
	return end.throttledPeriods - t.throttleStart.throttledPeriods, end.throttled - t.throttleStart.throttled, true
}

// snakeCase turns a lowerCamel stage name into snake_case.
func snakeCase(name string) string {
	var builder strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				builder.WriteByte('_')
			}
			builder.WriteRune(r - 'A' + 'a')
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}
