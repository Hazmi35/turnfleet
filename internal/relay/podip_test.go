// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"strings"
	"testing"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// podIPNode is a relay node that declares PodIP relay addresses. The
// declaration is what makes the mode's identities knowable before any pod
// exists: a relay is (address, port range) and the pod name is derived from it.
func podIPNode(name string, declared ...string) Node {
	return Node{Name: name, PodAddresses: declared}
}

// podIPSpec is a PodIP pool with two replicas, planning on the declared
// addresses of its nodes. It names no attachment: the block is what names one,
// and its absence is the mode's default-network spelling.
func podIPSpec() turnfleetv1alpha1.TURNPoolSpec {
	spec := baseSpec()
	spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode: turnfleetv1alpha1.AddressingModePodIP,
	}
	return spec
}

// TestComputePlanPodIP is the mode's contract: one relay per address a node
// declares, each owning the whole port range, so capacity is addresses x ports
// exactly as in ExternalIPPool.
func TestComputePlanPodIP(t *testing.T) {
	t.Parallel()

	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     podIPSpec(),
		Nodes: []Node{
			podIPNode("worker-2", "203.0.113.22"),
			podIPNode("worker-1", "203.0.113.21"),
		},
	})

	if len(plan.Relays) != 2 {
		t.Fatalf("placed %d relays, want 2", len(plan.Relays))
	}
	if plan.Relays[0].NodeName != "worker-1" || plan.Relays[1].NodeName != "worker-2" {
		t.Errorf("relays placed on %s/%s, want worker-1/worker-2 (sorted by node name)",
			plan.Relays[0].NodeName, plan.Relays[1].NodeName)
	}
	if plan.Relays[0].Address != "203.0.113.21" {
		t.Errorf("first relay advertises %s, want the address declared on worker-1", plan.Relays[0].Address)
	}
	for _, planned := range plan.Relays {
		if planned.BindAddress != planned.Address {
			t.Errorf("relay %s binds %s but advertises %s: a PodIP relay holds the address it advertises",
				planned.Name, planned.BindAddress, planned.Address)
		}
		if !planned.PortRange.Same(PortRange{Start: 49152, End: 49199}) {
			t.Errorf("relay %s owns %s, want the whole pool range", planned.Name, planned.PortRange)
		}
	}
	if plan.PortsTotal != 96 {
		t.Errorf("portsTotal = %d, want 96 (two addresses, 48 ports each)", plan.PortsTotal)
	}
}

// TestComputePlanPodIPIsStable is the property across restarts: the same
// declarations in any observation order produce the same relays in the same
// order, so a restart or a leader change cannot move a relay.
func TestComputePlanPodIPIsStable(t *testing.T) {
	t.Parallel()

	spec := podIPSpec()
	spec.Replicas = 3
	nodes := []Node{
		podIPNode("worker-1", "203.0.113.21", "203.0.113.24"),
		podIPNode("worker-2", "203.0.113.22"),
	}

	first := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: nodes})
	// The declarations arrive in another order, which the planner must not care
	// about.
	second := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{
		podIPNode("worker-2", "203.0.113.22"),
		podIPNode("worker-1", "203.0.113.24", "203.0.113.21"),
	}})

	// Pinned first: three declared addresses and three replicas must place
	// three relays, and without this a planner that placed nothing would pass
	// the size comparison below with 0 == 0 and never enter the loop.
	if len(first.Relays) != 3 {
		t.Fatalf("planned %d relays, want 3: three declared addresses on two nodes", len(first.Relays))
	}
	if len(first.Relays) != len(second.Relays) {
		t.Fatalf("plans differ in size: %d vs %d", len(first.Relays), len(second.Relays))
	}
	for i := range first.Relays {
		if first.Relays[i] != second.Relays[i] {
			t.Errorf("relay %d differs between runs: %+v vs %+v", i, first.Relays[i], second.Relays[i])
		}
	}
}

// TestComputePlanPodIPRejectsDuplicateAddress covers the failure the pool must
// never produce: two relays owning the same address and ports. Two nodes that
// declare one address are a real misconfiguration, and placing both would make
// the second process bind sockets the first already holds.
func TestComputePlanPodIPRejectsDuplicateAddress(t *testing.T) {
	t.Parallel()

	spec := podIPSpec()
	spec.Replicas = 3
	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     spec,
		Nodes: []Node{
			podIPNode("worker-1", "203.0.113.21"),
			podIPNode("worker-2", "203.0.113.21"), // the same address on another node
		},
	})

	if len(plan.Relays) != 1 {
		t.Fatalf("placed %d relays, want 1: one address can only be owned once", len(plan.Relays))
	}
	if plan.Unplaced != 2 {
		t.Errorf("unplaced = %d, want 2", plan.Unplaced)
	}
	if len(plan.Notes) == 0 {
		t.Error("no note explains why the other relays were not placed")
	}
}

// TestComputePlanPodIPAddressChanged is the drain the mode relies on: when the
// address a relay was planned with disappears from its node's declaration, the
// relay's identity changes, so its pod is no longer planned and drains instead
// of the relay silently re-advertising whatever address the pod happens to hold
// (ADR 6).
func TestComputePlanPodIPAddressChanged(t *testing.T) {
	t.Parallel()

	spec := podIPSpec()
	spec.Replicas = 1

	before := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{
		podIPNode("worker-1", "203.0.113.21"),
	}})
	after := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{
		podIPNode("worker-1", "203.0.113.99"),
	}})

	if len(before.Relays) != 1 || len(after.Relays) != 1 {
		t.Fatalf("expected one relay on each side, got %d and %d", len(before.Relays), len(after.Relays))
	}
	if before.Relays[0].Identity == after.Relays[0].Identity {
		t.Fatal("the relay kept its identity although its address changed: allocations on the old address would be stranded")
	}
	if after.Relays[0].Address != "203.0.113.99" {
		t.Errorf("the new relay advertises %s, want the newly declared address", after.Relays[0].Address)
	}
	for _, planned := range after.Relays {
		if planned.Address == before.Relays[0].Address {
			t.Errorf("the withdrawn address %s is still planned", before.Relays[0].Address)
		}
	}
}

