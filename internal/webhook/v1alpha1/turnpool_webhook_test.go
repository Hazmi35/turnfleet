// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package v1alpha1

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

const testImage = "coturn/coturn:4.18.0-r0@sha256:" +
	"bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3"

// secretRef builds the SecretKeySelector a pool's auth section needs.
func secretRef(name, key string) *corev1.SecretKeySelector {
	ref := &corev1.SecretKeySelector{}
	ref.Name = name
	ref.Key = key
	return ref
}

// validPool returns a pool that passes every rule, so each test can change
// exactly one thing.
func validPool() *turnfleetv1alpha1.TURNPool {
	pool := &turnfleetv1alpha1.TURNPool{
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas: 2,
			Dataplane: turnfleetv1alpha1.DataplaneSpec{
				Image: testImage,
			},
			Addressing: turnfleetv1alpha1.AddressingSpec{
				Mode: turnfleetv1alpha1.AddressingModeHostNetwork,
			},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 49152, End: 49300},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
				{Name: "turn-tcp", Protocol: turnfleetv1alpha1.ListenerProtocolTCP, Port: 3478},
			},
			Auth: turnfleetv1alpha1.AuthSpec{
				Realm:     "turnfleet.example.com",
				SecretRef: secretRef("turn-secret", "secret"),
			},
		},
	}
	pool.Name = "test"
	pool.Namespace = "default"
	return pool
}

func TestDefaulter(t *testing.T) {
	t.Parallel()

	defaulter := &TURNPoolDefaulter{}
	pool := validPool()
	pool.Spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{}

	if err := defaulter.Default(context.Background(), pool); err != nil {
		t.Fatalf("Default() returned an error: %v", err)
	}

	if pool.Spec.PeerPolicy == nil {
		t.Fatal("Default() did not create peerPolicy")
	}
	want := relay.DefaultDeniedCIDRs()
	if len(pool.Spec.PeerPolicy.DeniedCIDRs) != len(want) {
		t.Errorf("deniedCIDRs = %v, want the %d default entries %v", pool.Spec.PeerPolicy.DeniedCIDRs, len(want), want)
	}
	if pool.Spec.DrainTimeout == nil || pool.Spec.DrainTimeout.Duration != relay.DefaultDrainTimeout {
		t.Errorf("drainTimeout = %v, want %v", pool.Spec.DrainTimeout, relay.DefaultDrainTimeout)
	}
	if pool.Spec.Probe == nil || pool.Spec.Probe.Interval == nil || pool.Spec.Probe.Interval.Duration != relay.DefaultProbeInterval {
		t.Errorf("probe interval = %v, want %v", pool.Spec.Probe, relay.DefaultProbeInterval)
	}
	if pool.Spec.Probe.Timeout == nil || pool.Spec.Probe.Timeout.Duration != relay.DefaultProbeTimeout {
		t.Errorf("probe timeout = %v, want %v", pool.Spec.Probe.Timeout, relay.DefaultProbeTimeout)
	}
	if got := pool.Spec.Addressing.PartitionedIP.RelaysPerNode; got != relay.DefaultRelaysPerNode {
		t.Errorf("relaysPerNode = %d, want %d", got, relay.DefaultRelaysPerNode)
	}
	if got := pool.Spec.Addressing.Family; got != turnfleetv1alpha1.AddressingFamilyIPv4 {
		t.Errorf("family = %q, want %q: a pool that does not name one is an IPv4 pool", got, turnfleetv1alpha1.AddressingFamilyIPv4)
	}
}

