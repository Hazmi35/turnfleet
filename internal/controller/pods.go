// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// relayHash is the value of the pod's config-hash annotation.
//
// It covers everything that decides what the relay process runs and where: the
// rendered coturn configuration (which already contains the listeners, the port
// range, the realm, the secret and the peer policy), the object that
// configuration is read from, the content of the serving certificate the pod
// mounts (coturn reads it when it starts, so a rotation has to roll the relay —
// ADR 4), plus the pod fields the configuration does not carry, so changing the
// image, the resources or the placement rolls the relay just like changing the
// config does.
func relayHash(pool *turnfleetv1alpha1.TURNPool, state *relayState, tlsSecretHash string) (string, error) {
	projection := struct {
		Config    string
		Image     string
		Resources *corev1.ResourceRequirements
		Placement *turnfleetv1alpha1.PlacementSpec
		NodeName  string
		// ConfigSource names the object the pod mounts the configuration from.
		// It is deliberately not omitempty: a relay that still mounts the
		// object an earlier operator wrote has to be replaced even when the
		// bytes it reads are identical, so moving the configuration rolls every
		// fleet once, under drain (ADR 20).
		ConfigSource string
		// omitempty matters: a pool without a TLS listener hashes exactly as it
		// did before this field existed, so upgrading the operator does not roll
		// a fleet that never had a certificate to rotate.
		TLSSecret string `json:"TLSSecret,omitempty"`
	}{
		Config:       state.config,
		Image:        pool.Spec.Dataplane.Image,
		Resources:    pool.Spec.Dataplane.Resources,
		Placement:    pool.Spec.Placement,
		NodeName:     state.relay.NodeName,
		ConfigSource: "Secret/" + RelayConfigSecretName(pool),
		TLSSecret:    tlsSecretHash,
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("hashing the relay projection: %w", err)
	}
	return relay.ConfigHash(string(encoded)), nil
}

// reconcileRelayConfig keeps one Secret holding every relay's rendered coturn
// configuration, keyed by relay name. Each pod projects its own key to the path
// coturn reads, so a pool is one object instead of one per relay.
//
// A Secret and not a ConfigMap because the rendered configuration embeds the
// shared HMAC secret: as a ConfigMap, anything allowed to read ConfigMaps in
// the namespace could mint credentials for the pool's relays (ADR 20).
func (r *TURNPoolReconciler) reconcileRelayConfig(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, states []*relayState) error {
	data := make(map[string][]byte, len(states))
	for _, state := range states {
		if state.planned {
			data[state.relay.Name] = []byte(state.config)
		}
	}

	secret := &corev1.Secret{}
	secret.Name = RelayConfigSecretName(pool)
	secret.Namespace = pool.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		maps.Copy(secret.Labels, Labels(pool))
		secret.Data = data
		return controllerutil.SetControllerReference(pool, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconciling secret %s/%s: %w", pool.Namespace, RelayConfigSecretName(pool), err)
	}
	return r.deleteSupersededConfigMap(ctx, pool)
}

// deleteSupersededConfigMap removes the ConfigMap the releases before ADR 20
// kept the rendered configuration in. It holds the shared secret in the clear,
// so an upgrade has to take it away instead of leaving it next to the Secret; a
// relay pod that still mounts it keeps the file it read at startup and is
// replaced by the same pass, which changes the configuration hash.
//
// A ConfigMap of that name the pool does not own is left alone. This can go
// once no upgrade path starts from a release that wrote it.
func (r *TURNPoolReconciler) deleteSupersededConfigMap(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) error {
	name := RelayConfigSecretName(pool)
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, configMap)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading the superseded ConfigMap %s/%s: %w", pool.Namespace, name, err)
	case !metav1.IsControlledBy(configMap, pool):
		return nil
	}

	if err := r.Delete(ctx, configMap); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the superseded ConfigMap %s/%s: %w", pool.Namespace, name, err)
	}
	logf.FromContext(ctx).Info("deleted the relay configuration ConfigMap an earlier release wrote",
		"pool", pool.Name, "namespace", pool.Namespace, "configmap", name)
	return nil
}

