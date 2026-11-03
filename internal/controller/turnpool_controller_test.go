// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
	"github.com/Hazmi35/turnfleet/internal/relayinit"
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

// hostNetworkPoolOn is hostNetworkPool with one relay and a relay port range of
// its own. The identity check compares pools across the whole cluster and
// envtest never finishes terminating a namespace, so a pool an earlier test left
// behind still holds an address and a range: without a range of its own, a pool
// here would conflict with a pool that has nothing to do with it.
func hostNetworkPoolOn(name string, portStart int32) *turnfleetv1alpha1.TURNPool {
	pool := hostNetworkPool(name, 1)
	pool.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: portStart, End: portStart + 20}
	return pool
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

// TestReconcilePodIP covers the mode end to end in the reconcile loop: one
// relay per address a node declares, each in a pod with its own network
// namespace, attached to the pool's attachment.
func TestReconcilePodIP(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	// One node declares two addresses, because capacity in this mode is
	// addresses and not nodes: both relays must be able to live on that node.
	f.createPodIPNode("tf-podip-worker-1", "203.0.113.21/24", "203.0.113.22/24")

	pool := hostNetworkPool("tpool-podip", 2)
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode:  turnfleetv1alpha1.AddressingModePodIP,
		PodIP: turnfleetv1alpha1.PodIPSpec{AttachmentRef: "relay-pods"},
	}
	f.createPool(pool)

	f.reconcile("tpool-podip")
	f.reconcile("tpool-podip")
	f.markPodsReady("tpool-podip")
	f.reconcileUntil("tpool-podip", "two ready relays", func() bool {
		pool := f.pool("tpool-podip")
		return pool.Status != nil && pool.Status.ReadyRelays != nil && *pool.Status.ReadyRelays == 2
	})

	addresses := map[string]bool{}
	for _, relayStatus := range f.pool("tpool-podip").Status.Relays {
		if relayStatus.RelayAddress != nil {
			addresses[*relayStatus.RelayAddress] = true
		}
	}
	for _, want := range []string{"203.0.113.21", "203.0.113.22"} {
		if !addresses[want] {
			t.Errorf("no relay advertises the declared address %s: addresses are %v", want, addresses)
		}
	}

	pods := f.pods("tpool-podip")
	if len(pods) != 2 {
		t.Fatalf("created %d relay pods, want 2", len(pods))
	}
	for _, pod := range pods {
		if pod.Spec.HostNetwork {
			t.Errorf("relay pod %s shares the node's network namespace, so it cannot hold the address it advertises", pod.Name)
		}
		if pod.Annotations[NetworksAnnotation] == "" {
			t.Errorf("relay pod %s is not attached to the pool's attachment", pod.Name)
		}
		if affinity := pod.Spec.Affinity; affinity != nil && affinity.PodAntiAffinity != nil &&
			len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			t.Errorf("relay pod %s has required anti-affinity: the second relay on its node could never schedule", pod.Name)
		}
	}

	if got := f.pool("tpool-podip").Status.PortsTotal; got == nil || *got != 96 {
		t.Errorf("portsTotal = %v, want 96 (two addresses of 48 ports)", got)
	}
}

