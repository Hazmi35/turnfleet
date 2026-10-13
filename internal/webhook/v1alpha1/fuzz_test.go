// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package v1alpha1

import (
	"context"
	"encoding/binary"
	"testing"

	corev1 "k8s.io/api/core/v1"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// FuzzWebhookValidation checks that validation never panics on an arbitrary
// spec, and that anything it admits can be planned without two relays ending up
// with overlapping ports on one address. Validation is the last line of defence
// in front of the planner, so the property has to hold for every admitted pool.
func FuzzWebhookValidation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00, 0xc0, 0x00, 0xc0, 0x2f})
	f.Add([]byte{0xff, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05})
	f.Add(seedPool(0x01))
	f.Add(seedPool(0x02))

	validator := &TURNPoolValidator{}
	defaulter := &TURNPoolDefaulter{}

	f.Fuzz(func(t *testing.T, data []byte) {
		pool := poolFromBytes(data)
		if err := defaulter.Default(context.Background(), pool); err != nil {
			return
		}

		if _, err := validator.ValidateCreate(context.Background(), pool); err != nil {
			return
		}
		if _, err := validator.ValidateUpdate(context.Background(), pool.DeepCopy(), pool); err != nil {
			// An update of an object against itself must be admitted; anything
			// else is a bug worth failing on.
			t.Fatalf("validating a pool against itself failed: %v", err)
		}

		// Whatever is admitted must plan without overlapping ports.
		plan := relay.ComputePlan(&relay.PlanInput{
			PoolName: pool.Name,
			Spec:     pool.Spec,
			Nodes: []relay.Node{
				{Name: "worker-1", Addresses: []relay.NodeAddress{
					{Address: "203.0.113.11", BindAddress: "203.0.113.11"},
					{Address: "2001:db8::11", BindAddress: "2001:db8::11"},
				}},
				{Name: "worker-2", Addresses: []relay.NodeAddress{
					{Address: "203.0.113.12", BindAddress: "203.0.113.12"},
					{Address: "2001:db8::12", BindAddress: "2001:db8::12"},
				}},
			},
		})

		seen := map[string][]relay.PortRange{}
		for _, planned := range plan.Relays {
			for _, existing := range seen[planned.Address] {
				if existing.Overlaps(planned.PortRange) {
					t.Fatalf("an admitted pool planned overlapping relays on %s: %s and %s (mode %s)",
						planned.Address, existing, planned.PortRange, pool.Spec.Addressing.Mode)
				}
			}
			seen[planned.Address] = append(seen[planned.Address], planned.PortRange)
		}
	})
}

// poolFromBytes turns fuzz input into a pool, including values the webhook is
// expected to reject.
func poolFromBytes(data []byte) *turnfleetv1alpha1.TURNPool {
	byteAt := func(i int) byte {
		if i < len(data) {
			return data[i]
		}
		return 0
	}
	portAt := func(i int) int32 {
		return int32(binary.BigEndian.Uint16([]byte{byteAt(i), byteAt(i + 1)})) % 65536
	}

	mode := []turnfleetv1alpha1.AddressingMode{
		turnfleetv1alpha1.AddressingModeHostNetwork,
		turnfleetv1alpha1.AddressingModeExternalIPPool,
		turnfleetv1alpha1.AddressingModePartitionedIP,
		"SomethingElse",
		"",
	}[int(byteAt(0))%5]

	// byteAt(11) is past every byte the seeds use, so the stored corpus keeps
	// meaning what it meant before the family existed.
	family := []turnfleetv1alpha1.AddressingFamily{
		turnfleetv1alpha1.AddressingFamilyIPv4,
		turnfleetv1alpha1.AddressingFamilyIPv6,
		turnfleetv1alpha1.AddressingFamilyDualStack,
		"",
	}[int(byteAt(11))%4]

	pool := &turnfleetv1alpha1.TURNPool{
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: int32(byteAt(1)%8) + 1,
			Dataplane: turnfleetv1alpha1.DataplaneSpec{
				Image: "coturn/coturn:4.18.0-r0@sha256:" +
					"bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3",
			},
			Addressing:     turnfleetv1alpha1.AddressingSpec{Mode: mode, Family: family},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: portAt(2), End: portAt(4)},
			Auth: turnfleetv1alpha1.AuthSpec{
				Realm:     "turnfleet.fuzz",
				SecretRef: fuzzSecretRef(),
			},
		},
	}

	protocols := []turnfleetv1alpha1.ListenerProtocol{
		turnfleetv1alpha1.ListenerProtocolUDP,
		turnfleetv1alpha1.ListenerProtocolTCP,
		turnfleetv1alpha1.ListenerProtocolTLS,
		"",
	}
	pool.Name = "fuzz"
	pool.Namespace = "default"

	listenerCount := int(byteAt(6) % 4)
	for i := range listenerCount {
		listener := turnfleetv1alpha1.ListenerSpec{
			Name:     "listener-" + string(rune('a'+i)),
			Protocol: protocols[int(byteAt(7+i))%len(protocols)],
			Port:     portAt(8 + 2*i),
		}
		if listener.Protocol == turnfleetv1alpha1.ListenerProtocolTLS && byteAt(9+i)%2 == 0 {
			listener.TLSSecretRef = &corev1.LocalObjectReference{Name: "tls-cert"}
		}
		pool.Spec.Listeners = append(pool.Spec.Listeners, listener)
	}

	switch mode {
	case turnfleetv1alpha1.AddressingModePartitionedIP:
		relaysPerNode := int32(byteAt(5)%8) + 1
		pool.Spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: relaysPerNode}
	case turnfleetv1alpha1.AddressingModeExternalIPPool:
		pool.Spec.Addressing.ExternalIPPool = &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{
				{NodeName: "worker-1", Address: "198.51.100.11"},
				{NodeName: "worker-1", Address: "2001:db8:100::1"},
			},
		}
	case turnfleetv1alpha1.AddressingModeHostNetwork:
	}

	return pool
}

// fuzzSecretRef builds the secret selector the fuzzed pools point at, without a
// literal that embeds another struct.
func fuzzSecretRef() *corev1.SecretKeySelector {
	ref := &corev1.SecretKeySelector{}
	ref.Name = "turn-secret"
	ref.Key = "secret"
	return ref
}

func seedPool(marker byte) []byte {
	return []byte{
		0x00,       // mode: HostNetwork
		marker,     // replicas
		0xc0, 0x00, // start 49152
		0xc0, 0x2f, // end 49199
		0x00,       // relaysPerNode
		0x01,       // one listener
		0x00,       // protocol: udp
		0x0d, 0x96, // port 3478
		0x00, 0x00,
	}
}
