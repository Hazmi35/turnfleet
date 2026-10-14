// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package vantage

import (
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Hazmi35/turnfleet/internal/probe"
)

func published(poolName string, payload *Payload, t *testing.T) *corev1.ConfigMap {
	t.Helper()

	data, err := payload.Data()
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(poolName), Namespace: "turnfleet-test", Labels: Labels(poolName)},
		Data:       data,
	}
}

// TestPayloadRoundTrip covers the contract between the runner that writes a
// result and the controller that reads it.
func TestPayloadRoundTrip(t *testing.T) {
	observedAt := time.Date(2026, 10, 13, 21, 4, 5, 0, time.UTC)
	payload := &Payload{
		Pool:       "demo",
		ObservedAt: observedAt,
		Results: []probe.Result{
			{URI: "turn:203.0.113.10:3478?transport=udp", Succeeded: true, RelayedAddress: "203.0.113.10", RelayedPort: 49152, Message: "ok"},
			{URI: "turn:203.0.113.11:3478?transport=udp", Message: "cannot reach the relay listener"},
		},
	}

	configMap := published("demo", payload, t)

	decoded, err := Parse(configMap, configMap.Data, "demo")
	if err != nil {
		t.Fatalf("parsing a published payload: %v", err)
	}
	if !decoded.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %s, want %s", decoded.ObservedAt, observedAt)
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("decoded %d results, want 2: %+v", len(decoded.Results), decoded.Results)
	}
	failed := decoded.Failures()
	if len(failed) != 1 || failed[0].URI != "turn:203.0.113.11:3478?transport=udp" {
		t.Errorf("Failures() = %+v, want only the second listener", failed)
	}

	poolName, ok := PoolForConfigMap(configMap)
	if !ok || poolName != "demo" {
		t.Errorf("PoolForConfigMap = %q, %t, want demo, true", poolName, ok)
	}
}

// TestParseRejectsUnusablePayloads covers what the controller must refuse
// rather than report as a verdict.
func TestParseRejectsUnusablePayloads(t *testing.T) {
	observedAt := time.Date(2026, 10, 13, 21, 4, 5, 0, time.UTC)

	tests := map[string]struct {
		poolName string
		object   *corev1.ConfigMap
		data     map[string]string
		wantErr  error
	}{
		"nothing published": {
			poolName: "demo",
			object:   &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName("demo")}},
			data:     map[string]string{DataKey: ""},
			wantErr:  ErrNoResult,
		},
		"no observation time": {
			poolName: "demo",
			object:   &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName("demo")}},
			data:     map[string]string{DataKey: `{"pool":"demo","results":[]}`},
		},
		"not json": {
			poolName: "demo",
			object:   &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName("demo")}},
			data:     map[string]string{DataKey: "{not json"},
		},
		"another pool": {
			poolName: "demo",
			object:   &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName("demo")}},
			data:     mustData(t, &Payload{Pool: "other", ObservedAt: observedAt}),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			payload, err := Parse(test.object, test.data, test.poolName)
			if err == nil {
				t.Fatalf("Parse accepted an unusable payload: %+v", payload)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Errorf("Parse error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

// TestPoolForConfigMapIgnoresOtherObjects covers the watch's filter: only a
// published result may map to a pool.
func TestPoolForConfigMapIgnoresOtherObjects(t *testing.T) {
	objects := map[string]*corev1.ConfigMap{
		"the pool's URI list": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "demo-turn-uris",
				Namespace: "turnfleet-test",
				Labels:    map[string]string{"app.kubernetes.io/name": "turnfleet", "app.kubernetes.io/instance": "demo", "app.kubernetes.io/component": "relay"},
			},
		},
		"a result named for another pool": {
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName("other"), Namespace: "turnfleet-test", Labels: Labels("demo")},
		},
		"an unlabelled ConfigMap": {
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName("demo"), Namespace: "turnfleet-test"},
		},
	}

	for name, object := range objects {
		if poolName, ok := PoolForConfigMap(object); ok {
			t.Errorf("%s mapped to pool %q, want no mapping", name, poolName)
		}
	}
}

func mustData(t *testing.T, payload *Payload) map[string]string {
	t.Helper()

	data, err := payload.Data()
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}
	return data
}
