// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
)

// PortRange is an inclusive range of relay ports a single relay owns.
type PortRange struct {
	Start int32
	End   int32
}

// Ports returns how many concurrent allocations the range can hold, which is
// the relay's capacity.
func (r PortRange) Ports() int32 {
	if r.End < r.Start {
		return 0
	}
	return r.End - r.Start + 1
}

// Contains reports whether port is inside the range.
func (r PortRange) Contains(port int32) bool {
	return port >= r.Start && port <= r.End
}

// Overlaps reports whether two ranges share at least one port. Relays that
// share a relay address must never overlap.
func (r PortRange) Overlaps(other PortRange) bool {
	return r.Start <= other.End && other.Start <= r.End
}

// String renders the range as "start-end".
func (r PortRange) String() string {
	return strconv.Itoa(int(r.Start)) + "-" + strconv.Itoa(int(r.End))
}

// Same reports whether two ranges are identical.
func (r PortRange) Same(other PortRange) bool {
	return r.Start == other.Start && r.End == other.End
}

// Relay is one planned relay: the unit the operator creates a pod for. A relay's
// identity is its relay address and port range, which together are the public
// relay identity clients are handed.
type Relay struct {
	// Name is the relay's pod name: "<pool>-relay-<5-char hash of the
	// identity>". It is derived from the identity so a controller restart, or
	// failover to another leader, recreates the same pod for the same identity.
	Name string

	// Identity is the stable hash of the relay address and port range.
	Identity string

	// NodeName is the node the relay runs on.
	NodeName string

	// Address is the address the relay advertises in XOR-RELAYED-ADDRESS.
	Address string

	// BindAddress is the local address the relay binds. It differs from Address
	// only when the advertised address is not local (a NAT/override case), in
	// which case the renderer emits coturn's public/private mapping.
	BindAddress string

	// PortRange is the slice of the pool's port space this relay owns.
	PortRange PortRange
}

// Capacity is the number of concurrent allocations this relay can hold.
func (r *Relay) Capacity() int32 { return r.PortRange.Ports() }

// Node is one relay node as the planner sees it.
type Node struct {
	// Name is the node's name.
	Name string

	// Address is the address a HostNetwork or PartitionedIP relay on this node
	// advertises: the node's ExternalIP, or the
	// turnfleet.hzmi.dev/relay-address override.
	Address string

	// BindAddress is the local address to bind, usually equal to Address. It
	// differs when Address is a NATed public address.
	BindAddress string

	// SecondaryAddresses are the addresses declared for this node in the pool's
	// externalIPPool, sorted. They are only used by the ExternalIPPool mode.
	SecondaryAddresses []string
}

// PlanInput is everything the planner needs. It never reads from the cluster
// itself: the controller observes, the planner decides.
type PlanInput struct {
	// PoolName is the TURNPool name, used for the relay pod names.
	PoolName string

	// Spec is the pool's spec.
	Spec turnfleetv1alpha1.TURNPoolSpec

	// Nodes are the candidate relay nodes, in any order.
	Nodes []Node
}

// Plan is the desired relay set.
type Plan struct {
	// Relays are the desired relays, ordered by node name and then by port
	// range, so the result is deterministic.
	Relays []Relay

	// Unplaced is how many of the requested replicas the addressing mode could
	// not place.
	Unplaced int32

	// Notes explain an incomplete placement, for the Ready condition message.
	Notes []string

	// PortsTotal is the sum of the relays' port ranges. For PartitionedIP this
	// is still one IP's port space: splitting an address does not create ports.
	PortsTotal int32
}

// RelayName builds the pod name for a relay identity.
//
// The name is derived from the identity, not from an index, so the same relay
// always has the same pod: a controller restart or a failover to another leader
// cannot shuffle relay identities, and a client's allocation keeps pointing at
// the process that holds it.
func RelayName(poolName, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	suffix := hex.EncodeToString(sum[:])[:5]
	return fmt.Sprintf("%s-relay-%s", poolName, suffix)
}

// identity builds the stable identity string for an address and port range.
func identity(address string, portRange PortRange) string {
	return address + "|" + portRange.String()
}

// ConfigHash returns the hash of a rendered coturn configuration. It is what
// the pod's turnfleet.hzmi.dev/config-hash annotation carries, so a change to
// the rendered configuration (or to the Secret it embeds) rolls the relay.
func ConfigHash(config string) string {
	sum := sha256.Sum256([]byte(config))
	return hex.EncodeToString(sum[:])
}

// ListenerURIs builds the TURN URIs clients can use to allocate on a relay.
//
// They are per relay and per listener: the credential issuer hands out the URI
// of a relay that has capacity, so a client never wastes a gathering cycle on a
// relay that is full or draining.
func ListenerURIs(address string, listeners []turnfleetv1alpha1.ListenerSpec) []string {
	uris := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		switch listener.Protocol {
		case turnfleetv1alpha1.ListenerProtocolUDP:
			uris = append(uris, fmt.Sprintf("turn:%s:%d?transport=udp", address, listener.Port))
		case turnfleetv1alpha1.ListenerProtocolTCP:
			uris = append(uris, fmt.Sprintf("turn:%s:%d?transport=tcp", address, listener.Port))
		case turnfleetv1alpha1.ListenerProtocolTLS:
			uris = append(uris, fmt.Sprintf("turns:%s:%d?transport=tcp", address, listener.Port))
		}
	}
	slices.Sort(uris)
	return uris
}
