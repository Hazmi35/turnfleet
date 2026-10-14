// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
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

	// ListenerPortOffset is added to every declared listener port.
	//
	// It is zero whenever a relay owns its address on its own (HostNetwork and
	// ExternalIPPool). In PartitionedIP mode several relays share one node
	// address, and a listening port can only be bound once per address, so each
	// relay gets its own listener port from the pool's base: without this, every
	// partitioned relay but the first would fail to bind and the mode would
	// serve a fraction of the capacity it claims.
	ListenerPortOffset int32

	// MetricsPortOffset is added to the metrics port.
	//
	// coturn's prometheus exporter does not bind the relay's own address: it
	// binds every address on the node ("prometheus exporter server will listen
	// on [::]:9641", verified against the pinned image). Relays that share a
	// node therefore need distinct metrics ports even when they own different
	// addresses, which is exactly what a DualStack pool does — one relay per
	// address family on the node.
	MetricsPortOffset int32
}

// Capacity is the number of concurrent allocations this relay can hold.
func (r *Relay) Capacity() int32 { return r.PortRange.Ports() }

// Node is one relay node as the planner sees it.
type Node struct {
	// Name is the node's name.
	Name string

	// Addresses are the node's own addresses for HostNetwork and PartitionedIP
	// relays: at most one per address family. They are sorted by family (IPv4
	// first), which is what makes a DualStack pool deterministic.
	Addresses []NodeAddress

	// SecondaryAddresses are the addresses declared for this node in the pool's
	// externalIPPool, sorted. They are only used by the ExternalIPPool mode.
	SecondaryAddresses []string

	// PodAddresses are the addresses the operator declares for this node for
	// PodIP relays, as written in the node's
	// turnfleet.hzmi.dev/relay-pod-addresses annotation: a bare address, or an
	// address with the prefix length the pod's attachment should request. One
	// relay per address. They are only used by the PodIP mode.
	PodAddresses []string
}

// NodeAddress is one address a relay on a node could advertise, with the local
// address it would bind.
type NodeAddress struct {
	// Address is the address the relay advertises in XOR-RELAYED-ADDRESS.
	Address string

	// BindAddress is the local address the relay binds. It differs from Address
	// when the advertised address is not local (a NAT public address, or the
	// turnfleet.hzmi.dev/relay-address override).
	BindAddress string
}

// addressOf returns the node's address in one family.
func (n *Node) addressOf(family turnfleetv1alpha1.AddressingFamily) (NodeAddress, bool) {
	for _, address := range n.Addresses {
		if AddressFamilyOf(address.Address) == family {
			return address, true
		}
	}
	return NodeAddress{}, false
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

// AddressFamilyOf reports the address family an address belongs to, or the
// empty family when the string is not an IP address. An IPv4-mapped IPv6
// address is IPv4: it reaches an IPv4 peer.
func AddressFamilyOf(address string) turnfleetv1alpha1.AddressingFamily {
	addr, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return ""
	}
	if addr.Unmap().Is4() {
		return turnfleetv1alpha1.AddressingFamilyIPv4
	}
	return turnfleetv1alpha1.AddressingFamilyIPv6
}

// AddressFamilies returns the families a pool plans relays in, IPv4 first so
// the plan is deterministic.
//
// An unset family is the default, IPv4. So is a value the CRD enum rejects:
// like the API default, the planner reads it as IPv4 rather than dropping every
// relay, and the webhook is what tells an author their value is not supported.
func AddressFamilies(family turnfleetv1alpha1.AddressingFamily) []turnfleetv1alpha1.AddressingFamily {
	switch family {
	case turnfleetv1alpha1.AddressingFamilyIPv6:
		return []turnfleetv1alpha1.AddressingFamily{turnfleetv1alpha1.AddressingFamilyIPv6}
	case turnfleetv1alpha1.AddressingFamilyDualStack:
		return []turnfleetv1alpha1.AddressingFamily{
			turnfleetv1alpha1.AddressingFamilyIPv4,
			turnfleetv1alpha1.AddressingFamilyIPv6,
		}
	default:
		return []turnfleetv1alpha1.AddressingFamily{turnfleetv1alpha1.AddressingFamilyIPv4}
	}
}

