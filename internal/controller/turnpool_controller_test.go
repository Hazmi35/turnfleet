// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

func hostNetworkPool(name string, replicas int32) *turnfleetv1alpha1.TURNPool {
	return &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: replicas,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 49152, End: 49199},
			Auth: turnfleetv1alpha1.AuthSpec{
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "turn-secret"},
					Key:                  "secret",
				},
			},
		},
	}
}

func conditionStatus(pool *turnfleetv1alpha1.TURNPool, conditionType string) string {
	if pool.Status == nil {
		return ""
	}
	for _, condition := range pool.Status.Conditions {
		if condition.Type == conditionType {
			return string(condition.Status)
		}
	}
	return ""
}

// TestReconcileHostNetwork covers the mode the e2e suite starts with: one relay
// per node, on the node's own address, each owning the whole port range.
func TestReconcileHostNetwork(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.11")
	f.createNode("tf-worker-2", "203.0.113.12")
	f.createNode("tf-worker-3", "203.0.113.13")

	f.createPool(hostNetworkPool("tpool-host", 2))
	f.reconcile("tpool-host")
	f.reconcile("tpool-host")

	pods := f.pods("tpool-host")
	if len(pods) != 2 {
		t.Fatalf("created %d relay pods, want 2 (one per node, in node order)", len(pods))
	}
	for i := range pods {
		assertRelayPod(t, &pods[i])
	}

	names := []string{pods[0].Name, pods[1].Name}
	slices.Sort(names)
	if !strings.Contains(names[0], "tpool-host-relay-") {
		t.Errorf("relay pod name %q does not follow <pool>-relay-<hash>", names[0])
	}

	// Announce nothing until a relay has answered a real Allocate.
	f.reconcile("tpool-host")
	if pool := f.pool("tpool-host"); pool.Status != nil && len(pool.Status.TurnURIs) > 0 {
		t.Fatalf("turnURIs is populated before any probe succeeded: %v", pool.Status.TurnURIs)
	}

	f.markPodsReady("tpool-host")
	f.reconcileUntil("tpool-host", "two ready relays", func() bool {
		return readyRelays(t, f, "tpool-host") == 2
	})

	pool := f.pool("tpool-host")
	assertHostNetworkStatus(t, pool, f)
}

// assertRelayPod checks the pod shape every mode must produce.
func assertRelayPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()

	if !pod.Spec.HostNetwork {
		t.Errorf("relay pod %s does not use hostNetwork: peer traffic would not reach the process holding the allocation", pod.Name)
	}
	if pod.Spec.NodeName != "" {
		t.Errorf("relay pod %s was pinned with spec.nodeName; it must be placed by affinity so the scheduler still applies", pod.Name)
	}
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatalf("relay pod %s has no node affinity: nothing pins it to its relay node", pod.Name)
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if terms == nil || len(terms.NodeSelectorTerms) == 0 {
		t.Fatalf("relay pod %s has no required node selector", pod.Name)
	}
	fields := terms.NodeSelectorTerms[0].MatchFields
	if len(fields) != 1 || fields[0].Key != "metadata.name" {
		t.Errorf("relay pod %s is not pinned by node name: %+v", pod.Name, fields)
	}
	if affinity.PodAntiAffinity == nil {
		t.Errorf("relay pod %s has no anti-affinity: HostNetwork mode is one relay per node", pod.Name)
	}
	if got := pod.Annotations[ConfigHashAnnotation]; got == "" {
		t.Errorf("relay pod %s has no config hash: a configuration change would never roll it", pod.Name)
	}
	if got := pod.Annotations[RelayIdentityAnnotation]; !strings.HasPrefix(got, "203.0.113.1") {
		t.Errorf("relay pod %s records identity %q, want an address and range", pod.Name, got)
	}
}

