package memory

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	memoryEnqueueTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_memory_operations_enqueued_total",
		Help: "Durable remote memory operations admitted by bounded kind.",
	}, []string{"kind"})
	memoryDispatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_memory_operation_dispatch_total",
		Help: "Remote memory dispatch outcomes by bounded class.",
	}, []string{"outcome"})
	memoryMaterializationLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "orka_memory_materialization_latency_seconds",
		Help:    "Time from durable operation admission to materialization.",
		Buckets: prometheus.DefBuckets,
	})
	memoryRetrievalLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "orka_memory_retrieval_latency_seconds",
		Help:    "Verified memory retrieval latency by bounded operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})
	memoryIncompleteTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "orka_memory_result_set_incomplete_total",
		Help: "Strict memory result sets rejected because bounded scan work was incomplete.",
	})
	memoryDivergenceTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_memory_materialization_issue_total",
		Help: "Verified memory materialization issues by bounded state.",
	}, []string{"state"})
)

func init() {
	metrics.Registry.MustRegister(
		memoryEnqueueTotal,
		memoryDispatchTotal,
		memoryMaterializationLatency,
		memoryRetrievalLatency,
		memoryIncompleteTotal,
		memoryDivergenceTotal,
	)
}
