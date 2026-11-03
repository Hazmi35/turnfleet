// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"fmt"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// Object names, labels and annotations this operator defines. They are part of
// the project's interface: an operator selects relay pods with them, and the
// e2e suite finds relays with them.
const (
	// RelayNodeLabel marks a node that may host relays. Relays bind on the node
	// address, so this label is the pool's placement contract.
	RelayNodeLabel = "turnfleet.hzmi.dev/relay-node"

	// RelayAddressAnnotation overrides the address a relay on a node advertises,
	// for nodes whose ExternalIP is not the address clients should use. It
	// replaces only the advertised address of the family it names. The address
	// the relay binds follows RelayBindAnnotation and the node itself, and is
	// never overridden.
	RelayAddressAnnotation = "turnfleet.hzmi.dev/relay-address"

	// RelayBindAnnotation is a NODE annotation that says which node address a
	// relay binds when the node has both an InternalIP and an ExternalIP in a
	// family and they differ. The value is RelayBindInternalIP or
	// RelayBindExternalIP. When it is absent, the relay pod decides when it
	// starts, by whether the node holds the ExternalIP on an interface (ADR 23).
	// It is not RelayBindAddressAnnotation, which is a POD annotation that
	// records the address a relay binds.
	RelayBindAnnotation = "turnfleet.hzmi.dev/relay-bind"

	// RelayBindInternalIP makes a relay bind the node's InternalIP and advertise
	// its ExternalIP, for a node whose ExternalIP is a 1:1 NAT address the node
	// does not hold. It skips the decision on the node.
	RelayBindInternalIP = "InternalIP"

	// RelayBindExternalIP makes a relay bind and advertise the node's ExternalIP,
	// for a node that holds it on an interface. It skips the decision on the
	// node.
	RelayBindExternalIP = "ExternalIP"

	// The PodIP mode's address declaration is relay.PodAddressesAnnotation: the
	// planner, the admission webhook and this controller all read it, so it
	// lives with the addressing rules.

	// NetworksAnnotation is Multus' network selection annotation: it is how a
	// PodIP relay pod is attached to the pool's secondary network and, when the
	// IPAM honours a request, how it is given the address its relay was planned
	// with.
	NetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

	// NetworkStatusAnnotation is Multus' network status annotation, where the
	// address a pod actually holds on a secondary attachment appears. The
	// controller reads a PodIP relay's address back from here, never from the
	// pod's primary address.
	NetworkStatusAnnotation = "k8s.v1.cni.cncf.io/network-status"

	// ConfigHashAnnotation carries the hash of the relay's rendered coturn
	// configuration and of the Secret it embeds. A change rolls the relay.
	ConfigHashAnnotation = "turnfleet.hzmi.dev/config-hash"

	// RelayIdentityAnnotation records the relay identity a pod serves, so the
	// controller can recognise its own relays after a status loss.
	RelayIdentityAnnotation = "turnfleet.hzmi.dev/relay-identity"

	// RelayBindAddressAnnotation records the local address a relay binds, which
	// differs from its advertised address when the node address is not the
	// address clients use.
	RelayBindAddressAnnotation = "turnfleet.hzmi.dev/relay-bind-address"

	// DrainStartedAtAnnotation records when a relay was first seen draining, so
	// drainTimeout is measured from that moment rather than from every
	// reconcile.
	DrainStartedAtAnnotation = "turnfleet.hzmi.dev/drain-started-at"

	// DrainingLabel marks a relay pod that is on its way out.
	DrainingLabel = "turnfleet.hzmi.dev/draining"

	// FinalizerName guards pool deletion until every relay has drained.
	FinalizerName = "turnfleet.hzmi.dev/drain"

	// ControllerName is the manager name used for labels and events.
	ControllerName = "turnfleet-controller"

	// ManagedByLabelValue is the value of app.kubernetes.io/managed-by.
	ManagedByLabelValue = ControllerName

	// AppName is the value of app.kubernetes.io/name on relay objects.
	AppName = "turnfleet"

	// relayComponent is the value of app.kubernetes.io/component on relay pods.
	relayComponent = "relay"

	// labelValueTrue is the label value that enables a boolean node or pod
	// label, such as the relay-node marker.
	labelValueTrue = "true"

	// configVolumeName, tlsVolumeName and tmpVolumeName are the pod volume names.
	configVolumeName = "turnfleet-config"
	tlsVolumeName    = "turnfleet-tls"
	tmpVolumeName    = "turnfleet-tmp"

	// configFileName is the name of the coturn configuration file in the volume
	// the relay container mounts at relay.ConfigPath.
	configFileName = "turnserver.conf"

	// candidatesVolumeName holds the two configurations of a relay that decides
	// its bind address on its node. The init container reads them, and writes
	// the chosen one to the volume named configVolumeName, which then is an
	// in-memory emptyDir instead of the Secret.
	candidatesVolumeName = "turnfleet-config-candidates"

	// candidatesMountPath and activeMountPath are where the init container sees
	// the candidates and the directory it writes the chosen configuration to.
	candidatesMountPath = "/turnfleet/candidates"
	activeMountPath     = "/turnfleet/active"

	// localConfigFile and mappedConfigFile are the candidate files, and the
	// suffixes of their keys in the configuration Secret.
	localConfigFile  = "local.conf"
	mappedConfigFile = "mapped.conf"

	// relayInitContainerName is the init container that decides the bind
	// address. Its termination message is the address it chose.
	relayInitContainerName = "relay-init"

	// tlsMountPath is where a tls listener's Secret is mounted.
	tlsMountPath = "/etc/turnfleet/tls"

	// tmpMountPath is a writable scratch directory, because the relay container
	// runs with a read-only root filesystem.
	tmpMountPath = "/tmp"

	// relayContainerName is the coturn container's name.
	relayContainerName = "relay"

	// turnserverPath is coturn's binary in the pinned image. Setting it as the
	// container command bypasses the image's entrypoint shell script, which
	// evaluates its arguments.
	turnserverPath = "/usr/bin/turnserver"

	// relayUserID is the uid the pinned coturn image runs as (nobody).
	relayUserID = 65534
)