// TestDefaulterDenyListFollowsTheFamily pins the pairing that keeps an IPv6 pool
// from starting with a deny list that can never match: the ranges come from the
// family the relays use.
func TestDefaulterDenyListFollowsTheFamily(t *testing.T) {
	t.Parallel()

	defaulter := &TURNPoolDefaulter{}
	for _, tc := range []turnfleetv1alpha1.AddressingFamily{
		turnfleetv1alpha1.AddressingFamilyIPv4,
		turnfleetv1alpha1.AddressingFamilyIPv6,
		turnfleetv1alpha1.AddressingFamilyDualStack,
	} {
		pool := validPool()
		pool.Spec.Addressing.Family = tc
		if err := defaulter.Default(context.Background(), pool); err != nil {
			t.Fatalf("Default() returned an error for family %s: %v", tc, err)
		}
		want := relay.DefaultDeniedCIDRsFor(tc)
		if got := pool.Spec.PeerPolicy.DeniedCIDRs; !slices.Equal(got, want) {
			t.Errorf("deniedCIDRs for family %s = %v, want %v", tc, got, want)
		}
		// Whatever the family, the defaults must be ranges the pool can use,
		// which is what the validating webhook checks next.
		if errs := validatePeerPolicy(&pool.Spec, field.NewPath("spec").Child("peerPolicy")); len(errs) > 0 {
			t.Errorf("the default deny list for family %s is rejected by the validator: %v", tc, errs)
		}
	}
}

// TestDefaulterKeepsExplicitValues makes sure defaulting never overwrites what
// an operator set: a pool with its own deny list must keep it.
func TestDefaulterKeepsExplicitValues(t *testing.T) {
	t.Parallel()

	defaulter := &TURNPoolDefaulter{}
	pool := validPool()
	pool.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: []string{"203.0.113.0/24"}}
	pool.Spec.DrainTimeout = &metav1.Duration{Duration: 5 * time.Minute}
	pool.Spec.Probe = &turnfleetv1alpha1.ProbeSpec{Interval: &metav1.Duration{Duration: time.Minute}}

	if err := defaulter.Default(context.Background(), pool); err != nil {
		t.Fatalf("Default() returned an error: %v", err)
	}

	if len(pool.Spec.PeerPolicy.DeniedCIDRs) != 1 || pool.Spec.PeerPolicy.DeniedCIDRs[0] != "203.0.113.0/24" {
		t.Errorf("deniedCIDRs = %v, want the operator's single entry", pool.Spec.PeerPolicy.DeniedCIDRs)
	}
	if pool.Spec.DrainTimeout.Duration != 5*time.Minute {
		t.Errorf("drainTimeout = %v, want 5m", pool.Spec.DrainTimeout.Duration)
	}
	if pool.Spec.Probe.Interval.Duration != time.Minute {
		t.Errorf("probe interval = %v, want 1m", pool.Spec.Probe.Interval.Duration)
	}
	if pool.Spec.Probe.Timeout == nil {
		t.Error("probe timeout was left unset; defaulting must fill in the missing half")
	}
}

func TestValidateCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*turnfleetv1alpha1.TURNPool)
		wantErr bool
		// wantField is the field path the error must name, checked as a prefix.
		wantField string
	}{
		{
			name:   "valid HostNetwork pool",
			mutate: func(*turnfleetv1alpha1.TURNPool) {},
		},
		{
			name: "valid ExternalIPPool pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{
							{NodeName: "worker-1", Address: "203.0.113.10"},
							{NodeName: "worker-2", Address: "203.0.113.11"},
						},
					},
				}
			},
		},
		{
			name: "valid PartitionedIP pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
					PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 4},
				}
			},
		},
		{
			name: "valid PodIP pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:  turnfleetv1alpha1.AddressingModePodIP,
					PodIP: &turnfleetv1alpha1.PodIPSpec{AttachmentRef: "relay-pods"},
				}
			},
		},
		{
			name: "valid PodIP pool without an attachment",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				// No podIP block at all: the relay binds the pod's own address
				// on the default network, and the addresses are declared on the
				// relay nodes.
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModePodIP}
			},
		},
		{
			name: "podIP on a HostNetwork pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.PodIP = &turnfleetv1alpha1.PodIPSpec{AttachmentRef: "relay-pods"}
			},
			wantErr:   true,
			wantField: "spec.addressing.podIP",
		},
		{
			name: "externalIPPool on a PodIP pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:  turnfleetv1alpha1.AddressingModePodIP,
					PodIP: &turnfleetv1alpha1.PodIPSpec{},
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{
							{NodeName: "worker-1", Address: "203.0.113.10"},
						},
					},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.externalIPPool",
		},
		{
			name: "partitionedIP on a PodIP pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:          turnfleetv1alpha1.AddressingModePodIP,
					PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.partitionedIP",
		},
		{
			name: "valid IPv6 pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:   turnfleetv1alpha1.AddressingModeHostNetwork,
					Family: turnfleetv1alpha1.AddressingFamilyIPv6,
				}
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{
					DeniedCIDRs: relay.DefaultDeniedCIDRsV6(),
				}
			},
		},
		{
			name: "valid DualStack ExternalIPPool pool",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:   turnfleetv1alpha1.AddressingModeExternalIPPool,
					Family: turnfleetv1alpha1.AddressingFamilyDualStack,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{
							{NodeName: "worker-1", Address: "203.0.113.10"},
							{NodeName: "worker-2", Address: "2001:db8::10"},
						},
					},
				}
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{
					DeniedCIDRs: relay.DefaultDeniedCIDRsFor(turnfleetv1alpha1.AddressingFamilyDualStack),
				}
			},
		},
		{
			name: "IPv6 pool with an IPv4 declared address",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:   turnfleetv1alpha1.AddressingModeExternalIPPool,
					Family: turnfleetv1alpha1.AddressingFamilyIPv6,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{
							{NodeName: "worker-1", Address: "203.0.113.10"},
							{NodeName: "worker-2", Address: "2001:db8::10"},
						},
					},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.externalIPPool.addresses[0].address",
		},
		{
			name: "DualStack pool declaring one family only",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:   turnfleetv1alpha1.AddressingModeExternalIPPool,
					Family: turnfleetv1alpha1.AddressingFamilyDualStack,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{
							{NodeName: "worker-1", Address: "203.0.113.10"},
							{NodeName: "worker-2", Address: "203.0.113.11"},
						},
					},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.externalIPPool.addresses",
		},
		{
			name: "IPv4 pool with an IPv6 peer range",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: []string{"fc00::/7"}}
			},
			wantErr:   true,
			wantField: "spec.peerPolicy.deniedCIDRs[0]",
		},
		{
			name: "IPv6 pool with an IPv4 allowed range",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyIPv6
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{AllowedCIDRs: []string{"203.0.113.0/24"}}
			},
			wantErr:   true,
			wantField: "spec.peerPolicy.allowedCIDRs[0]",
		},
		{
			name: "port range end before start",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 49300, End: 49152}
			},
			wantErr:   true,
			wantField: "spec.relayPortRange",
		},
		{
			name: "port range missing",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{}
			},
			wantErr:   true,
			wantField: "spec.relayPortRange",
		},
		{
			name: "partitioned pool with too few ports per relay",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
					PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 8},
				}
				p.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 49152, End: 49156}
			},
			wantErr:   true,
			wantField: "spec.relayPortRange",
		},
		{
			name: "listener port inside the relay range",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Listeners[0].Port = 49160
			},
			wantErr:   true,
			wantField: "spec.listeners[0].port",
		},
		{
			name: "duplicate listener protocol",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Listeners = append(p.Spec.Listeners, turnfleetv1alpha1.ListenerSpec{
					Name: "turn-extra", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3479,
				})
			},
			wantErr:   true,
			wantField: "spec.listeners[2].protocol",
		},
		{
			name: "udp and tcp listeners on different ports",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Listeners[1].Port = 3479
			},
			wantErr:   true,
			wantField: "spec.listeners[1].port",
		},
		{
			name: "tls listener without a secret",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Listeners = append(p.Spec.Listeners, turnfleetv1alpha1.ListenerSpec{
					Name: "turns", Protocol: turnfleetv1alpha1.ListenerProtocolTLS, Port: 5349,
				})
			},
			wantErr:   true,
			wantField: "spec.listeners[2].tlsSecretRef",
		},
		{
			name: "plain listener with a certificate",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Listeners[0].TLSSecretRef = &corev1.LocalObjectReference{Name: "cert"}
			},
			wantErr:   true,
			wantField: "spec.listeners[0].tlsSecretRef",
		},
		{
			name: "mode-specific config for another mode",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2}
			},
			wantErr:   true,
			wantField: "spec.addressing.partitionedIP",
		},
		{
			name: "ExternalIPPool without addresses",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:           turnfleetv1alpha1.AddressingModeExternalIPPool,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.externalIPPool.addresses",
		},
		{
			name: "more replicas than addresses",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{{NodeName: "worker-1", Address: "203.0.113.10"}},
					},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.externalIPPool.addresses",
		},
		{
			name: "ExternalIPPool with an address that is not an IP",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
					ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
						Addresses: []turnfleetv1alpha1.ExternalAddress{
							{NodeName: "worker-1", Address: "turnfleet.example.net"},
							{NodeName: "worker-2", Address: "203.0.113.11"},
						},
					},
				}
			},
			wantErr:   true,
			wantField: "spec.addressing.externalIPPool.addresses[0].address",
		},
		{
			name: "PartitionedIP without relaysPerNode",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModePartitionedIP}
			},
			wantErr:   true,
			wantField: "spec.addressing.partitionedIP",
		},
		{
			name: "unknown addressing mode",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.Mode = "SomethingElse"
			},
			wantErr:   true,
			wantField: "spec.addressing.mode",
		},
		{
			name: "image without a digest",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Dataplane.Image = "coturn/coturn:4.18.0-r0"
			},
			wantErr:   true,
			wantField: "spec.dataplane.image",
		},
		{
			name: "image missing",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Dataplane.Image = ""
			},
			wantErr:   true,
			wantField: "spec.dataplane.image",
		},
		{
			name: "realm missing",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Auth.Realm = ""
			},
			wantErr:   true,
			wantField: "spec.auth.realm",
		},
		{
			name: "secret key missing",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Auth.SecretRef.Key = ""
			},
			wantErr:   true,
			wantField: "spec.auth.secretRef.key",
		},
		{
			name: "peer CIDR that is not a CIDR",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: []string{"10.0.0.1"}}
			},
			wantErr:   true,
			wantField: "spec.peerPolicy.deniedCIDRs[0]",
		},
		{
			name: "allowed peer CIDR that is empty",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{AllowedCIDRs: []string{" "}}
			},
			wantErr:   true,
			wantField: "spec.peerPolicy.allowedCIDRs[0]",
		},
		{
			name: "partitioned listener ports run into the relay range",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
					PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
				}
				// The second relay on the shared address would serve 3479,
				// which is inside the pool's relay port range.
				p.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 3478, End: 3480}
			},
			wantErr:   true,
			wantField: "spec.listeners[0].port",
		},
		{
			name: "partitioned listener ports run past the last port",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
					Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
					PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
				}
				p.Spec.Listeners[0].Port = 65535
				p.Spec.Listeners[1].Port = 65535
			},
			wantErr:   true,
			wantField: "spec.listeners[0].port",
		},
		{
			name: "probe timeout longer than the interval",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Probe = &turnfleetv1alpha1.ProbeSpec{
					Interval: &metav1.Duration{Duration: time.Second},
					Timeout:  &metav1.Duration{Duration: 10 * time.Second},
				}
			},
			wantErr:   true,
			wantField: "spec.probe.timeout",
		},
		{
			name: "drain timeout is negative",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.DrainTimeout = &metav1.Duration{Duration: -time.Second}
			},
			wantErr:   true,
			wantField: "spec.drainTimeout",
		},
	}

	validator := &TURNPoolValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool := validPool()
			tc.mutate(pool)

			_, err := validator.ValidateCreate(context.Background(), pool)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ValidateCreate() admitted an invalid pool")
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateCreate() rejected a valid pool: %v", err)
			case tc.wantErr:
				var statusErr *apierrors.StatusError
				if !asStatusError(err, &statusErr) {
					t.Fatalf("ValidateCreate() error is not an API status error: %v", err)
				}
				if len(statusErr.ErrStatus.Details.Causes) == 0 {
					t.Fatalf("ValidateCreate() error has no causes: %v", err)
				}
				cause := statusErr.ErrStatus.Details.Causes[0].Field
				if !strings.HasPrefix(cause, tc.wantField) {
					t.Errorf("ValidateCreate() reported field %q, want it to start with %q (all: %v)",
						cause, tc.wantField, causeFields(statusErr))
				}
			}
		})
	}
}