// TestReconcilePodIPDrainsARelayOnAnotherAddress is the stability contract
// across the read-back: a pod that came up on another address than the one its
// relay was planned with serves an address no client was given, so it is
// drained rather than announced.
func TestReconcilePodIPDrainsARelayOnAnotherAddress(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createPodIPNode("tf-podip-drift-1", "203.0.113.31/24")

	// No attachment: the relay binds the pod's own address, which is exactly
	// the case where a CNI with unstable IPAM shows up.
	pool := hostNetworkPool("tpool-podip-drift", 1)
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModePodIP}
	f.createPool(pool)

	f.reconcile("tpool-podip-drift")
	// envtest runs no kubelet, so the pod is given an address the pool did not
	// declare: the CNI handed out something else.
	f.markPodsReady("tpool-podip-drift")
	f.reconcile("tpool-podip-drift")

	status := f.pool("tpool-podip-drift").Status
	if status.ReadyRelays == nil || *status.ReadyRelays != 0 {
		t.Errorf("readyRelays = %v, want 0: a relay on another address must not be announced", status.ReadyRelays)
	}
	if len(status.TurnURIs) != 0 {
		t.Errorf("turnURIs = %v, want none for a relay that holds another address", status.TurnURIs)
	}
	if len(status.Relays) != 1 || status.Relays[0].Draining == nil || !*status.Relays[0].Draining {
		t.Errorf("relays = %+v, want the one relay draining on an address mismatch", status.Relays)
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

// TestRolloutOnIdleRelaysWithdrawsEachRelayBeforeItsPodGoes is the config-hash
// rollout on an idle pool, where every drain completes as soon as it is
// recorded. Each relay must leave turnURIs in the pass that records its drain,
// and only a later pass may delete its pod, one relay at a time. Reading "the
// drain is complete" as "the pod may go" instead deleted a relay whose URI was
// still published, and deleted several in the same pass.
func TestRolloutOnIdleRelaysWithdrawsEachRelayBeforeItsPodGoes(t *testing.T) {
	const name = "tpool-idle"

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-idle-1", "203.0.113.41")
	f.createNode("tf-idle-2", "203.0.113.42")
	f.createNode("tf-idle-3", "203.0.113.43")

	f.createPool(hostNetworkPool(name, 3))
	f.reconcile(name)
	f.reconcile(name)
	f.markPodsReady(name)
	f.reconcileUntil(name, "three ready relays", func() bool {
		return readyRelays(t, f, name) == 3
	})

	// The relays hold no allocation, so nothing slows a drain down: only the
	// order of the passes decides what a credential issuer is handed.
	f.scraper.allocations = 0
	urisOf := publishedURIsByRelay(t, f, name)
	before := podsByName(f.pods(name))

	f.rotateSecret("0011223344556677")

	for pass := 0; ; pass++ {
		if pass >= reconcileAttempts {
			t.Fatalf("the rollout did not finish in %d passes", reconcileAttempts)
		}

		// What the pass reads: the relay pods that exist, and the relays the
		// status published as out of the URI list.
		present := podsByName(f.pods(name))
		withdrawn := withdrawnRelays(f.pool(name))

		f.reconcile(name)
		f.markPodsReady(name)
		f.clock.Advance(time.Second)

		pods := f.pods(name)
		inService := podsByName(pods)

		gone := missingNames(present, inService)
		if len(gone) > 1 {
			t.Fatalf("pass %d deleted %d relay pods at once (%v): a rollout takes one relay's capacity out of service at a time",
				pass, len(gone), gone)
		}
		for _, podName := range gone {
			if !withdrawn[podName] {
				t.Errorf("pass %d deleted the pod of relay %s in the same pass the last status published it as serving: a consumer is handed a URI for a relay whose pod is already gone", pass, podName)
			}
		}

		status := f.pool(name)
		if draining := drainingRelays(status.Status); len(draining) > 1 {
			t.Errorf("pass %d publishes %d draining relays (%v): a pool drains one relay at a time", pass, len(draining), draining)
		}

		// No URI a consumer can read may point at a relay whose pod is gone.
		published := f.publishedURIs(name)
		for _, relayStatus := range status.Status.Relays {
			if _, serving := inService[relayStatus.Name]; serving {
				continue
			}
			for _, uri := range urisOf[relayStatus.Name] {
				if slices.Contains(status.Status.TurnURIs, uri) {
					t.Errorf("pass %d: relay %s has no pod, but status.turnURIs still publishes %s", pass, relayStatus.Name, uri)
				}
				if slices.Contains(published, uri) {
					t.Errorf("pass %d: relay %s has no pod, but %s still publishes %s",
						pass, relayStatus.Name, URIsConfigMapName(status), uri)
				}
			}
		}

		if rolledOut(inService, before) {
			return
		}
	}
}

// publishedURIsByRelay records the URIs each relay of a healthy pool publishes,
// so a spec can tell which relay a URI belongs to after that relay's pod is
// gone.
func publishedURIsByRelay(t *testing.T, f *testFixture, name string) map[string][]string {
	t.Helper()

	pool := f.pool(name)
	uris := map[string][]string{}
	if pool.Status == nil {
		t.Fatalf("pool %s has no status: nothing is published", name)
	}
	for _, relayStatus := range pool.Status.Relays {
		if len(relayStatus.TurnURIs) > 0 {
			uris[relayStatus.Name] = relayStatus.TurnURIs
		}
	}
	if len(uris) == 0 {
		t.Fatalf("no relay of pool %s publishes a URI, so no URI can be attributed to one", name)
	}
	return uris
}

// podsByName indexes relay pods by name, leaving out the ones that are being
// deleted: a pod with a deletion timestamp serves nothing a consumer can reach.
func podsByName(pods []corev1.Pod) map[string]*corev1.Pod {
	byName := make(map[string]*corev1.Pod, len(pods))
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			continue
		}
		byName[pods[i].Name] = &pods[i]
	}
	return byName
}