// assertHostNetworkStatus checks the pool-level status of a healthy pool.
func assertHostNetworkStatus(t *testing.T, pool *turnfleetv1alpha1.TURNPool, f *testFixture) {
	t.Helper()

	for conditionType, want := range map[string]string{
		ConditionReady:            string(metav1.ConditionTrue),
		ConditionProbeSucceeded:   string(metav1.ConditionTrue),
		ConditionCapacityPressure: string(metav1.ConditionFalse),
	} {
		if got := conditionStatus(pool, conditionType); got != want {
			t.Errorf("%s = %s, want %s (%v)", conditionType, got, want, pool.Status.Conditions)
		}
	}
	if len(pool.Status.TurnURIs) != 2 {
		t.Errorf("turnURIs = %v, want two per-relay URIs", pool.Status.TurnURIs)
	}
	for _, uri := range pool.Status.TurnURIs {
		if !strings.HasPrefix(uri, "turn:203.0.113.1") {
			t.Errorf("URI %q does not point at a relay address", uri)
		}
	}
	if pool.Status.Selector == nil || !strings.Contains(*pool.Status.Selector, "app.kubernetes.io/instance=tpool-host") {
		t.Errorf("selector = %v, want the pool's relay label selector", pool.Status.Selector)
	}
	if pool.Status.PortsTotal == nil || *pool.Status.PortsTotal != 96 {
		t.Errorf("portsTotal = %v, want 96 (two addresses of 48 ports)", pool.Status.PortsTotal)
	}

	uris := &corev1.ConfigMap{}
	if err := k8sClient.Get(f.ctx, namespaced(f.namespace, URIsConfigMapName(pool)), uris); err != nil {
		t.Fatalf("reading the turn URIs ConfigMap: %v", err)
	}
	listed := strings.Split(strings.TrimSpace(uris.Data["uris"]), "\n")
	if len(listed) != len(pool.Status.TurnURIs) {
		t.Errorf("ConfigMap lists %d URIs, status lists %d", len(listed), len(pool.Status.TurnURIs))
	}
	if len(f.prober.calls) == 0 {
		t.Error("no probe was issued: readiness would be a guess")
	}
	for _, call := range f.prober.calls {
		if call.ExpectAddress == "" || call.ExpectPortRange == nil {
			t.Error("a probe ran without asserting the relayed address and range")
		}
		if call.Username == "" || call.Password == "" {
			t.Error("a probe ran without credentials derived from the shared secret")
		}
	}
}

// TestReconcilePartitionedIP covers several relays sharing one address with
// disjoint port slices, and the capacity accounting that goes with it.
func TestReconcilePartitionedIP(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "10.0.0.11")
	f.createNode("tf-worker-2", "10.0.0.12")

	pool := hostNetworkPool("tpool-part", 4)
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
		PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
	}
	f.createPool(pool)

	f.reconcile("tpool-part")
	f.reconcile("tpool-part")
	f.markPodsReady("tpool-part")
	f.reconcileUntil("tpool-part", "four ready relays", func() bool {
		pool := f.pool("tpool-part")
		return pool.Status != nil && pool.Status.ReadyRelays != nil && *pool.Status.ReadyRelays == 4
	})

	pods := f.pods("tpool-part")
	if len(pods) != 4 {
		t.Fatalf("created %d relay pods, want 4", len(pods))
	}
	for _, pod := range pods {
		if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAntiAffinity != nil &&
			len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			t.Errorf("relay pod %s has required anti-affinity, which would forbid a second relay on its node", pod.Name)
		}
	}

	status := f.pool("tpool-part").Status
	rangesPerAddress := map[string][]relay.PortRange{}
	for _, relayStatus := range status.Relays {
		address := ""
		if relayStatus.RelayAddress != nil {
			address = *relayStatus.RelayAddress
		}
		rangesPerAddress[address] = append(rangesPerAddress[address], relay.PortRange{
			Start: relayStatus.PortRange.Start,
			End:   relayStatus.PortRange.End,
		})
	}
	if len(rangesPerAddress) != 2 {
		t.Fatalf("relays are spread over %d addresses, want 2: %v", len(rangesPerAddress), rangesPerAddress)
	}
	for address, ranges := range rangesPerAddress {
		if len(ranges) != 2 {
			t.Errorf("address %s hosts %d relays, want 2", address, len(ranges))
		}
		for i := range ranges {
			for j := i + 1; j < len(ranges); j++ {
				if ranges[i].Overlaps(ranges[j]) {
					t.Errorf("ranges %s and %s overlap on address %s", ranges[i], ranges[j], address)
				}
			}
		}
	}
	// Two addresses of 48 ports, even though four relays run.
	if status.PortsTotal == nil || *status.PortsTotal != 96 {
		t.Errorf("portsTotal = %v, want 96: splitting an address does not create ports", status.PortsTotal)
	}
}

