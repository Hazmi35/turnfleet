// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// What a pass asks the work queue to do next is part of the controller's
// contract: too long a probe interval leaves a dead relay announced, a zero one
// spins the queue, and a missing retry leaves a pool stuck until something
// unrelated wakes it. Four outcomes exist and each is pinned below:
//
//   - ctrl.Result{}, nil — nothing to come back for (turnpool_controller.go:101
//     when the pool is gone; status.go:333 and status.go:361 on the deletion
//     path).
//   - RequeueAfter: secretRetryInterval — the auth Secret is unreadable, so
//     there is nothing to converge (turnpool_controller.go:154).
//   - RequeueAfter: drainPollInterval — a drain is in flight, either from a
//     scale-down (turnpool_controller.go:210 through requeueAfter,
//     :215-220) or from a deletion (status.go:402).
//   - RequeueAfter: probeInterval(pool) — steady state
//     (turnpool_controller.go:210, :222-227).
//
// There is no immediate requeue: `grep -rn 'Requeue:' internal/ cmd/` finds no
// `Requeue: true` outside a `RequeueAfter`, so a pass never asks to be re-run
// at once.
//
// The expected intervals are written as literals, not as the constants they
// mirror. Asserting a constant against itself is exactly how these contracts
// went untested — `requireRequeueAfter(..., secretRetryInterval)` passes
// whatever secretRetryInterval holds, so a retry retuned from 10s to 11s, or to
// an hour, fails nothing. Each literal names the constant it mirrors, so
// changing a policy interval means updating the number here in the same commit,
// which is the point.
//
// Every pool here gets a port range no other spec uses: the cross-pool identity
// check is cluster-wide and envtest never finishes terminating a namespace, so
// ranges shared with another test's leftover pool are reported as conflicts.

// requireRequeueAfter pins the interval a pass asked for and that it asked for
// nothing else. The whole result is compared, so an immediate requeue — the
// deprecated Requeue field, which no pass sets — or a priority fails this too,
// without reading the deprecated field to check it.
func requireRequeueAfter(t *testing.T, what string, result ctrl.Result, err error, want time.Duration) {
	t.Helper()

	if err != nil {
		t.Fatalf("%s: Reconcile() returned an error: %v", what, err)
	}
	if result != (ctrl.Result{RequeueAfter: want}) {
		t.Errorf("%s: Reconcile() asked for %+v, want RequeueAfter %s and nothing else", what, result, want)
	}
}

// requireNoRequeue pins a pass that asked for nothing.
func requireNoRequeue(t *testing.T, what string, result ctrl.Result, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("%s: Reconcile() returned an error: %v", what, err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("%s: Reconcile() asked for %+v, want nothing", what, result)
	}
}

// TestReconcileOnAMissingPoolAsksForNothing covers the first outcome: a request
// for a pool that is not there is not retried, it is dropped.
func TestReconcileOnAMissingPoolAsksForNothing(t *testing.T) {
	f := newFixture(t)

	result, err := f.reconcileResult("tpool-absent")
	requireNoRequeue(t, "reconciling a pool that does not exist", result, err)
}

// TestRequeueWhileTheAuthSecretIsMissing covers the retry that keeps a pool
// from needing a manual poke when its Secret is created: the pass reports the
// pool as degraded and comes back in secretRetryInterval. The second half pins
// that the pool leaves that path — and its interval — once the Secret is there.
func TestRequeueWhileTheAuthSecretIsMissing(t *testing.T) {
	const name = "tpool-requeue-secret"

	f := newFixture(t)
	f.createNode("tf-requeue-1", "203.0.113.71")
	// No f.createSecret(): the pool names a Secret that does not exist.
	f.createPool(hostNetworkPoolOn(name, 52500))

	result, err := f.reconcileResult(name)
	// 10s is secretRetryInterval (turnpool_controller.go:46).
	requireRequeueAfter(t, "reconciling without the auth Secret", result, err, 10*time.Second)

	if got := conditionStatus(f.pool(name), ConditionDegraded); got != string(metav1.ConditionTrue) {
		t.Errorf("Degraded = %q while the Secret is missing, want True", got)
	}
	if pods := f.pods(name); len(pods) != 0 {
		t.Errorf("%d relay pods were created without the auth Secret: a relay that cannot authenticate a client is worse than none", len(pods))
	}

	// The Secret appears: the pass that sees it must stop retrying at the
	// Secret's interval and move to the steady-state one.
	f.createSecret()
	result, err = f.reconcileResult(name)
	// 10s is relay.DefaultProbeInterval (internal/relay/peer.go:36).
	requireRequeueAfter(t, "reconciling once the auth Secret exists", result, err, 10*time.Second)
}

