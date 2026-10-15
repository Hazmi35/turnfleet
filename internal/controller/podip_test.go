// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// plannedPodIPAddress is the address every planned PodIP relay fixture in this
// file holds.
const plannedPodIPAddress = "203.0.113.21"

// podIPPool is a PodIP pool with an attachment, which is the explicit form of
// the mode.
func podIPPool(attachment string) *turnfleetv1alpha1.TURNPool {
	pool := hostNetworkPool("demo", 2)
	pool.Namespace = "turnfleet"
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode:  turnfleetv1alpha1.AddressingModePodIP,
		PodIP: turnfleetv1alpha1.PodIPSpec{AttachmentRef: attachment},
	}
	return pool
}

// plannedPodIPRelay is one PodIP relay as the planner would hand it over.
func plannedPodIPRelay() relay.Relay {
	identity := plannedPodIPAddress + "|49152-49199"
	return relay.Relay{
		Name:        relay.RelayName("demo", identity),
		Identity:    identity,
		NodeName:    "worker-1",
		Address:     plannedPodIPAddress,
		BindAddress: plannedPodIPAddress,
		PortRange:   relay.PortRange{Start: 49152, End: 49199},
	}
}

// relayPod builds a relay pod in the state the read-back sees.
func relayPod(addresses []corev1.PodIP, status string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "demo-relay-abcde",
			Namespace:   "turnfleet",
			Annotations: map[string]string{},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: addresses},
	}
	if len(addresses) > 0 {
		pod.Status.PodIP = addresses[0].IP
	}
	if status != "" {
		pod.Annotations[NetworkStatusAnnotation] = status
	}
	return pod
}

// networkStatusAnnotation renders Multus' network-status annotation for the
// pool's attachment.
func networkStatusAnnotation(t *testing.T, ips ...string) string {
	t.Helper()

	encoded, err := json.Marshal([]networkStatus{{Name: "turnfleet/relay-pods", IPs: ips}})
	if err != nil {
		t.Fatalf("encoding the network status: %v", err)
	}
	return string(encoded)
}

// TestPodIPRelayPodsRunInTheirOwnNetworkNamespace is the difference the mode
// exists for: the relay holds the pod's address, so the pod must not share the
// node's network namespace, and its DNS must be the cluster's, not the host's.
func TestPodIPRelayPodsRunInTheirOwnNetworkNamespace(t *testing.T) {
	t.Parallel()

	pool := podIPPool("relay-pods")
	state := &relayState{relay: plannedPodIPRelay(), addressRequest: "203.0.113.21/24"}

	pod, err := desiredPod(pool, state)
	if err != nil {
		t.Fatalf("desiredPod() failed: %v", err)
	}
	if pod.Spec.HostNetwork {
		t.Error("a PodIP relay pod must not run in the host network namespace: its address would be the node's")
	}
	if pod.Spec.DNSPolicy != corev1.DNSClusterFirst {
		t.Errorf("DNSPolicy = %q, want %q for a pod with its own network namespace", pod.Spec.DNSPolicy, corev1.DNSClusterFirst)
	}

	// The pod is attached to the pool's attachment and asks it for the address
	// the relay was planned with, so the CNI has the chance to hand out exactly
	// that address.
	var requested []multusAttachment
	annotation := pod.Annotations[NetworksAnnotation]
	if annotation == "" {
		t.Fatalf("the pod carries no %s annotation:\n%v", NetworksAnnotation, pod.Annotations)
	}
	if err := json.Unmarshal([]byte(annotation), &requested); err != nil {
		t.Fatalf("the %s annotation is not valid JSON (%q): %v", NetworksAnnotation, annotation, err)
	}
	if len(requested) != 1 || requested[0].Name != "relay-pods" {
		t.Errorf("attachment annotation = %q, want one element naming relay-pods", annotation)
	} else if len(requested[0].IPs) != 1 || requested[0].IPs[0] != "203.0.113.21/24" {
		t.Errorf("requested addresses = %v, want the declared declaration 203.0.113.21/24", requested[0].IPs)
	}
	if strings.Contains(annotation, `"namespace"`) {
		t.Errorf("the attachment is in the pool's namespace, so no namespace belongs in the annotation: %q", annotation)
	}
}

// TestEveryOtherModeKeepsTheHostNetwork keeps the mode boundary visible: only
// PodIP runs in a pod network namespace.
func TestEveryOtherModeKeepsTheHostNetwork(t *testing.T) {
	t.Parallel()

	for _, mode := range []turnfleetv1alpha1.AddressingMode{
		turnfleetv1alpha1.AddressingModeHostNetwork,
		turnfleetv1alpha1.AddressingModeExternalIPPool,
		turnfleetv1alpha1.AddressingModePartitionedIP,
	} {
		pool := hostNetworkPool("demo", 1)
		pool.Spec.Addressing.Mode = mode
		state := &relayState{relay: plannedPodIPRelay()}

		pod, err := desiredPod(pool, state)
		if err != nil {
			t.Fatalf("desiredPod() failed for %s: %v", mode, err)
		}
		if !pod.Spec.HostNetwork {
			t.Errorf("mode %s: HostNetwork = false, want true: the relay binds a node address", mode)
		}
		if pod.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
			t.Errorf("mode %s: DNSPolicy = %q, want %q", mode, pod.Spec.DNSPolicy, corev1.DNSClusterFirstWithHostNet)
		}
		if _, ok := pod.Annotations[NetworksAnnotation]; ok {
			t.Errorf("mode %s: the pod carries a %s annotation although the pool declares no attachment", mode, NetworksAnnotation)
		}
	}
}

