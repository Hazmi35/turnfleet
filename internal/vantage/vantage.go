// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package vantage carries the result a runner publishes for a pool and defines
// where it is published.
//
// The probe's own result (internal/probe) is one observation of one listener.
// A runner turns a pass over every listener URI a pool publishes into one
// Payload and writes it into the pool's vantage ConfigMap; the controller reads
// that ConfigMap and reports the outcome as a condition. The payload travels
// between two processes with no shared memory, so it is a versioned JSON
// document rather than a Go struct passed around.
//
// The publisher is deliberately not privileged: it writes a ConfigMap and
// nothing else. Where the runner runs is the operator's decision and the
// controller cannot verify it, so nothing here claims the observation came from
// outside the cluster; see docs/adr/0015-vantage-result-delivery.md.
package vantage

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Hazmi35/turnfleet/internal/probe"
)

// DataKey is the key inside the vantage ConfigMap that holds the payload.
const DataKey = "vantage.json"

// Labels of the vantage ConfigMap. The controller finds a pool's result by
// these labels, not by an owner reference: the runner creates the object, so
// there is no owner to set and nothing to garbage-collect it.
const (
	// componentLabel marks the ConfigMap as a published vantage result.
	componentLabel = "app.kubernetes.io/component"

	// componentLabelValue is the value the controller selects on.
	componentLabelValue = "vantage"

	// instanceLabel names the pool the result is about.
	instanceLabel = "app.kubernetes.io/instance"

	// nameLabel and managedByLabel identify the project and the writer.
	nameLabel      = "app.kubernetes.io/name"
	nameLabelValue = "turnfleet"
	managedByLabel = "app.kubernetes.io/managed-by"

	// managedByLabelValue names the binary that writes the object, not the
	// controller: the controller reads this ConfigMap and never writes it.
	managedByLabelValue = "turnfleet-probe"
)

// ErrNoResult reports that an object carries no published result at all, which
// is different from one that cannot be read.
var ErrNoResult = errors.New("no vantage result is published")

// ConfigMapName is the ConfigMap a pool's vantage result is published in.
func ConfigMapName(poolName string) string {
	return fmt.Sprintf("%s-vantage", poolName)
}

// Labels are the labels every published result carries.
func Labels(poolName string) map[string]string {
	return map[string]string{
		nameLabel:      nameLabelValue,
		instanceLabel:  poolName,
		componentLabel: componentLabelValue,
		managedByLabel: managedByLabelValue,
	}
}

// PoolForConfigMap reports the pool a published result belongs to, and whether
// the object is a published result at all. The controller uses it to map a
// ConfigMap event to the pool it must reconcile.
func PoolForConfigMap(object metav1.Object) (string, bool) {
	labels := object.GetLabels()
	if labels[nameLabel] != nameLabelValue || labels[componentLabel] != componentLabelValue {
		return "", false
	}
	poolName, ok := labels[instanceLabel]
	if !ok || poolName == "" || object.GetName() != ConfigMapName(poolName) {
		return "", false
	}
	return poolName, true
}

// Payload is one runner's pass over a pool's listeners.
type Payload struct {
	// Pool is the pool the pass was about. The controller refuses a payload
	// that names another pool, which is what a copied runner manifest produces.
	Pool string `json:"pool"`

	// ObservedAt is when the pass finished. The controller stops treating a
	// result as current once it is older than the age it allows, so a runner
	// that stopped running cannot leave "reachable" standing.
	ObservedAt time.Time `json:"observedAt"`

	// Results holds one entry per listener URI, in the order probed.
	Results []probe.Result `json:"results"`
}

// Failures returns the results that did not succeed.
func (p *Payload) Failures() []probe.Result {
	failed := make([]probe.Result, 0, len(p.Results))
	for _, result := range p.Results {
		if !result.Succeeded {
			failed = append(failed, result)
		}
	}
	return failed
}

// Data renders the payload as the ConfigMap's data. It is indented so that
// `kubectl get configmap -o yaml` is readable: the payload is what an operator
// looks at when a pool reports itself unreachable.
func (p *Payload) Data() (map[string]string, error) {
	encoded, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the vantage result: %w", err)
	}
	return map[string]string{DataKey: string(encoded)}, nil
}

// Parse reads a published payload out of a ConfigMap's data. It returns
// ErrNoResult when the ConfigMap carries nothing to read.
func Parse(object metav1.Object, data map[string]string, poolName string) (*Payload, error) {
	raw := data[DataKey]
	if raw == "" {
		return nil, ErrNoResult
	}

	payload := &Payload{}
	if err := json.Unmarshal([]byte(raw), payload); err != nil {
		return nil, fmt.Errorf("decoding the vantage result in ConfigMap %s/%s: %w",
			object.GetNamespace(), object.GetName(), err)
	}
	// A payload the controller cannot place in time is worse than no payload:
	// freshness is what keeps an old success from standing in for a current
	// one, and a result that names another pool would be reported for the
	// wrong one.
	if payload.ObservedAt.IsZero() {
		return nil, fmt.Errorf("the vantage result in ConfigMap %s/%s has no observation time",
			object.GetNamespace(), object.GetName())
	}
	if payload.Pool != poolName {
		return nil, fmt.Errorf("the vantage result in ConfigMap %s/%s is about pool %q, want %q",
			object.GetNamespace(), object.GetName(), payload.Pool, poolName)
	}
	return payload, nil
}