// TestRequeueInSteadyStateUsesTheDefaultProbeInterval pins the interval a
// healthy pool is re-checked at when it declares none of its own.
func TestRequeueInSteadyStateUsesTheDefaultProbeInterval(t *testing.T) {
	const name = "tpool-requeue-default"

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-requeue-2", "203.0.113.72")
	f.createPool(hostNetworkPoolOn(name, 52520))

	f.reconcile(name)
	f.markPodsReady(name)

	result, err := f.reconcileResult(name)
	// 10s is relay.DefaultProbeInterval (internal/relay/peer.go:36).
	requireRequeueAfter(t, "reconciling a healthy pool with no probe interval of its own", result, err, 10*time.Second)
}

// TestRequeueInSteadyStateFollowsTheSpecProbeInterval pins the other half: the
// interval is the pool's, not a constant the operator picked.
func TestRequeueInSteadyStateFollowsTheSpecProbeInterval(t *testing.T) {
	const name = "tpool-requeue-spec-interval"

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-requeue-3", "203.0.113.73")

	pool := hostNetworkPoolOn(name, 52540)
	pool.Spec.Probe = &turnfleetv1alpha1.ProbeSpec{Interval: &metav1.Duration{Duration: 7 * time.Second}}
	f.createPool(pool)

	f.reconcile(name)
	f.markPodsReady(name)

	result, err := f.reconcileResult(name)
	requireRequeueAfter(t, "reconciling a pool with a 7s probe interval", result, err, 7*time.Second)
}

// TestRequeueWhileDrainingAScaledDownPool covers the drain poll from a
// scale-down: a relay on its way out is re-evaluated every drainPollInterval,
// not every probe interval, because the pod has to go as soon as its
// allocations do. Once the drain is over the pool is back on the probe
// interval.
func TestRequeueWhileDrainingAScaledDownPool(t *testing.T) {
	const name = "tpool-requeue-drain"

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-requeue-4", "203.0.113.74")
	f.createNode("tf-requeue-5", "203.0.113.75")

	pool := hostNetworkPool(name, 2)
	pool.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 52560, End: 52587}
	pool.Spec.DrainTimeout = &metav1.Duration{Duration: 10 * time.Minute}
	f.createPool(pool)

	f.reconcile(name)
	f.reconcile(name)
	f.markPodsReady(name)
	f.reconcileUntil(name, "two ready relays", func() bool {
		return readyRelays(t, f, name) == 2
	})

	// The relays are busy, so the drain cannot finish inside one pass.
	f.scraper.allocations = 3
	latest := f.pool(name)
	latest.Spec.Replicas = 1
	if err := k8sClient.Update(f.ctx, latest); err != nil {
		t.Fatalf("scaling the pool down: %v", err)
	}

	f.reconcile(name)
	result, err := f.reconcileResult(name)
	// 1s is drainPollInterval (turnpool_controller.go:50).
	requireRequeueAfter(t, "reconciling while a relay drains", result, err, time.Second)

	// Let the drain finish: the pool is steady again and the interval widens.
	f.scraper.allocations = 0
	f.reconcileUntil(name, "the drained pod to be deleted", func() bool {
		return len(f.pods(name)) == 1
	})
	result, err = f.reconcileResult(name)
	requireRequeueAfter(t, "reconciling after the drain finished", result, err, 10*time.Second)
}

