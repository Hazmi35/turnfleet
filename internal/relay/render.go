// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relay

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
)

// Paths inside the relay container.
const (
	// ConfigPath is where the rendered configuration is mounted.
	ConfigPath = "/etc/turnfleet/turnserver.conf"

	// ConfigVolumeName is the volume holding the rendered configuration.
	ConfigVolumeName = "turnfleet-config"

	// TLSMountPath is where a tls listener's Secret is mounted.
	TLSMountPath = "/etc/turnfleet/tls"

	// TLSCertFile and TLSKeyFile are the file names inside the mounted Secret.
	TLSCertFile = "tls.crt"
	TLSKeyFile  = "tls.key"
)

// RenderInput is everything needed to render one relay's configuration.
type RenderInput struct {
	// Relay is the relay being rendered.
	Relay Relay

	// PoolNamespace and PoolName identify the pool, for the comment header.
	PoolNamespace string
	PoolName      string

	// Spec is the pool spec.
	Spec turnfleetv1alpha1.TURNPoolSpec

	// DeniedCIDRs and AllowedCIDRs are the effective peer policy: the pool's
	// own lists plus whatever the controller observed (the pod CIDRs of the
	// relay nodes, for example).
	DeniedCIDRs  []string
	AllowedCIDRs []string

	// AuthSecret is the shared HMAC secret value.
	AuthSecret string
}

// Render produces the coturn configuration file for one relay.
//
// Every flag emitted here exists in the pinned coturn image: the renderer is
// covered by a test that checks each one against the captured
// `turnserver --help` output of that image
// (testdata/coturn-4.18.0-r0-help.txt). Adding a flag means re-capturing that
// file from a new pin.
func Render(in *RenderInput) (string, error) {
	if err := validateRenderInput(in); err != nil {
		return "", err
	}

	plainPort, tlsPort, hasPlain, hasTLS, err := listenerPorts(in.Spec.Listeners)
	if err != nil {
		return "", err
	}

	denied, err := PeerRanges(in.DeniedCIDRs)
	if err != nil {
		return "", fmt.Errorf("rendering denied peers: %w", err)
	}
	allowed, err := PeerRanges(in.AllowedCIDRs)
	if err != nil {
		return "", fmt.Errorf("rendering allowed peers: %w", err)
	}

	var b strings.Builder
	writeHeader(&b, in)
	writeAddressing(&b, in)
	writeListeners(&b, in, plainPort, tlsPort, hasPlain, hasTLS)
	writeCapacity(&b, in)
	writeAuth(&b, in)
	writePeerPolicy(&b, denied, allowed)
	writeObservability(&b)
	writeExtraArgs(&b, in.Spec.Dataplane.ExtraArgs)

	return b.String(), nil
}

// validateRenderInput refuses to render a relay that could not serve: a
// configuration whose capacity is a lie is worse than a failed reconcile.
func validateRenderInput(in *RenderInput) error {
	switch {
	case in.Relay.Address == "":
		return errors.New("relay address is empty: a relay must advertise the address it was planned with")
	case in.Relay.BindAddress == "":
		return errors.New("relay bind address is empty")
	case in.AuthSecret == "":
		return errors.New("auth secret is empty: the relay would refuse every credential")
	case in.Spec.Auth.Realm == "":
		return errors.New("auth realm is empty")
	case in.Relay.PortRange.Ports() <= 0:
		return fmt.Errorf("relay %s has an empty port range %s", in.Relay.Name, in.Relay.PortRange)
	}
	return nil
}

func writeHeader(b *strings.Builder, in *RenderInput) {
	fmt.Fprintf(b, "# Turnfleet relay configuration for pool %s/%s, relay %s.\n",
		in.PoolNamespace, in.PoolName, in.Relay.Name)
	fmt.Fprintf(b, "# identity: %s\n", in.Relay.Identity)
	fmt.Fprintf(b, "# node: %s, relay address: %s, ports: %s\n",
		in.Relay.NodeName, in.Relay.Address, in.Relay.PortRange)
	b.WriteString("# Generated from the TURNPool spec. Do not edit: it is replaced on every rollout.\n")
}

// writeAddressing binds the listener and the relay sockets on the address the
// relay owns, so traffic addressed to (address, port) reaches this process and
// no other.
func writeAddressing(b *strings.Builder, in *RenderInput) {
	fmt.Fprintf(b, "listening-ip=%s\n", in.Relay.BindAddress)
	fmt.Fprintf(b, "relay-ip=%s\n", in.Relay.BindAddress)
	if in.Relay.Address != in.Relay.BindAddress {
		// The advertised address is not local (NAT or an explicit override):
		// coturn has to be told what to put in XOR-RELAYED-ADDRESS.
		fmt.Fprintf(b, "external-ip=%s/%s\n", in.Relay.Address, in.Relay.BindAddress)
	}
}

