// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// update rewrites the golden files: `go test ./internal/relay -update`.
var update = flag.Bool("update", false, "rewrite the golden files")

const testSecret = "0123456789abcdef0123456789abcdef"

func renderSpec(mode turnfleetv1alpha1.AddressingMode) turnfleetv1alpha1.TURNPoolSpec {
	spec := turnfleetv1alpha1.TURNPoolSpec{
		Replicas: 2,
		Dataplane: turnfleetv1alpha1.DataplaneSpec{
			Image: "coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3",
		},
		Addressing:     turnfleetv1alpha1.AddressingSpec{Mode: mode},
		RelayPortRange: turnfleetv1alpha1.PortRange{Start: 49152, End: 49199},
		Listeners: []turnfleetv1alpha1.ListenerSpec{
			{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
		},
		Auth: turnfleetv1alpha1.AuthSpec{Realm: "turnfleet.example.com"},
	}
	switch mode {
	case turnfleetv1alpha1.AddressingModePartitionedIP:
		spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 4}
	case turnfleetv1alpha1.AddressingModeExternalIPPool:
		spec.Addressing.ExternalIPPool = &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{
				{NodeName: "worker-1", Address: "198.51.100.11"},
			},
		}
	case turnfleetv1alpha1.AddressingModePodIP:
		spec.Addressing.PodIP = turnfleetv1alpha1.PodIPSpec{AttachmentRef: "relay-pods"}
	case turnfleetv1alpha1.AddressingModeHostNetwork:
	}
	return spec
}

// v6Spec is the same pool, planned on IPv6 addresses.
func v6Spec(mode turnfleetv1alpha1.AddressingMode) turnfleetv1alpha1.TURNPoolSpec {
	spec := renderSpec(mode)
	spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyIPv6
	return spec
}

// externalV6Spec declares an IPv6 secondary address, which is what an IPv6
// ExternalIPPool pool plans on.
func externalV6Spec() turnfleetv1alpha1.TURNPoolSpec {
	spec := v6Spec(turnfleetv1alpha1.AddressingModeExternalIPPool)
	spec.Addressing.ExternalIPPool = &turnfleetv1alpha1.ExternalIPPoolSpec{
		Addresses: []turnfleetv1alpha1.ExternalAddress{
			{NodeName: "worker-1", Address: "2001:db8:100::11"},
		},
	}
	return spec
}