// TestRequeueWhileDeletingAPoolThatStillHasRelays covers the second drain poll,
// the one on the deletion path (status.go:402), and the pass that then asks for
// nothing: once the relays are gone the finalizer comes off and there is
// nothing left to come back for (status.go:361).
func TestRequeueWhileDeletingAPoolThatStillHasRelays(t *testing.T) {
	const name = "tpool-requeue-delete"

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-requeue-6", "203.0.113.76")
	f.createPool(hostNetworkPoolOn(name, 52580))

	f.reconcile(name)
	f.reconcile(name)
	f.markPodsReady(name)
	f.reconcileUntil(name, "one ready relay", func() bool {
		return readyRelays(t, f, name) == 1
	})

	// A client holds an allocation, so the relay may not be deleted yet.
	f.scraper.allocations = 2
	if err := k8sClient.Delete(f.ctx, f.pool(name)); err != nil {
		t.Fatalf("deleting the pool: %v", err)
	}

	result, err := f.reconcileResult(name)
	// 1s is drainPollInterval, the interval status.go:402 returns.
	requireRequeueAfter(t, "reconciling a deleted pool whose relay is still draining", result, err, time.Second)

	if !controllerutil.ContainsFinalizer(f.pool(name), FinalizerName) {
		t.Fatal("the drain finalizer was removed while a relay still held allocations")
	}

	// The allocation goes; the relay is deleted and the pool can finish.
	f.scraper.allocations = 0
	f.reconcileUntil(name, "the last relay to be deleted", func() bool {
		return len(f.pods(name)) == 0
	})

	result, err = f.reconcileResult(name)
	requireNoRequeue(t, "reconciling a drained pool whose finalizer can go", result, err)

	if err := k8sClient.Get(f.ctx, namespaced(f.namespace, name), &turnfleetv1alpha1.TURNPool{}); !apierrors.IsNotFound(err) {
		t.Errorf("the pool still exists after its finalizer was removed: err=%v", err)
	}
}

// TestReconcileIsIdempotentOnUnchangedState is the property the whole
// level-based design rests on: a pass over state nothing has changed must
// schedule the same work and write nothing. A pass that rewrote its own output
// every time would bump every resourceVersion in the namespace forever, and
// every watcher in it — including this controller — would reconcile in a loop.
//
// "Writes nothing" is checked where it can be observed: the resourceVersion of
// every object a pass can write. The pool and the relay configuration Secret
// are in the snapshot, as is the URI ConfigMap and the relay pods.
func TestReconcileIsIdempotentOnUnchangedState(t *testing.T) {
	const name = "tpool-requeue-idempotent"

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-requeue-7", "203.0.113.77")
	f.createPool(hostNetworkPoolOn(name, 52600))

	f.reconcile(name)
	f.reconcile(name)
	f.markPodsReady(name)
	f.reconcileUntil(name, "one ready relay", func() bool {
		return readyRelays(t, f, name) == 1
	})

	// The first of the two passes settles anything still in flight; the second
	// is the one that must be a no-op, and it must ask for exactly the same
	// work as the one before it.
	f.reconcile(name)
	before := f.objectVersions(name)

	// The snapshot has to be worth comparing: one entry per object a pass can
	// write, each of them really there. A snapshot that came back empty (or
	// with "absent" for an object the pass should have made) would let the
	// comparison below pass without checking anything.
	if len(before) != 4 {
		t.Fatalf("the snapshot holds %d objects (%v), want the pool, its pod, its relay configuration Secret and its URI ConfigMap", len(before), before)
	}
	for key, version := range before {
		if version == "absent" {
			t.Fatalf("%s does not exist before the steady-state pass, so its resourceVersion proves nothing: %v", key, before)
		}
	}
	t.Logf("steady-state resourceVersions: %v", before)

	// resourceVersion alone cannot show a write the API server discards: an
	// Update whose content is unchanged comes back with the same version and
	// writes nothing to etcd. The client sees the request, so the pass is held
	// to issuing none.
	mutationsBefore := f.api.count()

	first, err := f.reconcileResult(name)
	if err != nil {
		t.Fatalf("first steady-state pass failed: %v", err)
	}
	after := f.objectVersions(name)

	if first.RequeueAfter != 10*time.Second {
		t.Errorf("a steady-state pass asked to be re-run in %s, want the 10s probe interval", first.RequeueAfter)
	}
	for key, version := range before {
		if got := after[key]; got != version {
			t.Errorf("%s moved from resourceVersion %s to %s: a pass over unchanged state wrote it", key, version, got)
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			t.Errorf("%s appeared during a steady-state pass", key)
		}
	}
	if issued := f.api.since(mutationsBefore); len(issued) != 0 {
		t.Errorf("a steady-state pass issued %d mutating request(s): %v; a pass over state nothing changed must write nothing, or every watcher in the namespace reconciles again",
			len(issued), issued)
	}

	second, err := f.reconcileResult(name)
	if err != nil {
		t.Fatalf("second steady-state pass failed: %v", err)
	}
	if second != first {
		t.Errorf("two passes over the same state asked for %+v then %+v, want the same work", first, second)
	}
}