// missingNames returns the names of the first set the second set does not hold,
// sorted.
func missingNames[T any](before, after map[string]T) []string {
	var missing []string
	for name := range before {
		if _, there := after[name]; !there {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// withdrawnRelays names the relays the pool's status publishes as draining with
// no URI: the ones a credential issuer can no longer hand out.
func withdrawnRelays(pool *turnfleetv1alpha1.TURNPool) map[string]bool {
	withdrawn := map[string]bool{}
	if pool.Status == nil {
		return withdrawn
	}
	for _, relayStatus := range pool.Status.Relays {
		if relayStatus.Draining != nil && *relayStatus.Draining && len(relayStatus.TurnURIs) == 0 {
			withdrawn[relayStatus.Name] = true
		}
	}
	return withdrawn
}

// drainingRelays names the relays a status publishes as draining.
func drainingRelays(status *turnfleetv1alpha1.TURNPoolStatus) []string {
	if status == nil {
		return nil
	}
	var draining []string
	for _, relayStatus := range status.Relays {
		if relayStatus.Draining != nil && *relayStatus.Draining {
			draining = append(draining, relayStatus.Name)
		}
	}
	return draining
}

// rolledOut reports whether every relay pod of the pool runs a configuration
// other than the one it ran before the rollout.
func rolledOut(byName, before map[string]*corev1.Pod) bool {
	if len(byName) != len(before) {
		return false
	}
	for name, pod := range byName {
		previous, known := before[name]
		if !known {
			return false
		}
		if pod.Annotations[ConfigHashAnnotation] == previous.Annotations[ConfigHashAnnotation] {
			return false
		}
	}
	return true
}

// TestRotatingATLSSecretRollsTheRelays covers the half of ADR 4 that coturn's
// startup behaviour makes necessary: a relay reads its serving certificate when
// it starts, so replacing the certificate has to roll the relay. The Secret's
// content is part of the configuration hash; without that, a rotation would
// leave every running relay serving the old certificate until something else
// happened to restart it.
func TestRotatingATLSSecretRollsTheRelays(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "203.0.113.41")

	const servingCertName = "tpool-tls-serving-cert"
	f.createTLSSecret(servingCertName, "first-certificate")

	pool := hostNetworkPool("tpool-tls", 1)
	pool.Spec.Listeners = []turnfleetv1alpha1.ListenerSpec{
		{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
		{
			Name:         "turns",
			Protocol:     turnfleetv1alpha1.ListenerProtocolTLS,
			Port:         5349,
			TLSSecretRef: &corev1.LocalObjectReference{Name: servingCertName},
		},
	}
	f.createPool(pool)

	f.reconcile("tpool-tls")
	f.reconcile("tpool-tls")
	f.markPodsReady("tpool-tls")

	pods := f.pods("tpool-tls")
	if len(pods) != 1 {
		t.Fatalf("the pool has %d relay pods, want 1", len(pods))
	}
	before := pods[0].Annotations[ConfigHashAnnotation]
	if before == "" {
		t.Fatal("the relay pod carries no config hash")
	}

	f.rotateTLSSecret(servingCertName, "second-certificate")

	// The relay either rolls (its drain had no allocations to wait for) or
	// starts draining on its way out; what must not happen is that nothing
	// moved, which is what an unhashed Secret produces.
	f.reconcileUntil("tpool-tls", "the relay rolled after its certificate rotated", func() bool {
		for _, pod := range f.pods("tpool-tls") {
			if pod.Annotations[ConfigHashAnnotation] != before || pod.Annotations[DrainStartedAtAnnotation] != "" {
				return true
			}
		}
		return false
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

// TestPeerPolicyDefaultFollowsTheFamilyWithoutTheWebhook is the IPv6 half of
// the same install path: nothing defaulted the spec, and a relay only reaches
// peers in its own family, so the v6 pool has to get the v6 list. The v4 list
// would deny nothing this relay can route to and leave fc00::/7 and the whole
// IPv4-mapped space open.
func TestPeerPolicyDefaultFollowsTheFamilyWithoutTheWebhook(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-worker-1", "2001:db8::21")

	pool := hostNetworkPool("tpool-v6-no-policy", 1)
	pool.Spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyIPv6
	pool.Spec.PeerPolicy = nil
	f.createPool(pool)
	f.reconcile("tpool-v6-no-policy")

	rendered := f.relayConfig("tpool-v6-no-policy")
	// fc00::/7 is the IPv6 RFC 1918, where an in-cluster service network often
	// lives; ::ffff:0:0/96 covers the metadata address in its mapped form.
	if !strings.Contains(rendered, "denied-peer-ip=fc00::") {
		t.Errorf("an IPv6 pool that declares no peerPolicy does not deny unique local addresses:\n%s", rendered)
	}
	if !strings.Contains(rendered, "denied-peer-ip=::ffff:0.0.0.0-::ffff:255.255.255.255") {
		t.Errorf("an IPv6 pool that declares no peerPolicy does not deny the IPv4-mapped space:\n%s", rendered)
	}
	if strings.Contains(rendered, "169.254.0.0-169.254.255.255") {
		t.Errorf("an IPv6 pool was given the IPv4 default deny list, which denies nothing its relay can reach:\n%s", rendered)
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

// testHelperImage is the image the fixture's controller says its relays' init
// containers run.
const testHelperImage = "ghcr.io/hazmi35/turnfleet-controller:test"

// natPool is a one-relay HostNetwork pool with a TCP listener, so the relay pod
// carries a readiness probe, on a relay port range of its own.
func natPool(name string, portStart int32) *turnfleetv1alpha1.TURNPool {
	pool := hostNetworkPoolOn(name, portStart)
	pool.Spec.Listeners = []turnfleetv1alpha1.ListenerSpec{
		{Name: "turn-tcp", Protocol: turnfleetv1alpha1.ListenerProtocolTCP, Port: 3478},
	}
	return pool
}

// reportInitContainer plays the kubelet: the relay-init container of a pod ran
// to completion and left a termination message.
func (f *testFixture) reportInitContainer(pod *corev1.Pod, exitCode int32, message string) {
	f.t.Helper()

	patch := client.MergeFrom(pod.DeepCopy())
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: relayInitContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exitCode,
			Message:  message,
		}},
	}}
	if err := k8sClient.Status().Patch(f.ctx, pod, patch); err != nil {
		f.t.Fatalf("reporting the init container of pod %s: %v", pod.Name, err)
	}
}

// secretFile reads one key of a pool's relay configuration Secret.
func (f *testFixture) secretFile(pool, key string) (string, bool) {
	f.t.Helper()

	name := RelayConfigSecretName(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: pool}})
	secret := &corev1.Secret{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, secret); err != nil {
		f.t.Fatalf("getting secret %s: %v", name, err)
	}
	value, ok := secret.Data[key]
	return string(value), ok
}

// requireCandidates checks the pool's relay Secret for a relay that decides its
// bind address on the node: both candidates, nothing else, and each binds what
// it should.
func (f *testFixture) requireCandidates(pool, relayName string) {
	f.t.Helper()
	t := f.t

	local, ok := f.secretFile(pool, candidateKey(relayName, localConfigFile))
	if !ok {
		t.Fatalf("the configuration Secret has no local candidate for %s", relayName)
	}
	mapped, ok := f.secretFile(pool, candidateKey(relayName, mappedConfigFile))
	if !ok {
		t.Fatalf("the configuration Secret has no mapped candidate for %s", relayName)
	}
	if _, plain := f.secretFile(pool, relayName); plain {
		t.Errorf("the configuration Secret holds a single configuration for %s, which would bind an address the node may not hold", relayName)
	}
	requireContains(t, "local candidate", local, "listening-ip=13.212.26.172\n", "listening-ip=127.0.0.1\n", "relay-ip=13.212.26.172\n")
	requireAbsent(t, "local candidate", local, "external-ip=", "10.42.0.134")
	requireContains(t, "mapped candidate", mapped,
		"listening-ip=10.42.0.134\n", "listening-ip=127.0.0.1\n", "relay-ip=10.42.0.134\n", "external-ip=13.212.26.172/10.42.0.134\n")
	requireAbsent(t, "mapped candidate", mapped, "listening-ip=13.212.26.172", "relay-ip=13.212.26.172")
}

// TestReconcileLetsTheNodeDecideTheBind covers a node whose ExternalIP differs
// from its InternalIP and that did not say which one it holds: a cloud's NATed
// public node, or a node that holds its public address. The controller cannot
// tell them apart, so the relay pod does.
func TestReconcileLetsTheNodeDecideTheBind(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNATNode("tf-detect-1", "10.42.0.134", "13.212.26.172", nil)

	f.createPool(natPool("tpool-detect", 50000))
	f.reconcile("tpool-detect")
	f.reconcile("tpool-detect")

	status := f.pool("tpool-detect").Status
	if status == nil || len(status.Relays) != 1 || status.Relays[0].RelayAddress == nil {
		t.Fatalf("status = %+v, want one relay with an address", status)
	}
	if got := *status.Relays[0].RelayAddress; got != "13.212.26.172" {
		t.Errorf("the relay advertises %s, want the ExternalIP 13.212.26.172 in both cases", got)
	}

	pods := f.pods("tpool-detect")
	if len(pods) != 1 {
		t.Fatalf("got %d relay pods, want 1", len(pods))
	}
	pod := &pods[0]
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("pod has %d init containers, want the relay-init one", len(pod.Spec.InitContainers))
	}
	initContainer := pod.Spec.InitContainers[0]
	if initContainer.Name != relayInitContainerName || initContainer.Image != testHelperImage {
		t.Errorf("init container = %s on %s, want %s on %s",
			initContainer.Name, initContainer.Image, relayInitContainerName, testHelperImage)
	}
	for _, want := range []string{"relay-init", "--local-address=13.212.26.172", "--mapped-address=10.42.0.134"} {
		if !slices.Contains(initContainer.Args, want) {
			t.Errorf("init container args %v lack %q", initContainer.Args, want)
		}
	}
	if _, recorded := pod.Annotations[RelayBindAddressAnnotation]; recorded {
		t.Errorf("the pod records bind address %q before its init container chose one", pod.Annotations[RelayBindAddressAnnotation])
	}
	requireTCPProbeOnLoopback(t, pod)

	// The relay reads its configuration from memory the init container fills,
	// and the Secret holds both candidates under keys derived from the relay.
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == configVolumeName && (volume.EmptyDir == nil || volume.EmptyDir.Medium != corev1.StorageMediumMemory) {
			t.Errorf("config volume = %+v, want an in-memory emptyDir", volume.VolumeSource)
		}
	}
	f.requireCandidates("tpool-detect", pod.Name)

	// Until the init container reports, nothing is recorded. When it does, the
	// controller writes the choice onto the pod.
	f.reconcile("tpool-detect")
	pods = f.pods("tpool-detect")
	if _, recorded := pods[0].Annotations[RelayBindAddressAnnotation]; recorded {
		t.Fatalf("the pod records bind address %q before its init container reported", pods[0].Annotations[RelayBindAddressAnnotation])
	}
	f.reportInitContainer(&pods[0], 0, "10.42.0.134")
	f.reconcile("tpool-detect")
	pods = f.pods("tpool-detect")
	if got := pods[0].Annotations[RelayBindAddressAnnotation]; got != "10.42.0.134" {
		t.Errorf("the pod records bind address %q, want the InternalIP 10.42.0.134 the init container reported", got)
	}
}

