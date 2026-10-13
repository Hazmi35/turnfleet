// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AddressingMode selects how relay addresses are assigned to nodes.
//
// The choice decides capacity, not just placement: a relay's capacity is the
// number of ports in its own range, so only modes that add relay addresses add
// capacity. See docs/capacity-model.md.
//
// +kubebuilder:validation:Enum=HostNetwork;ExternalIPPool;PartitionedIP
type AddressingMode string

const (
	// AddressingModeHostNetwork runs exactly one relay per relay node, binding
	// the node's ExternalIP (or the turnfleet.hzmi.dev/relay-address override).
	// Capacity grows with the number of labelled nodes.
	AddressingModeHostNetwork AddressingMode = "HostNetwork"

	// AddressingModeExternalIPPool binds relays to specific secondary addresses
	// declared in addressing.externalIPPool.addresses, one relay per address.
	// Capacity grows with the number of declared addresses.
	AddressingModeExternalIPPool AddressingMode = "ExternalIPPool"

	// AddressingModePartitionedIP runs several relays per node on the node's
	// single address, each owning a disjoint slice of the relay port range. It
	// adds concurrency and isolation but NOT capacity: the sum of the slices is
	// still one IP's port space.
	AddressingModePartitionedIP AddressingMode = "PartitionedIP"
)

// ListenerProtocol is the transport a relay listener accepts.
//
// coturn serves UDP and TCP on the same "plain" listening port and TLS/DTLS on
// its own port, so a pool may declare at most one listener of each protocol and
// its UDP and TCP listeners must share a port.
//
// +kubebuilder:validation:Enum=udp;tcp;tls
type ListenerProtocol string

const (
	// ListenerProtocolUDP accepts plain UDP on the pool's plain listening port.
	ListenerProtocolUDP ListenerProtocol = "udp"

	// ListenerProtocolTCP accepts plain TCP on the pool's plain listening port.
	ListenerProtocolTCP ListenerProtocol = "tcp"

	// ListenerProtocolTLS accepts TLS (and DTLS) on the listener's own port.
	ListenerProtocolTLS ListenerProtocol = "tls"
)

// PortRange is an inclusive range of UDP ports a relay may allocate.
//
// A relay's capacity in concurrent allocations equals the number of ports in
// its range. Ranges of relays that share one address must not overlap; the
// planner partitions them and the webhook rejects a pool that cannot be
// partitioned.
type PortRange struct {
	// start is the first port of the range, inclusive.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Start int32 `json:"start,omitempty"`

	// end is the last port of the range, inclusive. It must be greater than or
	// equal to start.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	End int32 `json:"end,omitempty"`
}