func goldenInputs(t *testing.T) map[string]RenderInput {
	t.Helper()

	hostnetwork := renderSpec(turnfleetv1alpha1.AddressingModeHostNetwork)
	partitioned := renderSpec(turnfleetv1alpha1.AddressingModePartitionedIP)
	external := renderSpec(turnfleetv1alpha1.AddressingModeExternalIPPool)
	podip := renderSpec(turnfleetv1alpha1.AddressingModePodIP)

	tlsSpec := renderSpec(turnfleetv1alpha1.AddressingModeHostNetwork)
	tlsSpec.Listeners = []turnfleetv1alpha1.ListenerSpec{
		{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
		{Name: "turn-tcp", Protocol: turnfleetv1alpha1.ListenerProtocolTCP, Port: 3478},
		{Name: "turns", Protocol: turnfleetv1alpha1.ListenerProtocolTLS, Port: 5349, TLSSecretRef: &corev1.LocalObjectReference{Name: "turnfleet-tls"}},
	}

	denied := DefaultDeniedCIDRs()
	deniedV6 := DefaultDeniedCIDRsV6()

	return map[string]RenderInput{
		"hostnetwork": {
			Relay: Relay{
				Name:        "demo-relay-a1b2c",
				Identity:    "203.0.113.10|49152-49199",
				NodeName:    "worker-1",
				Address:     "203.0.113.10",
				BindAddress: "203.0.113.10",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          hostnetwork,
			DeniedCIDRs:   denied,
			AllowedCIDRs:  []string{"203.0.113.0/24"},
			AuthSecret:    testSecret,
		},
		"partitionedip": {
			Relay: Relay{
				Name:        "demo-relay-d4e5f",
				Identity:    "10.0.0.11|49164-49175",
				NodeName:    "worker-1",
				Address:     "10.0.0.11",
				BindAddress: "10.0.0.11",
				PortRange:   PortRange{Start: 49164, End: 49175},
				// The second relay on this shared address: its listener port is
				// offset so it does not collide with the first relay's, and its
				// metrics port is offset for the same reason.
				ListenerPortOffset: 1,
				MetricsPortOffset:  1,
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          partitioned,
			DeniedCIDRs:   denied,
			AuthSecret:    testSecret,
		},
		"externalippool": {
			Relay: Relay{
				Name:        "demo-relay-0a1b2",
				Identity:    "198.51.100.11|49152-49199",
				NodeName:    "worker-1",
				Address:     "198.51.100.11",
				BindAddress: "198.51.100.11",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "turnfleet",
			PoolName:      "demo",
			Spec:          external,
			DeniedCIDRs:   denied,
			AuthSecret:    testSecret,
		},
		"podip": {
			// A PodIP relay binds the address the pod holds and advertises it:
			// there is no public/private mapping, because the address is the
			// pod's own.
			Relay: Relay{
				Name:        "demo-relay-5e6f7",
				Identity:    "203.0.113.21|49152-49199",
				NodeName:    "worker-1",
				Address:     "203.0.113.21",
				BindAddress: "203.0.113.21",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          podip,
			DeniedCIDRs:   denied,
			AuthSecret:    testSecret,
		},
		"podip-v6": {
			Relay: Relay{
				Name:        "demo-relay-6f7a8",
				Identity:    "2001:db8:100::21|49152-49199",
				NodeName:    "worker-1",
				Address:     "2001:db8:100::21",
				BindAddress: "2001:db8:100::21",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          v6Spec(turnfleetv1alpha1.AddressingModePodIP),
			DeniedCIDRs:   deniedV6,
			AuthSecret:    testSecret,
		},
		"hostnetwork-nat": {
			// The advertised address is a public address that is not local: the
			// renderer must emit coturn's public/private mapping.
			Relay: Relay{
				Name:        "demo-relay-f6a7b",
				Identity:    "203.0.113.77|49152-49199",
				NodeName:    "worker-1",
				Address:     "203.0.113.77",
				BindAddress: "10.0.0.11",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          hostnetwork,
			DeniedCIDRs:   denied,
			AuthSecret:    testSecret,
		},
		"hostnetwork-tls": {
			Relay: Relay{
				Name:        "demo-relay-c8d9e",
				Identity:    "203.0.113.10|49152-49199",
				NodeName:    "worker-1",
				Address:     "203.0.113.10",
				BindAddress: "203.0.113.10",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          tlsSpec,
			DeniedCIDRs:   denied,
			AuthSecret:    testSecret,
		},
		// One golden per addressing mode for an IPv6 relay, plus the NAT case:
		// the same flags with IPv6 values, and the IPv6 peer deny list.
		"hostnetwork-v6": {
			Relay: Relay{
				Name:        "demo-relay-1a2b3",
				Identity:    "2001:db8::10|49152-49199",
				NodeName:    "worker-1",
				Address:     "2001:db8::10",
				BindAddress: "2001:db8::10",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          v6Spec(turnfleetv1alpha1.AddressingModeHostNetwork),
			DeniedCIDRs:   deniedV6,
			AllowedCIDRs:  []string{"2001:db8:100::/48"},
			AuthSecret:    testSecret,
		},
		"partitionedip-v6": {
			Relay: Relay{
				Name:        "demo-relay-2b3c4",
				Identity:    "2001:db8::11|49164-49175",
				NodeName:    "worker-1",
				Address:     "2001:db8::11",
				BindAddress: "2001:db8::11",
				PortRange:   PortRange{Start: 49164, End: 49175},
				// The second relay on this address: the listener port and the
				// metrics port are both offset, because one address can only be
				// listened on once and coturn's metrics endpoint binds every
				// address on the node.
				ListenerPortOffset: 1,
				MetricsPortOffset:  1,
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          v6Spec(turnfleetv1alpha1.AddressingModePartitionedIP),
			DeniedCIDRs:   deniedV6,
			AuthSecret:    testSecret,
		},
		"externalippool-v6": {
			Relay: Relay{
				Name:        "demo-relay-3c4d5",
				Identity:    "2001:db8:100::11|49152-49199",
				NodeName:    "worker-1",
				Address:     "2001:db8:100::11",
				BindAddress: "2001:db8:100::11",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "turnfleet",
			PoolName:      "demo",
			Spec:          externalV6Spec(),
			DeniedCIDRs:   deniedV6,
			AuthSecret:    testSecret,
		},
		"hostnetwork-v6-nat": {
			// A public IPv6 address that is not local: coturn's external-ip
			// mapping takes "public/private" for IPv6 as it does for IPv4, and
			// reports the public address to the allocations of its own family.
			Relay: Relay{
				Name:        "demo-relay-4d5e6",
				Identity:    "2001:db8:ffff::77|49152-49199",
				NodeName:    "worker-1",
				Address:     "2001:db8:ffff::77",
				BindAddress: "fd00::11",
				PortRange:   PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          v6Spec(turnfleetv1alpha1.AddressingModeHostNetwork),
			DeniedCIDRs:   deniedV6,
			AuthSecret:    testSecret,
		},
	}
}

// TestRenderGoldenFiles pins the rendered coturn configuration per addressing
// mode and per listener set. A change to the configuration is a change to the
// data plane, so it must be visible in a diff.
func TestRenderGoldenFiles(t *testing.T) {
	t.Parallel()

	for name, input := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := Render(&input)
			if err != nil {
				t.Fatalf("Render() failed: %v", err)
			}
			golden := filepath.Join("testdata", name+".conf")

			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatalf("writing golden file: %v", err)
				}
				return
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading golden file (run `go test ./internal/relay -update` to create it): %v", err)
			}
			if got != string(want) {
				t.Errorf("rendered configuration differs from %s:\n--- want ---\n%s\n--- got ---\n%s", golden, want, got)
			}
		})
	}
}

// TestRenderIsDeterministic matters because the rendered configuration is
// hashed into the pod's config-hash annotation: a renderer that is not
// deterministic would roll every relay on every reconcile.
func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()

	for name, input := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			first, err := Render(&input)
			if err != nil {
				t.Fatalf("Render() failed: %v", err)
			}
			for range 5 {
				again, err := Render(&input)
				if err != nil {
					t.Fatalf("Render() failed: %v", err)
				}
				if again != first {
					t.Fatal("Render() is not deterministic: the config hash would churn and roll relays forever")
				}
				if ConfigHash(again) != ConfigHash(first) {
					t.Fatal("ConfigHash() is not deterministic")
				}
			}
		})
	}
}

// TestRenderedFlagsExistInPinnedCoturn is the guard that keeps the renderer
// honest about the data plane it configures: every option it emits must appear
// in the `turnserver --help` output of the pinned image, captured in testdata.
// A flag that does not exist there would either be ignored or abort coturn.
func TestRenderedFlagsExistInPinnedCoturn(t *testing.T) {
	t.Parallel()

	help, err := os.ReadFile(filepath.Join("testdata", "coturn-4.18.0-r0-help.txt"))
	if err != nil {
		t.Fatalf("reading the captured coturn help output: %v", err)
	}
	helpText := string(help)

	for name, input := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			config, err := Render(&input)
			if err != nil {
				t.Fatalf("Render() failed: %v", err)
			}
			for _, option := range configOptions(config) {
				if !strings.Contains(helpText, "--"+option) {
					t.Errorf("rendered option %q does not exist in coturn 4.18.0-r0: "+
						"re-capture testdata/coturn-4.18.0-r0-help.txt from the pinned image before adding it", option)
				}
			}
		})
	}
}

