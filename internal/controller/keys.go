// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"fmt"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
)

// Object names, labels and annotations this operator defines. They are part of
// the project's interface: an operator selects relay pods with them, and the
// e2e suite finds relays with them.
const (
	// RelayNodeLabel marks a node that may host relays. Relays bind on the node
	// address, so this label is the pool's placement contract.
	RelayNodeLabel = "turnfleet.hzmi.dev/relay-node"

	// RelayAddressAnnotation overrides the address a relay on a node advertises
	// (and binds), for nodes whose ExternalIP is not the address clients should
	// use.
	RelayAddressAnnotation = "turnfleet.hzmi.dev/relay-address"

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

	// configVolumeName and tlsVolumeName are the pod volume names.
	configVolumeName = "turnfleet-config"
	tlsVolumeName    = "turnfleet-tls"
	tmpVolumeName    = "turnfleet-tmp"

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

// RelayConfigMapName is the ConfigMap holding every relay's rendered coturn
// configuration for a pool.
func RelayConfigMapName(pool *turnfleetv1alpha1.TURNPool) string {
	return fmt.Sprintf("%s-relay-config", pool.Name)
}

// URIsConfigMapName is the ConfigMap the credential issuer reads to hand out
// per-relay URIs.
func URIsConfigMapName(pool *turnfleetv1alpha1.TURNPool) string {
	return fmt.Sprintf("%s-turn-uris", pool.Name)
}