// TestComputePlanPodIPStripsThePrefix pins what a declaration with a prefix
// means: the prefix is what the pod's attachment asks its IPAM for, never part
// of the relay identity or of the address coturn binds.
func TestComputePlanPodIPStripsThePrefix(t *testing.T) {
	t.Parallel()

	spec := podIPSpec()
	spec.Replicas = 2
	spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyDualStack
	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     spec,
		Nodes: []Node{
			podIPNode("worker-1", "203.0.113.21/24", "2001:db8:100::21/64"),
		},
	})

	if len(plan.Relays) != 2 {
		t.Fatalf("placed %d relays, want 2 (one per family)", len(plan.Relays))
	}
	if plan.Relays[0].Address != "203.0.113.21" {
		t.Errorf("IPv4 relay advertises %q, want the bare address", plan.Relays[0].Address)
	}
	if plan.Relays[1].Address != "2001:db8:100::21" {
		t.Errorf("IPv6 relay advertises %q, want the bare address", plan.Relays[1].Address)
	}
}

// TestComputePlanPodIPSkipsOtherFamilies keeps the honest failure mode: an
// IPv4-only declaration contributes nothing to an IPv6 pool.
func TestComputePlanPodIPSkipsOtherFamilies(t *testing.T) {
	t.Parallel()

	spec := podIPSpec()
	spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyIPv6
	plan := ComputePlan(&PlanInput{
		PoolName: "demo",
		Spec:     spec,
		Nodes:    []Node{podIPNode("worker-1", "203.0.113.21/24")},
	})

	if len(plan.Relays) != 0 {
		t.Fatalf("placed %d relays on an IPv4-only declaration, want 0", len(plan.Relays))
	}
	if plan.Unplaced != 2 {
		t.Errorf("unplaced = %d, want 2", plan.Unplaced)
	}
}

func TestPodAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		declared string
		want     string
		ok       bool
	}{
		{"203.0.113.21", "203.0.113.21", true},
		{"203.0.113.21/24", "203.0.113.21", true},
		{" 2001:db8::21 ", "2001:db8::21", true},
		{"2001:db8::21/64", "2001:db8::21", true},
		{"::ffff:203.0.113.21", "203.0.113.21", true},
		{"", "", false},
		{"   ", "", false},
		{"not-an-address", "", false},
		{"203.0.113.21/33", "", false},
		{"203.0.113.21/24/24", "", false},
	}
	for _, tc := range tests {
		got, ok := PodAddress(tc.declared)
		if got != tc.want || ok != tc.ok {
			t.Errorf("PodAddress(%q) = (%q, %v), want (%q, %v)", tc.declared, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIndividuallyRoutable(t *testing.T) {
	t.Parallel()

	// A private, carrier-grade NAT or unique local address is legal: the
	// peers may route it, and the external vantage point is what proves they
	// do. This is the hub-and-spoke cluster unblocked here.
	routable := []string{
		"203.0.113.21", "198.51.100.7", "2001:db8:100::21",
		"10.0.0.5", "172.16.5.5", "192.168.1.5", "100.64.0.5",
		"fd00::1", "fc00::1",
	}
	for _, address := range routable {
		if ok, reason := IndividuallyRoutable(address); !ok {
			t.Errorf("IndividuallyRoutable(%q) = false (%s), want true", address, reason)
		}
	}

	// Each of these is a reason told to the operator, so the reason is checked
	// too: it is what the rejection message names. These are the addresses that
	// cannot work at all, however the peers are routed.
	unroutable := map[string]string{
		"169.254.169.254": "link-local",
		"127.0.0.1":       "loopback",
		"0.0.0.0":         "unspecified",
		"224.0.0.1":       "multicast",
		"fe80::1":         "link-local",
		"::1":             "loopback",
		"not-an-ip":       "not an IP address",
	}
	for address, want := range unroutable {
		ok, reason := IndividuallyRoutable(address)
		if ok {
			t.Errorf("IndividuallyRoutable(%q) = true, want false", address)
			continue
		}
		if !strings.Contains(reason, want) {
			t.Errorf("IndividuallyRoutable(%q) reason = %q, want it to name %q", address, reason, want)
		}
	}
}

func TestPodIPAttachmentParts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ref      string
		poolNS   string
		wantNS   string
		wantName string
		wantOK   bool
	}{
		{"macvlan-conf", "turnfleet", "turnfleet", "macvlan-conf", true},
		{" network-team/sriov-net ", "turnfleet", "network-team", "sriov-net", true},
		{"", "turnfleet", "", "", false},
		{"   ", "turnfleet", "", "", false},
		{"/name", "turnfleet", "", "", false},
		{"ns/", "turnfleet", "", "", false},
		{"a/b/c", "turnfleet", "a", "b/c", true},
	}
	for _, tc := range tests {
		namespace, name, ok := PodIPAttachmentParts(tc.ref, tc.poolNS)
		if namespace != tc.wantNS || name != tc.wantName || ok != tc.wantOK {
			t.Errorf("PodIPAttachmentParts(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.ref, tc.poolNS, namespace, name, ok, tc.wantNS, tc.wantName, tc.wantOK)
		}
	}
}
