// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// A relay identity is an address and the ports a relay owns on it, and it is
// unique across the cluster: two relays holding the same ports on one address
// fight over the same sockets, and both pools end up serving less than they
// report. The ruler rejects that inside one pool (and the webhook rejects it
// before admission), but no admission check spans two TURNPools, so the
// reconciler compares them and reports every overlap it finds as a condition
// on each pool that holds one. Admission is not the place for it: a webhook
// sees one object and a cache it cannot trust to be the whole picture
// (docs/adr/0012-cross-pool-address-conflict.md).
//
// What is compared is what pools hold, never what they ask for:
//
//   - For the pool being reconciled, the identities are the ones this pass
//     planned, which are the ones its status is about to publish.
//   - For every other pool, they are the relays its status reports.
//
// A pool that requests an address but holds no relay on it therefore takes
// part in no conflict, and neither does a pool whose relay on that address is
// already gone.

// heldRelay is one relay identity a pool holds: the address the relay
// advertises in XOR-RELAYED-ADDRESS and the ports it owns.
type heldRelay struct {
	address   string
	portRange relay.PortRange
}

// key is the identity in comparable string form, "<address>/<start>-<end>".
func (h heldRelay) key() string {
	return h.address + "/" + h.portRange.String()
}

// heldRelays lists the relay identities a pool's status reports. The status is
// the only record of what a pool holds: the spec's requested range says
// nothing about what is running.
func heldRelays(pool *turnfleetv1alpha1.TURNPool) []heldRelay {
	if pool.Status == nil {
		return nil
	}
	held := make([]heldRelay, 0, len(pool.Status.Relays))
	for i := range pool.Status.Relays {
		entry := &pool.Status.Relays[i]
		if entry.RelayAddress == nil || *entry.RelayAddress == "" {
			continue
		}
		held = append(held, heldRelay{
			address:   *entry.RelayAddress,
			portRange: relay.PortRange{Start: entry.PortRange.Start, End: entry.PortRange.End},
		})
	}
	return held
}

// plannedRelays lists the relay identities a reconcile pass plans, in the form
// the conflict check compares.
func plannedRelays(states []*relayState) []heldRelay {
	held := make([]heldRelay, 0, len(states))
	for _, state := range states {
		if state.relay.Address == "" {
			continue
		}
		held = append(held, heldRelay{
			address:   state.relay.Address,
			portRange: state.relay.PortRange,
		})
	}
	return held
}

// heldAddresses is the set of addresses a pool's held relays advertise.
func heldAddresses(pool *turnfleetv1alpha1.TURNPool) map[string]struct{} {
	held := heldRelays(pool)
	addresses := make(map[string]struct{}, len(held))
	for _, identity := range held {
		addresses[identity.address] = struct{}{}
	}
	return addresses
}

// sharesAddress reports whether any of the given relays sits on one of the
// addresses.
func sharesAddress(held []heldRelay, addresses map[string]struct{}) bool {
	for _, identity := range held {
		if _, ok := addresses[identity.address]; ok {
			return true
		}
	}
	return false
}

// heldIdentityKeys is the sorted set of identities a pool holds, so two status
// versions can be compared for "did anything about the held identities change".
func heldIdentityKeys(pool *turnfleetv1alpha1.TURNPool) []string {
	held := heldRelays(pool)
	keys := make([]string, 0, len(held))
	for _, identity := range held {
		keys = append(keys, identity.key())
	}
	slices.Sort(keys)
	return keys
}

// addressConflict is one identity this pool holds that another pool holds too,
// on the same address and with overlapping ports.
type addressConflict struct {
	identity   heldRelay
	other      types.NamespacedName
	otherRange relay.PortRange
}

