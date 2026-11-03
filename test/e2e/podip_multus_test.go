//go:build e2e

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package e2e

import (
	"context"
	"encoding/binary"
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
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// multusReachTimeout bounds the wait for the relay to answer from the host once
// the pool reports it ready: the controller probes from inside the cluster, and
// the host reaches the pod's secondary address through the node's bridge, which
// has to learn the new interface first.
const multusReachTimeout = time.Minute

// podIPMultusAttachment is the fixture's name (test/fixtures/podip-attachment.yaml).
const podIPMultusAttachment = "podip-macvlan"

// TestPodIPRelayOnAMultusAttachment covers the PodIP data path against a real
// secondary interface. The relay node declares an address on the kind network,
// the pool names a Multus attachment (macvlan with static IPAM), and the relay
// pod holds that address on net1 while its default-network address stays
// another one. The case shows three things:
//
//   - the relay advertises the attachment's address, in its status, in
//     status.turnURIs and in the <pool>-turn-uris ConfigMap, and never the
//     node's InternalIP;
//   - the relay answers a real TURN Allocate, and its XOR-RELAYED-ADDRESS is
//     the attachment's address;
//   - both are observed from the test host, outside the cluster's network,
//     with the probe CLI and the probe package, where ADR 7 says reachability
//     is decided, and not only from a pod.
//
// What kind proves is the mechanism: the declared address reaches the pod
// through the CNI's "ips" request and comes back through the network-status
// read-back. It does not prove a cloud's IPAM (azure-ipam, SR-IOV), which this
// attachment replaces with the static plugin.
func TestPodIPRelayOnAMultusAttachment(t *testing.T) {
	const name = "e2e-podip-multus"

	ctx := context.Background()

	if _, err := os.Stat(probeBinary); err != nil {
		t.Fatalf("the probe binary %s is missing: run 'make build' first (%v)", probeBinary, err)
	}

	node := relayNode("-worker3")
	nodeAddress := nodeInternalIPv4(ctx, t, node)
	plan, reason := planAttachment(t, nodeAddress)
	if reason != "" {
		// The host has to reach the secondary address, or the check that
		// matters (ADR 7) cannot run. Asserting the in-cluster path alone
		// would claim more than this host can show.
		t.Skipf("the data path cannot be probed from this host: %s", reason)
	}
	attachmentAddress := plan.address

	requireMultus(ctx, t)

	// Cleanups run last in, first out: the pool goes first, then the shim, the
	// node's declaration and the attachment, so nothing is deleted under a pod
	// that still uses it.
	applyFixture(t, "podip-attachment.yaml")
	annotateNode(t, node, relay.PodAddressesAnnotation, plan.declaration)
	attachMacvlanShim(t, node, plan)

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 1,
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode:  turnfleetv1alpha1.AddressingModePodIP,
				PodIP: turnfleetv1alpha1.PodIPSpec{AttachmentRef: podIPMultusAttachment},
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51900, End: 51920},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34789},
			},
		},
	})

	// A failure logs what the pool, its pods, the attachment and the controller
	// were doing. This runs before the pool is deleted.
	t.Cleanup(func() {
		if t.Failed() {
			dumpPoolState(t, name)
			dumpAttachmentState(t, name)
		}
	})

	status := waitForRelays(ctx, t, name, 1)
	if len(status.Status.Relays) != 1 {
		t.Fatalf("pool has %d relays, want 1: %v", len(status.Status.Relays), status.Status.Relays)
	}
	relayStatus := status.Status.Relays[0]

	requireAttachmentAddressAdvertised(ctx, t, name, &relayStatus, attachmentAddress, nodeAddress)
	requireSecondaryInterface(ctx, t, name, attachmentAddress)

	host, port := relayListenerHostPort(t, &relayStatus)
	portRange := relay.PortRange{Start: relayStatus.PortRange.Start, End: relayStatus.PortRange.End}

	// The relay is read from outside the cluster: this process runs on the
	// host, and the address is on the kind network, not behind a pod network.
	var allocated probe.Result
	waitFor(ctx, t, "the relay to answer a TURN Allocate from the host", multusReachTimeout, func(ctx context.Context) (bool, error) {
		username, password := probe.Credentials(sharedSecret, probe.DefaultUser, probe.DefaultCredentialTTL, time.Now())
		allocated = probe.Allocate(ctx, &probe.Config{
			Endpoint:        probe.Endpoint{Host: host, Port: port, Transport: probe.TransportUDP},
			Username:        username,
			Password:        password,
			Timeout:         5 * time.Second,
			ExpectAddress:   attachmentAddress,
			ExpectPortRange: &portRange,
		})
		return allocated.Succeeded, nil
	})
	if allocated.RelayedAddress != attachmentAddress {
		t.Errorf("the probe package reports XOR-RELAYED-ADDRESS %q, want the attachment's %s", allocated.RelayedAddress, attachmentAddress)
	}

	// The operator-facing tool reports the same, as a consumer reads it.
	uri := fmt.Sprintf("turn:%s:%d?transport=udp", host, port)
	cli, _ := hostProbe(ctx, t, uri,
		"--expect-address", attachmentAddress,
		"--expect-port-range", fmt.Sprintf("%d-%d", relayStatus.PortRange.Start, relayStatus.PortRange.End),
	)
	if !cli.Succeeded {
		t.Fatalf("turnfleet-probe failed from the host against %s: %s", uri, cli.Message)
	}
	if cli.RelayedAddress != attachmentAddress {
		t.Errorf("turnfleet-probe reports relayed address %q, want the attachment's %s", cli.RelayedAddress, attachmentAddress)
	}
	if cli.RelayedAddress == nodeAddress {
		t.Errorf("turnfleet-probe reports the node's InternalIP %s as the relayed address", nodeAddress)
	}
	if cli.RelayedPort < relayStatus.PortRange.Start || cli.RelayedPort > relayStatus.PortRange.End {
		t.Errorf("turnfleet-probe reports relayed port %d, outside %d-%d",
			cli.RelayedPort, relayStatus.PortRange.Start, relayStatus.PortRange.End)
	}

	// The relay runs in the pod's network namespace, so the node's address
	// does not answer on the listener port: advertising it would be the
	// failure this mode exists to remove.
	username, password := probe.Credentials(sharedSecret, probe.DefaultUser, probe.DefaultCredentialTTL, time.Now())
	onNode := probe.Allocate(ctx, &probe.Config{
		Endpoint: probe.Endpoint{Host: nodeAddress, Port: port, Transport: probe.TransportUDP},
		Username: username,
		Password: password,
		Timeout:  3 * time.Second,
	})
	if onNode.Succeeded {
		t.Errorf("the node's InternalIP %s:%d answered a TURN Allocate, so the relay is not on the attachment", nodeAddress, port)
	}
}

