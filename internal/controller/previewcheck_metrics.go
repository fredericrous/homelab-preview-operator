package controller

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// Metrics live on controller-runtime's own registry, so they are served by the
// manager's metrics endpoint alongside the controller-runtime ones rather than
// needing a second server.
//
// There is deliberately no `pr` label anywhere: PR numbers are unbounded, and a
// per-PR time series would turn a year of previews into thousands of dead
// series that Prometheus keeps forever.
var (
	// previewCheckTotal counts terminal verdicts. It is incremented exactly
	// once per PreviewCheck, immediately after the status write that moves a
	// non-terminal phase to a terminal one SUCCEEDS — a conflicted write
	// returns before counting, and its retry re-reads a status that is still
	// non-terminal, so there is no double count.
	previewCheckTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previewcheck_total",
		Help: "Terminal PreviewCheck verdicts by app, phase and reason.",
	}, []string{"app", "phase", "reason"})

	// previewCheckDuration measures startedAt to the terminal phase.
	previewCheckDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "previewcheck_duration_seconds",
		Help:    "Seconds from a PreviewCheck starting to reaching a terminal phase.",
		Buckets: []float64{30, 60, 120, 300, 600, 900, 1800, 3600},
	}, []string{"app"})

	// previewCheckCheckTotal counts individual checks reaching a terminal phase.
	previewCheckCheckTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previewcheck_check_total",
		Help: "Individual PreviewCheck checks reaching a terminal phase.",
	}, []string{"app", "check", "phase"})

	activeGaugeOnce sync.Once
)

func init() {
	metrics.Registry.MustRegister(previewCheckTotal, previewCheckDuration, previewCheckCheckTotal)
}

// registerActiveGauge publishes previewcheck_active, the number of
// non-terminal PreviewChecks.
//
// It is a GaugeFunc walking the informer rather than a Gauge the reconciler
// increments, because a counter of "currently running" things drifts the moment
// a CR is deleted without a final reconcile — and a restart would reset it to
// zero while the checks carried on. sync.Once guards it because registering the
// same collector twice panics, and SetupWithManager is called once per manager
// but tests build several.
//
// A List error returns 0 rather than blocking or failing the scrape: an
// operator that cannot serve /metrics because the API server is slow is worse
// than one that briefly reports zero active checks.
func registerActiveGauge(c client.Client) {
	activeGaugeOnce.Do(func() {
		metrics.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "previewcheck_active",
			Help: "PreviewChecks that have not reached a terminal phase.",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			list := &previewv1.PreviewCheckList{}
			if err := c.List(ctx, list); err != nil {
				return 0
			}
			active := 0
			for i := range list.Items {
				if !list.Items[i].Status.Phase.IsTerminal() {
					active++
				}
			}
			return float64(active)
		}))
	})
}
