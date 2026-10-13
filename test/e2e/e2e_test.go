//go:build e2e

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/controller"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// controllerNamespace is where the manager runs (config/manager).
const controllerNamespace = "turnfleet-system"

// TestHostNetworkMode covers the mode that makes capacity grow with nodes: one
// relay per labelled node, each on its own address, each allocating for real.
func TestHostNetworkMode(t *testing.T) {
	const name = "e2e-host"

	ctx := context.Background()

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas:       3,
			Addressing:     turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModeHostNetwork},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51000, End: 51020},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34780},
			},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	status := waitForRelays(ctx, t, name, 3)
	if len(status.Status.TurnURIs) != 3 {
		t.Fatalf("pool publishes %d URIs, want one per relay: %v", len(status.Status.TurnURIs), status.Status.TurnURIs)
	}

	addresses := map[string]bool{}
	for _, relayStatus := range status.Status.Relays {
		if relayStatus.RelayAddress == nil {
			t.Fatalf("relay %s has no address", relayStatus.Name)
		}
		addresses[*relayStatus.RelayAddress] = true

		host, port := relayListenerHostPort(t, &relayStatus)
		if host != *relayStatus.RelayAddress {
			t.Errorf("relay %s publishes %s but advertises %s", relayStatus.Name, host, *relayStatus.RelayAddress)
		}
		portRange := relay.PortRange{Start: relayStatus.PortRange.Start, End: relayStatus.PortRange.End}
		result := probeRelay(t, host, port, portRange)
		if !result.Succeeded {
			t.Errorf("relay %s did not allocate: %s", relayStatus.Name, result.Message)
		}
		if result.RelayedPort < portRange.Start || result.RelayedPort > portRange.End {
			t.Errorf("relay %s relayed on port %d, outside its range %s", relayStatus.Name, result.RelayedPort, portRange)
		}
	}

	if len(addresses) != 3 {
		t.Errorf("three relays share %d addresses: %v; capacity only grows with distinct addresses", len(addresses), addresses)
	}
}

// TestPartitionedIPMode covers several relays sharing one node address: disjoint
// port ranges, disjoint listener ports, and both allocating.
func TestPartitionedIPMode(t *testing.T) {
	const name = "e2e-partitioned"

	ctx := context.Background()

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 2,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
				PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51100, End: 51120},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34781},
			},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	status := waitForRelays(ctx, t, name, 2)

	addresses := map[string]bool{}
	ranges := map[string][]relay.PortRange{}
	listenerPorts := map[int32]string{}

	for _, relayStatus := range status.Status.Relays {
		if relayStatus.RelayAddress == nil {
			t.Fatalf("relay %s has no address", relayStatus.Name)
		}
		addresses[*relayStatus.RelayAddress] = true
		ranges[*relayStatus.RelayAddress] = append(ranges[*relayStatus.RelayAddress],
			relay.PortRange{Start: relayStatus.PortRange.Start, End: relayStatus.PortRange.End})

		host, port := relayListenerHostPort(t, &relayStatus)
		if other, clash := listenerPorts[port]; clash {
			t.Errorf("relays %s and %s both listen on %s:%d: a listening socket binds once per address",
				other, relayStatus.Name, host, port)
		}
		listenerPorts[port] = relayStatus.Name

		portRange := relay.PortRange{Start: relayStatus.PortRange.Start, End: relayStatus.PortRange.End}
		result := probeRelay(t, host, port, portRange)
		if !result.Succeeded {
			t.Errorf("relay %s did not allocate: %s", relayStatus.Name, result.Message)
		}
	}

	if len(addresses) != 1 {
		t.Errorf("PartitionedIP used %d addresses, want the one shared node address: %v", len(addresses), addresses)
	}
	for address, rangesOnAddress := range ranges {
		for i := range rangesOnAddress {
			for j := i + 1; j < len(rangesOnAddress); j++ {
				if rangesOnAddress[i].Overlaps(rangesOnAddress[j]) {
					t.Errorf("address %s has overlapping ranges %s and %s", address, rangesOnAddress[i], rangesOnAddress[j])
				}
			}
		}
	}
	// Splitting an address adds relays, not ports.
	if status.Status.PortsTotal == nil || *status.Status.PortsTotal != 21 {
		t.Errorf("portsTotal = %v, want 21 (one address, 21 ports)", status.Status.PortsTotal)
	}
}