// reconcilePods converges the relay pods: it drains what must go, deletes what
// has finished draining, and creates what should exist.
//
// Drains are serialized. A rollout or a scale-down therefore removes one
// relay's capacity from service at a time: the point of draining is that a
// client's allocation keeps working, and removing several relays at once would
// trade that away for speed.
//
// The returned bool reports whether a drain is in flight, so the caller can
// come back quickly instead of waiting a whole probe interval.
func (r *TURNPoolReconciler) reconcilePods(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, states []*relayState) (bool, error) {
	drainInFlight := false

	for _, state := range states {
		if state.pod == nil || !drainRequired(state) {
			continue
		}
		if state.draining {
			drainInFlight = true
			continue
		}
		if drainInFlight {
			// Another relay is already draining; wait for it.
			continue
		}
		if err := r.startDrain(ctx, pool, state); err != nil {
			return true, err
		}
		drainInFlight = true
	}

	for _, state := range states {
		if state.pod == nil || !drainRequired(state) {
			continue
		}
		if !r.drainComplete(ctx, pool, state) {
			drainInFlight = true
			continue
		}
		if err := r.Delete(ctx, state.pod); err != nil && !apierrors.IsNotFound(err) {
			return drainInFlight, fmt.Errorf("deleting drained relay %s: %w", state.relay.Name, err)
		}
		r.Recorder.Eventf(pool, nil, corev1.EventTypeNormal, "RelayDrained", "Delete",
			"relay %s drained (last known allocations %d) and its pod was deleted", state.relay.Name, state.allocations)
	}

	for _, state := range states {
		if state.pod != nil || !state.planned {
			continue
		}
		if err := r.createPod(ctx, pool, state); err != nil {
			return drainInFlight, err
		}
	}

	return drainInFlight, nil
}

// drainRequired reports whether a relay's pod has to go: it is no longer
// planned, or its configuration hash changed.
func drainRequired(state *relayState) bool {
	if !state.planned {
		return true
	}
	if state.addressChanged {
		// The pod holds another address than the relay was planned with, so it
		// serves an address no client was told about: it goes, and the replace
		// starts from the declared address again (ADR 6).
		return true
	}
	return state.pod.Annotations[ConfigHashAnnotation] != state.hash
}

// startDrain marks a relay as draining. The relay disappears from turnURIs in
// the same pass (the status is written from this state), so it stops being
// handed out before its pod is touched.
func (r *TURNPoolReconciler) startDrain(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, state *relayState) error {
	patch := client.MergeFrom(state.pod.DeepCopy())
	if state.pod.Annotations == nil {
		state.pod.Annotations = map[string]string{}
	}
	if state.pod.Labels == nil {
		state.pod.Labels = map[string]string{}
	}
	now := r.now().UTC()
	state.pod.Annotations[DrainStartedAtAnnotation] = now.Format(time.RFC3339)
	state.pod.Labels[DrainingLabel] = "true"

	if err := r.Patch(ctx, state.pod, patch); err != nil {
		return fmt.Errorf("marking relay %s draining: %w", state.relay.Name, err)
	}
	state.draining = true
	state.drainStarted = now
	r.Recorder.Eventf(pool, nil, corev1.EventTypeNormal, "RelayDraining", "Drain",
		"relay %s is draining: it is out of turnURIs and will be deleted once its allocations are gone", state.relay.Name)
	if state.addressChanged {
		// A relay that came up on another address is not the relay clients were
		// given, so it drains instead of advertising it. The event names both
		// addresses: without it an operator only sees a relay that never
		// settles, and the cause is a declaration (or a CNI) that does not
		// deliver the declared address.
		r.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, "RelayAddressChanged", "Drain",
			"relay %s holds address %s but was planned on %s: it is draining rather than advertising an address its clients were not given",
			state.relay.Name, state.observedAddress, state.relay.Address)
	}
	logf.FromContext(ctx).V(1).Info("started draining a relay",
		"relay", state.relay.Name, "pool", pool.Name, "namespace", pool.Namespace)
	return nil
}