// TestReconcileRecordsAHeldExternalIP covers the node that holds its ExternalIP:
// the init container reports it, and the pod records it.
func TestReconcileRecordsAHeldExternalIP(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNATNode("tf-detect-2", "10.42.0.135", "203.0.113.35", nil)

	f.createPool(natPool("tpool-held", 50100))
	f.reconcile("tpool-held")
	f.reconcile("tpool-held")

	pods := f.pods("tpool-held")
	if len(pods) != 1 {
		t.Fatalf("got %d relay pods, want 1", len(pods))
	}
	f.reportInitContainer(&pods[0], 0, "203.0.113.35")
	f.reconcile("tpool-held")
	if got := f.pods("tpool-held")[0].Annotations[RelayBindAddressAnnotation]; got != "203.0.113.35" {
		t.Errorf("the pod records bind address %q, want the ExternalIP 203.0.113.35", got)
	}
}

// TestReconcileIgnoresAFailedInitContainer covers an init container that could
// not choose: its termination message is a reason, and nothing is recorded.
func TestReconcileIgnoresAFailedInitContainer(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNATNode("tf-detect-3", "10.42.0.136", "203.0.113.36", nil)

	f.createPool(natPool("tpool-failed", 50200))
	f.reconcile("tpool-failed")
	f.reconcile("tpool-failed")

	pods := f.pods("tpool-failed")
	f.reportInitContainer(&pods[0], 1, "neither 203.0.113.36 nor 10.42.0.136 is assigned to an interface of this node")
	f.reconcile("tpool-failed")
	if got, recorded := f.pods("tpool-failed")[0].Annotations[RelayBindAddressAnnotation]; recorded {
		t.Errorf("the pod records bind address %q for an init container that failed", got)
	}
}

