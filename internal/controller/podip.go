// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// PodIP mode is the one mode whose relay address is held by the pod rather
// than by the node, so everything address-related about it lives here: how the
// operator declares the address, how the pod is asked for it, and how the
// controller reads back what the pod actually got.
//
// The address is declared on the relay node
// (turnfleet.hzmi.dev/relay-pod-addresses), which is what makes the relay
// identity knowable before the pod exists: the pod name is derived from
// (address, port range), so an address that only arrived after scheduling
// would leave the pod unnameable. The planner turns each declaration into one
// relay; the controller asks the pod's Multus attachment for that exact
// address; and if the CNI gives the pod another one, the relay drains instead
// of advertising an address that is not the one clients were told
// (docs/adr/0006-drain-semantics.md, docs/adr/0018-addressing-mode-podip.md).

// podIPAttachment is a parsed addressing.podIP.attachmentRef.
type podIPAttachment struct {
	namespace string
	name      string
}

// podIPAttachmentRef parses the pool's attachment reference: "name" in the
// pool's namespace, or "namespace/name". A pool without a podIP block, or with
// an empty attachmentRef, has no attachment and binds the pod's own address on
// the default network.
func podIPAttachmentRef(pool *turnfleetv1alpha1.TURNPool) (podIPAttachment, bool) {
	if pool.Spec.Addressing.PodIP == nil || pool.Spec.Addressing.PodIP.AttachmentRef == nil {
		return podIPAttachment{}, false
	}
	namespace, name, ok := relay.PodIPAttachmentParts(*pool.Spec.Addressing.PodIP.AttachmentRef, pool.Namespace)
	if !ok {
		return podIPAttachment{}, false
	}
	return podIPAttachment{namespace: namespace, name: name}, true
}

// attachmentAddress returns the address a pod holds on one attachment, read
// from Multus' network-status annotation. The annotation is the only place an
// attachment's address appears: PodIP and PodIPs carry the primary CNI's
// addresses, which for a Multus pod is the default network.
func attachmentAddress(pod *corev1.Pod, attachment podIPAttachment) (string, bool) {
	raw := strings.TrimSpace(pod.Annotations[NetworkStatusAnnotation])
	if raw == "" {
		return "", false
	}
	var statuses []networkStatus
	if err := json.Unmarshal([]byte(raw), &statuses); err != nil {
		return "", false
	}
	// Multus names an attachment by its bare name, and by "namespace/name"
	// when the attachment lives in another namespace than the pod.
	names := []string{attachment.name, attachment.namespace + "/" + attachment.name}
	for _, status := range statuses {
		if !slices.Contains(names, status.Name) {
			continue
		}
		for _, ip := range status.IPs {
			if address, ok := relay.PodAddress(ip); ok {
				return address, true
			}
		}
	}
	return "", false
}

// networkStatus is one element of Multus' k8s.v1.cni.cncf.io/network-status
// annotation. Only the fields the read-back uses are modelled.
type networkStatus struct {
	Name string   `json:"name"`
	IPs  []string `json:"ips"`
}

// defaultNetworkAddress returns the pod's own address on the cluster's default
// network.
func defaultNetworkAddress(pod *corev1.Pod) (string, bool) {
	if address, ok := relay.PodAddress(pod.Status.PodIP); ok {
		return address, true
	}
	for _, ip := range pod.Status.PodIPs {
		if address, ok := relay.PodAddress(ip.IP); ok {
			return address, true
		}
	}
	return "", false
}

// relayAddressOnPod is the address the scheduled pod actually holds for the
// relay: the attachment's address when the pool names one, otherwise the pod's
// own address on the default network.
//
// The second return value is false while the address is not observable yet: a
// pod that is not scheduled, or whose CNI has not written the network-status
// annotation. An unobservable address is not a mismatch, it is a wait.
func relayAddressOnPod(pool *turnfleetv1alpha1.TURNPool, pod *corev1.Pod) (string, bool) {
	if attachment, ok := podIPAttachmentRef(pool); ok {
		return attachmentAddress(pod, attachment)
	}
	return defaultNetworkAddress(pod)
}