// drainComplete reports whether a draining relay may be deleted: its
// allocations are gone, it is not running at all, or drainTimeout expired.
//
// The timeout is what keeps a dead node from blocking a rollout forever: when
// the node is gone its allocations are gone with it, but nothing can report
// that, so the wait is bounded.
func (r *TURNPoolReconciler) drainComplete(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, state *relayState) bool {
	if state.pod.Status.PodIP == "" || state.pod.Status.Phase != corev1.PodRunning {
		// Nothing can be serving allocations on a pod that is not running.
		state.allocations = 0
		state.portsInUse = 0
		return true
	}

	state.allocations = r.scrapeAllocations(ctx, state)
	state.portsInUse = state.allocations
	if state.allocations == 0 {
		return true
	}

	if state.drainStarted.IsZero() {
		// The drain has not been recorded yet; startDrain does that first.
		return false
	}
	timeout := drainTimeout(pool)
	if r.now().Sub(state.drainStarted) < timeout {
		return false
	}

	r.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, "DrainTimeout", "Drain",
		"relay %s still holds %d allocations after %s; deleting its pod anyway", state.relay.Name, state.allocations, timeout)
	logf.FromContext(ctx).Info("drain timeout expired",
		"relay", state.relay.Name, "pool", pool.Name, "allocations", state.allocations, "timeout", timeout.String())
	return true
}

// createPod creates the pod for a relay.
func (r *TURNPoolReconciler) createPod(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, state *relayState) error {
	pod, err := desiredPod(pool, state)
	if err != nil {
		return fmt.Errorf("building relay pod %s: %w", state.relay.Name, err)
	}
	if err := controllerutil.SetControllerReference(pool, pod, r.Scheme); err != nil {
		return fmt.Errorf("setting the owner of relay pod %s: %w", pod.Name, err)
	}
	if err := r.Create(ctx, pod); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating relay pod %s: %w", pod.Name, err)
	}
	r.Recorder.Eventf(pool, nil, corev1.EventTypeNormal, "RelayCreated", "Create",
		"relay %s created on node %s for %s", state.relay.Name, state.relay.NodeName, state.relay.PortRange)
	return nil
}

// desiredPod builds the pod one relay runs in.
//
// The pod runs with hostNetwork on the node the planner chose, because that is
// what makes peer traffic to (address, port) reach the process that owns the
// port: no Service and no load balancer sits between the peer and the relay, so
// no flow hash or front-end decision picks the process that receives the packet.
func desiredPod(pool *turnfleetv1alpha1.TURNPool, state *relayState) (*corev1.Pod, error) {
	pod := &corev1.Pod{Spec: podSpec(pool, state)}
	pod.Name = state.relay.Name
	pod.Namespace = pool.Namespace
	pod.Labels = podLabels(pool)
	pod.Annotations = map[string]string{
		ConfigHashAnnotation:       state.hash,
		RelayIdentityAnnotation:    state.relay.Identity,
		RelayBindAddressAnnotation: state.relay.BindAddress,
	}
	if attachment, ok := podIPAttachmentRef(pool); ok {
		annotation, err := podIPNetworksAnnotation(pool.Namespace, attachment, state.addressRequest)
		if err != nil {
			return nil, err
		}
		pod.Annotations[NetworksAnnotation] = annotation
	}
	return pod, nil
}

func podLabels(pool *turnfleetv1alpha1.TURNPool) map[string]string {
	podLabels := Labels(pool)
	if pool.Spec.Placement != nil {
		maps.Copy(podLabels, pool.Spec.Placement.NodeSelector)
	}
	return podLabels
}

func podSpec(pool *turnfleetv1alpha1.TURNPool, state *relayState) corev1.PodSpec {
	// A PodIP relay owns the address the pod holds, so the pod runs in its own
	// network namespace: sharing the node's would mean sharing the node's
	// address, which is the mode it exists to avoid. Every other mode binds an
	// address that belongs to the node, so it needs the host's network.
	hostNetwork := pool.Spec.Addressing.Mode != turnfleetv1alpha1.AddressingModePodIP
	dnsPolicy := corev1.DNSClusterFirst
	if hostNetwork {
		dnsPolicy = corev1.DNSClusterFirstWithHostNet
	}

	spec := corev1.PodSpec{
		HostNetwork:                   hostNetwork,
		DNSPolicy:                     dnsPolicy,
		RestartPolicy:                 corev1.RestartPolicyAlways,
		NodeSelector:                  nodeSelector(pool),
		Tolerations:                   tolerations(pool),
		Affinity:                      affinity(pool, state),
		TerminationGracePeriodSeconds: new(int64(30)),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:      new(int64(relayUserID)),
			RunAsGroup:     new(int64(relayUserID)),
			RunAsNonRoot:   new(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{relayContainer(pool, state)},
		Volumes: []corev1.Volume{
			{
				Name: configVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: secretVolumeSource(RelayConfigSecretName(pool), state.relay.Name),
				},
			},
			// The relay runs with a read-only root filesystem, so it gets a
			// scratch directory.
			emptyDirVolume(tmpVolumeName),
		},
	}

	if tlsSecret := tlsSecretName(pool.Spec.Listeners); tlsSecret != "" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: tlsSecret},
			},
		})
	}
	return spec
}

