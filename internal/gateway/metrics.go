/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package gateway

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	gatewayIngressTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_gateway_ingress_total",
		Help: "Normalized gateway ingress outcomes.",
	}, []string{"result"})
	gatewayDispatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_gateway_dispatch_total",
		Help: "Gateway event dispatch outcomes.",
	}, []string{"result"})
	gatewayDeliveryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_gateway_delivery_total",
		Help: "Gateway delivery attempt outcomes.",
	}, []string{"result"})
	gatewayDeadLettersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orka_gateway_dead_letters_total",
		Help: "Gateway event and delivery dead letters.",
	}, []string{"kind"})
	gatewayQueueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orka_gateway_queue_depth",
		Help: "Current gateway event and delivery queue depth.",
	}, []string{"kind"})
	gatewayQueueOldestAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orka_gateway_queue_oldest_age_seconds",
		Help: "Age of the oldest pending gateway event or due delivery.",
	}, []string{"kind"})
	gatewayTaskDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "orka_gateway_task_duration_seconds",
		Help:    "Duration of Tasks created from gateway events.",
		Buckets: prometheus.DefBuckets,
	})
	gatewayDispatchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "orka_gateway_dispatch_latency_seconds",
		Help:    "Time from durable ingress receipt to deterministic Task linkage.",
		Buckets: prometheus.DefBuckets,
	})
	gatewayDeliveryLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "orka_gateway_delivery_latency_seconds",
		Help:    "Time from outbox creation to delivered adapter response.",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	metrics.Registry.MustRegister(
		gatewayIngressTotal,
		gatewayDispatchTotal,
		gatewayDeliveryTotal,
		gatewayDeadLettersTotal,
		gatewayQueueDepth,
		gatewayQueueOldestAge,
		gatewayTaskDuration,
		gatewayDispatchLatency,
		gatewayDeliveryLatency,
	)
}
