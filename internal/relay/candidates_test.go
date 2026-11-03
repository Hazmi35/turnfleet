// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// detectingInputs are relays whose bind address the node decides: the node has
// an InternalIP and a different ExternalIP, and did not say which it holds.
func detectingInputs() map[string]RenderInput {
	return map[string]RenderInput{
		"hostnetwork-detect": {
			Relay: Relay{
				Name:             "demo-relay-b3c4d",
				Identity:         "13.212.26.172|49152-49199",
				NodeName:         "worker-1",
				Address:          "13.212.26.172",
				BindAddress:      "10.42.0.134",
				LocalBindAddress: "13.212.26.172",
				PortRange:        PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          renderSpec(turnfleetv1alpha1.AddressingModeHostNetwork),
			DeniedCIDRs:   DefaultDeniedCIDRs(),
			AuthSecret:    testSecret,
		},
		"hostnetwork-detect-override": {
			Relay: Relay{
				Name:             "demo-relay-c4d5e",
				Identity:         "198.51.100.77|49152-49199",
				NodeName:         "worker-1",
				Address:          "198.51.100.77",
				BindAddress:      "10.42.0.134",
				LocalBindAddress: "13.212.26.172",
				PortRange:        PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          renderSpec(turnfleetv1alpha1.AddressingModeHostNetwork),
			DeniedCIDRs:   DefaultDeniedCIDRs(),
			AuthSecret:    testSecret,
		},
		"hostnetwork-v6-detect": {
			Relay: Relay{
				Name:             "demo-relay-d5e6f",
				Identity:         "2001:db8::11|49152-49199",
				NodeName:         "worker-1",
				Address:          "2001:db8::11",
				BindAddress:      "fd00::11",
				LocalBindAddress: "2001:db8::11",
				PortRange:        PortRange{Start: 49152, End: 49199},
			},
			PoolNamespace: "default",
			PoolName:      "demo",
			Spec:          v6Spec(turnfleetv1alpha1.AddressingModeHostNetwork),
			DeniedCIDRs:   DefaultDeniedCIDRsV6(),
			AuthSecret:    testSecret,
		},
	}
}

// TestRenderCandidatesGoldenFiles pins both configurations of a relay that
// decides its bind address on the node: one for a node that holds the address
// (no external-ip), one for a node behind 1:1 NAT (external-ip maps the
// advertised address to the private one).
func TestRenderCandidatesGoldenFiles(t *testing.T) {
	t.Parallel()

	for name, input := range detectingInputs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := RenderCandidates(&input)
			if err != nil {
				t.Fatalf("RenderCandidates() failed: %v", err)
			}
			for variant, config := range map[string]string{"local": got.Local, "mapped": got.Mapped} {
				golden := filepath.Join("testdata", name+"-"+variant+".conf")
				if *update {
					if err := os.WriteFile(golden, []byte(config), 0o600); err != nil {
						t.Fatalf("writing golden file: %v", err)
					}
					continue
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("reading golden file (run `go test ./internal/relay -update` to create it): %v", err)
				}
				if config != string(want) {
					t.Errorf("rendered %s configuration differs from %s:\n--- want ---\n%s\n--- got ---\n%s",
						variant, golden, want, config)
				}
			}
		})
	}
}

// TestRenderCandidatesBindWhatTheNodeHolds checks the property the two
// variants exist for, without the golden files: the held variant binds the
// local address and maps nothing, the mapped variant binds the private address
// and maps the advertised one, and both listen on loopback for the probe and
// bind the relay sockets to the same address as the listener.
func TestRenderCandidatesBindWhatTheNodeHolds(t *testing.T) {
	t.Parallel()

	input := detectingInputs()["hostnetwork-detect"]
	got, err := RenderCandidates(&input)
	if err != nil {
		t.Fatalf("RenderCandidates() failed: %v", err)
	}

	tests := []struct {
		name   string
		config string
		want   []string
		absent []string
	}{
		{
			name:   "held",
			config: got.Local,
			want:   []string{"listening-ip=13.212.26.172\n", "listening-ip=127.0.0.1\n", "relay-ip=13.212.26.172\n"},
			absent: []string{"external-ip=", "10.42.0.134"},
		},
		{
			name:   "mapped",
			config: got.Mapped,
			want: []string{
				"listening-ip=10.42.0.134\n", "listening-ip=127.0.0.1\n", "relay-ip=10.42.0.134\n",
				"external-ip=13.212.26.172/10.42.0.134\n",
			},
			absent: []string{"listening-ip=13.212.26.172", "relay-ip=13.212.26.172"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, want := range tc.want {
				if !strings.Contains(tc.config, want) {
					t.Errorf("configuration lacks %q:\n%s", want, tc.config)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(tc.config, absent) {
					t.Errorf("configuration has %q, which this variant must not:\n%s", absent, tc.config)
				}
			}
		})
	}
}

// TestRenderedCandidatesFlagsExistInPinnedCoturn keeps the candidates inside the
// options the pinned image has, like the other renderings.
func TestRenderedCandidatesFlagsExistInPinnedCoturn(t *testing.T) {
	t.Parallel()

	help, err := os.ReadFile(filepath.Join("testdata", "coturn-4.18.0-r0-help.txt"))
	if err != nil {
		t.Fatalf("reading the captured coturn help output: %v", err)
	}

	for name, input := range detectingInputs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := RenderCandidates(&input)
			if err != nil {
				t.Fatalf("RenderCandidates() failed: %v", err)
			}
			for _, config := range []string{got.Local, got.Mapped} {
				for _, option := range configOptions(config) {
					if !strings.Contains(string(help), "--"+option) {
						t.Errorf("rendered option %q does not exist in coturn 4.18.0-r0", option)
					}
				}
			}
		})
	}
}

