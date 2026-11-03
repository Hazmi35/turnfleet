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
	"k8s.io/apimachinery/pkg/types"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/controller"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// natReachTimeout bounds the wait for the relay to answer on its advertised
// address once the pool reports it ready: the host's translation and the
// bridge's neighbour entries have to settle.
const natReachTimeout = time.Minute

// natAdvertisedAddress is the public address of the case. It is a documentation
// address (RFC 5737) outside the kind network that no other case uses, and no
// kind node holds it: the host translates it (test/fixtures/nat-standin.sh).
const natAdvertisedAddress = "203.0.113.120"

// TestExternalIPPoolBindAddress covers an ExternalIPPool entry that declares the
// local address a relay binds apart from the public address it advertises, the
// shape of an Elastic IP: the node holds a private address and the network
// translates a public one to it.
//
// The node holds the private address, a secondary one attached to a kind node.
// The host stands in for the cloud's NAT with a 1:1 translation of the public
// address to it (test/fixtures/nat-standin.sh). No node holds the public
// address, so a relay that bound it, as an entry without bindAddress does,
// could not start. The case shows:
//
//   - the relay binds the private address, which the pod's annotation and the
//     rendered configuration record, and advertises the public one in its
//     status, in status.turnURIs and in the <pool>-turn-uris ConfigMap;
//   - a real TURN Allocate from the host, which is outside the cluster's
//     network, to the public address gets the public address back in
//     XOR-RELAYED-ADDRESS, through the probe package and through the
//     turnfleet-probe CLI.
//
// What kind proves is the mechanism: coturn's `external-ip=public/private`
// mapping and the controller's handling of the pair. It does not prove a cloud's
// NAT, nor attaching the address to the instance, which stays the operator's.
func TestExternalIPPoolBindAddress(t *testing.T) {
	const name = "e2e-pool-nat"

	ctx := context.Background()

	if _, err := os.Stat(probeBinary); err != nil {
		t.Fatalf("the probe binary %s is missing: run 'make build' first (%v)", probeBinary, err)
	}

	node := relayNode("-worker")
	nodeAddress := nodeInternalIPv4(ctx, t, node)
	bridge, reason := natStandInBridge(nodeAddress)
	if reason != "" {
		// Without the host's translation the check that matters (ADR 7, from
		// outside the cluster's network) cannot run, and asserting the
		// configuration alone would claim more than this host can show.
		t.Skipf("the NAT stand-in cannot run on this host: %s", reason)
	}
	private := secondaryAddress(3)
	advertised := natAdvertisedAddress

	// Cleanups run last in, first out: the pool goes first, then the
	// translation and the node's address, so nothing is removed under a relay
	// that still uses it.
	attachSecondaryAddress(t, node, private)
	attachNATStandIn(t, advertised, private, bridge)
	if held := nodeAddresses(t, node); strings.Contains(held, advertised+"/") {
		t.Fatalf("node %s holds %s, so a relay could bind it and the case would prove nothing:\n%s", node, advertised, held)
	}

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 1,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
				ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
					Addresses: []turnfleetv1alpha1.ExternalAddress{
						{NodeName: node, Address: advertised, BindAddress: private},
					},
				},
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51950, End: 51970},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34790},
			},
		},
	})

	// A failure logs what the pool, its pods and the controller were doing.
	// This runs before the pool is deleted.
	t.Cleanup(func() {
		if t.Failed() {
			dumpPoolState(t, name)
		}
	})

	status := waitForRelays(ctx, t, name, 1)
	if len(status.Status.Relays) != 1 {
		t.Fatalf("pool has %d relays, want 1: %v", len(status.Status.Relays), status.Status.Relays)
	}
	relayStatus := status.Status.Relays[0]

	requireNATAddressesPublished(ctx, t, name, &relayStatus, advertised, private)
	requireRelayBindsPrivateAddress(ctx, t, name, advertised, private)

	host, port := relayListenerHostPort(t, &relayStatus)
	portRange := relay.PortRange{Start: relayStatus.PortRange.Start, End: relayStatus.PortRange.End}

	// The relay is read from outside the cluster: this process runs on the
	// host, and the address it dials is the public one.
	var allocated probe.Result
	waitFor(ctx, t, "the relay to answer a TURN Allocate on its advertised address from the host", natReachTimeout, func(ctx context.Context) (bool, error) {
		username, password := probe.Credentials(sharedSecret, probe.DefaultUser, probe.DefaultCredentialTTL, time.Now())
		allocated = probe.Allocate(ctx, &probe.Config{
			Endpoint:        probe.Endpoint{Host: host, Port: port, Transport: probe.TransportUDP},
			Username:        username,
			Password:        password,
			Timeout:         5 * time.Second,
			ExpectAddress:   advertised,
			ExpectPortRange: &portRange,
		})
		return allocated.Succeeded, nil
	})
	if allocated.RelayedAddress != advertised {
		t.Errorf("the probe package reports XOR-RELAYED-ADDRESS %q, want the advertised %s", allocated.RelayedAddress, advertised)
	}

	// The operator-facing tool reports the same, as a consumer reads it.
	uri := fmt.Sprintf("turn:%s:%d?transport=udp", host, port)
	cli, _ := hostProbe(ctx, t, uri,
		"--expect-address", advertised,
		"--expect-port-range", fmt.Sprintf("%d-%d", relayStatus.PortRange.Start, relayStatus.PortRange.End),
	)
	if !cli.Succeeded {
		t.Fatalf("turnfleet-probe failed from the host against %s: %s", uri, cli.Message)
	}
	if cli.RelayedAddress != advertised {
		t.Errorf("turnfleet-probe reports relayed address %q, want the advertised %s", cli.RelayedAddress, advertised)
	}
	if cli.RelayedAddress == private {
		t.Errorf("turnfleet-probe reports the private bind address %s as the relayed address", private)
	}
	if cli.RelayedPort < relayStatus.PortRange.Start || cli.RelayedPort > relayStatus.PortRange.End {
		t.Errorf("turnfleet-probe reports relayed port %d, outside %d-%d",
			cli.RelayedPort, relayStatus.PortRange.Start, relayStatus.PortRange.End)
	}
}