// TestExternalIPPoolMode covers relays bound to secondary addresses that are
// already attached to nodes. The suite attaches them the way an operator would.
func TestExternalIPPoolMode(t *testing.T) {
	const name = "e2e-pool"

	ctx := context.Background()

	addresses := []turnfleetv1alpha1.ExternalAddress{
		{NodeName: relayNode("-worker"), Address: secondaryAddress(1)},
		{NodeName: relayNode("-worker2"), Address: secondaryAddress(2)},
	}
	for _, address := range addresses {
		attachSecondaryAddress(t, address.NodeName, address.Address)
	}

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 2,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode:           turnfleetv1alpha1.AddressingModeExternalIPPool,
				ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{Addresses: addresses},
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51200, End: 51220},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34782},
			},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	status := waitForRelays(ctx, t, name, 2)

	advertised := map[string]bool{}
	for _, relayStatus := range status.Status.Relays {
		if relayStatus.RelayAddress == nil {
			t.Fatalf("relay %s has no address", relayStatus.Name)
		}
		advertised[*relayStatus.RelayAddress] = true

		host, port := relayListenerHostPort(t, &relayStatus)
		portRange := relay.PortRange{Start: relayStatus.PortRange.Start, End: relayStatus.PortRange.End}
		result := probeRelay(t, host, port, portRange)
		if !result.Succeeded {
			t.Errorf("relay %s did not allocate on %s: %s", relayStatus.Name, host, result.Message)
		}
	}

	for _, want := range addresses {
		if !advertised[want.Address] {
			t.Errorf("no relay advertises %s; advertised: %v", want.Address, advertised)
		}
	}
}

// TestSaturationBecomesObservable is the case the project exists for: a relay
// whose ports are exhausted answers 508, the pool says so, and the other relays
// keep serving.
func TestSaturationBecomesObservable(t *testing.T) {
	const name = "e2e-saturation"

	ctx := context.Background()

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 2,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
			},
			// Twenty ports per relay: small enough to exhaust, large enough to
			// hold a few allocations.
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51300, End: 51319},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34783},
			},
			Probe:      &turnfleetv1alpha1.ProbeSpec{Interval: &metav1.Duration{Duration: 2 * time.Second}},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	status := waitForRelays(ctx, t, name, 2)
	if len(status.Status.Relays) != 2 {
		t.Fatalf("pool has %d relays, want 2", len(status.Status.Relays))
	}

	full := status.Status.Relays[0]
	other := status.Status.Relays[1]
	fullAddress, fullPort := relayListenerHostPort(t, &full)
	otherAddress, otherPort := relayListenerHostPort(t, &other)
	portRange := relay.PortRange{Start: full.PortRange.Start, End: full.PortRange.End}

	// Exhaust the first relay: hold every port in its range.
	release := exhaustRelay(t, fullAddress, fullPort, portRange)
	defer release()

	// The probe the operator runs must now observe 508, and the pool must say so.
	refused := probeRelay(t, fullAddress, fullPort, portRange)
	if refused.Succeeded {
		t.Fatal("the saturated relay still allocated: the test did not exhaust it")
	}
	if refused.ErrorCode != 508 {
		t.Errorf("saturated relay answered %d (%s), want 508 Insufficient Capacity", refused.ErrorCode, refused.Message)
	}

	waitFor(ctx, t, "CapacityPressure to become true", 90*time.Second, func(ctx context.Context) (bool, error) {
		return conditionTrue(ctx, t, name, controller.ConditionCapacityPressure), nil
	})

	// The counter the operator publishes must move. It is scoped to the pool, so
	// a capacity problem at one relay is attributable.
	waitFor(ctx, t, "turnfleet_allocation_failures_total{code=\"508\"} to count the refusal", 90*time.Second,
		func(ctx context.Context) (bool, error) {
			return strings.Contains(controllerMetrics(ctx, t), `turnfleet_allocation_failures_total{code="508"`), nil
		})

	// Saturation is per relay: the other relay still allocates.
	elsewhere := probeRelay(t, otherAddress, otherPort,
		relay.PortRange{Start: other.PortRange.Start, End: other.PortRange.End})
	if !elsewhere.Succeeded {
		t.Fatalf("the other relay failed too, so this is not per-relay saturation: %s", elsewhere.Message)
	}
}