// relayAddressChanged reports whether a scheduled PodIP relay pod holds an
// address other than the one it was planned with.
//
// A relay's identity is its address and port range, and every allocation a
// client holds is bound to it. A pod that came up on another address therefore
// serves an address no client was told about, so it drains (ADR 6) rather than
// re-advertising the address it happens to hold: silently changing it would
// move the relay and strand the allocations on it.
func relayAddressChanged(pool *turnfleetv1alpha1.TURNPool, planned *relay.Relay, pod *corev1.Pod) bool {
	if pool.Spec.Addressing.Mode != turnfleetv1alpha1.AddressingModePodIP {
		return false
	}
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	observed, ok := relayAddressOnPod(pool, pod)
	return ok && observed != planned.Address
}

// podAddressRequests maps "<node>|<address>" to the declaration the pod is
// asked for, so a pod is attached to the exact address its relay was planned
// with rather than to whichever address the IPAM happens to hand out.
func podAddressRequests(observed *nodeObservation) map[string]string {
	requests := make(map[string]string)
	for i := range observed.relays {
		node := &observed.relays[i]
		for _, declared := range node.PodAddresses {
			address, ok := relay.PodAddress(declared)
			if !ok {
				continue
			}
			requests[node.Name+"|"+address] = declared
		}
	}
	return requests
}

// usablePodAddresses drops the PodIP addresses this pool must not advertise,
// and reports each one with its reason.
//
// Admission checks the same rule, but node annotations change after admission:
// a node that is annotated with a private address later would otherwise turn
// every relay on it into one that allocates, probes healthy in-cluster and is
// unreachable from every peer. The reconcile re-checks the declaration and
// surfaces what it dropped, so the rule is enforced, not just advised
// (docs/adr/0012-cross-pool-address-conflict.md, for the same reasoning).
func usablePodAddresses(pool *turnfleetv1alpha1.TURNPool, observed *nodeObservation) []string {
	if pool.Spec.Addressing.Mode != turnfleetv1alpha1.AddressingModePodIP {
		return nil
	}

	var problems []string
	for i := range observed.relays {
		node := &observed.relays[i]
		kept := make([]string, 0, len(node.PodAddresses))
		for _, declared := range node.PodAddresses {
			address, ok := relay.PodAddress(declared)
			if !ok {
				problems = append(problems, fmt.Sprintf("node %s declares %q, which is not an IP address", node.Name, declared))
				continue
			}
			if routable, reason := relay.IndividuallyRoutable(address); !routable {
				problems = append(problems, fmt.Sprintf("node %s declares relay address %s, which %s", node.Name, address, reason))
				continue
			}
			kept = append(kept, declared)
		}
		node.PodAddresses = kept
	}
	return problems
}

// podIPNetworksAnnotation builds Multus' k8s.v1.cni.cncf.io/networks annotation
// that attaches the relay pod to the pool's attachment and requests the
// declared address.
//
// The "ips" entry is the per-pod IP request Multus forwards to the
// attachment's IPAM as the CNI "ips" capability. IPAM plugins that honour it
// (static IPAM, whereabouts) give the pod exactly the declared address; ones
// that do not (azure-ipam, SR-IOV with host-local) assign their own, and the
// read-back then drains the relay until the declaration and the CNI agree.
func podIPNetworksAnnotation(poolNamespace string, attachment podIPAttachment, request string) (string, error) {
	element := multusAttachment{Name: attachment.name}
	if attachment.namespace != poolNamespace {
		// Multus defaults the namespace to the pod's own, so it is only worth
		// writing when the attachment lives elsewhere.
		element.Namespace = attachment.namespace
	}
	if request != "" {
		element.IPs = []string{request}
	}
	encoded, err := json.Marshal([]multusAttachment{element})
	if err != nil {
		return "", fmt.Errorf("encoding the multus attachment %s/%s: %w", attachment.namespace, attachment.name, err)
	}
	return string(encoded), nil
}

// multusAttachment is one element of Multus' network selection annotation, with
// only the fields turnfleet sets.
type multusAttachment struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	IPs       []string `json:"ips,omitempty"`
}