// TestPodIPattachmentCrossNamespace checks the annotation form when the
// attachment lives in another namespace than the pool: Multus needs the
// namespace written out, or it looks for the attachment next to the pod.
func TestPodIPAttachmentCrossNamespace(t *testing.T) {
	t.Parallel()

	annotation, err := podIPNetworksAnnotation("turnfleet", podIPAttachment{namespace: "network-team", name: "sriov-net"}, "203.0.113.21")
	if err != nil {
		t.Fatalf("podIPNetworksAnnotation() failed: %v", err)
	}
	if !strings.Contains(annotation, `"namespace":"network-team"`) {
		t.Errorf("annotation %q does not name the attachment's namespace", annotation)
	}
}

// TestPodIPAddressReadBack is the stability check: the address a relay was
// planned with is compared with the address the pod actually holds, per flavour
// of the mode, and a mismatch is what drains the relay.
func TestPodIPAddressReadBack(t *testing.T) {
	t.Parallel()

	attachmentPool := podIPPool("relay-pods") // pool namespace "turnfleet"
	noAttachmentPool := podIPPool("")         // the pod's own address on the default network
	otherMode := hostNetworkPool("demo", 1)

	tests := []struct {
		name    string
		pool    *turnfleetv1alpha1.TURNPool
		pod     *corev1.Pod
		changed bool
	}{
		{
			name: "attachment holds the planned address",
			pool: attachmentPool,
			pod: relayPod([]corev1.PodIP{{IP: "10.244.0.9"}},
				networkStatusAnnotation(t, "203.0.113.21/24")),
		},
		{
			name: "attachment holds another address",
			pool: attachmentPool,
			pod: relayPod([]corev1.PodIP{{IP: "10.244.0.9"}},
				networkStatusAnnotation(t, "203.0.113.99/24")),
			changed: true,
		},
		{
			name: "the primary address is not the attachment's",
			pool: attachmentPool,
			// The pod IP equals the planned address, but the attachment holds
			// another one: the read-back must follow the attachment, not the
			// primary CNI.
			pod:     relayPod([]corev1.PodIP{{IP: "203.0.113.21"}}, networkStatusAnnotation(t, "203.0.113.99/24")),
			changed: true,
		},
		{
			name: "the attachment has not answered yet",
			pool: attachmentPool,
			pod:  relayPod([]corev1.PodIP{{IP: "10.244.0.9"}}, ""),
		},
		{
			name: "a pod that is not running is not judged",
			pool: attachmentPool,
			pod: func() *corev1.Pod {
				pod := relayPod([]corev1.PodIP{{IP: "10.244.0.9"}}, networkStatusAnnotation(t, "203.0.113.99/24"))
				pod.Status.Phase = corev1.PodPending
				return pod
			}(),
		},
		{
			name: "the pod's own address matches",
			pool: noAttachmentPool,
			pod:  relayPod([]corev1.PodIP{{IP: "203.0.113.21"}}, ""),
		},
		{
			name:    "the pod's own address moved",
			pool:    noAttachmentPool,
			pod:     relayPod([]corev1.PodIP{{IP: "203.0.113.99"}}, ""),
			changed: true,
		},
		{
			name: "another mode is never judged by its pod address",
			pool: otherMode,
			pod:  relayPod([]corev1.PodIP{{IP: "203.0.113.99"}}, ""),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			planned := plannedPodIPRelay()
			if got := relayAddressChanged(tc.pool, &planned, tc.pod); got != tc.changed {
				t.Errorf("relayAddressChanged() = %v, want %v", got, tc.changed)
			}
		})
	}
}

// TestDrainFollowsTheAddressChange pins the consequence: a relay on another
// address is drained even when its configuration hash is unchanged, because it
// serves an address no client was given.
func TestDrainFollowsTheAddressChange(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        "demo-relay-abcde",
		Annotations: map[string]string{ConfigHashAnnotation: "unchanged"},
	}}
	state := &relayState{
		relay:          plannedPodIPRelay(),
		planned:        true,
		hash:           "unchanged",
		pod:            pod,
		addressChanged: true,
	}
	if !drainRequired(state) {
		t.Error("a relay whose pod holds another address must drain")
	}

	state.addressChanged = false
	if drainRequired(state) {
		t.Error("an unchanged relay on its planned address must not drain")
	}
}