// requireNATAddressesPublished checks every place the pool publishes the
// relay: the relay's status, the pool's turnURIs and the URI ConfigMap the
// credential issuer reads. Each carries the advertised address and none the
// private one a client could not reach.
func requireNATAddressesPublished(ctx context.Context, t *testing.T, poolName string, status *turnfleetv1alpha1.RelayStatus, advertised, private string) {
	t.Helper()

	if status.RelayAddress == nil || *status.RelayAddress != advertised {
		t.Errorf("the relay reports address %v, want the advertised %s", status.RelayAddress, advertised)
	}

	current := pool(ctx, t, poolName)
	if current.Status == nil || len(current.Status.TurnURIs) == 0 {
		t.Fatalf("pool %s publishes no turnURIs although it reports a ready relay", poolName)
	}
	published := map[string][]string{
		"the relay's turnURIs": status.TurnURIs,
		"the pool's turnURIs":  current.Status.TurnURIs,
		"the URI ConfigMap":    strings.Fields(configMapURIs(ctx, t, poolName)),
	}
	for where, uris := range published {
		if len(uris) == 0 {
			t.Errorf("%s lists no URI", where)
		}
		for _, uri := range uris {
			if !strings.HasPrefix(uri, "turn:"+advertised+":") {
				t.Errorf("%s has %q, want a URI on the advertised address %s", where, uri, advertised)
			}
			if strings.Contains(uri, private) {
				t.Errorf("%s has %q, which is the private bind address %s", where, uri, private)
			}
		}
	}
}