// TestReconcileIgnoresAnUnexpectedBind covers a termination message that is an
// address, but not one the relay was given: it is not recorded.
func TestReconcileIgnoresAnUnexpectedBind(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNATNode("tf-detect-4", "10.42.0.141", "203.0.113.41", nil)

	f.createPool(natPool("tpool-unexpected", 50800))
	f.reconcile("tpool-unexpected")
	f.reconcile("tpool-unexpected")

	pods := f.pods("tpool-unexpected")
	f.reportInitContainer(&pods[0], 0, "192.0.2.1")
	f.reconcile("tpool-unexpected")
	if got, recorded := f.pods("tpool-unexpected")[0].Annotations[RelayBindAddressAnnotation]; recorded {
		t.Errorf("the pod records bind address %q, which is neither address the relay was given", got)
	}
}

// TestReconcileRelayBindSkipsTheDecision covers the node annotation: a node
// that says which address it holds gets a relay with one configuration, no init
// container and the probe on the bind address, exactly like any other relay.
func TestReconcileRelayBindSkipsTheDecision(t *testing.T) {
	tests := []struct {
		name         string
		annotation   string
		wantBind     string
		wantMapping  bool
		wantNodeName string
		portStart    int32
	}{
		{name: "tpool-bind-int", annotation: RelayBindInternalIP, wantBind: "10.42.0.137", wantMapping: true, wantNodeName: "tf-bind-int", portStart: 50300},
		{name: "tpool-bind-ext", annotation: RelayBindExternalIP, wantBind: "203.0.113.37", wantNodeName: "tf-bind-ext", portStart: 50400},
	}
	for _, tc := range tests {
		t.Run(tc.annotation, func(t *testing.T) {
			f := newFixture(t)
			f.createSecret()
			f.createNATNode(tc.wantNodeName, "10.42.0.137", "203.0.113.37", map[string]string{RelayBindAnnotation: tc.annotation})

			f.createPool(natPool(tc.name, tc.portStart))
			f.reconcile(tc.name)
			f.reconcile(tc.name)

			pods := f.pods(tc.name)
			if len(pods) != 1 {
				t.Fatalf("got %d relay pods, want 1", len(pods))
			}
			pod := &pods[0]
			if len(pod.Spec.InitContainers) != 0 {
				t.Errorf("pod has init containers %v, want none: the node said which address it holds", pod.Spec.InitContainers)
			}
			if got := pod.Annotations[RelayBindAddressAnnotation]; got != tc.wantBind {
				t.Errorf("the pod records bind address %q, want %q", got, tc.wantBind)
			}
			for i := range pod.Spec.Containers {
				if probe := pod.Spec.Containers[i].ReadinessProbe; probe != nil && probe.TCPSocket.Host != tc.wantBind {
					t.Errorf("readiness probe host = %q, want the bind address %q", probe.TCPSocket.Host, tc.wantBind)
				}
			}
			config, ok := f.secretFile(tc.name, pod.Name)
			if !ok {
				t.Fatalf("the configuration Secret has no configuration for %s", pod.Name)
			}
			requireContains(t, "configuration", config, "listening-ip="+tc.wantBind+"\n", "relay-ip="+tc.wantBind+"\n")
			requireAbsent(t, "configuration", config, "listening-ip=127.0.0.1")
			if mapped := strings.Contains(config, "external-ip=203.0.113.37/10.42.0.137\n"); mapped != tc.wantMapping {
				t.Errorf("configuration maps the ExternalIP = %v, want %v:\n%s", mapped, tc.wantMapping, config)
			}
		})
	}
}

// TestReconcileRejectsAnUnusableRelayBind covers a relay-bind value that is
// neither InternalIP nor ExternalIP: the pass fails and names the node, like an
// unusable relay-address does, and plans nothing on a guess.
func TestReconcileRejectsAnUnusableRelayBind(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNATNode("tf-bad-bind-1", "10.42.0.138", "203.0.113.38",
		map[string]string{RelayBindAnnotation: "Both"})

	f.createPool(natPool("tpool-bad-bind", 50500))
	_, err := f.reconcileResult("tpool-bad-bind")
	if err == nil || !strings.Contains(err.Error(), RelayBindAnnotation) {
		t.Fatalf("reconcile error = %v, want one that names %s", err, RelayBindAnnotation)
	}
	if pods := f.pods("tpool-bad-bind"); len(pods) != 0 {
		t.Errorf("got %d relay pods, want none", len(pods))
	}
}