// TestPeerPolicyRefusesClusterAddresses covers the peer policy: a relay must not
// be usable to reach the cluster's own addresses.
func TestPeerPolicyRefusesClusterAddresses(t *testing.T) {
	const name = "e2e-peer-policy"

	ctx := context.Background()

	// The cluster IP of the kubernetes service: reachable from the relay node,
	// and outside the relay's job.
	clusterIP := kubernetesServiceIP(ctx, t)

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 1,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51400, End: 51410},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34784},
			},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{
				DeniedCIDRs: []string{"10.0.0.0/8", "169.254.0.0/16", "127.0.0.0/8"},
			},
		},
	})

	status := waitForRelays(ctx, t, name, 1)
	address, port := relayListenerHostPort(t, &status.Status.Relays[0])

	username, password := probe.Credentials(sharedSecret, probe.DefaultUser, probe.DefaultCredentialTTL, time.Now())
	portRange := relay.PortRange{Start: status.Status.Relays[0].PortRange.Start, End: status.Status.Relays[0].PortRange.End}
	result := probe.Allocate(context.Background(), &probe.Config{
		Endpoint:        probe.Endpoint{Host: address, Port: port, Transport: probe.TransportUDP},
		Username:        username,
		Password:        password,
		Timeout:         5 * time.Second,
		ExpectAddress:   address,
		ExpectPortRange: &portRange,
		PeerAddress:     net.JoinHostPort(clusterIP, "443"),
	})

	if result.Succeeded {
		t.Fatalf("the relay relayed to the cluster IP %s, which the peer policy denies", clusterIP)
	}
	if !strings.Contains(result.Message, "permission") {
		t.Errorf("failure does not name the refused permission: %s", result.Message)
	}
}

