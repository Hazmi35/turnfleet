// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"fmt"
	"math"
	"slices"
	"strings"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// ComputePlan derives the desired relay set for a pool.
//
// It is deterministic: the same nodes and the same spec always produce the same
// relays, in the same order, so a controller restart, a leader change or an
// unrelated reconcile cannot move a relay. A relay moves only when its node or
// its address disappears.
//
// Two relays must never end up owning overlapping ports on the same relay
// address: that would let the second process bind a port the first one is
// already relaying on, and the capacity of the pool would silently be lower
// than the sum of its parts. Every candidate is therefore checked against the
// addresses already used before it is placed.
func ComputePlan(in *PlanInput) Plan {
	nodes := normalizeNodes(in.Nodes)

	replicas := max(in.Spec.Replicas, 0)

	// A pool whose port space is empty or inverted cannot host a relay: the
	// validation webhook rejects it, but the planner is also called with specs
	// that never passed admission (a fuzzer, a test, a future code path), and
	// placing a relay with an inverted range would render a coturn configuration
	// whose capacity is nonsense.
	declared := PortRange{Start: in.Spec.RelayPortRange.Start, End: in.Spec.RelayPortRange.End}
	if declared.Ports() <= 0 {
		return Plan{Unplaced: replicas, Notes: []string{fmt.Sprintf(
			"the pool's relay port range %d-%d holds no ports, so no relay can be placed",
			in.Spec.RelayPortRange.Start, in.Spec.RelayPortRange.End)}}
	}

	candidates := candidatesFor(&in.Spec, nodes)

	plan := Plan{Relays: make([]Relay, 0, min(int(replicas), len(candidates)))}
	used := map[string][]PortRange{}
	for _, candidate := range candidates {
		if int64(len(plan.Relays)) >= int64(replicas) {
			break
		}
		if overlapsAny(used[candidate.address], candidate.portRange) {
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"relay address %s is already used by another relay with overlapping ports %s: "+
					"adding replicas does not add capacity behind one address",
				candidate.address, candidate.portRange))
			continue
		}
		used[candidate.address] = append(used[candidate.address], candidate.portRange)
		plan.Relays = append(plan.Relays, candidate.relay(in.PoolName))
	}

	for _, relay := range plan.Relays {
		plan.PortsTotal += relay.PortRange.Ports()
	}
	plan.Unplaced = replicas - unplacedFrom(int64(len(plan.Relays)))
	if plan.Unplaced > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"%d of %d relays could not be placed: the %s mode makes %d relay addresses available",
			plan.Unplaced, replicas, in.Spec.Addressing.Mode, len(candidates)))
	}
	return plan
}

// unplacedFrom converts a relay count (which the cluster bounds far below
// math.MaxInt32) to the int32 the API uses.
func unplacedFrom(count int64) int32 {
	//nolint:gosec // the value is clamped to math.MaxInt32 immediately above.
	return int32(min(count, int64(math.MaxInt32)))
}

// candidate is a possible relay before it is checked for port overlap.
type candidate struct {
	nodeName           string
	address            string
	bindAddress        string
	portRange          PortRange
	listenerPortOffset int32
}

func (c candidate) relay(poolName string) Relay {
	id := identity(c.address, c.portRange)
	return Relay{
		Name:               RelayName(poolName, id),
		Identity:           id,
		NodeName:           c.nodeName,
		Address:            c.address,
		BindAddress:        c.bindAddress,
		PortRange:          c.portRange,
		ListenerPortOffset: c.listenerPortOffset,
	}
}

