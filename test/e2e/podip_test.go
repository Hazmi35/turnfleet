//go:build e2e

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/controller"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// podIPEventTimeout bounds each wait for the controller to act on a relay pod:
// the pod has to be created, scheduled, started and observed Running before its
// address can be read back.
const podIPEventTimeout = 3 * time.Minute

// TestPodIPRelayWhoseDeclaredAddressThePodCannotHold covers the PodIP read-back
// against a real pod. The relay node declares an address, and the pool names no
// attachment, so the pod comes up on its own address on the default network and
// cannot hold the declared one. The controller must notice, drain the relay
// instead of advertising an address its clients were not given, and never
// publish it.
//
// The declared address is a documentation address (RFC 5737), which admission
// and the reconciler both accept as individually routable: the relay fails
// because the pod does not hold the address, not because the declaration is
// invalid. The drain finishes because the relay holds no allocation, so the
// pod is replaced and the cycle repeats, which is why the case waits for each
// event and checks the invariants on every poll instead of at one instant.
func TestPodIPRelayWhoseDeclaredAddressThePodCannotHold(t *testing.T) {
	const (
		name     = "e2e-podip-unheld"
		declared = "203.0.113.21"
	)

	ctx := context.Background()

	// The annotation is registered for removal before the pool is created, so
	// the pool is deleted first and the node is clean when the next case runs.
	annotateNode(t, relayNode("-worker3"), relay.PodAddressesAnnotation, declared)

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 1,
			// No addressing.podIP: the relay binds the pod's own address.
			Addressing:     turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModePodIP},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51800, End: 51820},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34788},
			},
		},
	})

	// A case that cannot tell why it failed is only a red job, so a failure
	// logs what the pool, its pods and the controller were doing. This runs
	// before the pool is deleted, because cleanups run last in, first out.
	t.Cleanup(func() {
		if t.Failed() {
			dumpPoolState(t, name)
		}
	})

	// leaked is reported once. The relay must not be published at any poll.
	leaked := false
	assertNotPublished := func(ctx context.Context) {
		if leaked {
			return
		}
		if uris := publishedURIs(ctx, t, name); strings.Contains(uris, declared) {
			leaked = true
			t.Errorf("the relay on %s was published although its pod does not hold that address: %s", declared, uris)
		}
	}

	waitFor(ctx, t, "the controller to report RelayAddressChanged", podIPEventTimeout, func(ctx context.Context) (bool, error) {
		assertNotPublished(ctx)
		return len(poolEvents(ctx, t, name, "RelayAddressChanged")) > 0, nil
	})

	requireAddressChangedEvents(ctx, t, name, declared)

	// The relay is reported draining and not ready in one snapshot of the
	// status. The pod is replaced after each drain, so a poll catches a
	// draining relay on one of the cycles.
	var draining turnfleetv1alpha1.RelayStatus
	waitFor(ctx, t, "the relay to be reported draining and not ready", podIPEventTimeout, func(ctx context.Context) (bool, error) {
		assertNotPublished(ctx)
		current := pool(ctx, t, name)
		if current.Status == nil {
			return false, nil
		}
		for i := range current.Status.Relays {
			if status := current.Status.Relays[i]; status.Draining != nil && *status.Draining {
				draining = status
				return true, nil
			}
		}
		return false, nil
	})
	requireDrainingRelay(t, &draining, declared)

	// The relay holds no allocation, so the drain completes and the pod is
	// deleted. (With allocations the pod would stay until they reach zero or
	// drainTimeout expires, which TestScaleDownDrainsRelayWithAnActiveAllocation
	// covers.)
	waitFor(ctx, t, "the controller to report RelayDrained", podIPEventTimeout, func(ctx context.Context) (bool, error) {
		assertNotPublished(ctx)
		return len(poolEvents(ctx, t, name, "RelayDrained")) > 0, nil
	})

	// Whatever cycle the pool is in, it reports no ready relay and publishes no
	// URI, in the status and in the ConfigMap the credential issuer reads.
	requirePoolNotReady(ctx, t, name)
}

// requireAddressChangedEvents checks every RelayAddressChanged event of a pool:
// a warning that names the planned address and an address the pod holds, which
// is another one.
func requireAddressChangedEvents(ctx context.Context, t *testing.T, poolName, declared string) {
	t.Helper()

	events := poolEvents(ctx, t, poolName, "RelayAddressChanged")
	for i := range events {
		event := &events[i]
		if event.Type != corev1.EventTypeWarning {
			t.Errorf("RelayAddressChanged has type %q, want %q", event.Type, corev1.EventTypeWarning)
		}
		if !strings.Contains(event.Note, "planned on "+declared) {
			t.Errorf("RelayAddressChanged does not name the planned address %s: %s", declared, event.Note)
		}
		if strings.Contains(event.Note, "holds address "+declared+" ") {
			t.Errorf("RelayAddressChanged says the pod holds the declared address %s: %s", declared, event.Note)
		}
	}
}

// requireDrainingRelay checks a relay's status while it drains: it still
// carries the declared address, it is not ready, and it publishes nothing.
func requireDrainingRelay(t *testing.T, status *turnfleetv1alpha1.RelayStatus, declared string) {
	t.Helper()

	if status.RelayAddress == nil || *status.RelayAddress != declared {
		t.Errorf("the draining relay reports address %v, want the declared %s", status.RelayAddress, declared)
	}
	if status.Ready == nil || *status.Ready {
		t.Errorf("the draining relay's ready = %v, want false", status.Ready)
	}
	if len(status.TurnURIs) != 0 {
		t.Errorf("the draining relay publishes %v, want none", status.TurnURIs)
	}
}

