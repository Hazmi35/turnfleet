# API Reference

## Packages
- [turnfleet.hzmi.dev/v1alpha1](#turnfleethzmidevv1alpha1)


## turnfleet.hzmi.dev/v1alpha1

Package v1alpha1 contains API Schema definitions for the turnfleet v1alpha1 API group.

### Resource Types
- [TURNPool](#turnpool)
- [TURNPoolList](#turnpoollist)



#### AddressingMode

_Underlying type:_ _string_

AddressingMode selects how relay addresses are assigned to nodes.

The choice decides capacity, not just placement: a relay's capacity is the
number of ports in its own range, so only modes that add relay addresses add
capacity. See docs/capacity-model.md.

_Validation:_
- Enum: [HostNetwork ExternalIPPool PartitionedIP]

_Appears in:_
- [AddressingSpec](#addressingspec)

| Field | Description |
| --- | --- |
| `HostNetwork` | AddressingModeHostNetwork runs exactly one relay per relay node, binding<br />the node's ExternalIP (or the turnfleet.hzmi.dev/relay-address override).<br />Capacity grows with the number of labelled nodes.<br /> |
| `ExternalIPPool` | AddressingModeExternalIPPool binds relays to specific secondary addresses<br />declared in addressing.externalIPPool.addresses, one relay per address.<br />Capacity grows with the number of declared addresses.<br /> |
| `PartitionedIP` | AddressingModePartitionedIP runs several relays per node on the node's<br />single address, each owning a disjoint slice of the relay port range. It<br />adds concurrency and isolation but NOT capacity: the sum of the slices is<br />still one IP's port space.<br /> |


#### AddressingSpec



AddressingSpec selects how relays are placed and addressed.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[AddressingMode](#addressingmode)_ | mode is the addressing mode. It is immutable: changing it changes every<br />relay's advertised address and invalidates live allocations. |  | Enum: [HostNetwork ExternalIPPool PartitionedIP] <br />Required: \{\} <br /> |
| `externalIPPool` _[ExternalIPPoolSpec](#externalippoolspec)_ | externalIPPool configures AddressingModeExternalIPPool and must be set<br />(with at least one address) when mode is ExternalIPPool. Forbidden for the<br />other modes. |  | Optional: \{\} <br /> |
| `partitionedIP` _[PartitionedIPSpec](#partitionedipspec)_ | partitionedIP configures AddressingModePartitionedIP and must be set when<br />mode is PartitionedIP. Forbidden for the other modes. |  | Optional: \{\} <br /> |


#### AuthSpec



AuthSpec is the shared authentication material for the pool.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `realm` _string_ | realm is the TURN realm relays advertise and authenticate against. It is<br />immutable: changing it invalidates every credential the issuer signed for<br />this pool. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `secretRef` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#secretkeyselector-v1-core)_ | secretRef selects the shared HMAC secret used for TURN REST API<br />credentials (and therefore by the credential issuer, which is not part of<br />this operator). Rotating the selected value rolls every relay one at a<br />time. |  | Required: \{\} <br /> |


#### DataplaneSpec



DataplaneSpec configures the relay processes themselves.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _string_ | image is the coturn image every relay runs, pinned by digest<br />(`repo/name:tag@sha256:...`). A tag without a digest is rejected so a<br />pool cannot silently float to a new relay version. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#resourcerequirements-v1-core)_ | resources is the resource request and limit for each relay container.<br />Relay capacity is bounded by ports, not CPU, so this is about keeping the<br />process responsive under allocation churn. |  | Optional: \{\} <br /> |
| `extraArgs` _string array_ | extraArgs are appended verbatim to the rendered coturn configuration.<br />They are hashed into the relay's config-hash like any other change, but<br />they are not validated against the pinned image: use them for flags the<br />API does not model. |  | Optional: \{\} <br /> |


#### ExternalAddress



ExternalAddress is one secondary address already attached to a node.



_Appears in:_
- [ExternalIPPoolSpec](#externalippoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeName` _string_ | nodeName is the node the address is attached to. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `address` _string_ | address is the address itself. It is both the address a relay binds and<br />the address it advertises in XOR-RELAYED-ADDRESS. |  | MinLength: 1 <br />Required: \{\} <br /> |


#### ExternalIPPoolSpec



ExternalIPPoolSpec configures AddressingModeExternalIPPool.



_Appears in:_
- [AddressingSpec](#addressingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `addresses` _[ExternalAddress](#externaladdress) array_ | addresses are the secondary addresses available to the pool, one relay per<br />address. Each entry must be attached to the named node; the operator does<br />not attach addresses. The webhook requires at least one address when the<br />pool's mode is ExternalIPPool. |  | Optional: \{\} <br /> |


#### ListenerProtocol

_Underlying type:_ _string_

ListenerProtocol is the transport a relay listener accepts.

coturn serves UDP and TCP on the same "plain" listening port and TLS/DTLS on
its own port, so a pool may declare at most one listener of each protocol and
its UDP and TCP listeners must share a port.

_Validation:_
- Enum: [udp tcp tls]

_Appears in:_
- [ListenerSpec](#listenerspec)

| Field | Description |
| --- | --- |
| `udp` | ListenerProtocolUDP accepts plain UDP on the pool's plain listening port.<br /> |
| `tcp` | ListenerProtocolTCP accepts plain TCP on the pool's plain listening port.<br /> |
| `tls` | ListenerProtocolTLS accepts TLS (and DTLS) on the listener's own port.<br /> |


#### ListenerSpec



ListenerSpec is one TURN listener every relay serves.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name identifies the listener within the pool, for example "turn" or<br />"turns". It must be unique within spec.listeners. |  | MaxLength: 63 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `protocol` _[ListenerProtocol](#listenerprotocol)_ | protocol is the transport the listener accepts. |  | Enum: [udp tcp tls] <br />Required: \{\} <br /> |
| `port` _integer_ | port is the base listener port. Relays that own their address serve<br />exactly this port; in PartitionedIP mode, where several relays share one<br />address, each relay serves port + its slot index, because a listening<br />socket can only be bound once per address. The ports a pool uses must not<br />overlap spec.relayPortRange: a listener and an allocation cannot share a<br />port. |  | Maximum: 65535 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `tlsSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#localobjectreference-v1-core)_ | tlsSecretRef is the Secret holding the listener's certificate, with the<br />usual tls.crt and tls.key keys. Required for protocol tls, forbidden<br />otherwise. Rotating it changes every relay's config hash and rolls the<br />pool one relay at a time. |  | Optional: \{\} <br /> |


#### PartitionedIPSpec



PartitionedIPSpec configures AddressingModePartitionedIP.



_Appears in:_
- [AddressingSpec](#addressingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `relaysPerNode` _integer_ | relaysPerNode is how many relays share one node address. The pool's relay<br />port range is split into that many disjoint slices; their sum is still one<br />IP's port space, so this setting adds concurrency, not capacity. | 2 | Maximum: 64 <br />Minimum: 1 <br />Optional: \{\} <br /> |


#### PeerPolicySpec



PeerPolicySpec restricts which peers a relay may send to.

A relay must never be able to reach in-cluster or metadata addresses unless a
pool explicitly allows it: an allocation that peers can point anywhere is an
SSRF pivot into the cluster.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `deniedCIDRs` _string array_ | deniedCIDRs are rendered as coturn --denied-peer-ip entries. When empty<br />the webhook defaults them to RFC 1918, RFC 6598, loopback and link-local<br />(including 169.254.169.254), which also covers the cluster pod and service<br />CIDRs in the common case. The controller always appends the pod CIDRs it<br />observes on the relay nodes. |  | Optional: \{\} <br /> |
| `allowedCIDRs` _string array_ | allowedCIDRs are rendered as coturn --allowed-peer-ip entries and take<br />precedence over deniedCIDRs for the addresses they name. |  | Optional: \{\} <br /> |


#### PlacementSpec



PlacementSpec controls where relay pods may run.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeSelector` _object (keys:string, values:string)_ | nodeSelector is merged with the mandatory relay-node label selector, so it<br />can narrow the relay nodes used by this pool but never widen them. |  | Optional: \{\} <br /> |
| `tolerations` _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#toleration-v1-core) array_ | tolerations are added to the relay pods' tolerations, for clusters whose<br />relay nodes carry taints. |  | Optional: \{\} <br /> |


#### PortRange



PortRange is an inclusive range of UDP ports a relay may allocate.

A relay's capacity in concurrent allocations equals the number of ports in
its range. Ranges of relays that share one address must not overlap; the
planner partitions them and the webhook rejects a pool that cannot be
partitioned.



_Appears in:_
- [RelayStatus](#relaystatus)
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `start` _integer_ | start is the first port of the range, inclusive. |  | Maximum: 65535 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `end` _integer_ | end is the last port of the range, inclusive. It must be greater than or<br />equal to start. |  | Maximum: 65535 <br />Minimum: 1 <br />Required: \{\} <br /> |


#### ProbeSpec



ProbeSpec configures the periodic TURN Allocate probe.



_Appears in:_
- [TURNPoolSpec](#turnpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | interval is how often each ready relay is probed. The probe performs a<br />real allocation and releases it again, so it holds a port only for the<br />duration of the check. |  | Optional: \{\} <br /> |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | timeout bounds a single probe, including the allocation and the optional<br />round trip. |  | Optional: \{\} <br /> |


#### ProbeStatus



ProbeStatus is the outcome of the most recent probe of a relay.



_Appears in:_
- [RelayStatus](#relaystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `time` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | time is when the probe finished. |  | Optional: \{\} <br /> |
| `succeeded` _boolean_ | succeeded reports whether the relay answered the probe with the expected<br />relayed address and range. |  | Optional: \{\} <br /> |
| `latencyMillis` _integer_ | latencyMillis is the probe duration in milliseconds. Milliseconds rather<br />than seconds so short probes stay readable, and an integer because the API<br />has no floats. |  | Optional: \{\} <br /> |
| `message` _string_ | message explains a failed probe, including the TURN error code when the<br />relay rejected the allocation. |  | Optional: \{\} <br /> |


#### RelayStatus



RelayStatus is the observed state of one relay.



_Appears in:_
- [TURNPoolStatus](#turnpoolstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name is the relay identity: stable across restarts and controller<br />failover, derived from the relay's address and port range. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `podName` _string_ | podName is the pod running this relay. |  | Optional: \{\} <br /> |
| `nodeName` _string_ | nodeName is the node the relay runs on. |  | Optional: \{\} <br /> |
| `relayAddress` _string_ | relayAddress is the address the relay advertises in XOR-RELAYED-ADDRESS. |  | Optional: \{\} <br /> |
| `portRange` _[PortRange](#portrange)_ | portRange is the slice of the pool's port space this relay owns. Every<br />relay owns one, so it is always present. |  | Required: \{\} <br /> |
| `turnURIs` _string array_ | turnURIs are the per-relay listener URIs clients can allocate on. A relay<br />that is not ready or is draining has none. |  | Optional: \{\} <br /> |
| `ready` _boolean_ | ready reports whether the relay is serving: pod ready and last probe<br />successful. |  | Optional: \{\} <br /> |
| `draining` _boolean_ | draining reports whether the relay is on its way out. A draining relay is<br />removed from turnURIs before its pod is deleted. |  | Optional: \{\} <br /> |
| `allocations` _integer_ | allocations is the number of live allocations on the relay, read from<br />coturn's metrics endpoint. |  | Optional: \{\} <br /> |
| `portsInUse` _integer_ | portsInUse is the number of relay ports currently held by allocations. One<br />allocation occupies one port, so this equals allocations; it is reported<br />separately because consumers reason about ports. |  | Optional: \{\} <br /> |
| `portsTotal` _integer_ | portsTotal is the number of ports this relay owns. Capacity is portsTotal;<br />the ratio consumers compute is portsInUse/portsTotal. |  | Optional: \{\} <br /> |
| `lastProbe` _[ProbeStatus](#probestatus)_ | lastProbe is the most recent probe result, successful or not. |  | Optional: \{\} <br /> |


#### TURNPool



TURNPool is the Schema for the turnpools API.



_Appears in:_
- [TURNPoolList](#turnpoollist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `turnfleet.hzmi.dev/v1alpha1` | | |
| `kind` _string_ | `TURNPool` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[TURNPoolSpec](#turnpoolspec)_ | spec defines the desired state of TURNPool. |  | Required: \{\} <br /> |
| `status` _[TURNPoolStatus](#turnpoolstatus)_ | status defines the observed state of TURNPool. |  | Optional: \{\} <br /> |


#### TURNPoolList



TURNPoolList contains a list of TURNPool.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `turnfleet.hzmi.dev/v1alpha1` | | |
| `kind` _string_ | `TURNPoolList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[TURNPool](#turnpool) array_ |  |  |  |


#### TURNPoolSpec



TURNPoolSpec defines the desired state of TURNPool.



_Appears in:_
- [TURNPool](#turnpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | replicas is the desired number of relays. It is explicit rather than<br />optional because the number of relays is the capacity decision: the value<br />is capped by the addresses the addressing mode makes available, and the<br />Ready condition says when it cannot be reached. The upper bound reflects<br />where the rendered relay configurations stop fitting in one ConfigMap;<br />pools that need more relays than that need more than one pool. |  | Maximum: 256 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `dataplane` _[DataplaneSpec](#dataplanespec)_ | dataplane configures the relay processes. |  | Required: \{\} <br /> |
| `addressing` _[AddressingSpec](#addressingspec)_ | addressing selects how relays are placed and addressed. Immutable. |  | Required: \{\} <br /> |
| `relayPortRange` _[PortRange](#portrange)_ | relayPortRange is the pool's UDP port space. Each relay owns a disjoint<br />slice of it; for HostNetwork and ExternalIPPool that slice is the whole<br />range. Immutable: changing it invalidates live allocations. |  | Required: \{\} <br /> |
| `listeners` _[ListenerSpec](#listenerspec) array_ | listeners are the TURN listeners every relay serves. |  | MaxItems: 3 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `auth` _[AuthSpec](#authspec)_ | auth is the shared authentication material. realm is immutable. |  | Required: \{\} <br /> |
| `peerPolicy` _[PeerPolicySpec](#peerpolicyspec)_ | peerPolicy restricts which peers relays may send to. |  | Optional: \{\} <br /> |
| `drainTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | drainTimeout is how long a draining relay may keep its allocations before<br />it is deleted anyway. It bounds rollouts and scale-downs; it does not<br />extend a relay's life when its node is already gone. |  | Optional: \{\} <br /> |
| `probe` _[ProbeSpec](#probespec)_ | probe configures the per-relay TURN Allocate probe. |  | Optional: \{\} <br /> |
| `placement` _[PlacementSpec](#placementspec)_ | placement controls where relay pods may run. |  | Optional: \{\} <br /> |


#### TURNPoolStatus



TURNPoolStatus defines the observed state of TURNPool.



_Appears in:_
- [TURNPool](#turnpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions represent the current state of the TURNPool resource.<br />The condition types are:<br />  - Ready: the desired number of relays exist, are ready and are not<br />    draining.<br />  - ProbeSucceeded: every ready relay last probed successfully.<br />  - CapacityPressure: a relay is near or at its port limit, or a probe<br />    observed TURN error 508.<br />  - AddressConflict: this pool holds a relay identity another pool also<br />    holds, on the same address and with overlapping ports. Two relays<br />    cannot own the same ports on one address, so the affected pools serve<br />    less than they report. Disjoint ranges on a shared address are legal.<br />  - Degraded: fewer relays than desired, a relay is failing to converge,<br />    or a relay identity overlaps another pool's.<br />Ready and ProbeSucceeded are normal-true; CapacityPressure,<br />AddressConflict and Degraded are abnormal-true. |  | Optional: \{\} <br /> |
| `relays` _[RelayStatus](#relaystatus) array_ | relays is the per-relay observed state, keyed by relay name. |  | Optional: \{\} <br /> |
| `portsInUse` _integer_ | portsInUse is the total number of relay ports held by allocations across<br />the pool. |  | Optional: \{\} <br /> |
| `portsTotal` _integer_ | portsTotal is the total number of relay ports the pool owns. Capacity is<br />bounded by distinct relay addresses times ports per address, not by the<br />number of relays. |  | Optional: \{\} <br /> |
| `readyRelays` _integer_ | readyRelays is the number of relays that are ready and not draining. It is<br />the status field the scale subresource reports. |  | Optional: \{\} <br /> |
| `turnURIs` _string array_ | turnURIs lists the listener URIs of ready, non-draining relays. It is<br />mirrored into the <pool>-turn-uris ConfigMap for the credential issuer. |  | Optional: \{\} <br /> |
| `selector` _string_ | selector is the label selector matching the pool's relay pods, in string<br />form, exposed through the scale subresource for HPA and KEDA. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | observedGeneration is the generation of the spec this status was computed<br />from. |  | Optional: \{\} <br /> |


