// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"encoding/binary"
	"testing"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// FuzzRenderer checks the two properties the renderer and the planner must never
// break, whatever a pool spec says: they do not panic, and they never produce
// two relays that own overlapping ports on one address. An overlap would mean
// the second process binds a port the first one is already relaying on, and the
// pool's real capacity would be lower than the sum of its relays.
func FuzzRenderer(f *testing.F) {
	f.Add(seedSpec(0x01))
	f.Add(seedSpec(0x02))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		spec := specFromBytes(data)

		nodes := []Node{
			dualStackNode("worker-1", "203.0.113.11", "2001:db8::11"),
			dualStackNode("worker-2", "203.0.113.12", "2001:db8::12"),
			// An IPv4-only node: an IPv6 or DualStack pool must simply place
			// nothing on it, not fall back to its IPv4 address.
			v4Node("worker-3", "10.0.0.13"),
		}
		// PodIP mode plans on the addresses its nodes declare, so the fuzz
		// nodes declare some; otherwise the mode would place nothing and go
		// untested.
		nodes[0].PodAddresses = []string{"203.0.113.21", "2001:db8:100::21"}
		nodes[1].PodAddresses = []string{"203.0.113.22", "2001:db8:100::22/64"}
		nodes[2].PodAddresses = []string{"198.51.100.13"}

		plan := ComputePlan(&PlanInput{PoolName: "fuzz", Spec: spec, Nodes: nodes})
		assertNoOverlap(t, &plan, &spec)

		for i := range plan.Relays {
			relay := plan.Relays[i]
			config, err := Render(&RenderInput{
				Relay:         relay,
				PoolNamespace: "default",
				PoolName:      "fuzz",
				Spec:          spec,
				DeniedCIDRs:   DefaultDeniedCIDRs(),
				AuthSecret:    "0123456789abcdef",
			})
			if err != nil {
				// Refusing to render an unusable relay is a valid outcome.
				continue
			}
			if config == "" {
				t.Fatalf("rendered an empty configuration for relay %s", relay.Name)
			}
			if rangeParts := configOptions(config); len(rangeParts) == 0 {
				t.Fatalf("rendered a configuration with no options for relay %s", relay.Name)
			}
		}
	})
}

// assertNoOverlap fails when two planned relays on one address share a port.
func assertNoOverlap(t *testing.T, plan *Plan, spec *turnfleetv1alpha1.TURNPoolSpec) {
	t.Helper()

	seen := map[string][]PortRange{}
	for _, relay := range plan.Relays {
		if relay.PortRange.Ports() <= 0 {
			t.Fatalf("relay %s owns an empty port range %s", relay.Name, relay.PortRange)
		}
		for _, existing := range seen[relay.Address] {
			if existing.Overlaps(relay.PortRange) {
				t.Fatalf("relays on %s overlap: %s and %s (mode %s, range %d-%d)",
					relay.Address, existing, relay.PortRange, spec.Addressing.Mode,
					spec.RelayPortRange.Start, spec.RelayPortRange.End)
			}
		}
		seen[relay.Address] = append(seen[relay.Address], relay.PortRange)
	}

	// Disjointness is not enough: the slices of one address must not claim more
	// ports than that address has, or the plan would be promising capacity that
	// does not exist.
	poolRange := PortRange{Start: spec.RelayPortRange.Start, End: spec.RelayPortRange.End}
	for address, ranges := range seen {
		total := int32(0)
		for _, r := range ranges {
			total += r.Ports()
		}
		if total > poolRange.Ports() {
			t.Fatalf("relays on %s claim %d ports but the pool's range holds %d",
				address, total, poolRange.Ports())
		}
	}
}