// TestAddressOwningModesSkipRequiredAntiAffinity is the capacity bug this mode
// exposed: two relays may share a node whenever they own different addresses,
// so required anti-affinity must not be set for the modes that give every relay
// an address of its own. Without this the second relay on a node stays Pending
// forever and the pool serves half of what it reports.
func TestAddressOwningModesSkipRequiredAntiAffinity(t *testing.T) {
	t.Parallel()

	addressOwning := []turnfleetv1alpha1.AddressingMode{
		turnfleetv1alpha1.AddressingModeExternalIPPool,
		turnfleetv1alpha1.AddressingModePodIP,
		turnfleetv1alpha1.AddressingModePartitionedIP,
	}
	for _, mode := range addressOwning {
		pool := podIPPool("relay-pods")
		pool.Spec.Addressing.Mode = mode
		state := &relayState{relay: plannedPodIPRelay()}

		spec := podSpec(pool, state)
		if spec.Affinity == nil || spec.Affinity.PodAntiAffinity != nil {
			t.Errorf("mode %s: the pod carries required pod anti-affinity, so only one relay could ever schedule on a node", mode)
		}
	}

	// HostNetwork on one family is the case where one relay per node is the
	// rule: the second one would have to give up the node's only address.
	pool := hostNetworkPool("demo", 1)
	state := &relayState{relay: plannedPodIPRelay()}
	spec := podSpec(pool, state)
	if spec.Affinity == nil || spec.Affinity.PodAntiAffinity == nil {
		t.Error("HostNetwork must keep required anti-affinity: its nodes offer one relay address each")
	}
}

func TestNodePodAddresses(t *testing.T) {
	t.Parallel()

	node := func(annotation string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-1",
			Annotations: map[string]string{relay.PodAddressesAnnotation: annotation},
		}}
	}

	declared, err := nodePodAddresses(node("203.0.113.21, 203.0.113.22/24 ,2001:db8:100::21/64"))
	if err != nil {
		t.Fatalf("nodePodAddresses() failed: %v", err)
	}
	want := []string{"203.0.113.21", "203.0.113.22/24", "2001:db8:100::21/64"}
	if strings.Join(declared, ",") != strings.Join(want, ",") {
		t.Errorf("declared = %v, want %v", declared, want)
	}

	empty, err := nodePodAddresses(&corev1.Node{})
	if err != nil || empty != nil {
		t.Errorf("nodePodAddresses() with no annotation = (%v, %v), want (nil, nil)", empty, err)
	}

	if _, err := nodePodAddresses(node("203.0.113.21,rubbish")); err == nil {
		t.Error("a malformed declaration must be an error, not a silently dropped address")
	}
}

// TestUsablePodAddressesEnforcesRoutability is the reconcile-time half of the
// admission rule: an address a node declares that cannot work at all is dropped
// from the plan and reported, so an edit behind admission's back is still
// enforced. A private address is kept — whether the peers route it is the
// operator's judgement, and the vantage point is what proves it.
func TestUsablePodAddressesEnforcesRoutability(t *testing.T) {
	t.Parallel()

	pool := podIPPool("relay-pods")
	observed := &nodeObservation{relays: []relay.Node{{
		Name:         "worker-1",
		PodAddresses: []string{"203.0.113.21", "10.0.0.5", "127.0.0.5", "rubbish"},
	}}}

	problems := usablePodAddresses(pool, observed)
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want one per unusable declaration", problems)
	}
	if !strings.Contains(problems[0], "127.0.0.5") || !strings.Contains(problems[0], "loopback") {
		t.Errorf("the loopback address problem does not name the address and the reason: %q", problems[0])
	}
	if !strings.Contains(problems[1], "rubbish") || !strings.Contains(problems[1], "not an IP address") {
		t.Errorf("the malformed declaration problem does not name the declaration and the reason: %q", problems[1])
	}
	if got := observed.relays[0].PodAddresses; len(got) != 2 || got[0] != "203.0.113.21" || got[1] != "10.0.0.5" {
		t.Errorf("declared addresses kept = %v, want the public and the private one", got)
	}

	// Another mode declares no pod addresses, so nothing is filtered or
	// reported.
	other := hostNetworkPool("demo", 1)
	untouched := &nodeObservation{relays: []relay.Node{{Name: "worker-1", PodAddresses: []string{"10.0.0.5"}}}}
	if problems := usablePodAddresses(other, untouched); problems != nil {
		t.Errorf("problems = %v, want none for a non-PodIP pool", problems)
	}
	if len(untouched.relays[0].PodAddresses) != 1 {
		t.Error("a non-PodIP pool's observation was modified")
	}
}

// TestPodAddressRequests maps the planned relay back to the declaration the pod
// is asked for, which is what makes the pod receive the address its relay was
// planned with.
func TestPodAddressRequests(t *testing.T) {
	t.Parallel()

	observed := &nodeObservation{relays: []relay.Node{
		{Name: "worker-2", PodAddresses: []string{"203.0.113.22/24"}},
		{Name: "worker-1", PodAddresses: []string{"203.0.113.21"}},
	}}
	requests := podAddressRequests(observed)

	for key, want := range map[string]string{
		"worker-1|203.0.113.21": "203.0.113.21",
		"worker-2|203.0.113.22": "203.0.113.22/24",
	} {
		if got := requests[key]; got != want {
			t.Errorf("requests[%q] = %q, want %q", key, got, want)
		}
	}
}