// TestReconcileNeedsTheHelperImage covers a controller that does not know its
// own image: a relay that needs the decision is refused with an error that says
// how to settle it, instead of getting a pod that binds a guess.
func TestReconcileNeedsTheHelperImage(t *testing.T) {
	f := newFixture(t)
	f.reconciler.RelayHelperImage = ""
	f.createSecret()
	f.createNATNode("tf-no-helper-1", "10.42.0.139", "203.0.113.39", nil)

	f.createPool(natPool("tpool-no-helper", 50600))
	_, err := f.reconcileResult("tpool-no-helper")
	if err == nil || !strings.Contains(err.Error(), "--relay-helper-image") || !strings.Contains(err.Error(), RelayBindAnnotation) {
		t.Fatalf("reconcile error = %v, want one that names --relay-helper-image and %s", err, RelayBindAnnotation)
	}
	if pods := f.pods("tpool-no-helper"); len(pods) != 0 {
		t.Errorf("got %d relay pods, want none", len(pods))
	}
}

// TestReconcileResolvesTheHelperImageLazily covers a controller that reads its
// own image when a relay needs it: a failed read fails the pass and is retried,
// the first success is kept, and the pod runs what was read.
func TestReconcileResolvesTheHelperImageLazily(t *testing.T) {
	f := newFixture(t)
	f.reconciler.RelayHelperImage = ""
	calls := 0
	failing := true
	f.reconciler.ResolveRelayHelperImage = func(context.Context) (string, error) {
		calls++
		if failing {
			return "", errors.New("the API server is not reachable")
		}
		return "ghcr.io/hazmi35/turnfleet-controller:v9@sha256:" + strings.Repeat("ab", 32), nil
	}
	f.createSecret()
	f.createNATNode("tf-lazy-1", "10.42.0.140", "203.0.113.40", nil)
	f.createPool(natPool("tpool-lazy", 50700))

	_, err := f.reconcileResult("tpool-lazy")
	if err == nil || !strings.Contains(err.Error(), "the API server is not reachable") {
		t.Fatalf("reconcile error = %v, want the failed read", err)
	}

	failing = false
	f.reconcile("tpool-lazy")
	f.reconcile("tpool-lazy")
	pods := f.pods("tpool-lazy")
	if len(pods) != 1 || len(pods[0].Spec.InitContainers) != 1 {
		t.Fatalf("pods = %d, want one relay pod with its init container", len(pods))
	}
	if got := pods[0].Spec.InitContainers[0].Image; !strings.HasPrefix(got, "ghcr.io/hazmi35/turnfleet-controller:v9@sha256:") {
		t.Errorf("init container image = %q, want the image the resolver found", got)
	}
	if calls != 2 {
		t.Errorf("the resolver was called %d times, want 2: one failure, one success that is kept", calls)
	}
}

func requireTCPProbeOnLoopback(t *testing.T, pod *corev1.Pod) {
	t.Helper()

	probes := 0
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if container.ReadinessProbe == nil || container.ReadinessProbe.TCPSocket == nil {
			continue
		}
		probes++
		if got := container.ReadinessProbe.TCPSocket.Host; got != relay.LoopbackV4 {
			t.Errorf("readiness probe host = %q, want %q: the bind address is not known when the pod is created", got, relay.LoopbackV4)
		}
	}
	if probes == 0 {
		t.Error("the relay pod has no TCP readiness probe to check")
	}
}

func requireContains(t *testing.T, what, text string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s lacks %q:\n%s", what, want, text)
		}
	}
}

func requireAbsent(t *testing.T, what, text string, unwanted ...string) {
	t.Helper()

	for _, value := range unwanted {
		if strings.Contains(text, value) {
			t.Errorf("%s has %q:\n%s", what, value, text)
		}
	}
}

