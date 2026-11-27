/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"bufio"
	"bytes"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/orka-agents/orka/internal/acp"
)

var cgroupCPUStatPaths = []string{
	"/sys/fs/cgroup/cpu.stat",
	"/sys/fs/cgroup/cpu,cpuacct/cpu.stat",
	"/sys/fs/cgroup/cpu/cpu.stat",
}

type cpuThrottleSample struct {
	throttledPeriods uint64
	throttled        time.Duration
}

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

func parseCPUStat(data []byte) (cpuThrottleSample, bool) {
	var sample cpuThrottleSample
	periods, throttled := false, false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "nr_throttled":
			if parsed, err := strconv.ParseUint(value, 10, 64); err == nil {
				sample.throttledPeriods, periods = parsed, true
			}
		case "throttled_usec":
			if parsed, ok := parseThrottledTime(value, time.Microsecond); ok {
				sample.throttled, throttled = parsed, true
			}
		case "throttled_time":
			if parsed, ok := parseThrottledTime(value, time.Nanosecond); ok {
				sample.throttled, throttled = parsed, true
			}
		}
	}
	return sample, periods && throttled
}

func parseThrottledTime(value string, unit time.Duration) (time.Duration, bool) {
	count, err := strconv.ParseInt(value, 10, 64)
	if err != nil || count < 0 {
		return 0, false
	}
	if count > int64(math.MaxInt64/unit) {
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(count) * unit, true
}

type sessionStartStage struct {
	name     string
	duration time.Duration
}

type sessionStartTimings struct {
	now          func() time.Time
	readThrottle func() (cpuThrottleSample, bool)

	started      time.Time
	last         time.Time
	ended        time.Time
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

func (t *sessionStartTimings) mark(stage string) {
	if t == nil {
		return
	}
	now := t.now()
	t.stages = append(t.stages, sessionStartStage{name: stage, duration: now.Sub(t.last)})
	t.last = now
}

func (t *sessionStartTimings) recordAdapter(timings acp.StartupTimings) {
	if t == nil {
		return
	}
	t.adapter, t.adapterKnown = timings, true
}

func (t *sessionStartTimings) finish() {
	if t == nil {
		return
	}
	t.ended = t.now()
	if t.throttleStarted {
		t.throttleEnd, t.throttleEnded = t.readThrottle()
	}
}

func (t *sessionStartTimings) total() time.Duration {
	end := t.ended
	if end.IsZero() {
		end = t.last
	}
	return end.Sub(t.started)
}

func milliseconds(duration time.Duration) int64 { return duration.Milliseconds() }

func (t *sessionStartTimings) logAttrs() []any {
	if t == nil {
		return nil
	}
	attrs := []any{slog.Int64("totalMs", milliseconds(t.total()))}
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

func (t *sessionStartTimings) spanAttributes() []attribute.KeyValue {
	if t == nil {
		return nil
	}
	attrs := []attribute.KeyValue{attribute.Int64("orka.acp.session_start.total_ms", milliseconds(t.total()))}
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