// TestValidateUpdateImmutability covers the fields that cannot move without
// stranding live allocations, and the ones that must stay changeable so a pool
// can still be scaled and rotated.
func TestValidateUpdateImmutability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*turnfleetv1alpha1.TURNPool)
		wantErr   bool
		wantField string
	}{
		{
			name:    "replicas may change",
			mutate:  func(p *turnfleetv1alpha1.TURNPool) { p.Spec.Replicas = 7 },
			wantErr: false,
		},
		{
			name: "listeners may be added",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Listeners = append(p.Spec.Listeners, turnfleetv1alpha1.ListenerSpec{
					Name: "turns", Protocol: turnfleetv1alpha1.ListenerProtocolTLS, Port: 5349,
					TLSSecretRef: &corev1.LocalObjectReference{Name: "tls-cert"},
				})
			},
			wantErr: false,
		},
		{
			name: "the peer policy may change",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: []string{"203.0.113.0/24"}}
			},
			wantErr: false,
		},
		{
			name: "the shared secret may rotate",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Auth.SecretRef = secretRef("turn-secret-v2", "secret")
			},
			wantErr: false,
		},
		{
			name: "the drain timeout may change",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.DrainTimeout = &metav1.Duration{Duration: time.Minute}
			},
			wantErr: false,
		},
		{
			name: "the addressing mode is immutable",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModeHostNetwork}
			},
			wantErr:   true,
			wantField: "spec.addressing.mode",
		},
		{
			name: "the address family is immutable",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyDualStack
			},
			wantErr:   true,
			wantField: "spec.addressing.family",
		},
		{
			name: "unset and IPv4 are the same family",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.Family = turnfleetv1alpha1.AddressingFamilyIPv4
			},
			wantErr: false,
		},
		{
			name: "the relay port range is immutable",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.RelayPortRange.End = 49400
			},
			wantErr:   true,
			wantField: "spec.relayPortRange",
		},
		{
			name: "the realm is immutable",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Auth.Realm = "other.example.com"
			},
			wantErr:   true,
			wantField: "spec.auth.realm",
		},
		{
			name: "relaysPerNode is immutable",
			mutate: func(p *turnfleetv1alpha1.TURNPool) {
				p.Spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 3}
			},
			wantErr:   true,
			wantField: "spec.addressing.partitionedIP.relaysPerNode",
		},
	}

	validator := &TURNPoolValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			oldPool := validPool()
			oldPool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
				Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
				PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2},
			}
			newPool := oldPool.DeepCopy()
			// An update always carries a resourceVersion; the webhook does not
			// care, but set it so the object looks like a real update.
			newPool.ResourceVersion = "2"
			tc.mutate(newPool)

			_, err := validator.ValidateUpdate(context.Background(), oldPool, newPool)
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("ValidateUpdate() admitted an immutable change")
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateUpdate() rejected a legitimate change: %v", err)
			case tc.wantErr:
				var statusErr *apierrors.StatusError
				if !asStatusError(err, &statusErr) {
					t.Fatalf("ValidateUpdate() error is not an API status error: %v", err)
				}
				if fields := causeFields(statusErr); !containsField(fields, tc.wantField) {
					t.Errorf("ValidateUpdate() reported %v, want it to include %q", fields, tc.wantField)
				}
			}
		})
	}
}