// TestReconcileExternalIPPool covers relays bound to declared secondary
// addresses.
func TestReconcileExternalIPPool(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "10.0.0.11")
	f.createNode("tf-worker-2", "10.0.0.12")

	pool := hostNetworkPool("tpool-ext", 2)
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
		ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{
				{NodeName: "tf-worker-1", Address: "198.51.100.11"},
				{NodeName: "tf-worker-2", Address: "198.51.100.12"},
			},
		},
	}
	f.createPool(pool)

	f.reconcile("tpool-ext")
	f.reconcile("tpool-ext")
	f.markPodsReady("tpool-ext")
	f.reconcileUntil("tpool-ext", "two ready relays", func() bool {
		pool := f.pool("tpool-ext")
		return pool.Status != nil && pool.Status.ReadyRelays != nil && *pool.Status.ReadyRelays == 2
	})

	addresses := map[string]bool{}
	for _, relayStatus := range f.pool("tpool-ext").Status.Relays {
		if relayStatus.RelayAddress != nil {
			addresses[*relayStatus.RelayAddress] = true
		}
	}
	for _, want := range []string{"198.51.100.11", "198.51.100.12"} {
		if !addresses[want] {
			t.Errorf("no relay advertises %s: addresses are %v", want, addresses)
		}
	}
}

// TestDrainOnScaleDown is the drain state machine: a relay that is going away
// disappears from turnURIs while its pod still exists, and its pod is deleted
// only once its allocations are gone.
func TestDrainOnScaleDown(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.11")
	f.createNode("tf-worker-2", "203.0.113.12")

	pool := hostNetworkPool("tpool-scale", 2)
	pool.Spec.DrainTimeout = &metav1.Duration{Duration: 10 * time.Second}
	f.createPool(pool)

	f.reconcile("tpool-scale")
	f.reconcile("tpool-scale")
	f.markPodsReady("tpool-scale")
	f.reconcileUntil("tpool-scale", "two ready relays", func() bool {
		return readyRelays(t, f, "tpool-scale") == 2
	})

	// The removed relay is serving real allocations, so it must not be deleted
	// out from under them.
	f.scraper.allocations = 3

	latest := f.pool("tpool-scale")
	latest.Spec.Replicas = 1
	if err := k8sClient.Update(f.ctx, latest); err != nil {
		t.Fatalf("scaling the pool down: %v", err)
	}

	f.reconcile("tpool-scale")
	f.reconcile("tpool-scale")

	pods := f.pods("tpool-scale")
	if len(pods) != 2 {
		t.Fatalf("%d relay pods exist, want 2: the draining relay keeps its pod while it holds allocations", len(pods))
	}
	draining := 0
	for _, pod := range pods {
		if pod.Annotations[DrainStartedAtAnnotation] != "" {
			draining++
		}
	}
	if draining != 1 {
		t.Fatalf("%d pods are draining, want exactly 1: drains are serialized", draining)
	}

	status := f.pool("tpool-scale").Status
	if *status.ReadyRelays != 1 {
		t.Errorf("readyRelays = %d, want 1", *status.ReadyRelays)
	}
	for _, relayStatus := range status.Relays {
		if relayStatus.Draining != nil && *relayStatus.Draining && len(relayStatus.TurnURIs) > 0 {
			t.Errorf("draining relay %s is still announced in turnURIs: %v", relayStatus.Name, relayStatus.TurnURIs)
		}
	}
	if len(status.TurnURIs) != 1 {
		t.Errorf("turnURIs = %v, want the single remaining relay", status.TurnURIs)
	}

	// Release the allocations: the relay may now go.
	f.scraper.allocations = 0
	f.reconcileUntil("tpool-scale", "the drained pod is deleted", func() bool {
		return len(f.pods("tpool-scale")) == 1
	})
}

