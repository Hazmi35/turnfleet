// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"testing"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
)

func baseSpec() turnfleetv1alpha1.TURNPoolSpec {
	return turnfleetv1alpha1.TURNPoolSpec{
		Replicas: 2,
		Addressing: turnfleetv1alpha1.AddressingSpec{
			Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
		},
		RelayPortRange: turnfleetv1alpha1.PortRange{Start: 49152, End: 49199},
		Listeners: []turnfleetv1alpha1.ListenerSpec{
			{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
		},
	}
}

func TestComputePlanHostNetwork(t *testing.T) {
	t.Parallel()

	input := PlanInput{
		PoolName: "demo",
		Spec:     baseSpec(),
		Nodes: []Node{
			{Name: "worker-2", Address: "203.0.113.12", BindAddress: "203.0.113.12"},
			{Name: "worker-1", Address: "203.0.113.11", BindAddress: "203.0.113.11"},
			{Name: "worker-3", Address: "203.0.113.13", BindAddress: "203.0.113.13"},
		},
	}

	plan := ComputePlan(&input)
	if len(plan.Relays) != 2 {
		t.Fatalf("placed %d relays, want 2", len(plan.Relays))
	}
	// Nodes are visited in name order, so worker-1 and worker-2 are used.
	if plan.Relays[0].NodeName != "worker-1" || plan.Relays[1].NodeName != "worker-2" {
		t.Errorf("relays placed on %s/%s, want worker-1/worker-2", plan.Relays[0].NodeName, plan.Relays[1].NodeName)
	}
	for _, r := range plan.Relays {
		if !r.PortRange.Same(PortRange{Start: 49152, End: 49199}) {
			t.Errorf("relay %s owns %s, want the whole pool range", r.Name, r.PortRange)
		}
		if r.Capacity() != 48 {
			t.Errorf("relay %s capacity = %d, want 48", r.Name, r.Capacity())
		}
	}
	if plan.PortsTotal != 96 {
		t.Errorf("portsTotal = %d, want 96 (two addresses, 48 ports each)", plan.PortsTotal)
	}
	if plan.Unplaced != 0 {
		t.Errorf("unplaced = %d, want 0", plan.Unplaced)
	}
}

// TestComputePlanIsStable is the property that matters most: a controller
// restart, a leader change or a new node must not move a relay, because moving
// one strands the allocations it holds.
func TestComputePlanIsStable(t *testing.T) {
	t.Parallel()

	spec := baseSpec()
	spec.Replicas = 2
	first := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{
		{Name: "worker-1", Address: "203.0.113.11"},
		{Name: "worker-2", Address: "203.0.113.12"},
	}})
	second := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{
		{Name: "worker-2", Address: "203.0.113.12"},
		{Name: "worker-1", Address: "203.0.113.11"},
	}})

	if len(first.Relays) != len(second.Relays) {
		t.Fatalf("plans differ in size: %d vs %d", len(first.Relays), len(second.Relays))
	}
	for i := range first.Relays {
		if first.Relays[i] != second.Relays[i] {
			t.Errorf("relay %d differs between runs: %+v vs %+v", i, first.Relays[i], second.Relays[i])
		}
	}

	// A scale-up must not change the relays that already exist.
	spec.Replicas = 3
	third := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{
		{Name: "worker-1", Address: "203.0.113.11"},
		{Name: "worker-2", Address: "203.0.113.12"},
		{Name: "worker-3", Address: "203.0.113.13"},
	}})
	if len(third.Relays) != 3 {
		t.Fatalf("placed %d relays after scale-up, want 3", len(third.Relays))
	}
	for i := range first.Relays {
		if third.Relays[i] != first.Relays[i] {
			t.Errorf("scale-up moved relay %d: %+v became %+v", i, first.Relays[i], third.Relays[i])
		}
	}
}

// TestComputePlanRejectsOverlap covers the failure the planner must never
// produce: two relays on one address with overlapping ports. Two nodes that
// advertise the same address are a real misconfiguration, and placing a second
// relay there would silently halve the pool's real capacity.
func TestComputePlanRejectsOverlap(t *testing.T) {
	t.Parallel()

	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     baseSpec(),
		Nodes: []Node{
			{Name: "worker-1", Address: "203.0.113.11"},
			{Name: "worker-2", Address: "203.0.113.11"}, // same address
		},
	})

	if len(plan.Relays) != 1 {
		t.Fatalf("placed %d relays, want 1: two nodes advertising one address cannot both own the range", len(plan.Relays))
	}
	if plan.Unplaced != 1 {
		t.Errorf("unplaced = %d, want 1", plan.Unplaced)
	}
	if len(plan.Notes) == 0 {
		t.Error("no note explains why the second relay was not placed")
	}
}