// TestNodeAddresses covers the observation the family decision rests on: per
// family an advertised address, the address a relay binds, and the local
// address the node may hold instead. A family with an InternalIP and a
// different ExternalIP leaves the choice to the node unless relay-bind makes
// it, and the relay-address annotation replaces only the advertised address of
// the family it names.
func TestNodeAddresses(t *testing.T) {
	t.Parallel()

	node := func(annotations map[string]string, addresses ...corev1.NodeAddress) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "tf-worker-1", Annotations: annotations},
			Status:     corev1.NodeStatus{Addresses: addresses},
		}
	}
	internalV4 := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}
	externalV4 := corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.11"}
	internalV6 := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::11"}
	externalV6 := corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "2001:db8::11"}

	// detect is the address of a family the node decides: advertise the
	// ExternalIP, bind the InternalIP unless the node holds the ExternalIP.
	detectV4 := relay.NodeAddress{Address: "203.0.113.11", BindAddress: "10.0.0.11", LocalBindAddress: "203.0.113.11"}
	detectV6 := relay.NodeAddress{Address: "2001:db8::11", BindAddress: "fd00::11", LocalBindAddress: "2001:db8::11"}

	tests := []struct {
		name      string
		node      *corev1.Node
		want      []relay.NodeAddress
		wantError bool
	}{
		{
			name: "both addresses differ: the node decides what to bind",
			node: node(nil, internalV4, externalV4),
			want: []relay.NodeAddress{detectV4},
		},
		{
			name: "the order of the node's addresses does not matter",
			node: node(nil, externalV4, internalV4),
			want: []relay.NodeAddress{detectV4},
		},
		{
			name: "both addresses are equal",
			node: node(nil,
				corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "203.0.113.11"}, externalV4),
			want: []relay.NodeAddress{{Address: "203.0.113.11", BindAddress: "203.0.113.11"}},
		},
		{
			name: "external address only",
			node: node(nil, externalV4),
			want: []relay.NodeAddress{{Address: "203.0.113.11", BindAddress: "203.0.113.11"}},
		},
		{
			name: "internal address only",
			node: node(nil, internalV4),
			want: []relay.NodeAddress{{Address: "10.0.0.11", BindAddress: "10.0.0.11"}},
		},
		{
			name: "dual stack, both families differ",
			node: node(nil, internalV4, externalV4, internalV6, externalV6,
				corev1.NodeAddress{Type: corev1.NodeHostName, Address: "tf-worker-1"}),
			want: []relay.NodeAddress{detectV4, detectV6},
		},
		{
			name: "dual stack, IPv4 differs and IPv6 does not",
			node: node(nil, internalV4, externalV4,
				corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "2001:db8::11"}, externalV6),
			want: []relay.NodeAddress{detectV4, {Address: "2001:db8::11", BindAddress: "2001:db8::11"}},
		},
		{
			name: "dual stack, IPv6 has an external address only",
			node: node(nil, internalV4, externalV4, externalV6),
			want: []relay.NodeAddress{detectV4, {Address: "2001:db8::11", BindAddress: "2001:db8::11"}},
		},
		{
			name: "relay-bind ExternalIP binds and advertises the external address",
			node: node(map[string]string{RelayBindAnnotation: RelayBindExternalIP}, internalV4, externalV4),
			want: []relay.NodeAddress{{Address: "203.0.113.11", BindAddress: "203.0.113.11"}},
		},
		{
			name: "relay-bind ExternalIP covers every family",
			node: node(map[string]string{RelayBindAnnotation: RelayBindExternalIP},
				internalV4, externalV4, internalV6, externalV6),
			want: []relay.NodeAddress{
				{Address: "203.0.113.11", BindAddress: "203.0.113.11"},
				{Address: "2001:db8::11", BindAddress: "2001:db8::11"},
			},
		},
		{
			name: "relay-bind InternalIP binds the internal address and advertises the external one",
			node: node(map[string]string{RelayBindAnnotation: RelayBindInternalIP}, internalV4, externalV4),
			want: []relay.NodeAddress{{Address: "203.0.113.11", BindAddress: "10.0.0.11"}},
		},
		{
			name: "relay-bind on a node with an internal address only",
			node: node(map[string]string{RelayBindAnnotation: RelayBindExternalIP}, internalV4),
			want: []relay.NodeAddress{{Address: "10.0.0.11", BindAddress: "10.0.0.11"}},
		},
		{
			name:      "relay-bind with a value that is neither InternalIP nor ExternalIP",
			node:      node(map[string]string{RelayBindAnnotation: "internalip"}, internalV4, externalV4),
			wantError: true,
		},
		{
			name: "a name is not an address",
			node: node(nil,
				corev1.NodeAddress{Type: corev1.NodeHostName, Address: "tf-worker-1"},
				corev1.NodeAddress{Type: corev1.NodeExternalDNS, Address: "tf-worker-1.example.net"}),
		},
		{
			name: "relay-address replaces the advertised address, and the node still decides the bind",
			node: node(map[string]string{RelayAddressAnnotation: "198.51.100.77"}, internalV4, externalV4),
			want: []relay.NodeAddress{{Address: "198.51.100.77", BindAddress: "10.0.0.11", LocalBindAddress: "203.0.113.11"}},
		},
		{
			name: "relay-address overrides the family it names and leaves the other alone",
			node: node(map[string]string{RelayAddressAnnotation: "198.51.100.77"},
				internalV4, externalV4, internalV6, externalV6),
			want: []relay.NodeAddress{
				{Address: "198.51.100.77", BindAddress: "10.0.0.11", LocalBindAddress: "203.0.113.11"},
				detectV6,
			},
		},
		{
			name: "relay-address with relay-bind ExternalIP binds the external address",
			node: node(map[string]string{
				RelayAddressAnnotation: "198.51.100.77",
				RelayBindAnnotation:    RelayBindExternalIP,
			}, internalV4, externalV4),
			want: []relay.NodeAddress{{Address: "198.51.100.77", BindAddress: "203.0.113.11"}},
		},
		{
			name: "relay-address with relay-bind InternalIP binds the internal address",
			node: node(map[string]string{
				RelayAddressAnnotation: "198.51.100.77",
				RelayBindAnnotation:    RelayBindInternalIP,
			}, internalV4, externalV4),
			want: []relay.NodeAddress{{Address: "198.51.100.77", BindAddress: "10.0.0.11"}},
		},
		{
			name: "relay-address on a node with an external address only",
			node: node(map[string]string{RelayAddressAnnotation: "198.51.100.77"}, externalV4),
			want: []relay.NodeAddress{{Address: "198.51.100.77", BindAddress: "203.0.113.11"}},
		},
		{
			name:      "relay-address for a family the node does not have",
			node:      node(map[string]string{RelayAddressAnnotation: "2001:db8::77"}, externalV4),
			wantError: true,
		},
		{
			name:      "relay-address that is not an address",
			node:      node(map[string]string{RelayAddressAnnotation: "relay.example.net"}, externalV4),
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

// TestNodeAddressKeyFollowsRelayBind pins that changing the relay-bind
// annotation changes the node's key whenever it changes the plan, so the
// controller re-plans the node, and that a change that plans the same relays
// does not.
func TestNodeAddressKeyFollowsRelayBind(t *testing.T) {
	t.Parallel()

	node := func(annotations map[string]string, addresses ...corev1.NodeAddress) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "tf-worker-1", Annotations: annotations},
			Status:     corev1.NodeStatus{Addresses: addresses},
		}
	}
	internal := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}
	external := corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.11"}

	detect := nodeAddressKey(node(nil, internal, external))
	bindInternal := nodeAddressKey(node(map[string]string{RelayBindAnnotation: RelayBindInternalIP}, internal, external))
	bindExternal := nodeAddressKey(node(map[string]string{RelayBindAnnotation: RelayBindExternalIP}, internal, external))
	invalid := nodeAddressKey(node(map[string]string{RelayBindAnnotation: "Both"}, internal, external))

	keys := map[string]string{"detect": detect, "InternalIP": bindInternal, "ExternalIP": bindExternal, "invalid": invalid}
	for a, keyA := range keys {
		for b, keyB := range keys {
			if a < b && keyA == keyB {
				t.Errorf("%s and %s have the same node key %q, so changing the annotation between them would not re-plan the node", a, b, keyA)
			}
		}
	}

	// A node with one address plans the same relay whatever relay-bind says.
	single := nodeAddressKey(node(nil, external))
	if withAnnotation := nodeAddressKey(node(map[string]string{RelayBindAnnotation: RelayBindExternalIP}, external)); withAnnotation != single {
		t.Errorf("relay-bind changes the key of a node with one address: %q vs %q", withAnnotation, single)
	}
}