// configOptions extracts the option names from a rendered configuration.
func configOptions(config string) []string {
	var options []string
	for line := range strings.SplitSeq(config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, "=")
		options = append(options, strings.TrimSpace(name))
	}
	return options
}

// TestRenderRejectsUnusableInput covers the cases where rendering would produce
// a relay that cannot serve: better to refuse than to write a configuration
// whose capacity is a lie.
func TestRenderRejectsUnusableInput(t *testing.T) {
	t.Parallel()

	base := goldenInputs(t)["hostnetwork"]

	tests := []struct {
		name   string
		mutate func(*RenderInput)
	}{
		{"no relay address", func(in *RenderInput) { in.Relay.Address = "" }},
		{"no bind address", func(in *RenderInput) { in.Relay.BindAddress = "" }},
		{"no auth secret", func(in *RenderInput) { in.AuthSecret = "" }},
		{"no realm", func(in *RenderInput) { in.Spec.Auth.Realm = "" }},
		{"empty port range", func(in *RenderInput) { in.Relay.PortRange = PortRange{Start: 10, End: 9} }},
		{"unparseable denied CIDR", func(in *RenderInput) { in.DeniedCIDRs = []string{"not-a-cidr"} }},
		{"unparseable allowed CIDR", func(in *RenderInput) { in.AllowedCIDRs = []string{"10.0.0.0/33"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			input := base
			tc.mutate(&input)
			if _, err := Render(&input); err == nil {
				t.Fatal("Render() accepted input that cannot produce a working relay")
			}
		})
	}
}

// TestRenderPodIPBindsTheAddressThePodHolds pins the mode's data plane: the
// relay listens on and advertises the address the pod holds, and coturn's
// public/private mapping is not emitted, because there is nothing to map — the
// address is local to the pod.
func TestRenderPodIPBindsTheAddressThePodHolds(t *testing.T) {
	t.Parallel()

	input := goldenInputs(t)["podip"]
	config, err := Render(&input)
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	if !strings.Contains(config, "\nlistening-ip="+input.Relay.Address+"\n") {
		t.Errorf("the relay does not listen on its own address %s:\n%s", input.Relay.Address, config)
	}
	if !strings.Contains(config, "\nrelay-ip="+input.Relay.Address+"\n") {
		t.Errorf("the relay does not relay from its own address %s:\n%s", input.Relay.Address, config)
	}
	if strings.Contains(config, "external-ip=") {
		t.Errorf("a PodIP relay advertises the address it holds, so it must not render external-ip:\n%s", config)
	}
	// The deny list is the SSRF guard and stays in force for this mode too.
	if !strings.Contains(config, "denied-peer-ip=") {
		t.Errorf("the peer deny list is missing from a PodIP relay:\n%s", config)
	}
}

// TestConfigHashFollowsTheAddress is the property that keeps a relay from being
// recreated for nothing: the rendered configuration, and therefore the config
// hash, changes when the address changes and not when nothing does.
func TestConfigHashFollowsTheAddress(t *testing.T) {
	t.Parallel()

	first := goldenInputs(t)["podip"]
	second := first
	second.Relay.Address = "203.0.113.22"
	second.Relay.BindAddress = "203.0.113.22"
	second.Relay.Identity = "203.0.113.22|49152-49199"

	firstConfig, err := Render(&first)
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}
	secondConfig, err := Render(&second)
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	if ConfigHash(firstConfig) == ConfigHash(secondConfig) {
		t.Error("the config hash is the same for two different addresses: an address change would not roll the relay")
	}
}

