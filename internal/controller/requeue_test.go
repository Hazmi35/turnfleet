// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"testing"
	"time"

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

// requireRequeueAfter pins the interval a pass asked for, and that it asked for
// nothing else: no immediate requeue and no error.
func requireRequeueAfter(t *testing.T, what string, result ctrl.Result, err error, want time.Duration) {
	t.Helper()

	if err != nil {
		t.Fatalf("%s: Reconcile() returned an error: %v", what, err)
	}
	if result.Requeue {
		t.Errorf("%s: Reconcile() asked for an immediate requeue as well as RequeueAfter %s", what, result.RequeueAfter)
	}
	if result.RequeueAfter != want {
		t.Errorf("%s: Reconcile() asked to be re-run in %s, want %s", what, result.RequeueAfter, want)
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