// addressConflicts reports every relay identity this pool holds that another
// pool also holds with overlapping ports on the same address. Disjoint ranges
// on a shared address are legal and report nothing: they are what
// PartitionedIP mode does inside one pool, and the same reasoning holds across
// pools.
func (r *TURNPoolReconciler) addressConflicts(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, states []*relayState) ([]addressConflict, error) {
	mine := plannedRelays(states)
	if len(mine) == 0 {
		// Nothing held, nothing to conflict over: skip the list.
		return nil, nil
	}

	pools := &turnfleetv1alpha1.TURNPoolList{}
	if err := r.List(ctx, pools); err != nil {
		return nil, fmt.Errorf("listing pools to compare relay identities: %w", err)
	}

	var conflicts []addressConflict
	for i := range pools.Items {
		other := &pools.Items[i]
		if other.Namespace == pool.Namespace && other.Name == pool.Name {
			// Same-pool overlap is the planner's invariant, checked there.
			continue
		}
		for _, theirs := range heldRelays(other) {
			for _, identity := range mine {
				if identity.address != theirs.address || !identity.portRange.Overlaps(theirs.portRange) {
					continue
				}
				conflicts = append(conflicts, addressConflict{
					identity:   identity,
					other:      types.NamespacedName{Namespace: other.Namespace, Name: other.Name},
					otherRange: theirs.portRange,
				})
			}
		}
	}

	slices.SortFunc(conflicts, func(a, b addressConflict) int {
		return strings.Compare(a.identity.key()+a.other.String(), b.identity.key()+b.other.String())
	})
	return conflicts, nil
}

// describeConflicts is the condition message for a list of overlaps. It names
// the other pool and its ports, which is what an operator needs to act: move
// this pool or move that one.
func describeConflicts(conflicts []addressConflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		parts = append(parts, fmt.Sprintf("relay %s:%s overlaps a relay of pool %s (%s)",
			conflict.identity.address, conflict.identity.portRange, conflict.other, conflict.otherRange))
	}
	message := strings.Join(firstN(parts, 3), "; ")
	if len(conflicts) > 3 {
		message += fmt.Sprintf("; and %d more", len(conflicts)-3)
	}
	return message + ": two relays must not own the same ports on one address"
}

// poolsForHeldAddresses maps a pool whose held relay identities changed to
// every other pool that holds a relay on one of its addresses.
//
// This is the half of the check one reconcile cannot do. The pool that has to
// clear or raise its condition is the one that did not change, so it has to be
// woken by the change. The map is over addresses rather than over the overlap
// itself: a change can remove an overlap as well as add one, and the pool that
// keeps its ports still has to look at its own identity again.
//
// An index over held addresses would narrow the lookup further, at the cost of
// registering and maintaining a field index over status content. It is not
// worth it here: the List below is served from the informer cache, the fan-out
// is limited to the pools that share an address, and heldIdentitiesChanged
// keeps it to the events where a held identity actually changed, which is not
// the probe interval. poolsForNode lists pools the same way.
func (r *TURNPoolReconciler) poolsForHeldAddresses(ctx context.Context, obj client.Object) []ctrl.Request {
	changed, ok := obj.(*turnfleetv1alpha1.TURNPool)
	if !ok {
		return nil
	}
	addresses := heldAddresses(changed)
	if len(addresses) == 0 {
		return nil
	}

	pools := &turnfleetv1alpha1.TURNPoolList{}
	if err := r.List(ctx, pools); err != nil {
		logf.FromContext(ctx).Error(err, "listing pools for a held relay address change",
			"pool", changed.Name, "namespace", changed.Namespace)
		return nil
	}

	requests := make([]ctrl.Request, 0, len(pools.Items))
	for i := range pools.Items {
		other := &pools.Items[i]
		if other.Namespace == changed.Namespace && other.Name == changed.Name {
			continue
		}
		if sharesAddress(heldRelays(other), addresses) {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: other.Namespace,
				Name:      other.Name,
			}})
		}
	}
	return requests
}

// heldIdentitiesChanged triggers when the set of relay identities a pool's
// status reports changes: a relay appearing, disappearing or moving to another
// identity. Everything else a status carries — probe results, allocation
// counts, conditions — changes nothing for another pool, and waking it for
// those would put every pool that shares an address on the probe interval.
func heldIdentitiesChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPool, oldOK := e.ObjectOld.(*turnfleetv1alpha1.TURNPool)
			newPool, newOK := e.ObjectNew.(*turnfleetv1alpha1.TURNPool)
			if !oldOK || !newOK {
				return true
			}
			return !slices.Equal(heldIdentityKeys(oldPool), heldIdentityKeys(newPool))
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