// TestRenderNoTLSListenersDisablesTLS documents the interaction with coturn:
// without a tls listener there is no certificate, and coturn aborts if a
// tls-listening-port is configured without one.
func TestRenderNoTLSListenersDisablesTLS(t *testing.T) {
	t.Parallel()

	input := goldenInputs(t)["hostnetwork"]
	config, err := Render(&input)
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}
	if !strings.Contains(config, "\nno-tls\n") {
		t.Error("a pool without a tls listener must render no-tls")
	}
	if strings.Contains(config, "tls-listening-port") {
		t.Error("a pool without a tls listener must not configure a tls port")
	}
	if strings.Contains(config, "cert=") {
		t.Error("a pool without a tls listener must not reference a certificate")
	}
}

// TestRenderKeepsTCPRelaysAvailable documents the capacity decision behind not
// disabling TCP relay endpoints: coturn allocates both UDP and TCP relay ports
// from the configured min-port/max-port range (turnports_create builds the UDP
// and TCP maps from the same bounds), so the range bounds capacity either way,
// and a tls listener would be useless without TCP relays.
func TestRenderKeepsTCPRelaysAvailable(t *testing.T) {
	t.Parallel()

	input := goldenInputs(t)["hostnetwork"]
	config, err := Render(&input)
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}
	if strings.Contains(config, "no-tcp-relay") {
		t.Error("TCP relay endpoints must stay available: their ports come from the same range")
	}
}
