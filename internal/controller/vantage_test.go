// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/vantage"
)

// publishVantage writes a result into the pool's vantage ConfigMap, the way a
// runner does. The test is the runner here: the controller must read the
// published object and nothing else.
func (f *testFixture) publishVantage(poolName string, observedAt time.Time, results ...probe.Result) {
	f.t.Helper()

	f.publishResult(poolName, &vantage.Payload{Pool: poolName, ObservedAt: observedAt, Results: results})
}

// publishResult writes a payload into the pool's vantage ConfigMap, including
// payloads a well-behaved runner would never write.
func (f *testFixture) publishResult(poolName string, payload *vantage.Payload) {
	f.t.Helper()

	data, err := payload.Data()
	if err != nil {
		f.t.Fatalf("encoding the vantage result: %v", err)
	}

	name := vantage.ConfigMapName(poolName)
	configMap := &corev1.ConfigMap{}
	err = k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, configMap)
	if err != nil {
		configMap = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.namespace,
			Labels:    vantage.Labels(poolName),
		}}
		configMap.Data = data
		if err := k8sClient.Create(f.ctx, configMap); err != nil {
			f.t.Fatalf("publishing the vantage result: %v", err)
		}
		return
	}

	configMap.Data = data
	if err := k8sClient.Update(f.ctx, configMap); err != nil {
		f.t.Fatalf("republishing the vantage result: %v", err)
	}
}

// unpublishedVantage is a pool whose relay is ready and probed from inside the
// cluster, with nothing published for the vantage point yet.
func unpublishedVantage(t *testing.T, name string) *testFixture {
	t.Helper()

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.21")
	f.createPool(hostNetworkPool(name, 1))
	f.reconcileUntil(name, "the relay to be ready", func() bool {
		return conditionStatus(f.pool(name), ConditionReady) == string(metav1.ConditionTrue)
	})
	return f
}

// TestVantageResultCannotBeMaskedByTheInClusterProbe is the rule the delivery
// path exists for: a published failure stays in the status while the relay
// keeps answering a TURN Allocate from inside the cluster.
func TestVantageResultCannotBeMaskedByTheInClusterProbe(t *testing.T) {
	const name = "tpool-vantage"

	f := unpublishedVantage(t, name)

	pool := f.pool(name)
	if got := conditionStatus(pool, ConditionProbeSucceeded); got != string(metav1.ConditionTrue) {
		t.Fatalf("ProbeSucceeded = %s, want True before anything is published: the fixture pool is not healthy", got)
	}
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionUnknown) {
		t.Errorf("ReachableFromVantage = %s with nothing published, want Unknown: an in-cluster probe says nothing about the vantage point", got)
	}
	if len(pool.Status.TurnURIs) == 0 {
		t.Fatal("the pool publishes no turnURIs: there is nothing for a runner to probe")
	}
	uri := pool.Status.TurnURIs[0]

	f.publishVantage(name, f.clock.Now(), probe.Result{
		URI:     uri,
		Message: "cannot reach the relay listener: dial udp 203.0.113.21:3478: i/o timeout",
	})
	f.reconcile(name)

	pool = f.pool(name)
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionFalse) {
		t.Errorf("ReachableFromVantage = %s after a failing result was published, want False: %v",
			got, pool.Status.Conditions)
	}
	if got := conditionReason(pool, ConditionReachableFromVantage); got != reasonUnreachableFromVantage {
		t.Errorf("ReachableFromVantage reason = %s, want %s", got, reasonUnreachableFromVantage)
	}
	if message := conditionMessage(pool, ConditionReachableFromVantage); !strings.Contains(message, uri) {
		t.Errorf("ReachableFromVantage message %q does not name the listener that failed", message)
	}
	if got := conditionStatus(pool, ConditionDegraded); got != string(metav1.ConditionTrue) {
		t.Errorf("Degraded = %s while the vantage point cannot reach the pool, want True", got)
	}
	if got := conditionReason(pool, ConditionDegraded); got != reasonUnreachableFromVantage {
		t.Errorf("Degraded reason = %s, want %s: the vantage point is the only thing wrong", got, reasonUnreachableFromVantage)
	}

	// The in-cluster probe keeps passing, and the pool keeps publishing the
	// relay as usable. Neither clears the failing observation.
	for range 3 {
		f.clock.Advance(time.Second)
		f.reconcile(name)
	}
	pool = f.pool(name)
	if got := conditionStatus(pool, ConditionProbeSucceeded); got != string(metav1.ConditionTrue) {
		t.Errorf("ProbeSucceeded = %s, want True: the relay answers in-cluster", got)
	}
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionFalse) {
		t.Errorf("ReachableFromVantage = %s after further passes, want False: a passing in-cluster probe masked the vantage point", got)
	}
	if got := conditionStatus(pool, ConditionDegraded); got != string(metav1.ConditionTrue) {
		t.Errorf("Degraded = %s after further passes, want True", got)
	}

	// A passing result from the vantage point is what clears it, and only that.
	f.clock.Advance(time.Second)
	f.publishVantage(name, f.clock.Now(), probe.Result{
		URI:            uri,
		RelayedAddress: "203.0.113.21",
		RelayedPort:    49152,
		Succeeded:      true,
		Message:        "relay allocated and advertised the expected relayed address",
	})
	f.reconcile(name)

	pool = f.pool(name)
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionTrue) {
		t.Errorf("ReachableFromVantage = %s after the vantage point reported success, want True", got)
	}
	if got := conditionStatus(pool, ConditionDegraded); got != string(metav1.ConditionFalse) {
		t.Errorf("Degraded = %s after the vantage point reported success, want False", got)
	}
}