// TestRenderSeparatesDetectingFromPlainRelays pins that a relay that decides
// its bind address on the node is never rendered as a single configuration, and
// a relay that does not is never rendered as candidates: one would bind an
// address the node may not hold, the other would carry a loopback listener no
// other relay has.
func TestRenderSeparatesDetectingFromPlainRelays(t *testing.T) {
	t.Parallel()

	detecting := detectingInputs()["hostnetwork-detect"]
	if _, err := Render(&detecting); err == nil {
		t.Error("Render() accepted a relay whose bind address the node decides")
	}

	plain := goldenInputs(t)["hostnetwork"]
	if _, err := RenderCandidates(&plain); err == nil {
		t.Error("RenderCandidates() accepted a relay with a known bind address")
	}
}

// TestRenderCandidatesRejectsAMixedFamily covers a local address of another
// family than the advertised one: coturn maps within one family only.
func TestRenderCandidatesRejectsAMixedFamily(t *testing.T) {
	t.Parallel()

	input := detectingInputs()["hostnetwork-detect"]
	input.Relay.LocalBindAddress = "2001:db8::11"
	if _, err := RenderCandidates(&input); err == nil {
		t.Error("RenderCandidates() accepted a local address of another family than the advertised one")
	}
}

// TestListenerProbeAddress covers the address a readiness probe connects to.
func TestListenerProbeAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		relay Relay
		want  string
	}{
		{
			name:  "a known bind address is probed",
			relay: Relay{Address: "203.0.113.11", BindAddress: "10.0.0.11"},
			want:  "10.0.0.11",
		},
		{
			name:  "an IPv4 relay that detects its bind is probed on loopback",
			relay: Relay{Address: "203.0.113.11", BindAddress: "10.0.0.11", LocalBindAddress: "203.0.113.11"},
			want:  LoopbackV4,
		},
		{
			name:  "an IPv6 relay that detects its bind is probed on loopback",
			relay: Relay{Address: "2001:db8::11", BindAddress: "fd00::11", LocalBindAddress: "2001:db8::11"},
			want:  LoopbackV6,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.relay.ListenerProbeAddress(); got != tc.want {
				t.Errorf("ListenerProbeAddress() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestComputePlanCarriesTheDetectedBind covers the two modes that plan on node
// addresses: the relay keeps the node's advertised address as its identity and
// carries both candidate bind addresses, whichever one the node ends up
// holding.
func TestComputePlanCarriesTheDetectedBind(t *testing.T) {
	t.Parallel()

	nat := NodeAddress{Address: "13.212.26.172", BindAddress: "10.42.0.134", LocalBindAddress: "13.212.26.172"}
	node := Node{Name: "worker-1", Addresses: []NodeAddress{nat}}

	tests := []struct {
		name       string
		addressing turnfleetv1alpha1.AddressingSpec
		replicas   int32
	}{
		{
			name:       "HostNetwork",
			addressing: turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModeHostNetwork},
			replicas:   1,
		},
		{
			name: "PartitionedIP",
			addressing: turnfleetv1alpha1.AddressingSpec{
				Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
				PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
			},
			replicas: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := baseSpec()
			spec.Replicas = tc.replicas
			spec.Addressing = tc.addressing
			plan := ComputePlan(&PlanInput{PoolName: "demo", Spec: spec, Nodes: []Node{node}})
			if len(plan.Relays) != int(tc.replicas) {
				t.Fatalf("placed %d relays, want %d", len(plan.Relays), tc.replicas)
			}
			for _, r := range plan.Relays {
				if r.Address != nat.Address || r.BindAddress != nat.BindAddress || r.LocalBindAddress != nat.LocalBindAddress {
					t.Errorf("relay %s = address %s, bind %s, local bind %s, want the node's %+v",
						r.Name, r.Address, r.BindAddress, r.LocalBindAddress, nat)
				}
				if !r.DetectsBind() {
					t.Errorf("relay %s does not detect its bind address", r.Name)
				}
				if want := identity(nat.Address, r.PortRange); r.Identity != want {
					t.Errorf("relay %s has identity %q, want %q: the identity is the advertised address and the ports",
						r.Name, r.Identity, want)
				}
			}
		})
	}
}