// specFromBytes turns fuzz input into a spec. It deliberately produces values
// the webhook would reject (unsorted ranges, empty listeners, unknown modes) so
// the renderer has to cope with them without panicking.
func specFromBytes(data []byte) turnfleetv1alpha1.TURNPoolSpec {
	byteAt := func(i int) byte {
		if i < len(data) {
			return data[i]
		}
		return 0
	}
	portAt := func(i int) int32 {
		return int32(binary.BigEndian.Uint16([]byte{byteAt(i), byteAt(i + 1)})) % 65536
	}

	start := portAt(0)
	end := portAt(2)
	mode := []turnfleetv1alpha1.AddressingMode{
		turnfleetv1alpha1.AddressingModeHostNetwork,
		turnfleetv1alpha1.AddressingModeExternalIPPool,
		turnfleetv1alpha1.AddressingModePartitionedIP,
		turnfleetv1alpha1.AddressingModePodIP,
		"SomethingElse",
	}[int(byteAt(4))%5]

	// byteAt(11) is past every byte the seeds use, so the stored corpus keeps
	// meaning what it meant before the family existed.
	family := []turnfleetv1alpha1.AddressingFamily{
		turnfleetv1alpha1.AddressingFamilyIPv4,
		turnfleetv1alpha1.AddressingFamilyIPv6,
		turnfleetv1alpha1.AddressingFamilyDualStack,
		"",
	}[int(byteAt(11))%4]

	spec := turnfleetv1alpha1.TURNPoolSpec{
		Replicas: int32(byteAt(5)%8) + 1,
		Dataplane: turnfleetv1alpha1.DataplaneSpec{
			Image: "coturn/coturn:4.18.0-r0@sha256:" +
				"bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3",
		},
		Addressing:     turnfleetv1alpha1.AddressingSpec{Mode: mode, Family: family},
		RelayPortRange: turnfleetv1alpha1.PortRange{Start: start, End: end},
		Auth: turnfleetv1alpha1.AuthSpec{
			Realm: "turnfleet.fuzz",
		},
		PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{
			DeniedCIDRs:  DefaultDeniedCIDRs(),
			AllowedCIDRs: []string{"203.0.113.0/24"},
		},
	}

	if byteAt(6)%2 == 0 {
		spec.Listeners = []turnfleetv1alpha1.ListenerSpec{
			{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: portAt(7)},
		}
	}
	if byteAt(9)%3 == 0 {
		spec.Listeners = append(spec.Listeners, turnfleetv1alpha1.ListenerSpec{
			Name: "turn-tcp", Protocol: turnfleetv1alpha1.ListenerProtocolTCP, Port: portAt(7),
		})
	}

	switch mode {
	case turnfleetv1alpha1.AddressingModePartitionedIP:
		spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{
			RelaysPerNode: int32(byteAt(10)%8) + 1,
		}
	case turnfleetv1alpha1.AddressingModeExternalIPPool:
		spec.Addressing.ExternalIPPool = &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{
				{NodeName: "worker-1", Address: "198.51.100.11"},
				{NodeName: "worker-2", Address: "198.51.100.12"},
				{NodeName: "worker-1", Address: "2001:db8:100::1"},
				{NodeName: "worker-2", Address: "2001:db8:100::2"},
			},
		}
	case turnfleetv1alpha1.AddressingModePodIP:
		// PodIP is optional and its zero value is the default-network
		// spelling, so both spellings stay fuzzed: the block set with a
		// reference, and the block left zero.
		if byteAt(12)%2 == 0 {
			spec.Addressing.PodIP = turnfleetv1alpha1.PodIPSpec{AttachmentRef: "relay-pods"}
		}
	case turnfleetv1alpha1.AddressingModeHostNetwork:
		// Nothing mode-specific to configure.
	}

	return spec
}

func seedSpec(marker byte) []byte {
	// A valid HostNetwork pool: workers, a 48-port range and one listener.
	return []byte{
		0xc0, 0x00, // start 49152
		0xc0, 0x2f, // end 49199
		0x00,       // mode: HostNetwork
		marker,     // replicas
		0x00, 0x0d, // listener port 3478
		marker, 0x00, 0x01,
	}
}