// requireAttachmentAddressAdvertised checks every place the pool publishes the
// relay: the relay's status, the pool's turnURIs and the URI ConfigMap the
// credential issuer reads. Each carries the attachment's address and none the
// node's InternalIP.
func requireAttachmentAddressAdvertised(ctx context.Context, t *testing.T, poolName string, status *turnfleetv1alpha1.RelayStatus, attachmentAddress, nodeAddress string) {
	t.Helper()

	if status.RelayAddress == nil || *status.RelayAddress != attachmentAddress {
		t.Errorf("the relay reports address %v, want the attachment's %s", status.RelayAddress, attachmentAddress)
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
			if !strings.HasPrefix(uri, "turn:"+attachmentAddress+":") {
				t.Errorf("%s has %q, want a URI on the attachment's address %s", where, uri, attachmentAddress)
			}
			if strings.Contains(uri, nodeAddress) {
				t.Errorf("%s has %q, which is the node's InternalIP %s", where, uri, nodeAddress)
			}
		}
	}
}

// requireSecondaryInterface checks that the address the relay advertises is a
// secondary one: the pod has a default-network address that differs from it,
// and Multus reports the attachment's address for the interface the relay
// binds.
func requireSecondaryInterface(ctx context.Context, t *testing.T, poolName, attachmentAddress string) {
	t.Helper()

	pods := relayPods(ctx, t, poolName)
	if len(pods) != 1 {
		t.Fatalf("pool %s has %d relay pods, want 1", poolName, len(pods))
	}
	pod := &pods[0]
	if pod.Spec.HostNetwork {
		t.Errorf("relay pod %s uses hostNetwork, so it cannot hold an address of its own", pod.Name)
	}
	if pod.Status.PodIP == "" || pod.Status.PodIP == attachmentAddress {
		t.Errorf("relay pod %s has default-network address %q, want one that differs from the attachment's %s",
			pod.Name, pod.Status.PodIP, attachmentAddress)
	}
	networkStatus := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
	if !strings.Contains(networkStatus, `"`+attachmentAddress+`"`) {
		t.Errorf("relay pod %s network-status does not list %s: %s", pod.Name, attachmentAddress, networkStatus)
	}
}

// nodeInternalIPv4 reads the IPv4 InternalIP of a node: the address a
// hostNetwork relay would fall back to, and the one this case must not see.
func nodeInternalIPv4(ctx context.Context, t *testing.T, nodeName string) string {
	t.Helper()

	node := &corev1.Node{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		t.Fatalf("reading node %s: %v", nodeName, err)
	}
	for _, address := range node.Status.Addresses {
		if address.Type != corev1.NodeInternalIP {
			continue
		}
		if ip := net.ParseIP(address.Address); ip != nil && ip.To4() != nil {
			return address.Address
		}
	}
	t.Fatalf("node %s has no IPv4 InternalIP: %v", nodeName, node.Status.Addresses)
	return ""
}

// attachmentPlan is what the case reserves on the kind network: the address
// the relay pod holds on its attachment, the address of the node's own macvlan
// shim (see attachMacvlanShim), and the node's declaration of the first, with
// the network's prefix length.
type attachmentPlan struct {
	address     string
	shimAddress string
	declaration string
}