// emptyDirVolume is a scratch volume.
func emptyDirVolume(name string) corev1.Volume {
	return corev1.Volume{
		Name:         name,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// secretVolumeSource projects one relay's key out of the pool's configuration
// Secret, at the path coturn reads.
func secretVolumeSource(secretName, relayName string) *corev1.SecretVolumeSource {
	source := &corev1.SecretVolumeSource{}
	source.SecretName = secretName
	source.Items = []corev1.KeyToPath{{Key: relayName, Path: "turnserver.conf"}}
	return source
}

func relayContainer(pool *turnfleetv1alpha1.TURNPool, state *relayState) corev1.Container {
	container := corev1.Container{
		Name:            relayContainerName,
		Image:           pool.Spec.Dataplane.Image,
		ImagePullPolicy: relayImagePullPolicy,
		// Setting the command bypasses the image's entrypoint shell script,
		// which evaluates its arguments before exec'ing the server.
		Command: []string{turnserverPath},
		Args:    []string{"-c", relay.ConfigPath},
		Ports:   containerPorts(pool, &state.relay),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			// The pinned coturn image carries a file capability on
			// /usr/bin/turnserver (cap_net_bind_service), and the kernel refuses
			// to exec such a binary when the container cannot be granted that
			// capability: with everything dropped, the relay dies with "exec
			// /usr/bin/turnserver: operation not permitted". Adding it back is
			// also the only addition the restricted Pod Security Standard
			// allows.
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"NET_BIND_SERVICE"},
			},
		},
		Resources: resources(pool.Spec.Dataplane.Resources),
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      configVolumeName,
				MountPath: relay.ConfigPath,
				SubPath:   "turnserver.conf",
				ReadOnly:  true,
			},
			{
				Name:      tmpVolumeName,
				MountPath: tmpMountPath,
			},
		},
	}

	if tlsSecretName(pool.Spec.Listeners) != "" {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      tlsVolumeName,
			MountPath: tlsMountPath,
			ReadOnly:  true,
		})
	}

	// A tcp readiness probe only makes sense when the pool serves TCP. A pool
	// with only a UDP listener is checked by the controller's own TURN Allocate
	// probe instead, which is the honest check: an open UDP port proves nothing.
	if port := tcpReadinessPort(pool.Spec.Listeners, state.relay.ListenerPortOffset); port != 0 {
		container.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(port),
				// The listener binds the relay's own address, which is not the
				// pod IP when the pod is hostNetwork on a dual-stack node or the
				// relay advertises a NATed address. Probing the pod IP would
				// report a healthy relay as unready, forever.
				Host: state.relay.BindAddress,
			}},
			InitialDelaySeconds: 2,
			PeriodSeconds:       5,
			TimeoutSeconds:      2,
			FailureThreshold:    3,
		}
	}

	return container
}

func containerPorts(pool *turnfleetv1alpha1.TURNPool, planned *relay.Relay) []corev1.ContainerPort {
	ports := make([]corev1.ContainerPort, 0, len(pool.Spec.Listeners)+1)
	for i := range pool.Spec.Listeners {
		listener := &pool.Spec.Listeners[i]
		ports = append(ports, corev1.ContainerPort{
			Name:          listener.Name,
			ContainerPort: planned.ListenerPort(listener),
			Protocol:      protocolFor(listener.Protocol),
		})
	}
	// Declaring the ports is not cosmetic for a hostNetwork pod: the scheduler
	// treats them as host ports, which is what stops two relays on one node
	// from being scheduled onto the same port. A PodIP relay pod has a network
	// namespace of its own, so there the ports are informational: two relays on
	// one node bind the same listener port in different namespaces without
	// colliding, which is exactly what lets one node host several PodIP relays.
	ports = append(ports, corev1.ContainerPort{
		Name:          "metrics",
		ContainerPort: planned.MetricsPort(),
		Protocol:      corev1.ProtocolTCP,
	})
	return ports
}