// TestDrainTimeoutBoundsTheWait covers the other half of the drain contract: a
// relay that never reaches zero allocations is deleted anyway, because a stuck
// client must not block a rollout forever.
func TestDrainTimeoutBoundsTheWait(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.11")
	f.createNode("tf-worker-2", "203.0.113.12")

	pool := hostNetworkPool("tpool-timeout", 2)
	pool.Spec.DrainTimeout = &metav1.Duration{Duration: 5 * time.Second}
	f.createPool(pool)

	f.reconcile("tpool-timeout")
	f.reconcile("tpool-timeout")
	f.markPodsReady("tpool-timeout")
	f.reconcileUntil("tpool-timeout", "two ready relays", func() bool {
		return readyRelays(t, f, "tpool-timeout") == 2
	})

	f.scraper.allocations = 4
	latest := f.pool("tpool-timeout")
	latest.Spec.Replicas = 1
	if err := k8sClient.Update(f.ctx, latest); err != nil {
		t.Fatalf("scaling the pool down: %v", err)
	}

	f.reconcile("tpool-timeout")
	if len(f.pods("tpool-timeout")) != 2 {
		t.Fatal("the draining relay was deleted before drainTimeout expired")
	}

	f.reconcileUntil("tpool-timeout", "the drain timeout deletes the relay anyway", func() bool {
		return len(f.pods("tpool-timeout")) == 1
	})
}

// TestSecretRotationRollsOneRelayAtATime covers the rollout path: a change to
// the shared secret changes every relay's configuration hash, and the relays
// are replaced one at a time so only one relay's capacity leaves service.
func TestSecretRotationRollsOneRelayAtATime(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.11")
	f.createNode("tf-worker-2", "203.0.113.12")

	f.createPool(hostNetworkPool("tpool-roll", 2))

	f.reconcile("tpool-roll")
	f.reconcile("tpool-roll")
	f.markPodsReady("tpool-roll")
	f.reconcileUntil("tpool-roll", "two ready relays", func() bool {
		return readyRelays(t, f, "tpool-roll") == 2
	})

	before := map[string]string{}
	for _, pod := range f.pods("tpool-roll") {
		before[pod.Name] = pod.Annotations[ConfigHashAnnotation]
	}

	// The relays are busy, so a drain cannot finish instantly: that is what
	// makes serialization observable.
	f.scraper.allocations = 5

	f.rotateSecret("fedcba9876543210")
	f.reconcile("tpool-roll")
	f.reconcile("tpool-roll")

	pods := f.pods("tpool-roll")
	draining, oldHash := 0, 0
	for _, pod := range pods {
		if pod.Annotations[DrainStartedAtAnnotation] != "" {
			draining++
			continue
		}
		if pod.Annotations[ConfigHashAnnotation] == before[pod.Name] {
			oldHash++
		}
	}
	if draining == 0 {
		t.Fatal("no relay is draining after the shared secret changed: the configuration hash did not change")
	}
	if draining > 1 {
		t.Fatalf("%d relays are draining at once: a rotation must remove one relay's capacity at a time", draining)
	}
	if oldHash == 0 {
		t.Fatal("every relay was replaced at once: only one should have been rolled")
	}

	// Let the allocations go: the rotation finishes, one relay at a time.
	f.scraper.allocations = 0
	f.reconcileUntil("tpool-roll", "the rotation finished", func() bool {
		pods := f.pods("tpool-roll")
		if len(pods) != 2 {
			return false
		}
		for _, pod := range pods {
			if pod.Annotations[ConfigHashAnnotation] == before[pod.Name] {
				return false
			}
		}
		return true
	})
}