func TestComputePlanPartitionedIP(t *testing.T) {
	t.Parallel()

	spec := baseSpec()
	spec.Replicas = 4
	spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 50000, End: 50009}
	spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
		PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
	}

	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     spec,
		Nodes: []Node{
			{Name: "worker-1", Address: "10.0.0.11"},
			{Name: "worker-2", Address: "10.0.0.12"},
		},
	})

	if len(plan.Relays) != 4 {
		t.Fatalf("placed %d relays, want 4", len(plan.Relays))
	}
	// Both relays on one node share the node's address and must not overlap.
	byAddress := map[string][]PortRange{}
	for _, r := range plan.Relays {
		byAddress[r.Address] = append(byAddress[r.Address], r.PortRange)
	}
	for address, ranges := range byAddress {
		for i := range ranges {
			for j := i + 1; j < len(ranges); j++ {
				if ranges[i].Overlaps(ranges[j]) {
					t.Errorf("ranges %s and %s overlap on address %s", ranges[i], ranges[j], address)
				}
			}
		}
	}
	if len(byAddress) != 2 {
		t.Fatalf("relays use %d addresses, want 2", len(byAddress))
	}
	// Partitioning an address must not invent capacity: the slices of one
	// address add up to the pool's range, no more. Two addresses therefore hold
	// 2 x 10 ports even though four relays run.
	if plan.PortsTotal != 20 {
		t.Errorf("portsTotal = %d, want 20 (two addresses of 10 ports)", plan.PortsTotal)
	}
	for address, ranges := range byAddress {
		ports := int32(0)
		for _, r := range ranges {
			ports += r.Ports()
		}
		if ports != 10 {
			t.Errorf("address %s holds %d ports across %d relays, want the pool range of 10: "+
				"splitting an address adds concurrency, not capacity", address, ports, len(ranges))
		}
	}
	if got := plan.Relays[0].PortRange; !got.Same(PortRange{Start: 50000, End: 50004}) {
		t.Errorf("first relay owns %s, want 50000-50004 (remainder to the first slice)", got)
	}
}

func TestComputePlanExternalIPPool(t *testing.T) {
	t.Parallel()

	spec := baseSpec()
	spec.Replicas = 2
	spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
		ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{
				{NodeName: "worker-2", Address: "198.51.100.12"},
				{NodeName: "worker-1", Address: "198.51.100.11"},
				{NodeName: "gone", Address: "198.51.100.99"}, // node is not a relay node
			},
		},
	}

	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     spec,
		Nodes: []Node{
			{Name: "worker-1", Address: "10.0.0.11"},
			{Name: "worker-2", Address: "10.0.0.12"},
		},
	})

	if len(plan.Relays) != 2 {
		t.Fatalf("placed %d relays, want 2", len(plan.Relays))
	}
	if plan.Relays[0].Address != "198.51.100.11" || plan.Relays[1].Address != "198.51.100.12" {
		t.Errorf("relay addresses are %s and %s, want the declared ones in node order",
			plan.Relays[0].Address, plan.Relays[1].Address)
	}
	for _, r := range plan.Relays {
		if r.BindAddress != r.Address {
			t.Errorf("relay %s binds %s but advertises %s: a declared secondary address is local",
				r.Name, r.BindAddress, r.Address)
		}
	}
}

func TestPartitionPortRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		r      PortRange
		relays int32
		want   []PortRange
	}{
		{"evenly divisible", PortRange{1, 12}, 4, []PortRange{{1, 3}, {4, 6}, {7, 9}, {10, 12}}},
		{"remainder to the first slices", PortRange{10, 16}, 3, []PortRange{{10, 12}, {13, 14}, {15, 16}}},
		{"one relay", PortRange{100, 200}, 1, []PortRange{{100, 200}}},
		{"more relays than ports", PortRange{5, 6}, 4, []PortRange{{5, 5}, {6, 6}}},
		{"zero relays", PortRange{1, 10}, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := PartitionPortRange(tc.r, tc.relays)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d slices, want %d", len(got), len(tc.want))
			}
			total := int32(0)
			for i := range got {
				if !got[i].Same(tc.want[i]) {
					t.Errorf("slice %d = %s, want %s", i, got[i], tc.want[i])
				}
				total += got[i].Ports()
			}
			if total != tc.r.Ports() && tc.relays > 0 && tc.relays <= tc.r.Ports() {
				t.Errorf("slices hold %d ports, want %d: partitioning must not create or lose ports", total, tc.r.Ports())
			}
		})
	}
}

func TestRelayName(t *testing.T) {
	t.Parallel()

	a := RelayName("demo", "203.0.113.10|49152-49199")
	b := RelayName("demo", "203.0.113.10|49152-49199")
	c := RelayName("demo", "203.0.113.11|49152-49199")

	if a != b {
		t.Errorf("the same identity produced different names: %s and %s", a, b)
	}
	if a == c {
		t.Errorf("different identities produced the same name: %s", a)
	}
	if len(a) > 63 {
		t.Errorf("relay name %q is %d characters, which cannot be a pod name", a, len(a))
	}
	if a != "demo-relay-"+a[len("demo-relay-"):] {
		t.Errorf("relay name %q does not follow <pool>-relay-<hash>", a)
	}
	if suffix := a[len("demo-relay-"):]; len(suffix) != 5 {
		t.Errorf("relay name suffix %q is %d characters, want 5", suffix, len(suffix))
	}
}

func TestListenerURIs(t *testing.T) {
	t.Parallel()

	uris := ListenerURIs("203.0.113.10", []turnfleetv1alpha1.ListenerSpec{
		{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
		{Name: "turns", Protocol: turnfleetv1alpha1.ListenerProtocolTLS, Port: 5349},
		{Name: "turn-tcp", Protocol: turnfleetv1alpha1.ListenerProtocolTCP, Port: 3478},
	})

	want := []string{
		"turn:203.0.113.10:3478?transport=tcp",
		"turn:203.0.113.10:3478?transport=udp",
		"turns:203.0.113.10:5349?transport=tcp",
	}
	if len(uris) != len(want) {
		t.Fatalf("got %d URIs, want %d: %v", len(uris), len(want), uris)
	}
	for i := range want {
		if uris[i] != want[i] {
			t.Errorf("URI %d = %q, want %q", i, uris[i], want[i])
		}
	}
}