// DataplaneSpec configures the relay processes themselves.
type DataplaneSpec struct {
	// image is the coturn image every relay runs, pinned by digest
	// (`repo/name:tag@sha256:...`). A tag without a digest is rejected so a
	// pool cannot silently float to a new relay version.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image,omitempty"`

	// resources is the resource request and limit for each relay container.
	// Relay capacity is bounded by ports, not CPU, so this is about keeping the
	// process responsive under allocation churn.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// extraArgs are appended verbatim to the rendered coturn configuration.
	// They are hashed into the relay's config-hash like any other change, but
	// they are not validated against the pinned image: use them for flags the
	// API does not model.
	// +listType=atomic
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// ExternalAddress is one secondary address already attached to a node.
type ExternalAddress struct {
	// nodeName is the node the address is attached to.
	// +kubebuilder:validation:MinLength=1
	// +required
	NodeName string `json:"nodeName,omitempty"`

	// address is the address itself. It is both the address a relay binds and
	// the address it advertises in XOR-RELAYED-ADDRESS.
	// +kubebuilder:validation:MinLength=1
	// +required
	Address string `json:"address,omitempty"`
}

// ExternalIPPoolSpec configures AddressingModeExternalIPPool.
type ExternalIPPoolSpec struct {
	// addresses are the secondary addresses available to the pool, one relay per
	// address. Each entry must be attached to the named node; the operator does
	// not attach addresses. The webhook requires at least one address when the
	// pool's mode is ExternalIPPool.
	// +listType=map
	// +listMapKey=address
	// +optional
	Addresses []ExternalAddress `json:"addresses,omitempty"`
}

// PartitionedIPSpec configures AddressingModePartitionedIP.
type PartitionedIPSpec struct {
	// relaysPerNode is how many relays share one node address. The pool's relay
	// port range is split into that many disjoint slices; their sum is still one
	// IP's port space, so this setting adds concurrency, not capacity.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	// +default=2
	// +optional
	RelaysPerNode int32 `json:"relaysPerNode,omitempty"`
}

// AddressingSpec selects how relays are placed and addressed.
type AddressingSpec struct {
	// mode is the addressing mode. It is immutable: changing it changes every
	// relay's advertised address and invalidates live allocations.
	// +required
	Mode AddressingMode `json:"mode,omitempty"`

	// externalIPPool configures AddressingModeExternalIPPool and must be set
	// (with at least one address) when mode is ExternalIPPool. Forbidden for the
	// other modes.
	// +optional
	ExternalIPPool *ExternalIPPoolSpec `json:"externalIPPool,omitempty"`

	// partitionedIP configures AddressingModePartitionedIP and must be set when
	// mode is PartitionedIP. Forbidden for the other modes.
	// +optional
	PartitionedIP *PartitionedIPSpec `json:"partitionedIP,omitempty"`
}

// ListenerSpec is one TURN listener every relay serves.
type ListenerSpec struct {
	// name identifies the listener within the pool, for example "turn" or
	// "turns". It must be unique within spec.listeners.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name,omitempty"`

	// protocol is the transport the listener accepts.
	// +required
	Protocol ListenerProtocol `json:"protocol,omitempty"`

	// port is the listener port, bound on every relay's address. It must not
	// fall inside spec.relayPortRange: a listener and an allocation cannot share
	// a port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Port int32 `json:"port,omitempty"`

	// tlsSecretRef is the Secret holding the listener's certificate, with the
	// usual tls.crt and tls.key keys. Required for protocol tls, forbidden
	// otherwise. Rotating it changes every relay's config hash and rolls the
	// pool one relay at a time.
	// +optional
	TLSSecretRef *corev1.LocalObjectReference `json:"tlsSecretRef,omitempty"`
}

// AuthSpec is the shared authentication material for the pool.
type AuthSpec struct {
	// realm is the TURN realm relays advertise and authenticate against. It is
	// immutable: changing it invalidates every credential the issuer signed for
	// this pool.
	// +kubebuilder:validation:MinLength=1
	// +required
	Realm string `json:"realm,omitempty"`

	// secretRef selects the shared HMAC secret used for TURN REST API
	// credentials (and therefore by the credential issuer, which is not part of
	// this operator). Rotating the selected value rolls every relay one at a
	// time.
	// +required
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
}

// PeerPolicySpec restricts which peers a relay may send to.
//
// A relay must never be able to reach in-cluster or metadata addresses unless a
// pool explicitly allows it: an allocation that peers can point anywhere is an
// SSRF pivot into the cluster.
type PeerPolicySpec struct {
	// deniedCIDRs are rendered as coturn --denied-peer-ip entries. When empty
	// the webhook defaults them to RFC 1918, RFC 6598, loopback and link-local
	// (including 169.254.169.254), which also covers the cluster pod and service
	// CIDRs in the common case. The controller always appends the pod CIDRs it
	// observes on the relay nodes.
	// +listType=atomic
	// +optional
	DeniedCIDRs []string `json:"deniedCIDRs,omitempty"`

	// allowedCIDRs are rendered as coturn --allowed-peer-ip entries and take
	// precedence over deniedCIDRs for the addresses they name.
	// +listType=atomic
	// +optional
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`
}

// ProbeSpec configures the periodic TURN Allocate probe.
type ProbeSpec struct {
	// interval is how often each ready relay is probed. The probe performs a
	// real allocation and releases it again, so it holds a port only for the
	// duration of the check.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// timeout bounds a single probe, including the allocation and the optional
	// round trip.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// PlacementSpec controls where relay pods may run.
type PlacementSpec struct {
	// nodeSelector is merged with the mandatory relay-node label selector, so it
	// can narrow the relay nodes used by this pool but never widen them.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are added to the relay pods' tolerations, for clusters whose
	// relay nodes carry taints.
	// +listType=atomic
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// TURNPoolSpec defines the desired state of TURNPool.
type TURNPoolSpec struct {
	// replicas is the desired number of relays. It is explicit rather than
	// optional because the number of relays is the capacity decision: the value
	// is capped by the addresses the addressing mode makes available, and the
	// Ready condition says when it cannot be reached.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +required
	Replicas int32 `json:"replicas,omitempty"`

	// dataplane configures the relay processes.
	// +required
	Dataplane DataplaneSpec `json:"dataplane,omitzero"`

	// addressing selects how relays are placed and addressed. Immutable.
	// +required
	Addressing AddressingSpec `json:"addressing,omitzero"`

	// relayPortRange is the pool's UDP port space. Each relay owns a disjoint
	// slice of it; for HostNetwork and ExternalIPPool that slice is the whole
	// range. Immutable: changing it invalidates live allocations.
	// +required
	RelayPortRange PortRange `json:"relayPortRange,omitzero"`

	// listeners are the TURN listeners every relay serves.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +required
	Listeners []ListenerSpec `json:"listeners,omitempty"`

	// auth is the shared authentication material. realm is immutable.
	// +required
	Auth AuthSpec `json:"auth,omitzero"`

	// peerPolicy restricts which peers relays may send to.
	// +optional
	PeerPolicy *PeerPolicySpec `json:"peerPolicy,omitempty"`

	// drainTimeout is how long a draining relay may keep its allocations before
	// it is deleted anyway. It bounds rollouts and scale-downs; it does not
	// extend a relay's life when its node is already gone.
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`

	// probe configures the per-relay TURN Allocate probe.
	// +optional
	Probe *ProbeSpec `json:"probe,omitempty"`

	// placement controls where relay pods may run.
	// +optional
	Placement *PlacementSpec `json:"placement,omitempty"`
}

// ProbeStatus is the outcome of the most recent probe of a relay.
type ProbeStatus struct {
	// time is when the probe finished.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`

	// succeeded reports whether the relay answered the probe with the expected
	// relayed address and range.
	// +optional
	Succeeded *bool `json:"succeeded,omitempty"`

	// latencyMillis is the probe duration in milliseconds. Milliseconds rather
	// than seconds so short probes stay readable, and an integer because the API
	// has no floats.
	// +optional
	LatencyMillis *int64 `json:"latencyMillis,omitempty"`

	// message explains a failed probe, including the TURN error code when the
	// relay rejected the allocation.
	// +optional
	Message *string `json:"message,omitempty"`
}

// RelayStatus is the observed state of one relay.
type RelayStatus struct {
	// name is the relay identity: stable across restarts and controller
	// failover, derived from the relay's address and port range.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name,omitempty"`

	// podName is the pod running this relay.
	// +optional
	PodName *string `json:"podName,omitempty"`

	// nodeName is the node the relay runs on.
	// +optional
	NodeName *string `json:"nodeName,omitempty"`

	// relayAddress is the address the relay advertises in XOR-RELAYED-ADDRESS.
	// +optional
	RelayAddress *string `json:"relayAddress,omitempty"`

	// portRange is the slice of the pool's port space this relay owns. Every
	// relay owns one, so it is always present.
	// +required
	PortRange PortRange `json:"portRange,omitzero"`

	// turnURIs are the per-relay listener URIs clients can allocate on. A relay
	// that is not ready or is draining has none.
	// +listType=atomic
	// +optional
	TurnURIs []string `json:"turnURIs,omitempty"`

	// ready reports whether the relay is serving: pod ready and last probe
	// successful.
	// +optional
	Ready *bool `json:"ready,omitempty"`

	// draining reports whether the relay is on its way out. A draining relay is
	// removed from turnURIs before its pod is deleted.
	// +optional
	Draining *bool `json:"draining,omitempty"`

	// allocations is the number of live allocations on the relay, read from
	// coturn's metrics endpoint.
	// +optional
	Allocations *int32 `json:"allocations,omitempty"`

	// portsInUse is the number of relay ports currently held by allocations. One
	// allocation occupies one port, so this equals allocations; it is reported
	// separately because consumers reason about ports.
	// +optional
	PortsInUse *int32 `json:"portsInUse,omitempty"`

	// portsTotal is the number of ports this relay owns. Capacity is portsTotal;
	// the ratio consumers compute is portsInUse/portsTotal.
	// +optional
	PortsTotal *int32 `json:"portsTotal,omitempty"`

	// lastProbe is the most recent probe result, successful or not.
	// +optional
	LastProbe *ProbeStatus `json:"lastProbe,omitempty"`
}

// TURNPoolStatus defines the observed state of TURNPool.
type TURNPoolStatus struct {
	// conditions represent the current state of the TURNPool resource.
	//
	// The condition types are:
	//   - Ready: the desired number of relays exist, are ready and are not
	//     draining.
	//   - ProbeSucceeded: every ready relay last probed successfully.
	//   - CapacityPressure: a relay is near or at its port limit, or a probe
	//     observed TURN error 508.
	//   - Degraded: fewer relays than desired, or a relay is failing to converge.
	//
	// Ready and ProbeSucceeded are normal-true; CapacityPressure and Degraded are
	// abnormal-true.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// relays is the per-relay observed state, keyed by relay name.
	// +listType=map
	// +listMapKey=name
	// +optional
	Relays []RelayStatus `json:"relays,omitempty"`

	// portsInUse is the total number of relay ports held by allocations across
	// the pool.
	// +optional
	PortsInUse *int32 `json:"portsInUse,omitempty"`

	// portsTotal is the total number of relay ports the pool owns. Capacity is
	// bounded by distinct relay addresses times ports per address, not by the
	// number of relays.
	// +optional
	PortsTotal *int32 `json:"portsTotal,omitempty"`

	// readyRelays is the number of relays that are ready and not draining. It is
	// the status field the scale subresource reports.
	// +optional
	ReadyRelays *int32 `json:"readyRelays,omitempty"`

	// turnURIs lists the listener URIs of ready, non-draining relays. It is
	// mirrored into the <pool>-turn-uris ConfigMap for the credential issuer.
	// +listType=atomic
	// +optional
	TurnURIs []string `json:"turnURIs,omitempty"`

	// selector is the label selector matching the pool's relay pods, in string
	// form, exposed through the scale subresource for HPA and KEDA.
	// +optional
	Selector *string `json:"selector,omitempty"`

	// observedGeneration is the generation of the spec this status was computed
	// from.
	// +optional
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyRelays,selectorpath=.status.selector
// +kubebuilder:resource:path=turnpools,singular=turnpool,shortName=tpool,categories=turnfleet,scope=Namespaced
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.addressing.mode`
// +kubebuilder:printcolumn:name="Relays",type=integer,JSONPath=`.status.readyRelays`
// +kubebuilder:printcolumn:name="Ports In Use",type=integer,JSONPath=`.status.portsInUse`
// +kubebuilder:printcolumn:name="Ports Total",type=integer,JSONPath=`.status.portsTotal`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TURNPool is the Schema for the turnpools API.
type TURNPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of TURNPool.
	// +required
	Spec TURNPoolSpec `json:"spec,omitzero"`

	// status defines the observed state of TURNPool.
	// +optional
	Status *TURNPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TURNPoolList contains a list of TURNPool.
type TURNPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TURNPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TURNPool{}, &TURNPoolList{})
		return nil
	})
}