// TestValidateUpdateKeepsThePodIPAttachment pins the second half of PodIP
// immutability: the mode stays the same on both sides, and only the attachment
// moves. Replacing it changes which address every relay holds, so it strands
// live allocations exactly as changing the mode does.
func TestValidateUpdateKeepsThePodIPAttachment(t *testing.T) {
	t.Parallel()

	validator := &TURNPoolValidator{}
	oldPool := validPool()
	oldPool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode:  turnfleetv1alpha1.AddressingModePodIP,
		PodIP: &turnfleetv1alpha1.PodIPSpec{AttachmentRef: "relay-pods"},
	}

	if _, err := validator.ValidateUpdate(context.Background(), oldPool, oldPool.DeepCopy()); err != nil {
		t.Fatalf("an update that changes nothing was rejected: %v", err)
	}

	changed := oldPool.DeepCopy()
	changed.Spec.Addressing.PodIP.AttachmentRef = "relay-pods-v2"
	_, err := validator.ValidateUpdate(context.Background(), oldPool, changed)
	if err == nil {
		t.Fatal("changing the attachment was admitted: every relay's address would move")
	}
	var statusErr *apierrors.StatusError
	if !asStatusError(err, &statusErr) {
		t.Fatalf("error is not an API status error: %v", err)
	}
	if !containsField(causeFields(statusErr), "spec.addressing.podIP.attachmentRef") {
		t.Errorf("error names %v, want spec.addressing.podIP.attachmentRef", causeFields(statusErr))
	}
}