// writeListeners emits only the transports the pool declared. This matters for
// UDP: coturn serves UDP and TCP on the plain port unless told otherwise.
func writeListeners(b *strings.Builder, in *RenderInput, plainPort, tlsPort int32, hasPlain, hasTLS bool) {
	if hasPlain {
		fmt.Fprintf(b, "listening-port=%d\n", plainPort)
	}
	if hasTLS {
		fmt.Fprintf(b, "tls-listening-port=%d\n", tlsPort)
		fmt.Fprintf(b, "cert=%s/%s\n", TLSMountPath, TLSCertFile)
		fmt.Fprintf(b, "pkey=%s/%s\n", TLSMountPath, TLSKeyFile)
	}
	if !hasPlain {
		b.WriteString("no-udp\n")
		b.WriteString("no-tcp\n")
	} else {
		if !hasTransport(in.Spec.Listeners, turnfleetv1alpha1.ListenerProtocolUDP) {
			b.WriteString("no-udp\n")
		}
		if !hasTransport(in.Spec.Listeners, turnfleetv1alpha1.ListenerProtocolTCP) {
			b.WriteString("no-tcp\n")
		}
	}
	if !hasTLS {
		b.WriteString("no-tls\n")
	}
}

// writeCapacity emits the relay port range, which is this relay's capacity.
// coturn allocates UDP and TCP relay endpoints from this one range
// (turnports_create builds both maps from the same bounds), so the range bounds
// capacity for every transport and TCP relays stay available for clients that
// need them.
func writeCapacity(b *strings.Builder, in *RenderInput) {
	fmt.Fprintf(b, "min-port=%d\n", in.Relay.PortRange.Start)
	fmt.Fprintf(b, "max-port=%d\n", in.Relay.PortRange.End)
}

func writeAuth(b *strings.Builder, in *RenderInput) {
	fmt.Fprintf(b, "realm=%s\n", in.Spec.Auth.Realm)
	b.WriteString("use-auth-secret\n")
	fmt.Fprintf(b, "static-auth-secret=%s\n", in.AuthSecret)
	b.WriteString("fingerprint\n")
	b.WriteString("no-multicast-peers\n")
}

// writePeerPolicy renders the deny and allow lists. coturn takes IP or IP-IP
// ranges, never CIDR notation, so the caller expands every prefix first.
func writePeerPolicy(b *strings.Builder, denied, allowed []string) {
	for _, r := range denied {
		fmt.Fprintf(b, "denied-peer-ip=%s\n", r)
	}
	for _, r := range allowed {
		fmt.Fprintf(b, "allowed-peer-ip=%s\n", r)
	}
}

func writeObservability(b *strings.Builder) {
	// coturn's own metrics are the source of the allocation counts.
	b.WriteString("prometheus\n")
	fmt.Fprintf(b, "prometheus-port=%d\n", DefaultPrometheusPort)
	fmt.Fprintf(b, "prometheus-path=%s\n", DefaultPrometheusPath)

	// Logs go to stdout so kubectl logs works and nothing is written to a
	// read-only container filesystem.
	b.WriteString("log-file=stdout\n")
	b.WriteString("simple-log\n")
}

// writeExtraArgs appends the escape hatch verbatim, in the order given.
func writeExtraArgs(b *strings.Builder, args []string) {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		fmt.Fprintf(b, "%s\n", arg)
	}
}

// listenerPorts returns the plain and TLS listener ports and whether each kind
// is declared. coturn serves UDP and TCP on one plain port; the webhook already
// guarantees that the declared plain listeners agree.
func listenerPorts(listeners []turnfleetv1alpha1.ListenerSpec) (plain, tls int32, hasPlain, hasTLS bool, err error) {
	for _, listener := range listeners {
		switch listener.Protocol {
		case turnfleetv1alpha1.ListenerProtocolUDP, turnfleetv1alpha1.ListenerProtocolTCP:
			if hasPlain && plain != listener.Port {
				return 0, 0, false, false, fmt.Errorf(
					"listeners %d and %d disagree on the plain listening port", plain, listener.Port)
			}
			plain, hasPlain = listener.Port, true
		case turnfleetv1alpha1.ListenerProtocolTLS:
			if hasTLS && tls != listener.Port {
				return 0, 0, false, false, fmt.Errorf("more than one tls listener port (%d and %d)", tls, listener.Port)
			}
			tls, hasTLS = listener.Port, true
		default:
			return 0, 0, false, false, fmt.Errorf("unsupported listener protocol %q", listener.Protocol)
		}
	}
	return plain, tls, hasPlain, hasTLS, nil
}

func hasTransport(listeners []turnfleetv1alpha1.ListenerSpec, protocol turnfleetv1alpha1.ListenerProtocol) bool {
	for _, listener := range listeners {
		if listener.Protocol == protocol {
			return true
		}
	}
	return false
}

// SortRelays orders relays by node name and then by port range, which is the
// order the planner produces and the order the status and the URIs use.
func SortRelays(relays []Relay) {
	slices.SortFunc(relays, func(a, b Relay) int {
		if c := strings.Compare(a.NodeName, b.NodeName); c != 0 {
			return c
		}
		if c := strings.Compare(a.Address, b.Address); c != 0 {
			return c
		}
		return int(a.PortRange.Start - b.PortRange.Start)
	})
}