// Labels returns the labels every relay object of a pool carries.
func Labels(pool *turnfleetv1alpha1.TURNPool) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       AppName,
		"app.kubernetes.io/instance":   pool.Name,
		"app.kubernetes.io/component":  relayComponent,
		"app.kubernetes.io/managed-by": ManagedByLabelValue,
	}
}

// SelectorLabels returns the subset of labels used to select a pool's relays.
// They are also what the scale subresource exposes, so they must be stable.
func SelectorLabels(pool *turnfleetv1alpha1.TURNPool) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      AppName,
		"app.kubernetes.io/instance":  pool.Name,
		"app.kubernetes.io/component": relayComponent,
	}
}

// Selector is the string form of SelectorLabels, for status.selector.
func Selector(pool *turnfleetv1alpha1.TURNPool) string {
	return fmt.Sprintf("app.kubernetes.io/name=%s,app.kubernetes.io/instance=%s,app.kubernetes.io/component=%s",
		AppName, pool.Name, relayComponent)
}

// RelayConfigSecretName is the Secret holding every relay's rendered coturn
// configuration for a pool. It is a Secret and not a ConfigMap because the
// rendered configuration embeds the shared HMAC secret, and reading a Secret
// is a different permission than reading a ConfigMap (ADR 20).
func RelayConfigSecretName(pool *turnfleetv1alpha1.TURNPool) string {
	return fmt.Sprintf("%s-relay-config", pool.Name)
}

// URIsConfigMapName is the ConfigMap the credential issuer reads to hand out
// per-relay URIs.
func URIsConfigMapName(pool *turnfleetv1alpha1.TURNPool) string {
	return fmt.Sprintf("%s-turn-uris", pool.Name)
}