// requirePoolNotReady checks that a pool reports no ready relay and lists no
// URI, in the status and in the URI ConfigMap.
func requirePoolNotReady(ctx context.Context, t *testing.T, poolName string) {
	t.Helper()

	current := pool(ctx, t, poolName)
	if current.Status == nil {
		t.Fatalf("pool %s has no status", poolName)
	}
	if current.Status.ReadyRelays == nil || *current.Status.ReadyRelays != 0 {
		t.Errorf("readyRelays = %v, want 0", current.Status.ReadyRelays)
	}
	if len(current.Status.TurnURIs) != 0 {
		t.Errorf("turnURIs = %v, want none", current.Status.TurnURIs)
	}
	if conditionTrue(ctx, t, poolName, controller.ConditionReady) {
		t.Errorf("the pool reports %s while its only relay cannot hold its address", controller.ConditionReady)
	}
	if uris := configMapURIs(ctx, t, poolName); uris != "" {
		t.Errorf("ConfigMap %s lists %q, want no URI", controller.URIsConfigMapName(current), uris)
	}
}

// dumpPoolState logs the pool, its relay pods, the namespace events and the
// controller's recent log. Each is one best-effort kubectl call: it runs on a
// failing case, so an error here is logged and never replaces the failure.
func dumpPoolState(t *testing.T, poolName string) {
	t.Helper()

	for _, args := range [][]string{
		{"-n", testNamespace, "get", "turnpool", poolName, "-o", "yaml"},
		{"-n", testNamespace, "get", "pods", "-o", "wide"},
		{"-n", testNamespace, "get", "pods", "-l", "app.kubernetes.io/instance=" + poolName, "-o", "yaml"},
		{"-n", testNamespace, "get", "events.events.k8s.io", "-o", "wide"},
		{"-n", controllerNamespace, "logs", "deployment/" + controllerDeploymentName, "--tail=300"},
	} {
		full := append([]string{"--context", kubeContext}, args...)
		ctx, cancel := context.WithTimeout(context.Background(), kubectlTimeout)
		//nolint:gosec // the suite execs the tools it documents; the arguments come from the tests, not from untrusted input.
		output, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
		cancel()
		t.Logf("kubectl %s (error: %v):\n%s", strings.Join(args, " "), err, output)
	}
}

// annotateNode sets an annotation on a node and removes it when the test
// finishes, so the declaration does not outlive the case.
func annotateNode(t *testing.T, nodeName, key, value string) {
	t.Helper()

	ctx := context.Background()
	node := &corev1.Node{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		t.Fatalf("reading node %s: %v", nodeName, err)
	}
	patchNodeAnnotation(ctx, t, node, func(annotations map[string]string) map[string]string {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[key] = value
		return annotations
	})

	t.Cleanup(func() {
		node := &corev1.Node{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			t.Errorf("reading node %s to remove %s: %v", nodeName, key, err)
			return
		}
		patchNodeAnnotation(ctx, t, node, func(annotations map[string]string) map[string]string {
			delete(annotations, key)
			return annotations
		})
	})
}

// patchNodeAnnotation applies a change to a node's annotations as a merge
// patch, so it touches nothing else on the node.
func patchNodeAnnotation(ctx context.Context, t *testing.T, node *corev1.Node, change func(map[string]string) map[string]string) {
	t.Helper()

	patch := client.MergeFrom(node.DeepCopy())
	node.Annotations = change(node.Annotations)
	if err := k8sClient.Patch(ctx, node, patch); err != nil {
		t.Fatalf("patching the annotations of node %s: %v", node.Name, err)
	}
}

// poolEvents lists the events the controller recorded about a pool with the
// given reason.
func poolEvents(ctx context.Context, t *testing.T, poolName, reason string) []eventsv1.Event {
	t.Helper()

	list := &eventsv1.EventList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []eventsv1.Event
	for i := range list.Items {
		event := &list.Items[i]
		if event.Reason == reason && event.Regarding.Kind == "TURNPool" && event.Regarding.Name == poolName {
			out = append(out, *event)
		}
	}
	return out
}

// publishedURIs is everything a client could be handed for a pool: the status
// list and the ConfigMap the credential issuer reads, joined for a substring
// check.
func publishedURIs(ctx context.Context, t *testing.T, poolName string) string {
	t.Helper()

	status := pool(ctx, t, poolName).Status
	var published []string
	if status != nil {
		published = append(published, status.TurnURIs...)
		for i := range status.Relays {
			published = append(published, status.Relays[i].TurnURIs...)
		}
	}
	published = append(published, configMapURIs(ctx, t, poolName))
	return strings.Join(published, "\n")
}

// configMapURIs reads the pool's URI ConfigMap, which is empty until the
// controller first writes it.
func configMapURIs(ctx context.Context, t *testing.T, poolName string) string {
	t.Helper()

	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Namespace: testNamespace,
		Name:      controller.URIsConfigMapName(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}}),
	}
	if err := k8sClient.Get(ctx, key, configMap); err != nil {
		if client.IgnoreNotFound(err) != nil {
			t.Fatalf("reading ConfigMap %s: %v", key.Name, err)
		}
		return ""
	}
	return configMap.Data["uris"]
}
