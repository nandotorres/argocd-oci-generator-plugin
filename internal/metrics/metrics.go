// Package metrics exposes Prometheus instrumentation for the plugin.
//
// The two things worth alerting on are generation failures (which make Argo CD
// hold Applications unchanged) and request latency approaching the configured
// timeout, so both are first-class here. Registry calls are measured separately
// from total request time, which is what tells you whether slowness is ours or
// the registry's.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the collectors and the registry they are registered on.
type Metrics struct {
	registry *prometheus.Registry

	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	parameters      prometheus.Histogram

	registryRequests *prometheus.CounterVec
	registryDuration *prometheus.HistogramVec
}

// New builds the collectors on a dedicated registry, so the endpoint only
// exposes this process's metrics plus the standard Go/process collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocigen_getparams_requests_total",
			Help: "getparams.execute requests by HTTP status code.",
		}, []string{"code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ocigen_getparams_duration_seconds",
			Help:    "End-to-end getparams.execute duration. Compare against requestTimeoutSeconds.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"code"}),
		parameters: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ocigen_generated_parameters",
			Help:    "Parameter sets returned by a successful generation. A drop to zero means Applications will be pruned.",
			Buckets: []float64{0, 1, 2, 5, 10, 25, 50, 100, 250, 500},
		}),
		registryRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocigen_registry_requests_total",
			Help: "Calls to the upstream registry by operation and outcome.",
		}, []string{"operation", "outcome"}),
		registryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ocigen_registry_request_duration_seconds",
			Help:    "Upstream registry call duration by operation.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"operation"}),
	}

	reg.MustRegister(m.requests, m.requestDuration, m.parameters, m.registryRequests, m.registryDuration)
	return m
}

// Handler serves the metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// ObserveRequest records one getparams.execute request.
func (m *Metrics) ObserveRequest(status int, d time.Duration) {
	if m == nil {
		return
	}
	code := strconv.Itoa(status)
	m.requests.WithLabelValues(code).Inc()
	m.requestDuration.WithLabelValues(code).Observe(d.Seconds())
}

// ObserveParameters records how many parameter sets a successful generation
// produced.
func (m *Metrics) ObserveParameters(n int) {
	if m == nil {
		return
	}
	m.parameters.Observe(float64(n))
}

// ObserveRegistryCall records one upstream registry call. outcome is "success"
// or "error"; an absent repository counts as a success, because the registry
// answered.
func (m *Metrics) ObserveRegistryCall(operation string, err error, d time.Duration) {
	if m == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.registryRequests.WithLabelValues(operation, outcome).Inc()
	m.registryDuration.WithLabelValues(operation).Observe(d.Seconds())
}
