// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package relayinit is what the init container of a relay pod runs. It picks
// the relay's coturn configuration on the node, because only the node can say
// whether the address a relay advertises is assigned to one of its interfaces
// (ADR 23).
//
// The controller renders two configurations for such a relay: one that binds
// the node's ExternalIP, for a node that holds it, and one that binds the
// InternalIP and maps the ExternalIP to it, for a node behind 1:1 NAT. The init
// container copies the right one to where coturn reads it and reports the bind
// address it chose in its termination message, which the controller reads back.
// The package decides and copies. It knows nothing about coturn.
package relayinit

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// Command is the first argument that makes the controller binary run this
// package instead of the manager.
const Command = "relay-init"

// outputMode is the mode of the chosen configuration. It embeds the shared
// secret, and coturn runs as the user the init container ran as.
const outputMode = 0o400

// Variant is which configuration the node needs.
type Variant string

const (
	// Local is the configuration that binds the address the node holds.
	Local Variant = "local"

	// Mapped is the configuration that binds the other address and maps the
	// advertised one to it.
	Mapped Variant = "mapped"
)

// Decide picks the variant for a node. The local address wins when the node
// holds it. Otherwise the mapped address has to be held: a node that holds
// neither is not the node the relay was planned for, and a relay on it could
// not bind anything.
func Decide(local, mapped netip.Addr, held []netip.Addr) (Variant, error) {
	switch {
	case contains(held, local):
		return Local, nil
	case contains(held, mapped):
		return Mapped, nil
	default:
		return "", fmt.Errorf("neither %s nor %s is assigned to an interface of this node", local, mapped)
	}
}

func contains(addresses []netip.Addr, want netip.Addr) bool {
	for _, address := range addresses {
		if normalize(address) == normalize(want) {
			return true
		}
	}
	return false
}

// normalize drops what makes equal addresses compare unequal: an IPv4 address
// in its IPv6 form, and an IPv6 zone.
func normalize(address netip.Addr) netip.Addr {
	return address.Unmap().WithZone("")
}

// HeldAddresses lists the addresses assigned to the node's interfaces that are
// up. An address on an interface that is down is not local: the kernel drops
// its local route, and a bind to it fails.
func HeldAddresses() ([]netip.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing the node's interfaces: %w", err)
	}
	var held []netip.Addr
	for i := range interfaces {
		if interfaces[i].Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := interfaces[i].Addrs()
		if err != nil {
			return nil, fmt.Errorf("listing the addresses of interface %s: %w", interfaces[i].Name, err)
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			default:
				continue
			}
			if parsed, ok := netip.AddrFromSlice(ip); ok {
				held = append(held, normalize(parsed))
			}
		}
	}
	return held, nil
}

// Run is the init container's entry point. It returns the process exit code.
// held lists the addresses the node holds: HeldAddresses in the container, a
// fixed list in a test.
func Run(args []string, stderr io.Writer, held func() ([]netip.Addr, error)) int {
	options, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		report(stderr, "%s: %v\n", Command, err)
		return 2
	}

	bind, err := choose(options, held)
	if err != nil {
		report(stderr, "%s: %v\n", Command, err)
		// The termination message is what `kubectl describe pod` shows for an
		// init container that failed, so the reason is not only in the log.
		_ = os.WriteFile(options.messagePath, []byte(err.Error()), 0o600)
		return 1
	}
	if err := os.WriteFile(options.messagePath, []byte(bind.String()), 0o600); err != nil {
		report(stderr, "%s: writing the termination message: %v\n", Command, err)
		return 1
	}
	report(stderr, "%s: binding %s\n", Command, bind)
	return 0
}

// report writes a line for the operator. A failed write to stderr has nowhere
// to be reported.
func report(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

type options struct {
	localAddress  string
	mappedAddress string
	localConfig   string
	mappedConfig  string
	out           string
	messagePath   string
}

func parseOptions(args []string, stderr io.Writer) (*options, error) {
	var o options
	flags := flag.NewFlagSet(Command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.localAddress, "local-address", "", "the address to bind when the node holds it")
	flags.StringVar(&o.mappedAddress, "mapped-address", "", "the address to bind when the node does not hold --local-address")
	flags.StringVar(&o.localConfig, "local-config", "", "the configuration for a node that holds --local-address")
	flags.StringVar(&o.mappedConfig, "mapped-config", "", "the configuration for a node that does not")
	flags.StringVar(&o.out, "out", "", "where to write the chosen configuration")
	flags.StringVar(&o.messagePath, "termination-message", "/dev/termination-log",
		"where to write the bind address, for the controller to read from the pod status")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	for name, value := range map[string]string{
		"local-address": o.localAddress, "mapped-address": o.mappedAddress,
		"local-config": o.localConfig, "mapped-config": o.mappedConfig, "out": o.out,
	} {
		if value == "" {
			return nil, fmt.Errorf("--%s is required", name)
		}
	}
	return &o, nil
}

// choose decides the variant, writes its configuration and returns the address
// it binds.
func choose(o *options, held func() ([]netip.Addr, error)) (netip.Addr, error) {
	local, err := netip.ParseAddr(o.localAddress)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("--local-address %q: %w", o.localAddress, err)
	}
	mapped, err := netip.ParseAddr(o.mappedAddress)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("--mapped-address %q: %w", o.mappedAddress, err)
	}
	addresses, err := held()
	if err != nil {
		return netip.Addr{}, err
	}
	variant, err := Decide(local, mapped, addresses)
	if err != nil {
		return netip.Addr{}, err
	}

	source, bind := o.localConfig, local
	if variant == Mapped {
		source, bind = o.mappedConfig, mapped
	}
	if err := install(source, o.out); err != nil {
		return netip.Addr{}, err
	}
	return bind, nil
}

// install copies the chosen configuration to where coturn reads it. It writes a
// file beside the destination and renames it, so coturn never sees half a file.
func install(source, destination string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("reading the configuration: %w", err)
	}
	temporary := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp")
	//nolint:gosec // the destination is --out, an argument the controller sets on the pod it builds, in a volume it creates.
	if err := os.WriteFile(temporary, content, outputMode); err != nil {
		return fmt.Errorf("writing the configuration: %w", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("installing the configuration: %w", err)
	}
	return nil
}

// ParseBind reads the bind address an init container reported in its
// termination message. A message that is not an address, such as the reason an
// init container failed, reports nothing.
func ParseBind(message string) (string, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(message))
	if err != nil {
		return "", false
	}
	return normalize(address).String(), true
}
