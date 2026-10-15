// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"net/netip"
	"strings"
)

// PodIP mode is the one addressing mode whose relay address is held by the pod
// rather than by the node. Its contract lives here because the planner and the
// admission webhook both have to read the same declaration the controller
// observes: what an address looks like, and whether it may be advertised at all
// (docs/adr/0018-addressing-mode-podip.md).

// PodAddressesAnnotation declares, on a relay node, the addresses reserved for
// a PodIP pool's relays: one relay per address, each owning the whole relay
// port range. A declaration may carry the prefix length the pod's attachment
// should ask its IPAM for ("203.0.113.21/24"); the relay always advertises the
// bare address.
//
// The declaration is on the node, not in the pool spec, and that is what
// separates the mode from ExternalIPPool: there the address must already be
// attached to the node and the pool only names it, while here the operator
// reserves the address for a pod and says so where the address lives. The
// operator owns the IPAM; this operator does not.
const PodAddressesAnnotation = "turnfleet.hzmi.dev/relay-pod-addresses"

// PodAddress decodes a declared PodIP address: the bare address a relay binds
// and advertises, and whether the declaration is usable at all.
//
// A declaration may carry a prefix ("203.0.113.21/24"). The prefix is not part
// of the relay identity — a relay is (address, port range) — but it is what
// the CNI IPAM plugins want in a pod's "ips" request, so the controller passes
// the declaration through unchanged and the planner keeps the bare address.
func PodAddress(declared string) (string, bool) {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return "", false
	}
	if prefix, err := netip.ParsePrefix(declared); err == nil {
		return prefix.Addr().Unmap().String(), true
	}
	if addr, err := netip.ParseAddr(declared); err == nil {
		return addr.Unmap().String(), true
	}
	return "", false
}

// PodIPAttachmentParts splits addressing.podIP.attachmentRef into the namespace
// and name of the NetworkAttachmentDefinition it names. A reference without a
// namespace is the pool's own.
func PodIPAttachmentParts(ref, poolNamespace string) (namespace, name string, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", false
	}
	if namespace, name, found := strings.Cut(ref, "/"); found {
		namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
		if namespace == "" || name == "" {
			return "", "", false
		}
		return namespace, name, true
	}
	return poolNamespace, ref, true
}

// IndividuallyRoutable reports whether an address may be a PodIP relay
// address, and the reason it may not.
//
// A PodIP relay advertises the address the pod itself holds and no load
// balancer is in the path, so the address has to be one a peer's network can
// route to on its own. Whether it is, is the operator's judgement and the
// external vantage point (docs/adr/0007-probe-vantage-point.md) is what proves
// it: an address inside a private range (RFC 1918), the carrier-grade NAT
// range (RFC 6598) or an IPv6 unique local range (RFC 4193) is legal when the
// peers route it, which is a hub-and-spoke cluster whose pod CIDR is peered
// while the node subnet is not.
//
// This function therefore refuses only the addresses that cannot work at all —
// unspecified, loopback, link-local unicast and multicast — where admitting
// one would produce a relay that allocates, probes healthy in-cluster, and is
// unreachable from every peer.
func IndividuallyRoutable(address string) (routable bool, reason string) {
	addr, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return false, "is not an IP address"
	}
	addr = addr.Unmap()

	switch {
	case addr.IsUnspecified():
		return false, "is the unspecified address (0.0.0.0 or ::)"
	case addr.IsLoopback():
		return false, "is a loopback address"
	case addr.IsLinkLocalUnicast():
		return false, "is a link-local address (RFC 3927 or RFC 4291)"
	case addr.IsMulticast():
		return false, "is a multicast address"
	}
	return true, ""
}
