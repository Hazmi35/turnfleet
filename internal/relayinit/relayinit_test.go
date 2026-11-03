// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package relayinit

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addresses(t *testing.T, values ...string) []netip.Addr {
	t.Helper()

	out := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParseAddr(value))
	}
	return out
}

// TestDecide covers the decision table: which configuration a node needs from
// the addresses its interfaces hold.
func TestDecide(t *testing.T) {
	t.Parallel()

	const (
		external = "13.212.26.172"
		internal = "10.42.0.134"
	)
	tests := []struct {
		name    string
		held    []string
		want    Variant
		wantErr bool
	}{
		{
			name: "the node holds only its private address (1:1 NAT)",
			held: []string{"127.0.0.1", internal, "fe80::1"},
			want: Mapped,
		},
		{
			name: "the node holds the public address on an interface",
			held: []string{"127.0.0.1", external, internal},
			want: Local,
		},
		{
			name: "the node holds only the public address",
			held: []string{external},
			want: Local,
		},
		{
			name: "the address is held in its IPv6 form",
			held: []string{"::ffff:" + external},
			want: Local,
		},
		{
			name:    "the node holds neither address",
			held:    []string{"127.0.0.1", "192.0.2.1"},
			wantErr: true,
		},
		{
			name:    "the node holds no address",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Decide(netip.MustParseAddr(external), netip.MustParseAddr(internal), addresses(t, tc.held...))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Decide() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide() failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("Decide() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecideIPv6 covers the same decision in the other family, including the
// zone a link-local address carries, which does not make two addresses differ.
func TestDecideIPv6(t *testing.T) {
	t.Parallel()

	local := netip.MustParseAddr("2001:db8::11")
	mapped := netip.MustParseAddr("fd00::11")

	got, err := Decide(local, mapped, addresses(t, "::1", "fd00::11"))
	if err != nil || got != Mapped {
		t.Errorf("Decide() = %q, %v, want %q", got, err, Mapped)
	}
	got, err = Decide(local, mapped, addresses(t, "::1", "2001:db8::11", "fd00::11"))
	if err != nil || got != Local {
		t.Errorf("Decide() = %q, %v, want %q", got, err, Local)
	}
	got, err = Decide(local.WithZone("eth0"), mapped, addresses(t, "2001:db8::11"))
	if err != nil || got != Local {
		t.Errorf("Decide() with a zone = %q, %v, want %q", got, err, Local)
	}
}

type runFixture struct {
	args    []string
	out     string
	message string
}

// newRunFixture writes the two candidate configurations and returns the
// arguments the init container is started with.
func newRunFixture(t *testing.T) *runFixture {
	t.Helper()

	dir := t.TempDir()
	candidates := filepath.Join(dir, "candidates")
	active := filepath.Join(dir, "active")
	for _, path := range []string{candidates, active} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("creating %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(candidates, "local.conf"), []byte("listening-ip=13.212.26.172\n"), 0o600); err != nil {
		t.Fatalf("writing the local configuration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(candidates, "mapped.conf"), []byte("listening-ip=10.42.0.134\n"), 0o600); err != nil {
		t.Fatalf("writing the mapped configuration: %v", err)
	}

	f := &runFixture{
		out:     filepath.Join(active, "turnserver.conf"),
		message: filepath.Join(dir, "termination-log"),
	}
	f.args = []string{
		"--local-address=13.212.26.172",
		"--mapped-address=10.42.0.134",
		"--local-config=" + filepath.Join(candidates, "local.conf"),
		"--mapped-config=" + filepath.Join(candidates, "mapped.conf"),
		"--out=" + f.out,
		"--termination-message=" + f.message,
	}
	return f
}

func fixedAddresses(t *testing.T, values ...string) func() ([]netip.Addr, error) {
	t.Helper()

	held := addresses(t, values...)
	return func() ([]netip.Addr, error) { return held, nil }
}

// TestRunInstallsTheConfigurationTheNodeNeeds is the init container end to end,
// with the node's interfaces replaced by a fixed list: the chosen file is where
// coturn reads it, the other is not, and the termination message is the bind
// address the controller will read.
func TestRunInstallsTheConfigurationTheNodeNeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		held        []string
		wantConfig  string
		wantMessage string
	}{
		{
			name:        "behind 1:1 NAT",
			held:        []string{"127.0.0.1", "10.42.0.134"},
			wantConfig:  "listening-ip=10.42.0.134\n",
			wantMessage: "10.42.0.134",
		},
		{
			name:        "holding the public address",
			held:        []string{"127.0.0.1", "10.42.0.134", "13.212.26.172"},
			wantConfig:  "listening-ip=13.212.26.172\n",
			wantMessage: "13.212.26.172",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newRunFixture(t)
			var stderr bytes.Buffer
			if code := Run(f.args, &stderr, fixedAddresses(t, tc.held...)); code != 0 {
				t.Fatalf("Run() exited %d: %s", code, stderr.String())
			}

			config, err := os.ReadFile(f.out)
			if err != nil {
				t.Fatalf("reading the installed configuration: %v", err)
			}
			if string(config) != tc.wantConfig {
				t.Errorf("installed configuration = %q, want %q", config, tc.wantConfig)
			}
			info, err := os.Stat(f.out)
			if err != nil {
				t.Fatalf("stat of the installed configuration: %v", err)
			}
			if info.Mode().Perm() != outputMode {
				t.Errorf("installed configuration has mode %v, want %v: it embeds the shared secret", info.Mode().Perm(), os.FileMode(outputMode))
			}
			message, err := os.ReadFile(f.message)
			if err != nil {
				t.Fatalf("reading the termination message: %v", err)
			}
			if string(message) != tc.wantMessage {
				t.Errorf("termination message = %q, want %q", message, tc.wantMessage)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(f.out), ".*.tmp")); len(leftovers) != 0 {
				t.Errorf("temporary files left behind: %v", leftovers)
			}
		})
	}
}

// TestRunFailsWhenTheNodeHoldsNeitherAddress pins that a relay planned for
// another node does not start: nothing is installed, the exit code is not 0,
// and the termination message carries the reason instead of an address.
func TestRunFailsWhenTheNodeHoldsNeitherAddress(t *testing.T) {
	t.Parallel()

	f := newRunFixture(t)
	var stderr bytes.Buffer
	if code := Run(f.args, &stderr, fixedAddresses(t, "127.0.0.1", "192.0.2.1")); code != 1 {
		t.Fatalf("Run() exited %d, want 1", code)
	}
	if _, err := os.Stat(f.out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a configuration was installed on a node that holds neither address: %v", err)
	}
	message, err := os.ReadFile(f.message)
	if err != nil {
		t.Fatalf("reading the termination message: %v", err)
	}
	if !strings.Contains(string(message), "neither 13.212.26.172 nor 10.42.0.134") {
		t.Errorf("termination message = %q, want the reason", message)
	}
	if _, ok := ParseBind(string(message)); ok {
		t.Errorf("the controller would read %q as a bind address", message)
	}
}

// TestRunFailsWhenTheInterfacesCannotBeListed covers an error from the node.
func TestRunFailsWhenTheInterfacesCannotBeListed(t *testing.T) {
	t.Parallel()

	f := newRunFixture(t)
	var stderr bytes.Buffer
	failing := func() ([]netip.Addr, error) { return nil, errors.New("netlink refused") }
	if code := Run(f.args, &stderr, failing); code != 1 {
		t.Fatalf("Run() exited %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "netlink refused") {
		t.Errorf("stderr = %q, want the cause", stderr.String())
	}
}

// TestRunRejectsBadArguments covers the arguments the controller always sets,
// so a wiring mistake fails loudly instead of choosing a default.
func TestRunRejectsBadArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(args []string) []string
	}{
		{"no arguments", func([]string) []string { return nil }},
		{"no output", func(args []string) []string { return args[:4] }},
		{"an address that is not one", func(args []string) []string {
			return append(args, "--local-address=relay.example.net")
		}},
		{"an unknown flag", func(args []string) []string { return append(args, "--bogus") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newRunFixture(t)
			var stderr bytes.Buffer
			code := Run(tc.mutate(f.args), &stderr, fixedAddresses(t, "10.42.0.134"))
			if code == 0 {
				t.Fatalf("Run() accepted the arguments: %s", stderr.String())
			}
		})
	}
}

// TestParseBind covers what the controller reads back.
func TestParseBind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message string
		want    string
		wantOK  bool
	}{
		{"10.42.0.134", "10.42.0.134", true},
		{"10.42.0.134\n", "10.42.0.134", true},
		{"2001:DB8::11", "2001:db8::11", true},
		{"::ffff:10.42.0.134", "10.42.0.134", true},
		{"neither 13.212.26.172 nor 10.42.0.134 is assigned to an interface of this node", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := ParseBind(tc.message)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ParseBind(%q) = %q, %v, want %q, %v", tc.message, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestHeldAddressesFindsLoopback runs the real interface listing: every Linux
// node has the loopback address, which is up.
func TestHeldAddressesFindsLoopback(t *testing.T) {
	t.Parallel()

	held, err := HeldAddresses()
	if err != nil {
		t.Fatalf("HeldAddresses() failed: %v", err)
	}
	if !contains(held, netip.MustParseAddr("127.0.0.1")) {
		t.Errorf("HeldAddresses() = %v, want it to hold 127.0.0.1", held)
	}
}
