// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package relay turns a TURNPool spec into a set of relays. The planner assigns
// each relay a public relay identity (an address plus a disjoint port range) and
// the renderer turns one relay into a coturn configuration.
//
// Everything in this package is a pure function of its inputs: no client, no
// clock and no I/O. Kubernetes-facing behaviour lives in the controller.
package relay

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// Defaults applied by the defaulting webhook. They are constants rather than
// literals so the controller, the renderer and the tests agree on one value.
const (
	// DefaultReplicas is not a thing: replicas is required, because the number
	// of relays is the capacity decision.

	// DefaultDrainTimeout bounds how long a draining relay keeps its
	// allocations before its pod is deleted anyway.
	DefaultDrainTimeout = 30 * time.Second

	// DefaultProbeInterval is how often each ready relay is probed.
	DefaultProbeInterval = 10 * time.Second

	// DefaultProbeTimeout bounds a single probe.
	DefaultProbeTimeout = 3 * time.Second

	// DefaultRelaysPerNode is how many relays share one node address in
	// PartitionedIP mode when the pool does not say.
	DefaultRelaysPerNode int32 = 2

	// DefaultPrometheusPort is coturn's metrics port.
	DefaultPrometheusPort int32 = 9641

	// DefaultPrometheusPath is coturn's metrics path.
	DefaultPrometheusPath = "/metrics"

	// DefaultPlainListeningPort and DefaultTLSListeningPort are coturn's
	// well-known listener ports, used by examples and tests.
	DefaultPlainListeningPort int32 = 3478
	DefaultTLSListeningPort   int32 = 5349
)

// DefaultDeniedCIDRs is the peer deny list a pool gets when it does not define
// one.
//
// A relay that can reach any peer is an SSRF pivot from the cluster onto
// whatever it can route to, so the default denies every private, loopback and
// link-local destination — including the cloud metadata address
// 169.254.169.254 — plus the ranges reserved for multicast and future use. In
// the common case (RFC 1918 pod and service CIDRs) this also covers the
// cluster's own pod and service networks; the controller appends the pod CIDRs
// it observes on the relay nodes, and an operator can add a non-RFC 1918
// service CIDR explicitly.
func DefaultDeniedCIDRs() []string {
	return []string{
		"0.0.0.0/8",      // "this network"
		"10.0.0.0/8",     // RFC 1918
		"100.64.0.0/10",  // RFC 6598 (carrier-grade NAT)
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local, including 169.254.169.254
		"172.16.0.0/12",  // RFC 1918
		"192.0.0.0/24",   // IETF protocol assignments
		"192.168.0.0/16", // RFC 1918
		"198.18.0.0/15",  // benchmarking
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved
		"255.255.255.255/32",
	}
}

// DefaultDeniedCIDRsV6 is the IPv6 half of the default peer deny list.
//
// It mirrors the IPv4 list where IPv6 has an equivalent: loopback, link-local
// (fe80::/10), unique local addresses (fc00::/7, the IPv6 RFC 1918) and
// multicast. ::ffff:0:0/96 matters twice over: it is the whole IPv4-mapped
// space as an IPv6 prefix, so it denies the metadata address in its mapped form
// ::ffff:169.254.169.254 as well as the IPv4 list denies it directly.
//
// coturn matches a peer address against these ranges whichever family the
// allocation is in, so an IPv6 relay receives the IPv6 ranges and the IPv4 ones.
func DefaultDeniedCIDRsV6() []string {
	return []string{
		"::/128",        // unspecified ("this host")
		"::1/128",       // loopback
		"::ffff:0:0/96", // IPv4-mapped, including ::ffff:169.254.169.254
		"fc00::/7",      // unique local addresses (RFC 4193)
		"fe80::/10",     // link-local
		"ff00::/8",      // multicast
	}
}

// DefaultDeniedCIDRsFor returns the default deny list for a pool's address
// family: the IPv6 list for an IPv6 pool, the IPv4 list for an IPv4 one, and
// both for a DualStack pool, which has peers of both families.
//
// A relay only ever reaches peers in its own address family — a v6 relay socket
// sends to v6 addresses — so the ranges of the other family would only make the
// list longer without denying anything.
func DefaultDeniedCIDRsFor(family turnfleetv1alpha1.AddressingFamily) []string {
	switch family {
	case turnfleetv1alpha1.AddressingFamilyIPv6:
		return DefaultDeniedCIDRsV6()
	case turnfleetv1alpha1.AddressingFamilyDualStack:
		return MergeCIDRs(DefaultDeniedCIDRs(), DefaultDeniedCIDRsV6())
	default:
		return DefaultDeniedCIDRs()
	}
}