func protocolFor(protocol turnfleetv1alpha1.ListenerProtocol) corev1.Protocol {
	switch protocol {
	case turnfleetv1alpha1.ListenerProtocolUDP:
		return corev1.ProtocolUDP
	case turnfleetv1alpha1.ListenerProtocolTCP, turnfleetv1alpha1.ListenerProtocolTLS:
		return corev1.ProtocolTCP
	default:
		return corev1.ProtocolTCP
	}
}

// tcpReadinessPort is the port a TCP readiness probe can use, or 0 when the pool
// serves no TCP transport at all.
func tcpReadinessPort(listeners []turnfleetv1alpha1.ListenerSpec, portOffset int32) int32 {
	var tlsPort int32
	for _, listener := range listeners {
		switch listener.Protocol {
		case turnfleetv1alpha1.ListenerProtocolTCP:
			return listener.Port + portOffset
		case turnfleetv1alpha1.ListenerProtocolTLS:
			tlsPort = listener.Port + portOffset
		case turnfleetv1alpha1.ListenerProtocolUDP:
			// A UDP listener cannot answer a TCP readiness probe.
		default:
		}
	}
	return tlsPort
}

func tlsSecretName(listeners []turnfleetv1alpha1.ListenerSpec) string {
	for _, listener := range listeners {
		if listener.Protocol == turnfleetv1alpha1.ListenerProtocolTLS && listener.TLSSecretRef != nil {
			return listener.TLSSecretRef.Name
		}
	}
	return ""
}

// relayImagePullPolicy is IfNotPresent for every relay image. The validating webhook
// requires a digest, so the reference is immutable and a node that already has
// the image never needs to resolve it again; a cluster that preloads the image
// (kind, or an air-gapped registry mirror) therefore works without a pull.
const relayImagePullPolicy = corev1.PullIfNotPresent

func resources(spec *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if spec == nil {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		}
	}
	return *spec
}

func nodeSelector(pool *turnfleetv1alpha1.TURNPool) map[string]string {
	selector := map[string]string{RelayNodeLabel: labelValueTrue}
	if pool.Spec.Placement != nil {
		maps.Copy(selector, pool.Spec.Placement.NodeSelector)
	}
	return selector
}

func tolerations(pool *turnfleetv1alpha1.TURNPool) []corev1.Toleration {
	if pool.Spec.Placement == nil {
		return nil
	}
	return pool.Spec.Placement.Tolerations
}

// affinity pins a relay to the node the planner chose and, in HostNetwork mode,
// makes anti-affinity required so two relays never end up on one node where one
// would have to give up its port range.
func affinity(pool *turnfleetv1alpha1.TURNPool, state *relayState) *corev1.Affinity {
	affinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key:      "metadata.name",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{state.relay.NodeName},
					}},
				}},
			},
		},
	}

	// Two relays may share a node only when they own different addresses:
	// PartitionedIP splits one address between several relays, a DualStack pool
	// gives each of a node's two addresses its own relay, and PodIP and
	// ExternalIPPool give every relay an address of its own. Required
	// anti-affinity stays exactly when one relay per node is the rule — the
	// HostNetwork mode on a single family — because there the second relay
	// would have to give up the one address the node offers.
	switch {
	case pool.Spec.Addressing.Mode == turnfleetv1alpha1.AddressingModePartitionedIP,
		pool.Spec.Addressing.Mode == turnfleetv1alpha1.AddressingModeExternalIPPool,
		pool.Spec.Addressing.Mode == turnfleetv1alpha1.AddressingModePodIP,
		len(relay.AddressFamilies(pool.Spec.Addressing.Family)) > 1:
		return affinity
	}

	affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: SelectorLabels(pool)},
			TopologyKey:   "kubernetes.io/hostname",
		}},
	}
	return affinity
}