// candidatesFor lists every relay the addressing mode could place, in a stable
// order: nodes are visited in name order and, within a node, port slices are
// visited in ascending order. Relays are therefore filled node by node, which
// means a scale-up appends relays to the last node in use and a scale-down
// removes them from it: existing relays keep their identity.
func candidatesFor(spec *turnfleetv1alpha1.TURNPoolSpec, nodes []Node) []candidate {
	portRange := PortRange{Start: spec.RelayPortRange.Start, End: spec.RelayPortRange.End}

	switch spec.Addressing.Mode {
	case turnfleetv1alpha1.AddressingModeHostNetwork:
		out := make([]candidate, 0, len(nodes))
		for _, node := range nodes {
			if node.Address == "" {
				continue
			}
			out = append(out, candidate{
				nodeName:    node.Name,
				address:     node.Address,
				bindAddress: bindAddressFor(node),
				portRange:   portRange,
			})
		}
		return out

	case turnfleetv1alpha1.AddressingModeExternalIPPool:
		addresses := declaredAddresses(spec, nodes)
		out := make([]candidate, 0, len(addresses))
		for _, addr := range addresses {
			out = append(out, candidate{
				nodeName:    addr.NodeName,
				address:     addr.Address,
				bindAddress: addr.Address,
				portRange:   portRange,
			})
		}
		return out

	case turnfleetv1alpha1.AddressingModePartitionedIP:
		perNode := spec.Addressing.PartitionedIP.RelaysPerNode
		portSlices := PartitionPortRange(portRange, perNode)
		out := make([]candidate, 0, len(nodes)*len(portSlices))
		for _, node := range nodes {
			if node.Address == "" {
				continue
			}
			for slot, slice := range portSlices {
				out = append(out, candidate{
					nodeName:    node.Name,
					address:     node.Address,
					bindAddress: bindAddressFor(node),
					portRange:   slice,
					// Every relay on this address needs its own listening
					// port: a listening socket can only be bound once per
					// address.
					listenerPortOffset: int32(slot),
				})
			}
		}
		return out

	default:
		return nil
	}
}

// PartitionPortRange splits a pool's port space into relaysPerNode disjoint
// slices. The slices sum back to the original range, which is why several
// relays on one address add concurrency but not capacity. The remainder goes to
// the first slices, so every slice is non-empty as long as the range is at least
// as wide as the number of relays.
func PartitionPortRange(portRange PortRange, relaysPerNode int32) []PortRange {
	ports := portRange.Ports()
	if ports <= 0 || relaysPerNode <= 0 {
		return nil
	}
	if relaysPerNode > ports {
		relaysPerNode = ports
	}

	base := ports / relaysPerNode
	remainder := ports % relaysPerNode

	parts := make([]PortRange, 0, relaysPerNode)
	start := portRange.Start
	for i := range relaysPerNode {
		size := base
		if i < remainder {
			size++
		}
		parts = append(parts, PortRange{Start: start, End: start + size - 1})
		start += size
	}
	return parts
}

// normalizeNodes returns the relay nodes sorted by name, dropping entries that
// cannot host a relay, so planning is a pure function of the node set.
func normalizeNodes(nodes []Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Name == "" {
			continue
		}
		out = append(out, node)
	}
	slices.SortFunc(out, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// bindAddressFor is the address a relay binds. It is the node's own address
// unless the plan overrides the advertised one.
func bindAddressFor(node Node) string {
	if node.BindAddress != "" {
		return node.BindAddress
	}
	return node.Address
}

// declaredAddresses returns the pool's external addresses, sorted by node name
// and then by address, keeping only the entries whose node is a relay node.
func declaredAddresses(spec *turnfleetv1alpha1.TURNPoolSpec, nodes []Node) []turnfleetv1alpha1.ExternalAddress {
	if spec.Addressing.ExternalIPPool == nil {
		return nil
	}
	known := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		known[node.Name] = struct{}{}
	}

	out := make([]turnfleetv1alpha1.ExternalAddress, 0, len(spec.Addressing.ExternalIPPool.Addresses))
	for _, addr := range spec.Addressing.ExternalIPPool.Addresses {
		if _, ok := known[addr.NodeName]; !ok {
			continue
		}
		out = append(out, addr)
	}
	slices.SortFunc(out, func(a, b turnfleetv1alpha1.ExternalAddress) int {
		if c := strings.Compare(a.NodeName, b.NodeName); c != 0 {
			return c
		}
		return strings.Compare(a.Address, b.Address)
	})
	return out
}

func overlapsAny(existing []PortRange, candidate PortRange) bool {
	for _, r := range existing {
		if r.Overlaps(candidate) {
			return true
		}
	}
	return false
}
