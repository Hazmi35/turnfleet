// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// externalPool is a one-relay pool holding a declared external address on a
// node. Two pools built this way with the same address are the shape that
// produces a cross-pool conflict: one public address in front of two nodes, or
// one secondary address declared by two pools.
func externalPool(name, node, address string) *turnfleetv1alpha1.TURNPool {
	pool := hostNetworkPool(name, 1)
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
		ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{{NodeName: node, Address: address}},
		},
	}
	return pool
}

// requireCondition returns the pool's condition of that type, after checking
// its status.
func requireCondition(t *testing.T, pool *turnfleetv1alpha1.TURNPool, conditionType, status string) metav1.Condition {
	t.Helper()

	if pool.Status == nil {
		t.Fatalf("pool %s has no status", pool.Name)
	}
	for _, condition := range pool.Status.Conditions {
		if condition.Type != conditionType {
			continue
		}
		if string(condition.Status) != status {
			t.Errorf("pool %s: %s is %s, want %s: %s",
				pool.Name, conditionType, condition.Status, status, condition.Message)
		}
		if condition.Reason == "" {
			t.Errorf("pool %s: %s carries no reason", pool.Name, conditionType)
		}
		if condition.ObservedGeneration != pool.Generation {
			t.Errorf("pool %s: %s was observed at generation %d, want %d",
				pool.Name, conditionType, condition.ObservedGeneration, pool.Generation)
		}
		return condition
	}
	t.Fatalf("pool %s has no %s condition: %+v", pool.Name, conditionType, pool.Status.Conditions)
	return metav1.Condition{}
}

// TestCrossPoolAddressConflict covers the state no admission webhook can
// reject: two pools holding the same relay address with overlapping ports.
// Each pool reports it on its own status, and each clears it once the other
// pool stops holding that address.
func TestCrossPoolAddressConflict(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "10.0.0.11")
	f.createNode("tf-worker-2", "10.0.0.12")

	f.createPool(externalPool("tpool-a", "tf-worker-1", "198.51.100.11"))
	f.createPool(externalPool("tpool-b", "tf-worker-2", "198.51.100.11"))

	// Each pool compares its own relays with the identities the other pools
	// publish, so the pair sees each other once both have a status.
	f.reconcile("tpool-a")
	f.reconcile("tpool-b")
	f.reconcile("tpool-a")
	f.reconcile("tpool-b")

	for _, name := range []string{"tpool-a", "tpool-b"} {
		pool := f.pool(name)
		conflict := requireCondition(t, pool, ConditionAddressConflict, "True")
		if conflict.Reason != "RelayIdentityOverlap" {
			t.Errorf("pool %s: AddressConflict reason is %q, want RelayIdentityOverlap", name, conflict.Reason)
		}
		if !strings.Contains(conflict.Message, "198.51.100.11") {
			t.Errorf("pool %s: AddressConflict message names no address: %q", name, conflict.Message)
		}
		// The other pool is the actionable half of the message.
		other := "tpool-b"
		if name == "tpool-b" {
			other = "tpool-a"
		}
		if !strings.Contains(conflict.Message, other) {
			t.Errorf("pool %s: AddressConflict message does not name %s: %q", name, other, conflict.Message)
		}
		requireCondition(t, pool, ConditionDegraded, "True")
	}

	// Pool b moves off the shared address. Declared addresses may be added and
	// removed, so this is the move the message asks for; the relay that held
	// the address drains first.
	latest := f.pool("tpool-b")
	latest.Spec.Addressing.ExternalIPPool.Addresses = []turnfleetv1alpha1.ExternalAddress{
		{NodeName: "tf-worker-2", Address: "198.51.100.20"},
	}
	if err := k8sClient.Update(f.ctx, latest); err != nil {
		t.Fatalf("moving pool b off the shared address: %v", err)
	}

	f.reconcileUntil("tpool-b", "pool b holds only its new address", func() bool {
		held := heldRelays(f.pool("tpool-b"))
		return len(held) == 1 && held[0].address == "198.51.100.20"
	})
	requireCondition(t, f.pool("tpool-b"), ConditionAddressConflict, "False")
	requireCondition(t, f.pool("tpool-b"), ConditionDegraded, "False")

	// Pool a has not changed: its next pass has to clear the condition it was
	// reporting.
	f.reconcile("tpool-a")
	requireCondition(t, f.pool("tpool-a"), ConditionAddressConflict, "False")
	requireCondition(t, f.pool("tpool-a"), ConditionDegraded, "False")
}