func TestValidatePortRangePartitioning(t *testing.T) {
	t.Parallel()

	// The number of ports must cover every relay that shares the address, and
	// the boundary is inclusive.
	tests := []struct {
		name      string
		start     int32
		end       int32
		relays    int32
		wantErr   bool
		wantField string
	}{
		{name: "exactly enough ports", start: 49152, end: 49153, relays: 2},
		{name: "one port short", start: 49152, end: 49152, relays: 2, wantErr: true, wantField: "spec.relayPortRange"},
		{name: "single relay", start: 50000, end: 50000, relays: 1},
		{name: "wide range", start: 20000, end: 65535, relays: 64},
	}

	validator := &TURNPoolValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool := validPool()
			pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
				Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
				PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: tc.relays},
			}
			pool.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: tc.start, End: tc.end}

			_, err := validator.ValidateCreate(context.Background(), pool)
			if tc.wantErr && err == nil {
				t.Fatal("ValidateCreate() admitted a range that cannot be partitioned")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateCreate() rejected a valid range: %v", err)
			}
		})
	}
}

// TestValidateDeleteIsUnconditional documents that deletion is gated by the
// drain finalizer, not by admission: refusing deletion would leave a pool that
// can never be removed when its nodes are gone.
func TestValidateDeleteIsUnconditional(t *testing.T) {
	t.Parallel()

	validator := &TURNPoolValidator{}
	warnings, err := validator.ValidateDelete(context.Background(), validPool())
	if err != nil {
		t.Fatalf("ValidateDelete() returned an error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("ValidateDelete() returned warnings: %v", warnings)
	}
}

// TestValidateSpecMatchesPathRoot keeps the paths the webhook reports in step
// with the API: a change to the field names should make this fail loudly rather
// than silently produce errors pointing at nothing.
func TestValidateSpecMatchesPathRoot(t *testing.T) {
	t.Parallel()

	pool := validPool()
	pool.Spec.Listeners[0].Port = 49160
	errs := validateSpec(&pool.Spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("validateSpec() found no error for a listener inside the relay range")
	}
	if got := errs[0].Field; got != "spec.listeners[0].port" {
		t.Errorf("field = %q, want %q", got, "spec.listeners[0].port")
	}
}

func asStatusError(err error, target **apierrors.StatusError) bool {
	return errors.As(err, target)
}

func causeFields(err *apierrors.StatusError) []string {
	fields := make([]string, 0, len(err.ErrStatus.Details.Causes))
	for _, cause := range err.ErrStatus.Details.Causes {
		fields = append(fields, cause.Field)
	}
	return fields
}

func containsField(fields []string, want string) bool {
	return slices.Contains(fields, want)
}