// PeerRange converts a CIDR into the value coturn accepts for
// --denied-peer-ip and --allowed-peer-ip.
//
// coturn 4.18.0-r0 rejects CIDR notation outright — "Aborting: invalid
// denied-peer-ip value 203.0.113.0/24. Use IP or IP-IP range (CIDR is not
// supported)" — so a prefix is expanded to the inclusive range it covers, and a
// single-address prefix collapses to one address.
func PeerRange(cidr string) (string, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return "", fmt.Errorf("parsing peer CIDR %q: %w", cidr, err)
	}
	prefix = prefix.Masked()
	first := prefix.Addr()
	last, err := lastAddr(prefix)
	if err != nil {
		return "", err
	}
	if first == last {
		return first.String(), nil
	}
	return first.String() + "-" + last.String(), nil
}

// PeerCIDRFamily reports the address family a peer CIDR names, so a pool can
// be told that a range it declares can never match its relays' peers.
//
// The family is the one the notation belongs to, and a prefix is not unmapped
// the way a bare address is: ::ffff:0:0/96 is an IPv6 prefix that covers the
// whole IPv4-mapped space, while ::ffff:203.0.113.9 is an IPv4 address.
func PeerCIDRFamily(cidr string) (turnfleetv1alpha1.AddressingFamily, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return "", fmt.Errorf("parsing peer CIDR %q: %w", cidr, err)
	}
	if prefix.Addr().Is4() {
		return turnfleetv1alpha1.AddressingFamilyIPv4, nil
	}
	return turnfleetv1alpha1.AddressingFamilyIPv6, nil
}

// lastAddr returns the last address covered by prefix.
func lastAddr(prefix netip.Prefix) (netip.Addr, error) {
	bits := prefix.Bits()
	addr := prefix.Addr()
	if addr.Is4() {
		v := addr.As4()
		n := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
		hostBits := 32 - bits
		if hostBits < 0 || hostBits > 32 {
			return netip.Addr{}, fmt.Errorf("invalid prefix length %d for %s", bits, addr)
		}
		var mask uint32
		if hostBits < 32 {
			mask = (uint32(1) << hostBits) - 1
		} else {
			mask = ^uint32(0)
		}
		n |= mask
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n)
		return netip.AddrFrom4(b), nil
	}
	if addr.Is6() {
		v := addr.As16()
		hostBits := 128 - bits
		if hostBits < 0 || hostBits > 128 {
			return netip.Addr{}, fmt.Errorf("invalid prefix length %d for %s", bits, addr)
		}
		for i := range hostBits {
			byteIndex := 15 - i/8
			v[byteIndex] |= byte(1) << (i % 8)
		}
		return netip.AddrFrom16(v), nil
	}
	return netip.Addr{}, fmt.Errorf("unsupported address family in %s", prefix)
}

// PeerRanges converts a list of CIDRs into coturn peer range values, dropping
// duplicates and returning them in a stable order so the rendered configuration
// does not churn.
func PeerRanges(cidrs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(cidrs))
	out := make([]string, 0, len(cidrs))
	for _, cidr := range cidrs {
		r, err := PeerRange(cidr)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	slices.Sort(out)
	return out, nil
}

// MergeCIDRs returns the union of the given CIDR lists, deduplicated, with the
// base list first so that the pool's own policy reads in front of everything the
// controller adds.
func MergeCIDRs(base []string, extra ...[]string) []string {
	seen := make(map[string]struct{}, len(base))
	out := make([]string, 0, len(base))
	appendAll := func(list []string) {
		for _, cidr := range list {
			cidr = strings.TrimSpace(cidr)
			if cidr == "" {
				continue
			}
			if _, ok := seen[cidr]; ok {
				continue
			}
			seen[cidr] = struct{}{}
			out = append(out, cidr)
		}
	}
	appendAll(base)
	for _, list := range extra {
		appendAll(list)
	}
	return out
}

// DurationOrDefault returns the duration a spec field carries, or the fallback
// when the field is unset or non-positive. It keeps the "unset means default"
// rule in one place for the webhook, the controller and the renderer.
func DurationOrDefault(d *metav1.Duration, fallback time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return fallback
	}
	return d.Duration
}
