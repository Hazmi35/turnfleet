// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package metrics defines the turnfleet_ metric set and the coturn scrape that
// feeds it.
//
// Label cardinality is deliberately limited to namespace, pool and relay.
// Client addresses and usernames are never labels: they are unbounded and
// private.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/Hazmi35/turnfleet/internal/relay"
)

const (
	// metricNamespace prefixes every turnfleet metric.
	metricNamespace = "turnfleet"

	// labelNamespace, labelPool and labelRelay are the only labels the relay
	// metrics carry.
	labelNamespace = "namespace"
	labelPool      = "pool"
	labelRelay     = "relay"
)

// labelNames are the only labels the relay metrics carry.
//
//nolint:gochecknoglobals // shared label set for every collector
var labelNames = []string{labelNamespace, labelPool, labelRelay}

var (
	relayAllocations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "relay_allocations",
		Help:      "Current number of TURN allocations held by a relay, read from coturn's metrics endpoint.",
	}, labelNames)

	relayPortsInUse = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "relay_ports_in_use",
		Help:      "Relay ports currently held by allocations. One allocation occupies one port.",
	}, labelNames)

	relayPortsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "relay_ports_total",
		Help:      "Relay ports this relay owns. Capacity is this number, not the number of relays.",
	}, labelNames)

	allocationFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "allocation_failures_total",
		Help: "TURN allocation failures observed by the probe, by TURN error code. " +
			"code=\"508\" is Insufficient Capacity: the relay's port range is exhausted.",
	}, append(append([]string{}, labelNames...), "code"))

	probeSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "probe_success",
		Help:      "Whether the last TURN Allocate probe of a relay succeeded (1) or not (0).",
	}, labelNames)

	probeDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "probe_duration_seconds",
		Help:      "Duration of the last TURN Allocate probe of a relay.",
	}, labelNames)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		relayAllocations,
		relayPortsInUse,
		relayPortsTotal,
		allocationFailures,
		probeSuccess,
		probeDuration,
	)
}

// SetRelay publishes one relay's capacity and usage.
func SetRelay(namespace, pool, relayName string, allocations, portsInUse, portsTotal int32) {
	labels := prometheus.Labels{labelNamespace: namespace, labelPool: pool, labelRelay: relayName}
	relayAllocations.With(labels).Set(float64(allocations))
	relayPortsInUse.With(labels).Set(float64(portsInUse))
	relayPortsTotal.With(labels).Set(float64(portsTotal))
}

// SetProbe publishes the outcome of one relay probe.
func SetProbe(namespace, pool, relayName string, succeeded bool, duration time.Duration) {
	labels := prometheus.Labels{labelNamespace: namespace, labelPool: pool, labelRelay: relayName}
	value := 0.0
	if succeeded {
		value = 1.0
	}
	probeSuccess.With(labels).Set(value)
	probeDuration.With(labels).Set(duration.Seconds())
}

// RecordAllocationFailure counts a TURN allocation failure the probe observed.
//
// coturn exposes no per-error-code failure counters, so the probe is the source
// of truth for code 508 (Insufficient Capacity), which is the saturation signal.
func RecordAllocationFailure(namespace, pool, relayName string, code int) {
	allocationFailures.With(prometheus.Labels{
		labelNamespace: namespace,
		labelPool:      pool,
		labelRelay:     relayName,
		"code":         strconv.Itoa(code),
	}).Inc()
}

// ForgetPool drops the gauge series of a pool before the caller republishes the
// relays that still exist, so a deleted relay or pool leaves no stale sample
// behind.
//
// The allocation failure counter is deliberately left alone: it is cumulative,
// and the controller calls ForgetPool on every reconcile, so deleting it would
// reset the saturation signal to zero several times a minute. A counter that
// keeps its history across reconciles is the honest representation; the series
// disappears with the process.
func ForgetPool(namespace, pool string) {
	selector := prometheus.Labels{labelNamespace: namespace, labelPool: pool}
	relayAllocations.DeletePartialMatch(selector)
	relayPortsInUse.DeletePartialMatch(selector)
	relayPortsTotal.DeletePartialMatch(selector)
	probeSuccess.DeletePartialMatch(selector)
	probeDuration.DeletePartialMatch(selector)
}

// CoturnSample is what the controller reads out of coturn's own metrics.
type CoturnSample struct {
	// Allocations is the current allocation count, summed over coturn's socket
	// types.
	Allocations int32
	// Samples holds every metric coturn exposed, for diagnostics.
	Samples map[string]float64
}

// coturnAllocationMetric is coturn's gauge of current allocations, labelled by
// socket type. The exposition carries no coturn_ prefix:
// turn_total_allocations{type="UDP"} (verified against the pinned image).
const coturnAllocationMetric = "turn_total_allocations"

// ParseCoturnMetrics reads coturn's Prometheus text exposition format and sums
// the allocation gauge across its socket-type labels.
func ParseCoturnMetrics(r io.Reader) (CoturnSample, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return CoturnSample{}, fmt.Errorf("parsing coturn metrics: %w", err)
	}

	sample := CoturnSample{Samples: map[string]float64{}}
	for name, family := range families {
		for _, metric := range family.GetMetric() {
			value := metricValue(metric)
			sample.Samples[name] += value
			if name == coturnAllocationMetric {
				sample.Allocations += int32(value)
			}
		}
	}
	return sample, nil
}

// metricValue reads a sample regardless of whether coturn exposed it as a gauge
// or a counter: the pinned version uses gauges for allocations and counters for
// traffic.
func metricValue(metric *dto.Metric) float64 {
	switch {
	case metric.GetGauge() != nil:
		return metric.GetGauge().GetValue()
	case metric.GetCounter() != nil:
		return metric.GetCounter().GetValue()
	case metric.GetUntyped() != nil:
		return metric.GetUntyped().GetValue()
	default:
		return 0
	}
}

// ScrapeCoturn reads a relay's metrics endpoint.
//
// The scrape is deliberately short: a relay that cannot answer in time is
// reported as a scrape error and the controller keeps its last known counts
// rather than inventing capacity pressure out of a timeout.
func ScrapeCoturn(ctx context.Context, client *http.Client, url string) (CoturnSample, error) {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CoturnSample{}, fmt.Errorf("building metrics request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return CoturnSample{}, fmt.Errorf("scraping %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return CoturnSample{}, fmt.Errorf("scraping %s: unexpected status %s", url, resp.Status)
	}
	sample, err := ParseCoturnMetrics(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return CoturnSample{}, fmt.Errorf("scraping %s: %w", url, err)
	}
	return sample, nil
}

// MetricsURL builds the coturn metrics URL for a relay reachable at address.
func MetricsURL(address string, port int32, path string) (string, error) {
	if address == "" {
		return "", errors.New("relay address is empty")
	}
	if port == 0 {
		port = relay.DefaultPrometheusPort
	}
	if path == "" {
		path = relay.DefaultPrometheusPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://%s:%d%s", address, port, path), nil
}