// TestCarryTransitionTimesKeepsOnlyUnchangedTransitionTimes pins the helper that
// makes a rebuilt status comparable, and with it the API's meaning of
// LastTransitionTime — the time a condition last transitioned. Both directions
// are checked: carrying the time of a condition that did change would freeze
// LastTransitionTime forever, which is the same bug from the other side.
func TestCarryTransitionTimesKeepsOnlyUnchangedTransitionTimes(t *testing.T) {
	t.Parallel()

	stored := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	previous := &turnfleetv1alpha1.TURNPoolStatus{Conditions: []metav1.Condition{
		{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "RelaysReady",
			Message:            "2 of 2 relays are ready",
			ObservedGeneration: 3,
			LastTransitionTime: metav1.NewTime(stored),
		},
		{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             "AsExpected",
			Message:            "every planned relay exists and is placeable",
			ObservedGeneration: 3,
			LastTransitionTime: metav1.NewTime(stored),
		},
	}}

	unchanged := metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "RelaysReady",
		Message: "2 of 2 relays are ready", ObservedGeneration: 3,
	}
	changed := metav1.Condition{
		Type: ConditionDegraded, Status: metav1.ConditionTrue, Reason: "RelayIdentityOverlap",
		Message: "another pool holds a relay on this pool's address", ObservedGeneration: 3,
	}
	fresh := metav1.Condition{
		Type: ConditionCapacityPressure, Status: metav1.ConditionFalse, Reason: "CapacityAvailable",
		Message: "0 of 48 ports in use across 1 relay(s)", ObservedGeneration: 3,
	}

	next := &turnfleetv1alpha1.TURNPoolStatus{Conditions: []metav1.Condition{unchanged, changed, fresh}}
	carryTransitionTimes(previous, next)

	if got := next.Conditions[0].LastTransitionTime.Time; !got.Equal(stored) {
		t.Errorf("an unchanged condition's transition time = %s, want the stored %s", got, stored)
	}
	if got := next.Conditions[1].LastTransitionTime; !got.IsZero() {
		t.Errorf("a changed condition's transition time = %s, want it left unset so meta.SetStatusCondition stamps the transition", got)
	}
	if got := next.Conditions[2].LastTransitionTime; !got.IsZero() {
		t.Errorf("a new condition's transition time = %s, want it left unset so meta.SetStatusCondition stamps it", got)
	}

	// The first pass of a pool's life has nothing to carry, and must not panic
	// on the nil status it reads.
	carryTransitionTimes(nil, &turnfleetv1alpha1.TURNPoolStatus{Conditions: []metav1.Condition{unchanged}})
}

// objectVersions snapshots the resourceVersion of everything one pass can
// write: the pool itself, its relay pods, its relay configuration Secret and
// its URI ConfigMap. A missing object is recorded as absent rather than
// failing, so a pass that creates one shows up as a change.
func (f *testFixture) objectVersions(poolName string) map[string]string {
	f.t.Helper()

	versions := map[string]string{}

	pool := &turnfleetv1alpha1.TURNPool{}
	if err := k8sClient.Get(f.ctx, namespaced(f.namespace, poolName), pool); err != nil {
		f.t.Fatalf("reading pool %s: %v", poolName, err)
	}
	versions["TURNPool/"+poolName] = pool.ResourceVersion

	pods := f.pods(poolName)
	for i := range pods {
		versions["Pod/"+pods[i].Name] = pods[i].ResourceVersion
	}

	secret := &corev1.Secret{}
	versions["Secret/"+RelayConfigSecretName(pool)] = "absent"
	if err := k8sClient.Get(f.ctx, namespaced(f.namespace, RelayConfigSecretName(pool)), secret); err == nil {
		versions["Secret/"+RelayConfigSecretName(pool)] = secret.ResourceVersion
	}

	configMap := &corev1.ConfigMap{}
	versions["ConfigMap/"+URIsConfigMapName(pool)] = "absent"
	if err := k8sClient.Get(f.ctx, namespaced(f.namespace, URIsConfigMapName(pool)), configMap); err == nil {
		versions["ConfigMap/"+URIsConfigMapName(pool)] = configMap.ResourceVersion
	}

	return versions
}
