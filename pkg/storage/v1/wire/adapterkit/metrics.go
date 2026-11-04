package adapterkit

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/orka-agents/orka/pkg/storage/v1/wire"
)

// durationBuckets are the upper bounds, in seconds, of the call duration
// histogram.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// metrics holds the adapter's metrics and writes them in the Prometheus text
// exposition format. Labels take only bounded values: method names from the
// protocol, gRPC codes, ErrorInfo reasons and credential names.
type metrics struct {
	mu        sync.Mutex
	requests  map[[3]string]uint64
	durations map[string]*histogram
	reloads   map[[2]string]uint64
	ready     bool
}

type histogram struct {
	buckets []uint64
	count   uint64
	sum     float64
}

func newMetrics() *metrics {
	return &metrics{
		requests:  map[[3]string]uint64{},
		durations: map[string]*histogram{},
		reloads:   map[[2]string]uint64{},
	}
}

// ObserveCall implements wire.CallObserver.
func (m *metrics) ObserveCall(call wire.CallInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[3]string{call.Method, call.Code.String(), call.Reason}]++
	h := m.durations[call.Method]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		m.durations[call.Method] = h
	}
	seconds := call.Duration.Seconds()
	for index, bound := range durationBuckets {
		if seconds <= bound {
			h.buckets[index]++
		}
	}
	h.count++
	h.sum += seconds
}

func (m *metrics) credentialReload(credential string, ok bool) {
	result := "success"
	if !ok {
		result = "failure"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloads[[2]string{credential, result}]++
}

func (m *metrics) setReady(ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = ready
}

// exposition renders the metrics in the text format. It holds the lock only
// while rendering, so a slow scrape never delays the calls that record metrics.
func (m *metrics) exposition() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out strings.Builder

	out.WriteString("# HELP orka_storage_server_requests_total Storage calls the adapter finished, by method, gRPC code and storage reason.\n")
	out.WriteString("# TYPE orka_storage_server_requests_total counter\n")
	requestKeys := make([][3]string, 0, len(m.requests))
	for key := range m.requests {
		requestKeys = append(requestKeys, key)
	}
	sort.Slice(requestKeys, func(i, j int) bool {
		return strings.Join(requestKeys[i][:], "\x00") < strings.Join(requestKeys[j][:], "\x00")
	})
	for _, key := range requestKeys {
		fmt.Fprintf(&out, "orka_storage_server_requests_total{method=%s,code=%s,reason=%s} %d\n",
			quote(key[0]), quote(key[1]), quote(key[2]), m.requests[key])
	}

	out.WriteString("# HELP orka_storage_server_request_duration_seconds Storage call duration, by method.\n")
	out.WriteString("# TYPE orka_storage_server_request_duration_seconds histogram\n")
	methods := make([]string, 0, len(m.durations))
	for method := range m.durations {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	for _, method := range methods {
		h := m.durations[method]
		for index, bound := range durationBuckets {
			fmt.Fprintf(&out, "orka_storage_server_request_duration_seconds_bucket{method=%s,le=%s} %d\n",
				quote(method), quote(formatFloat(bound)), h.buckets[index])
		}
		fmt.Fprintf(&out, "orka_storage_server_request_duration_seconds_bucket{method=%s,le=\"+Inf\"} %d\n", quote(method), h.count)
		fmt.Fprintf(&out, "orka_storage_server_request_duration_seconds_sum{method=%s} %s\n", quote(method), formatFloat(h.sum))
		fmt.Fprintf(&out, "orka_storage_server_request_duration_seconds_count{method=%s} %d\n", quote(method), h.count)
	}

	out.WriteString("# HELP orka_storage_server_credential_reloads_total Credential loads, by credential and result.\n")
	out.WriteString("# TYPE orka_storage_server_credential_reloads_total counter\n")
	reloadKeys := make([][2]string, 0, len(m.reloads))
	for key := range m.reloads {
		reloadKeys = append(reloadKeys, key)
	}
	sort.Slice(reloadKeys, func(i, j int) bool {
		return reloadKeys[i][0]+"\x00"+reloadKeys[i][1] < reloadKeys[j][0]+"\x00"+reloadKeys[j][1]
	})
	for _, key := range reloadKeys {
		fmt.Fprintf(&out, "orka_storage_server_credential_reloads_total{credential=%s,result=%s} %d\n",
			quote(key[0]), quote(key[1]), m.reloads[key])
	}

	out.WriteString("# HELP orka_storage_server_ready Whether the adapter's providers are ready to serve.\n")
	out.WriteString("# TYPE orka_storage_server_ready gauge\n")
	ready := 0
	if m.ready {
		ready = 1
	}
	fmt.Fprintf(&out, "orka_storage_server_ready %d\n", ready)
	return out.String()
}

// quote renders a label value with the escapes the text format requires.
func quote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value) + `"`
}

func formatFloat(value float64) string {
	if math.IsInf(value, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