// TestDisjointRangesOnOneAddressAreLegal pins the other half of the rule: two
// pools may share a relay address as long as their ranges do not overlap, which
// is what PartitionedIP mode does inside one pool.
func TestDisjointRangesOnOneAddressAreLegal(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "10.0.0.11")
	f.createNode("tf-worker-2", "10.0.0.12")

	f.createPool(externalPool("tpool-a", "tf-worker-1", "198.51.100.11"))

	other := externalPool("tpool-b", "tf-worker-2", "198.51.100.11")
	other.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 49200, End: 49247}
	f.createPool(other)

	f.reconcile("tpool-a")
	f.reconcile("tpool-b")
	f.reconcile("tpool-a")

	for _, name := range []string{"tpool-a", "tpool-b"} {
		requireCondition(t, f.pool(name), ConditionAddressConflict, "False")
		requireCondition(t, f.pool(name), ConditionDegraded, "False")
	}
}

// TestPoolsForHeldAddresses covers the watch that clears a conflict: the pool
// that has to react is the one that did not change, so a change in another
// pool's held addresses has to wake it.
func TestPoolsForHeldAddresses(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "10.0.0.11")
	f.createNode("tf-worker-2", "10.0.0.12")
	f.createNode("tf-worker-3", "10.0.0.13")

	f.createPool(externalPool("tpool-a", "tf-worker-1", "198.51.100.11"))
	f.createPool(externalPool("tpool-b", "tf-worker-2", "198.51.100.11"))
	f.createPool(externalPool("tpool-c", "tf-worker-3", "198.51.100.30"))

	for _, name := range []string{"tpool-a", "tpool-b", "tpool-c"} {
		f.reconcile(name)
	}

	requests := f.reconciler.poolsForHeldAddresses(f.ctx, f.pool("tpool-a"))
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: f.namespace, Name: "tpool-b"}},
	}
	if len(requests) != 1 || requests[0] != want[0] {
		t.Errorf("pool a's address wakes %v, want only pool b", requests)
	}

	if requests := f.reconciler.poolsForHeldAddresses(f.ctx, f.pool("tpool-c")); len(requests) != 0 {
		t.Errorf("a pool on its own address wakes %v, want nobody", requests)
	}

	// A pool that holds nothing is nobody's peer.
	empty := externalPool("tpool-empty", "tf-worker-1", "198.51.100.11")
	if requests := f.reconciler.poolsForHeldAddresses(f.ctx, empty); len(requests) != 0 {
		t.Errorf("a pool with no status wakes %v, want nobody", requests)
	}
}

// TestHeldIdentitiesChanged pins what wakes the peers: a change in the relay
// identities a pool holds, and nothing else. Waking on every status write would
// put every pool that shares an address on the probe interval.
func TestHeldIdentitiesChanged(t *testing.T) {
	poolWith := func(probeSucceeded bool, relays ...turnfleetv1alpha1.RelayStatus) *turnfleetv1alpha1.TURNPool {
		pool := hostNetworkPool("tpool-a", 1)
		pool.Status = &turnfleetv1alpha1.TURNPoolStatus{
			Relays: relays,
			Conditions: []metav1.Condition{{
				Type:    ConditionAddressConflict,
				Status:  metav1.ConditionFalse,
				Reason:  "NoOverlap",
				Message: "nothing overlaps",
			}},
		}
		if probeSucceeded {
			relayStatus := &pool.Status.Relays[0]
			relayStatus.LastProbe = &turnfleetv1alpha1.ProbeStatus{Succeeded: new(true)}
		}
		return pool
	}
	relay := turnfleetv1alpha1.RelayStatus{
		Name:         "tpool-a-relay-abcde",
		RelayAddress: new("198.51.100.11"),
		PortRange:    turnfleetv1alpha1.PortRange{Start: 49152, End: 49199},
	}
	moved := relay
	moved.PortRange = turnfleetv1alpha1.PortRange{Start: 49200, End: 49247}

	tests := []struct {
		name string
		old  *turnfleetv1alpha1.TURNPool
		new  *turnfleetv1alpha1.TURNPool
		want bool
	}{
		{
			name: "a probe result changes",
			old:  poolWith(false, relay),
			new:  poolWith(true, relay),
			want: false,
		},
		{
			name: "a relay appears",
			old:  poolWith(false),
			new:  poolWith(false, relay),
			want: true,
		},
		{
			name: "a relay disappears",
			old:  poolWith(false, relay),
			new:  poolWith(false),
			want: true,
		},
		{
			name: "a relay moves to another range",
			old:  poolWith(false, relay),
			new:  poolWith(false, moved),
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := heldIdentitiesChanged().Update(event.UpdateEvent{ObjectOld: test.old, ObjectNew: test.new})
			if got != test.want {
				t.Errorf("heldIdentitiesChanged = %v, want %v", got, test.want)
			}
		})
	}

	if !heldIdentitiesChanged().Delete(event.DeleteEvent{Object: poolWith(false, relay)}) {
		t.Error("deleting a pool that holds relays wakes nobody")
	}
	if heldIdentitiesChanged().Generic(event.GenericEvent{Object: poolWith(false, relay)}) {
		t.Error("a generic event wakes peers")
	}
}
