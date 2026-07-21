// Package metrics exposes Prometheus counters for this provider's
// CloudProvider operations.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RecordDrift counts the number of drift detections grouped by reason
	// (ImageDrift, NetworkDrift, ...).
	RecordDrift = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "karpenter_hetzner_drift_total",
			Help: "Total number of Hetzner NodeClaim drift detections, labeled by reason.",
		},
		[]string{"reason"},
	)

	// RecordOperation is a generic operations counter labeled by operation
	// (create_instance, delete_instance, ...) and outcome (ok|error).
	RecordOperation = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "karpenter_hetzner_operations_total",
			Help: "Total number of Hetzner Cloud API operations, labeled by operation and outcome.",
		},
		[]string{"operation", "outcome"},
	)
)

func init() {
	metrics.Registry.MustRegister(RecordDrift, RecordOperation)
}