// planAttachment picks the addresses the case needs on the network the host
// reaches the node through, because the kind subnet differs between hosts
// (Docker's is usually 172.18.0.0/16, Podman's 10.89.0.0/24).
//
// The network is found from the host's own interface that holds an address in
// the node's subnet (the bridge kind creates). The addresses are taken from
// the top of that subnet, twenty and twenty-one below the broadcast address,
// where the container runtime's allocator, which hands out from the bottom,
// does not reach, and they are checked against the host's own address and the
// node's. A non-empty reason says why the host cannot do this, which skips the
// case.
func planAttachment(t *testing.T, nodeAddress string) (plan attachmentPlan, reason string) {
	t.Helper()

	node := net.ParseIP(nodeAddress).To4()
	interfaceAddresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("listing the host's addresses: %v", err)
	}
	for _, interfaceAddress := range interfaceAddresses {
		subnet, ok := interfaceAddress.(*net.IPNet)
		if !ok || subnet.IP.To4() == nil || !subnet.Contains(node) {
			continue
		}
		ones, bits := subnet.Mask.Size()
		if bits != 32 || ones > 26 {
			return attachmentPlan{}, fmt.Sprintf("the host's network %s around the node is too small to reserve unused addresses", subnet)
		}
		// The last host address of the network is 1<<(32-ones) - 2 above its
		// network address.
		base := binary.BigEndian.Uint32(subnet.IP.Mask(subnet.Mask).To4())
		below := func(n uint32) net.IP {
			address := make(net.IP, net.IPv4len)
			binary.BigEndian.PutUint32(address, base+uint32(1)<<(32-ones)-1-n)
			return address
		}
		address, shim := below(20), below(21)
		for _, taken := range []net.IP{node, subnet.IP} {
			if address.Equal(taken) || shim.Equal(taken) {
				return attachmentPlan{}, fmt.Sprintf("%s or %s is already in use on %s", address, shim, subnet)
			}
		}
		return attachmentPlan{
			address:     address.String(),
			shimAddress: shim.String(),
			declaration: fmt.Sprintf("%s/%d", address, ones),
		}, ""
	}
	return attachmentPlan{}, fmt.Sprintf("none of the host's interfaces has an address in the network of node address %s, so the host has no route to a secondary address there", nodeAddress)
}

// attachMacvlanShim lets the node reach the relay pod's macvlan address, and
// removes the shim when the test finishes. A macvlan child and its parent
// cannot talk to each other, so the controller, which may run on the relay's
// node, could not probe the relay without it
// (test/fixtures/macvlan-shim.sh).
func attachMacvlanShim(t *testing.T, node string, plan attachmentPlan) {
	t.Helper()

	script := filepath.Join(repoRoot(t), "test", "fixtures", "macvlan-shim.sh")
	runCommand(t, script, node, plan.shimAddress, plan.address, "add")
	t.Cleanup(func() { runCommand(t, script, node, plan.shimAddress, plan.address, "del") })
}

// requireMultus fails with the fix when the cluster has no Multus: it was
// created before `make kind-up` installed it.
func requireMultus(ctx context.Context, t *testing.T) {
	t.Helper()

	if out, err := kubectlOutput(ctx, "get", "crd", "network-attachment-definitions.k8s.cni.cncf.io"); err != nil {
		t.Fatalf("the cluster has no Multus NetworkAttachmentDefinition CRD: run 'make kind-up', which installs Multus and the CNI plugins (%v)\n%s", err, out)
	}
}

// applyFixture applies a file from test/fixtures and deletes what it created
// when the test finishes.
func applyFixture(t *testing.T, file string) {
	t.Helper()

	path := filepath.Join(repoRoot(t), "test", "fixtures", file)
	kubectl(context.Background(), t, "apply", "-f", path)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), kubectlTimeout)
		defer cancel()
		if out, err := kubectlOutput(ctx, "delete", "--ignore-not-found", "-f", path); err != nil {
			t.Errorf("deleting fixture %s: %v\n%s", file, err, out)
		}
	})
}

// dumpAttachmentState logs what explains a relay that never got its
// secondary address: the attachment, the relay pods' events and Multus' own log.
func dumpAttachmentState(t *testing.T, poolName string) {
	t.Helper()

	for _, args := range [][]string{
		{"-n", testNamespace, "get", "network-attachment-definitions.k8s.cni.cncf.io", "-o", "yaml"},
		{"-n", testNamespace, "get", "pods", "-l", "app.kubernetes.io/instance=" + poolName, "-o", "wide"},
		{"-n", testNamespace, "get", "events", "--sort-by=.lastTimestamp"},
		{"-n", "kube-system", "logs", "daemonset/kube-multus-ds", "--tail=100"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), kubectlTimeout)
		output, err := kubectlOutput(ctx, args...)
		cancel()
		t.Logf("kubectl %s (error: %v):\n%s", strings.Join(args, " "), err, output)
	}
}

// kubectlOutput runs kubectl against the kind cluster and returns its output
// and the error, for the callers that decide what a failure means.
func kubectlOutput(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--context", kubeContext}, args...)
	//nolint:gosec // the suite execs the tools it documents; the arguments come from the tests, not from untrusted input.
	output, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	return string(output), err
}