// PlansFamily reports whether a pool's family selection includes one family.
func PlansFamily(selected, family turnfleetv1alpha1.AddressingFamily) bool {
	return slices.Contains(AddressFamilies(selected), family)
}

// ListenerURIs builds the TURN URIs clients can use to allocate on a relay.
//
// They are per relay and per listener: the credential issuer hands out the URI
// of a relay that has capacity, so a client never wastes a gathering cycle on a
// relay that is full or draining.
//
// The authority is wrapped the way RFC 7065 requires, so an IPv6 relay address
// reads turn:[2001:db8::1]:3478 rather than an unparseable turn:2001:db8::1:3478.
func ListenerURIs(r *Relay, listeners []turnfleetv1alpha1.ListenerSpec) []string {
	uris := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		port := listener.Port + r.ListenerPortOffset
		hostPort := net.JoinHostPort(r.Address, strconv.Itoa(int(port)))
		switch listener.Protocol {
		case turnfleetv1alpha1.ListenerProtocolUDP:
			uris = append(uris, fmt.Sprintf("turn:%s?transport=udp", hostPort))
		case turnfleetv1alpha1.ListenerProtocolTCP:
			uris = append(uris, fmt.Sprintf("turn:%s?transport=tcp", hostPort))
		case turnfleetv1alpha1.ListenerProtocolTLS:
			uris = append(uris, fmt.Sprintf("turns:%s?transport=tcp", hostPort))
		}
	}
	slices.Sort(uris)
	return uris
}

// ListenerPort is the port this relay serves for one of the pool's listeners.
func (r *Relay) ListenerPort(listener *turnfleetv1alpha1.ListenerSpec) int32 {
	return listener.Port + r.ListenerPortOffset
}

// MetricsPort is the port coturn serves its metrics endpoint on.
//
// It is offset for the same reason the listener ports are: relays on one node
// cannot both bind one port, and coturn's metrics endpoint binds every address
// on the node rather than the relay's own address.
func (r *Relay) MetricsPort() int32 {
	return DefaultPrometheusPort + r.MetricsPortOffset
}

// HighestListenerPort is the largest listener port this relay serves.
func (r *Relay) HighestListenerPort(listeners []turnfleetv1alpha1.ListenerSpec) int32 {
	highest := int32(0)
	for i := range listeners {
		if port := r.ListenerPort(&listeners[i]); port > highest {
			highest = port
		}
	}
	return highest
}

// ParseIdentity decodes a relay identity back into the address and port range it
// was built from. The controller uses it to recognise a relay pod whose status
// entry was lost, without trusting the pod's annotations for anything else.
func ParseIdentity(identity string) (string, PortRange, error) {
	address, rangePart, ok := strings.Cut(identity, "|")
	if !ok || address == "" {
		return "", PortRange{}, fmt.Errorf("relay identity %q is not <address>|<start>-<end>", identity)
	}
	start, end, ok := strings.Cut(rangePart, "-")
	if !ok {
		return "", PortRange{}, fmt.Errorf("relay identity %q has no port range", identity)
	}
	startPort, err := strconv.ParseInt(start, 10, 32)
	if err != nil {
		return "", PortRange{}, fmt.Errorf("relay identity %q has an unreadable start port: %w", identity, err)
	}
	endPort, err := strconv.ParseInt(end, 10, 32)
	if err != nil {
		return "", PortRange{}, fmt.Errorf("relay identity %q has an unreadable end port: %w", identity, err)
	}
	return address, PortRange{Start: int32(startPort), End: int32(endPort)}, nil
}