// TestNodeLossDrainsTheRelay covers the case the planner is built for: when a
// relay's node disappears, its relay is drained rather than left behind.
func TestNodeLossDrainsTheRelay(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.11")
	f.createNode("tf-worker-2", "203.0.113.12")
	f.createNode("tf-worker-3", "203.0.113.13")

	f.createPool(hostNetworkPool("tpool-node-loss", 2))
	f.reconcile("tpool-node-loss")
	f.reconcile("tpool-node-loss")
	f.markPodsReady("tpool-node-loss")
	f.reconcileUntil("tpool-node-loss", "two ready relays", func() bool {
		return readyRelays(t, f, "tpool-node-loss") == 2
	})

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tf-worker-1"}}
	if err := k8sClient.Delete(f.ctx, node); err != nil {
		t.Fatalf("deleting relay node tf-worker-1: %v", err)
	}

	f.scraper.allocations = 0
	f.reconcileUntil("tpool-node-loss", "a relay moved off the lost node", func() bool {
		status := f.pool("tpool-node-loss").Status
		if status == nil || status.ReadyRelays == nil || *status.ReadyRelays != 2 {
			return false
		}
		for _, relayStatus := range status.Relays {
			if relayStatus.Ready != nil && *relayStatus.Ready && relayStatus.NodeName != nil && *relayStatus.NodeName == "tf-worker-1" {
				return false
			}
		}
		return true
	})

	status := f.pool("tpool-node-loss").Status
	for _, relayStatus := range status.Relays {
		if relayStatus.NodeName != nil && *relayStatus.NodeName == "tf-worker-1" && relayStatus.Draining != nil && !*relayStatus.Draining {
			t.Errorf("relay %s is still served from the lost node", relayStatus.Name)
		}
	}
}

// TestMissingSecretIsDegraded covers the honest failure: without the shared
// secret no relay can authenticate a client, so the pool says so instead of
// creating relays that would refuse everyone.
func TestMissingSecretIsDegraded(t *testing.T) {
	f := newFixture(t)
	f.createNode("tf-worker-1", "203.0.113.11")

	f.createPool(hostNetworkPool("tpool-node-secret", 1))
	f.reconcile("tpool-node-secret")
	f.reconcile("tpool-node-secret")

	pool := f.pool("tpool-node-secret")
	if got := conditionStatus(pool, ConditionDegraded); got != string(metav1.ConditionTrue) {
		t.Errorf("Degraded = %q, want True", got)
	}
	if got := conditionStatus(pool, ConditionReady); got != string(metav1.ConditionFalse) {
		t.Errorf("Ready = %q, want False", got)
	}
	if len(f.pods("tpool-node-secret")) != 0 {
		t.Error("relay pods were created although the pool cannot authenticate clients")
	}
}

// TestFinalizerDrainsBeforeDeletion covers pool deletion: the finalizer holds
// the object until every relay is gone, and the URIs are cleared first.
func TestFinalizerDrainsBeforeDeletion(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.11")

	f.createPool(hostNetworkPool("tpool-delete", 1))
	f.reconcile("tpool-delete")
	f.reconcile("tpool-delete")
	f.markPodsReady("tpool-delete")
	f.reconcileUntil("tpool-delete", "one ready relay", func() bool {
		return readyRelays(t, f, "tpool-delete") == 1
	})

	f.scraper.allocations = 2
	if err := k8sClient.Delete(f.ctx, f.pool("tpool-delete")); err != nil {
		t.Fatalf("deleting the pool: %v", err)
	}

	f.reconcile("tpool-delete")
	if len(f.pods("tpool-delete")) != 1 {
		t.Fatal("the relay pod was deleted while the pool still held allocations")
	}

	uris := &corev1.ConfigMap{}
	if err := k8sClient.Get(f.ctx, namespaced(f.namespace, URIsConfigMapName(f.pool("tpool-delete"))), uris); err != nil {
		t.Fatalf("reading the turn URIs ConfigMap: %v", err)
	}
	if uris.Data["uris"] != "" {
		t.Errorf("the URI list still hands out relays for a pool that is being deleted: %q", uris.Data["uris"])
	}

	f.scraper.allocations = 0
	f.reconcileUntil("tpool-delete", "the pool is gone", func() bool {
		pool := &turnfleetv1alpha1.TURNPool{}
		err := k8sClient.Get(f.ctx, namespaced(f.namespace, "tpool-delete"), pool)
		return err != nil
	})
}

