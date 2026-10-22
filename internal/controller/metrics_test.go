// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"testing"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gaugeValue reads one series of the controller's registry. It reports false
// when no sample carries that namespace, pool and relay.
func gaugeValue(t *testing.T, metricName, namespace, pool, relay string) (float64, bool) {
	t.Helper()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering the controller's metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, sample := range family.GetMetric() {
			labels := make(map[string]string, len(sample.GetLabel()))
			for _, label := range sample.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["namespace"] == namespace && labels["pool"] == pool && labels["relay"] == relay {
				return sample.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// TestProbeMetricsSurviveReconcile is the regression test for a pass that
// probed a relay and then wiped the probe gauges in the same pass, which left
// turnfleet_probe_success and turnfleet_probe_duration_seconds unscrapeable.
// The gauge is asserted after the probing pass and again after a later pass
// that only republishes the last result from the status.
func TestProbeMetricsSurviveReconcile(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-metrics-node", "203.0.113.71")
	f.createPool(hostNetworkPoolOn("tpool-probe-metrics", 52000))

	f.reconcile("tpool-probe-metrics")
	f.markPodsReady("tpool-probe-metrics")
	f.reconcile("tpool-probe-metrics")

	pods := f.pods("tpool-probe-metrics")
	if len(pods) != 1 {
		t.Fatalf("created %d relay pods, want 1", len(pods))
	}
	relayName := pods[0].Name

	assertProbeGauges := func(when string) {
		t.Helper()

		value, ok := gaugeValue(t, "turnfleet_probe_success", f.namespace, "tpool-probe-metrics", relayName)
		if !ok {
			t.Fatalf("turnfleet_probe_success for %s is not published %s", relayName, when)
		}
		if value != 1 {
			t.Errorf("turnfleet_probe_success for %s = %v, want 1: the probe succeeded", relayName, value)
		}
		if _, ok := gaugeValue(t, "turnfleet_probe_duration_seconds", f.namespace,
			"tpool-probe-metrics", relayName); !ok {
			t.Fatalf("turnfleet_probe_duration_seconds for %s is not published %s", relayName, when)
		}
	}

	assertProbeGauges("after the pass that probed the relay")

	// The fake clock has not advanced, so this pass is not due and republishes
	// the status's last probe result instead of measuring again.
	f.reconcile("tpool-probe-metrics")
	assertProbeGauges("after a pass that did not probe the relay")
}