// requireRelayBindsPrivateAddress checks what the controller decided for the
// relay: its pod records the private address as the bind address and has no
// init container (an entry declares its bind address, so the node is not asked,
// ADR 23), and its configuration binds the private address and maps the
// advertised one to it.
func requireRelayBindsPrivateAddress(ctx context.Context, t *testing.T, poolName, advertised, private string) {
	t.Helper()

	pods := relayPods(ctx, t, poolName)
	if len(pods) != 1 {
		t.Fatalf("pool %s has %d relay pods, want 1", poolName, len(pods))
	}
	pod := &pods[0]
	if got := pod.Annotations[controller.RelayBindAddressAnnotation]; got != private {
		t.Errorf("relay pod %s records bind address %q, want the entry's bindAddress %s", pod.Name, got, private)
	}
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("relay pod %s has init containers %v, want none: the entry declares the bind address", pod.Name, pod.Spec.InitContainers)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{
		Namespace: testNamespace,
		Name:      controller.RelayConfigSecretName(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}}),
	}
	if err := k8sClient.Get(ctx, key, secret); err != nil {
		t.Fatalf("reading the relay configuration Secret %s: %v", key.Name, err)
	}
	config := string(secret.Data[pod.Name])
	for _, want := range []string{
		"listening-ip=" + private + "\n",
		"relay-ip=" + private + "\n",
		"external-ip=" + advertised + "/" + private + "\n",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("the configuration of %s lacks %q:\n%s", pod.Name, strings.TrimSpace(want), redactSecret(config))
		}
	}
}

// redactSecret drops the shared secret from a configuration before it is
// logged.
func redactSecret(config string) string {
	var kept []string
	for line := range strings.SplitSeq(config, "\n") {
		if !strings.HasPrefix(line, "static-auth-secret=") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// natStandInBridge finds the host interface the kind network hangs off, the
// bridge the host's translation routes the advertised address through. A
// non-empty reason says why the host cannot run the stand-in, which skips the
// case: iptables and a passwordless sudo are what the translation needs, and
// the GitHub-hosted runners have both.
func natStandInBridge(nodeAddress string) (bridge, reason string) {
	if _, err := exec.LookPath("iptables"); err != nil {
		return "", "iptables is not installed"
	}
	if os.Geteuid() != 0 {
		if _, err := exec.LookPath("sudo"); err != nil {
			return "", "this is not root and sudo is not installed"
		}
		if err := exec.CommandContext(context.Background(), "sudo", "-n", "true").Run(); err != nil {
			return "", "this is not root and sudo asks for a password"
		}
	}

	node := net.ParseIP(nodeAddress).To4()
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Sprintf("listing the host's interfaces: %v", err)
	}
	for i := range interfaces {
		addresses, err := interfaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			if subnet, ok := address.(*net.IPNet); ok && subnet.IP.To4() != nil && subnet.Contains(node) {
				return interfaces[i].Name, ""
			}
		}
	}
	return "", fmt.Sprintf("none of the host's interfaces has an address in the network of node address %s", nodeAddress)
}

// attachNATStandIn translates the advertised address to the private one on the
// host and removes the translation when the test finishes
// (test/fixtures/nat-standin.sh).
func attachNATStandIn(t *testing.T, advertised, private, bridge string) {
	t.Helper()

	script := filepath.Join(repoRoot(t), "test", "fixtures", "nat-standin.sh")
	t.Cleanup(func() { runCommand(t, script, advertised, private, bridge, "del") })
	runCommand(t, script, advertised, private, bridge, "add")
}

// nodeAddresses lists the addresses a kind node holds on its interfaces.
func nodeAddresses(t *testing.T, node string) string {
	t.Helper()

	//nolint:gosec // the node name is the suite's own.
	output, err := exec.CommandContext(context.Background(), "docker", "exec", node, "ip", "-o", "addr", "show").CombinedOutput()
	if err != nil {
		t.Fatalf("listing the addresses of node %s: %v\n%s", node, err, output)
	}
	return string(output)
}