// TestPeerPolicyDefaultsWithoutTheWebhook covers the install path config/default
// takes: the controller runs with admission webhooks disabled, so nothing has
// defaulted the spec, and the deny list is the operator's own responsibility.
// A relay with no deny list can be pointed at the metadata service.
func TestPeerPolicyDefaultsWithoutTheWebhook(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.21")

	pool := hostNetworkPool("tpool-no-policy", 1)
	pool.Spec.PeerPolicy = nil
	f.createPool(pool)
	f.reconcile("tpool-no-policy")

	rendered := f.relayConfig("tpool-no-policy")
	// 169.254.0.0/16 holds 169.254.169.254, the metadata address: denying it is
	// what keeps an allocation from becoming an SSRF pivot into the cluster.
	if !strings.Contains(rendered, "denied-peer-ip=169.254.0.0-169.254.255.255") {
		t.Errorf("a pool that declares no peerPolicy does not deny link-local peers:\n%s", rendered)
	}
}

// TestPeerPolicyFromTheSpecIsNotWidened pins the other side of the same rule:
// a pool that declares its own deny list gets that list, not the default
// silently added to it.
func TestPeerPolicyFromTheSpecIsNotWidened(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.22")

	pool := hostNetworkPool("tpool-own-policy", 1)
	pool.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: []string{"203.0.113.0/24"}}
	f.createPool(pool)
	f.reconcile("tpool-own-policy")

	rendered := f.relayConfig("tpool-own-policy")
	if !strings.Contains(rendered, "denied-peer-ip=203.0.113.0-203.0.113.255") {
		t.Errorf("the pool's own deny list is not rendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "169.254.0.0-169.254.255.255") {
		t.Errorf("the operator's default list was added to a pool that declares its own:\n%s", rendered)
	}
}

func readyRelays(t *testing.T, f *testFixture, name string) int32 {
	t.Helper()

	pool := f.pool(name)
	if pool.Status == nil || pool.Status.ReadyRelays == nil {
		return 0
	}
	return *pool.Status.ReadyRelays
}

// namespaced builds the key the client uses to look an object up.
func namespaced(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

// TestDualStackRunsOneRelayPerFamily is the controller half of the DualStack
// decision: a dual-stack node hosts one relay per address family, each owning
// the whole port range, and the two relays get distinct metrics ports because
// coturn's metrics endpoint binds every address on the node.
func TestDualStackRunsOneRelayPerFamily(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createDualStackNode("tf-worker-1", "203.0.113.11", "2001:db8::11")

	pool := hostNetworkPool("tpool-dual", 2)
	pool.Spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyDualStack
	f.createPool(pool)

	f.reconcile("tpool-dual")
	f.reconcile("tpool-dual")

	status := f.pool("tpool-dual").Status
	if status == nil || len(status.Relays) != 2 {
		t.Fatalf("status lists %v relays, want 2 (one per family on one node)", status)
	}
	byFamily := map[turnfleetv1alpha1.AddressingFamily]string{}
	for _, relayStatus := range status.Relays {
		if relayStatus.NodeName == nil || *relayStatus.NodeName != "tf-worker-1" {
			t.Errorf("relay %s runs on %v, want tf-worker-1: its address is on that node", relayStatus.Name, relayStatus.NodeName)
		}
		if relayStatus.RelayAddress == nil {
			t.Fatalf("relay %s has no address in status", relayStatus.Name)
		}
		if got := relayStatus.PortRange; got.Start != 49152 || got.End != 49199 {
			t.Errorf("relay %s owns %d-%d, want the whole pool range: a family is a second address, not a second slice",
				relayStatus.Name, got.Start, got.End)
		}
		family := relay.AddressFamilyOf(*relayStatus.RelayAddress)
		if previous, duplicate := byFamily[family]; duplicate {
			t.Errorf("relays %s and %s both use %s addresses", previous, relayStatus.Name, family)
		}
		byFamily[family] = relayStatus.Name
	}
	if len(byFamily) != 2 {
		t.Fatalf("relays cover %v, want one address per family", byFamily)
	}

	// The metrics port is not part of the status, so read it from the pods: two
	// relays on one node must not both serve metrics on 9641.
	metricsPorts := map[int32]string{}
	for _, pod := range f.pods("tpool-dual") {
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.Name != "metrics" {
					continue
				}
				if other, clash := metricsPorts[port.ContainerPort]; clash {
					t.Errorf("pods %s and %s both declare metrics port %d", other, pod.Name, port.ContainerPort)
				}
				metricsPorts[port.ContainerPort] = pod.Name
			}
			// The listener binds the relay's own address, not the pod IP, which
			// a dual-stack hostNetwork pod only has one of.
			if probe := container.ReadinessProbe; probe != nil && probe.TCPSocket != nil {
				if probe.TCPSocket.Host == "" {
					t.Errorf("pod %s probes the pod IP, which is not the address the relay binds", pod.Name)
				}
			}
		}
	}
	if len(metricsPorts) != 2 {
		t.Errorf("relay pods declare metrics ports %v, want one distinct port per relay on the node", metricsPorts)
	}
}