// TestScaleDownDrainsRelayWithAnActiveAllocation covers the drain contract: a
// relay that is going away leaves the URI list while it still serves, and its pod
// survives until the allocation is released.
func TestScaleDownDrainsRelayWithAnActiveAllocation(t *testing.T) {
	const name = "e2e-drain"

	ctx := context.Background()

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 2,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51500, End: 51520},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34785},
			},
			DrainTimeout: &metav1.Duration{Duration: 2 * time.Minute},
			Probe:        &turnfleetv1alpha1.ProbeSpec{Interval: &metav1.Duration{Duration: 2 * time.Second}},
			PeerPolicy:   &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	status := waitForRelays(ctx, t, name, 2)

	// The planner fills relays node by node, so scaling down removes the last
	// relay: the one on the second node.
	var doomed turnfleetv1alpha1.RelayStatus
	for _, relayStatus := range status.Status.Relays {
		if relayStatus.NodeName != nil && *relayStatus.NodeName == relayNode("-worker2") {
			doomed = relayStatus
		}
	}
	if doomed.Name == "" {
		t.Fatalf("no relay is on %s: %v", relayNode("-worker2"), status.Status.Relays)
	}
	doomedAddress, doomedPort := relayListenerHostPort(t, &doomed)
	doomedRange := relay.PortRange{Start: doomed.PortRange.Start, End: doomed.PortRange.End}

	// A client is using the relay that is about to be removed.
	release := holdAllocation(t, doomedAddress, doomedPort)
	allocated := probeRelay(t, doomedAddress, doomedPort, doomedRange)
	if allocated.Succeeded {
		// The probe allocated a port as well; release it immediately so the
		// allocation count comes back down.
		t.Log("probe allocated on the doomed relay; the held allocation is what matters")
	}

	scale(ctx, t, name, 1)

	waitFor(ctx, t, "the drained relay to leave turnURIs", 2*time.Minute, func(ctx context.Context) (bool, error) {
		current := pool(ctx, t, name)
		for _, uri := range current.Status.TurnURIs {
			if strings.Contains(uri, doomedAddress) {
				return false, nil
			}
		}
		return len(current.Status.TurnURIs) > 0, nil
	})

	if len(relayPods(ctx, t, name)) != 2 {
		t.Fatal("the draining relay's pod was deleted while it still held an allocation")
	}
	draining := relayStatus(ctx, t, name, doomed.Name)
	if draining.Draining == nil || !*draining.Draining {
		t.Error("the relay is not reported as draining")
	}
	if draining.Ready != nil && *draining.Ready {
		t.Error("a draining relay is still reported ready")
	}

	// Releasing the allocation lets the drain finish.
	release()

	waitFor(ctx, t, "the drained relay's pod to be deleted", 2*time.Minute, func(ctx context.Context) (bool, error) {
		return len(relayPods(ctx, t, name)) == 1, nil
	})

	final := pool(ctx, t, name)
	if final.Status.ReadyRelays == nil || *final.Status.ReadyRelays != 1 {
		t.Errorf("readyRelays = %v, want 1", final.Status.ReadyRelays)
	}
}

// TestProbeCLIFromTheHost covers the operator-facing tool: the same check the
// controller runs, from outside the cluster, with JSON it can be parsed from.
func TestProbeCLIFromTheHost(t *testing.T) {
	const name = "e2e-probe-cli"

	ctx := context.Background()

	if _, err := os.Stat(probeBinary); err != nil {
		t.Fatalf("the probe binary %s is missing: run 'make build' first (%v)", probeBinary, err)
	}

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 1,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51600, End: 51610},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34786},
			},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	status := waitForRelays(ctx, t, name, 1)
	relayStatus := status.Status.Relays[0]
	address, port := relayListenerHostPort(t, &relayStatus)

	result, elapsed := hostProbe(ctx, t, fmt.Sprintf("turn:%s:%d?transport=udp", address, port),
		"--expect-address", address,
		"--expect-port-range", fmt.Sprintf("%d-%d", relayStatus.PortRange.Start, relayStatus.PortRange.End),
	)

	if !result.Succeeded {
		t.Fatalf("turnfleet-probe failed from the host: %s", result.Message)
	}
	if result.RelayedAddress != address {
		t.Errorf("probe reported relayed address %q, want %q", result.RelayedAddress, address)
	}
	if result.RelayedPort < relayStatus.PortRange.Start || result.RelayedPort > relayStatus.PortRange.End {
		t.Errorf("probe reported relayed port %d, outside %d-%d",
			result.RelayedPort, relayStatus.PortRange.Start, relayStatus.PortRange.End)
	}
	// LatencyMillis is truncated to whole milliseconds, so a relay on the same
	// host as the probe legitimately reports 0 and asserting more than that
	// would fail whenever the allocation is quick. What is worth checking is
	// that the probe did not claim more time than its own process took.
	if result.LatencyMillis > elapsed.Milliseconds() {
		t.Errorf("probe reported latency %dms, more than the %dms the turnfleet-probe process took",
			result.LatencyMillis, elapsed.Milliseconds())
	}
	if result.URI == "" {
		t.Error("probe reported no URI")
	}
}

// conditionTrue reports whether a pool's condition is True.
func conditionTrue(ctx context.Context, t *testing.T, poolName, conditionType string) bool {
	t.Helper()

	current := pool(ctx, t, poolName)
	if current.Status == nil {
		return false
	}
	for _, condition := range current.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionTrue
		}
	}
	return false
}