// TestRelayHashOfAPlainRelayIsUnchanged pins the config hash of a relay that
// does not decide its bind address on the node to the value the operator
// computed before that decision existed (the values come from the hash on main
// at 6e2c085). Upgrading must not roll a relay that needs no init container.
func TestRelayHashOfAPlainRelayIsUnchanged(t *testing.T) {
	t.Parallel()

	pool := hostNetworkPool("demo", 1)
	pool.Spec.Dataplane.Image = "coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3"
	state := &relayState{relay: relay.Relay{NodeName: "worker-1"}, config: "listening-ip=203.0.113.10\n"}

	for tlsSecretHash, want := range map[string]string{
		"":       "01436bbcb89540a6b2873c802a88983f51894ef15413cf67f134e97e0d4d2362",
		"abc123": "36f59f5710413fb18f5b88f602dc5135a2cfae5a7ac91b3c63486bd01ac3f850",
	} {
		got, err := relayHash(pool, state, tlsSecretHash)
		if err != nil {
			t.Fatalf("relayHash() failed: %v", err)
		}
		if got != want {
			t.Errorf("relayHash(tls %q) = %s, want %s: a relay that needs no init container would roll on upgrade", tlsSecretHash, got, want)
		}
	}
}

// TestRelayHashCoversTheCandidates pins what rolls a relay that decides its
// bind address on the node: a change to either candidate configuration. A
// change to the image of its init container does not: every controller release
// has another image, and a release must not restart every relay on a
// two-address node.
func TestRelayHashCoversTheCandidates(t *testing.T) {
	t.Parallel()

	pool := hostNetworkPool("demo", 1)
	base := func() *relayState {
		return &relayState{
			relay:       relay.Relay{NodeName: "worker-1"},
			candidates:  &relay.Candidates{Local: "listening-ip=13.212.26.172\n", Mapped: "listening-ip=10.42.0.134\n"},
			helperImage: "ghcr.io/hazmi35/turnfleet-controller:v1",
		}
	}
	hashOf := func(state *relayState) string {
		t.Helper()

		hash, err := relayHash(pool, state, "")
		if err != nil {
			t.Fatalf("relayHash() failed: %v", err)
		}
		return hash
	}

	original := hashOf(base())
	if again := hashOf(base()); again != original {
		t.Fatal("relayHash() is not deterministic")
	}

	changes := map[string]func(*relayState){
		"local candidate":  func(s *relayState) { s.candidates.Local += "# changed\n" },
		"mapped candidate": func(s *relayState) { s.candidates.Mapped += "# changed\n" },
	}
	for name, change := range changes {
		state := base()
		change(state)
		if got := hashOf(state); got == original {
			t.Errorf("changing the %s leaves the hash alone, so the relay would not roll", name)
		}
	}

	newImage := base()
	newImage.helperImage = "ghcr.io/hazmi35/turnfleet-controller:v2"
	if got := hashOf(newImage); got != original {
		t.Error("a new helper image changes the hash, so every controller release would roll every relay on a two-address node")
	}

	// A relay with candidates carries the decision version, and one without
	// does not.
	plain := base()
	plain.candidates, plain.config = nil, "listening-ip=10.42.0.134\n"
	if decisionVersion(plain) != "" || decisionVersion(base()) != relayinit.Version {
		t.Error("decisionVersion() must be empty for a relay without candidates and relayinit.Version for one with")
	}

	// Moving text between the two candidates is a change too.
	swapped := base()
	swapped.candidates.Local, swapped.candidates.Mapped = swapped.candidates.Mapped, swapped.candidates.Local
	if got := hashOf(swapped); got == original {
		t.Error("swapping the candidates leaves the hash alone")
	}
}