// TestNodeAddresses covers the observation the family decision rests on: one
// address per family, ExternalIP preferred, and the relay-address annotation
// overriding the advertised address of the family it names.
func TestNodeAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		node      *corev1.Node
		want      []relay.NodeAddress
		wantError bool
	}{
		{
			name: "external address only",
			node: &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.11"},
			}}},
			want: []relay.NodeAddress{{Address: "203.0.113.11", BindAddress: "203.0.113.11"}},
		},
		{
			name: "internal address when there is no external one",
			node: &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "203.0.113.11"},
			}}},
			want: []relay.NodeAddress{{Address: "203.0.113.11", BindAddress: "203.0.113.11"}},
		},
		{
			name: "both families, external preferred in each",
			node: &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.11"},
				{Type: corev1.NodeInternalIP, Address: "fd00::11"},
				{Type: corev1.NodeExternalIP, Address: "2001:db8::11"},
				{Type: corev1.NodeHostName, Address: "tf-worker-1"},
			}}},
			want: []relay.NodeAddress{
				{Address: "203.0.113.11", BindAddress: "203.0.113.11"},
				{Address: "2001:db8::11", BindAddress: "2001:db8::11"},
			},
		},
		{
			name: "a name is not an address",
			node: &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "tf-worker-1"},
				{Type: corev1.NodeExternalDNS, Address: "tf-worker-1.example.net"},
			}}},
		},
		{
			name: "the annotation overrides the family it names",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{RelayAddressAnnotation: "203.0.113.77"}},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeExternalIP, Address: "203.0.113.11"},
					{Type: corev1.NodeExternalIP, Address: "2001:db8::11"},
				}},
			},
			want: []relay.NodeAddress{
				{Address: "203.0.113.77", BindAddress: "203.0.113.11"},
				{Address: "2001:db8::11", BindAddress: "2001:db8::11"},
			},
		},
		{
			name: "an annotation for a family the node does not have",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{RelayAddressAnnotation: "2001:db8::77"}},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeExternalIP, Address: "203.0.113.11"},
				}},
			},
			wantError: true,
		},
		{
			name: "an annotation that is not an address",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{RelayAddressAnnotation: "relay.example.net"}},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeExternalIP, Address: "203.0.113.11"},
				}},
			},
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := nodeAddresses(tc.node)
			if tc.wantError {
				if err == nil {
					t.Fatalf("nodeAddresses() accepted %+v and returned %+v", tc.node.Status.Addresses, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("nodeAddresses() failed: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("nodeAddresses() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