// scale changes a pool's replica count.
func scale(ctx context.Context, t *testing.T, name string, replicas int32) {
	t.Helper()

	current := pool(ctx, t, name)
	current.Spec.Replicas = replicas
	if err := k8sClient.Update(ctx, current); err != nil {
		t.Fatalf("scaling %s to %d: %v", name, replicas, err)
	}
}

// secondaryAddress returns an address in the kind bridge network that is not
// assigned to any node, for the external address case.
func secondaryAddress(index int) string {
	return fmt.Sprintf("172.18.255.%d", 100+index)
}

// attachSecondaryAddress adds an address to a kind node container and removes it
// again when the test finishes.
func attachSecondaryAddress(t *testing.T, node, address string) {
	t.Helper()

	script := filepath.Join(repoRoot(t), "test", "fixtures", "attach-secondary-address.sh")
	runCommand(t, script, node, address, "add")
	t.Cleanup(func() { runCommand(t, script, node, address, "del") })
}

// repoRoot finds the module root from the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("finding the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}

func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()

	//nolint:gosec // the fixture script and its arguments are the test's own.
	cmd := exec.CommandContext(context.Background(), name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}

// kubernetesServiceIP is the cluster IP of the kubernetes service, the most
// obviously in-cluster address a relay must not be able to reach.
func kubernetesServiceIP(ctx context.Context, t *testing.T) string {
	t.Helper()

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, service); err != nil {
		t.Fatalf("reading the kubernetes service: %v", err)
	}
	return service.Spec.ClusterIP
}

// exhaustRelay holds every port in a relay's range with real TURN allocations.
// The allocations are released when the returned function is called.
func exhaustRelay(t *testing.T, address string, port int32, portRange relay.PortRange) func() {
	t.Helper()

	allocations := make([]net.PacketConn, 0, portRange.Ports())
	clients := make([]*turnClient, 0, portRange.Ports())
	deadline := time.Now().Add(20 * time.Second)

	// len() is bounded by a pool's port range, which the API caps well below
	// math.MaxInt32.
	//nolint:gosec // a count of held allocations cannot overflow int32.
	for int32(len(allocations)) < portRange.Ports() {
		session := dialTurn(t, address, port)
		packetConn, err := session.allocate(t.Context())
		if err != nil {
			session.close()
			if time.Now().After(deadline) {
				t.Fatalf("could not exhaust the relay after %d allocations: %v", len(allocations), err)
			}
			continue
		}
		allocations = append(allocations, packetConn)
		clients = append(clients, session)
		if time.Now().After(deadline) {
			t.Fatalf("exhausting the relay took too long: %d of %d ports held", len(allocations), portRange.Ports())
		}
	}

	return func() {
		for _, allocation := range allocations {
			_ = allocation.Close()
		}
		for _, client := range clients {
			client.close()
		}
	}
}

// holdAllocation holds one allocation on a relay until the returned function is
// called.
func holdAllocation(t *testing.T, address string, port int32) func() {
	t.Helper()

	session := dialTurn(t, address, port)
	allocation, err := session.allocate(context.Background())
	if err != nil {
		session.close()
		t.Fatalf("holding an allocation on %s:%d: %v", address, port, err)
	}
	return func() {
		_ = allocation.Close()
		session.close()
	}
}

// controllerMetrics reads the operator's own metrics through the API server's
// service proxy.
//
// The suite runs on the host and a kind pod address is not routable from there,
// so the read goes through the API server. That is also why the e2e overlay
// serves metrics over plain HTTP: one request, no port-forward, no long-lived
// process.
func controllerMetrics(ctx context.Context, t *testing.T) string {
	t.Helper()

	path := fmt.Sprintf("/api/v1/namespaces/%s/services/http:%s:8443/proxy/metrics",
		controllerNamespace, metricsServiceName)
	return kubectl(ctx, t, "get", "--raw", path)
}
