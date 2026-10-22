package storage

import (
	"fmt"
	"strings"
	"time"
)

var latencyBounds = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

func (m *MetricsTracker) ObserveLatency(duration time.Duration) {
	seconds := duration.Seconds()
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()
	m.latencyCount++
	m.latencySum += seconds
	for i, bound := range latencyBounds {
		if seconds <= bound {
			m.latencyBuckets[i]++
		}
	}
}

func (m *MetricsTracker) formatLatency() string {
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()
	var out strings.Builder
	out.WriteString("# HELP stiva_request_duration_seconds S3 request duration in seconds.\n# TYPE stiva_request_duration_seconds histogram\n")
	for i, bound := range latencyBounds {
		fmt.Fprintf(&out, "stiva_request_duration_seconds_bucket{le=\"%g\"} %d\n", bound, m.latencyBuckets[i])
	}
	fmt.Fprintf(&out, "stiva_request_duration_seconds_bucket{le=\"+Inf\"} %d\nstiva_request_duration_seconds_sum %g\nstiva_request_duration_seconds_count %d\n", m.latencyCount, m.latencySum, m.latencyCount)
	return out.String()
}
