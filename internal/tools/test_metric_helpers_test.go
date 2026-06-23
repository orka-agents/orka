package tools

import "go.opentelemetry.io/otel/sdk/metric/metricdata"

func countMetricDataPoints(rm metricdata.ResourceMetrics, name string) int {
	count := 0
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[int64]:
				count += len(data.DataPoints)
			case metricdata.Histogram[float64]:
				count += len(data.DataPoints)
			}
		}
	}
	return count
}