// TestVantageResultStaleness covers the other way a published result stops
// speaking for the pool: nobody is running the runner any more.
func TestVantageResultStaleness(t *testing.T) {
	const name = "tpool-vantage-stale"

	f := unpublishedVantage(t, name)
	uri := f.pool(name).Status.TurnURIs[0]

	f.publishVantage(name, f.clock.Now(), probe.Result{URI: uri, Succeeded: true, Message: "ok"})
	f.reconcile(name)
	if got := conditionStatus(f.pool(name), ConditionReachableFromVantage); got != string(metav1.ConditionTrue) {
		t.Fatalf("ReachableFromVantage = %s with a fresh result, want True", got)
	}

	// One missed pass is not a verdict; three are.
	f.clock.Advance(VantageResultMaxAge - time.Minute)
	f.reconcile(name)
	if got := conditionStatus(f.pool(name), ConditionReachableFromVantage); got != string(metav1.ConditionTrue) {
		t.Errorf("ReachableFromVantage = %s just inside the allowed age, want True", got)
	}

	f.clock.Advance(2 * time.Minute)
	f.reconcile(name)
	pool := f.pool(name)
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionUnknown) {
		t.Errorf("ReachableFromVantage = %s with a result older than %s, want Unknown: %v",
			got, VantageResultMaxAge, pool.Status.Conditions)
	}
	if got := conditionReason(pool, ConditionReachableFromVantage); got != reasonVantageResultStale {
		t.Errorf("ReachableFromVantage reason = %s, want %s", got, reasonVantageResultStale)
	}
	// Stale is not a failure: the pool is still doing what it can, it is the
	// observation that is out of date.
	if got := conditionStatus(pool, ConditionDegraded); got != string(metav1.ConditionFalse) {
		t.Errorf("Degraded = %s with only a stale vantage result, want False", got)
	}
}

// TestVantageResultItCannotUse covers the payloads the controller must not
// report as a verdict: one about another pool, and one with nothing in it.
func TestVantageResultItCannotUse(t *testing.T) {
	const name = "tpool-vantage-invalid"

	f := unpublishedVantage(t, name)
	uri := f.pool(name).Status.TurnURIs[0]

	f.publishResult(name, &vantage.Payload{
		Pool:       "another-pool",
		ObservedAt: f.clock.Now(),
		Results:    []probe.Result{{URI: uri, Succeeded: true, Message: "ok"}},
	})
	f.reconcile(name)
	pool := f.pool(name)
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionUnknown) {
		t.Errorf("ReachableFromVantage = %s for a result about another pool, want Unknown", got)
	}
	if got := conditionReason(pool, ConditionReachableFromVantage); got != reasonVantageResultInvalid {
		t.Errorf("ReachableFromVantage reason = %s, want %s", got, reasonVantageResultInvalid)
	}

	// A pass with nothing to probe is not evidence that anything is reachable.
	f.publishVantage(name, f.clock.Now())
	f.reconcile(name)
	pool = f.pool(name)
	if got := conditionStatus(pool, ConditionReachableFromVantage); got != string(metav1.ConditionUnknown) {
		t.Errorf("ReachableFromVantage = %s for a result with no listener URI, want Unknown", got)
	}
	if got := conditionReason(pool, ConditionReachableFromVantage); got != reasonNoVantageTargets {
		t.Errorf("ReachableFromVantage reason = %s, want %s", got, reasonNoVantageTargets)
	}
}

// TestVantagePoolIsMapFunc covers the watch: a published result must enqueue the
// pool it is about, and nothing else may.
func TestVantagePoolIsMapFunc(t *testing.T) {
	r := &TURNPoolReconciler{}

	requests := r.poolsForVantageResult(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vantage.ConfigMapName("demo"),
			Namespace: "turnfleet-test",
			Labels:    vantage.Labels("demo"),
		},
	})
	if len(requests) != 1 || requests[0].Name != "demo" || requests[0].Namespace != "turnfleet-test" {
		t.Errorf("a published result enqueued %+v, want the pool demo in turnfleet-test", requests)
	}

	// The pool's own URI ConfigMap carries different labels and must not be
	// mistaken for a published result.
	if requests := r.poolsForVantageResult(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-turn-uris",
			Namespace: "turnfleet-test",
			Labels:    Labels(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}),
		},
	}); len(requests) != 0 {
		t.Errorf("a relay ConfigMap enqueued %+v, want nothing", requests)
	}
}

// conditionReason and conditionMessage read one condition's fields.
func conditionReason(pool *turnfleetv1alpha1.TURNPool, conditionType string) string {
	for _, condition := range pool.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Reason
		}
	}
	return ""
}

func conditionMessage(pool *turnfleetv1alpha1.TURNPool, conditionType string) string {
	for _, condition := range pool.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Message
		}
	}
	return ""
}
