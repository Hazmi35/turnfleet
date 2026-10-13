// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Command turnfleet-probe performs a real TURN Allocate against a relay
// listener and reports what happened, as JSON.
//
// It is the same check the controller runs periodically, which is the point: an
// operator debugging a pool runs exactly what the operator trusts, from outside
// the cluster, where the answer is authoritative. An in-cluster probe can pass
// while external reachability fails (hairpin NAT, a firewall in front of the
// relay node); see docs/capacity-model.md.
//
//	turnfleet-probe --uri "turn:203.0.113.10:3478?transport=udp" \
//	  --secret-file ./turn-secret --expect-address 203.0.113.10 \
//	  --expect-port-range 49152-49199 --json
//
// Exit status is 0 when the relay allocated and advertised the expected relayed
// address, 1 when the check failed, and 2 when the arguments are wrong.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hzmi/turnfleet/internal/probe"
	"github.com/hzmi/turnfleet/internal/relay"
	"github.com/hzmi/turnfleet/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

type options struct {
	uri             string
	secretFile      string
	secret          string
	user            string
	ttl             time.Duration
	timeout         time.Duration
	expectAddress   string
	expectPortRange string
	peerAddress     string
	peerEcho        string
	jsonOutput      bool
	showVersion     bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var opts options
	flags := flag.NewFlagSet("turnfleet-probe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.uri, "uri", "", "TURN URI of the relay listener to probe, e.g. turn:203.0.113.10:3478?transport=udp")
	flags.StringVar(&opts.secretFile, "secret-file", "", "file holding the pool's shared HMAC secret ('-' reads stdin)")
	flags.StringVar(&opts.secret, "secret", "", "the shared HMAC secret itself (prefer --secret-file: a command line is visible to other processes)")
	flags.StringVar(&opts.user, "user", probe.DefaultUser, "user name embedded in the probe credential")
	flags.DurationVar(&opts.ttl, "ttl", probe.DefaultCredentialTTL, "how long the probe credential is valid")
	flags.DurationVar(&opts.timeout, "timeout", probe.DefaultTimeout, "how long the whole probe may take")
	flags.StringVar(&opts.expectAddress, "expect-address", "", "address the relay must advertise in XOR-RELAYED-ADDRESS")
	flags.StringVar(&opts.expectPortRange, "expect-port-range", "", "port range the relayed port must fall in, e.g. 49152-49199")
	flags.StringVar(&opts.peerAddress, "peer-address", "", "peer to create a permission for and send to, to check the relayed path")
	flags.StringVar(&opts.peerEcho, "peer-echo", "", "local address to run an echo peer on, making the round trip self-contained")
	flags.BoolVar(&opts.jsonOutput, "json", false, "print the result as JSON")
	flags.BoolVar(&opts.showVersion, "version", false, "print the version and exit")

	flags.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "turnfleet-probe probes a Turnfleet relay with a real TURN Allocate.\n\n")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if opts.showVersion {
		v, commit, date := version.Info()
		_, _ = fmt.Fprintf(stdout, "turnfleet-probe %s (%s, %s)\n", v, commit, date)
		return exitOK
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if opts.uri == "" {
		_, _ = fmt.Fprintln(stderr, "--uri is required")
		return exitUsage
	}

	cfg, err := buildConfig(&opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid arguments: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result := probe.Allocate(ctx, &cfg)

	if opts.jsonOutput {
		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "encoding the result: %v\n", err)
			return exitFailure
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", encoded)
	} else {
		writeHumanResult(stdout, &result)
	}

	if !result.Succeeded {
		return exitFailure
	}
	return exitOK
}

func buildConfig(opts *options) (probe.Config, error) {
	endpoint, err := probe.ParseURI(opts.uri)
	if err != nil {
		return probe.Config{}, err
	}

	secret, err := readSecret(opts)
	if err != nil {
		return probe.Config{}, err
	}
	if secret == "" {
		return probe.Config{}, errors.New("the shared secret is empty")
	}

	username, password := probe.Credentials(secret, opts.user, opts.ttl, time.Now())

	cfg := probe.Config{
		Endpoint:      endpoint,
		Username:      username,
		Password:      password,
		Timeout:       opts.timeout,
		ExpectAddress: opts.expectAddress,
		PeerAddress:   opts.peerAddress,
		EchoListen:    opts.peerEcho,
		Logger:        stderrLogger{},
	}

	if opts.expectPortRange != "" {
		portRange, err := parsePortRange(opts.expectPortRange)
		if err != nil {
			return probe.Config{}, err
		}
		cfg.ExpectPortRange = &portRange
	}
	return cfg, nil
}

func readSecret(opts *options) (string, error) {
	switch {
	case opts.secret != "" && opts.secretFile != "":
		return "", errors.New("use either --secret or --secret-file, not both")
	case opts.secret != "":
		return opts.secret, nil
	case opts.secretFile == "-":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", fmt.Errorf("reading the secret from stdin: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	case opts.secretFile != "":
		data, err := os.ReadFile(opts.secretFile)
		if err != nil {
			return "", fmt.Errorf("reading the secret file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	default:
		return "", errors.New("one of --secret or --secret-file is required")
	}
}

// parsePortRange accepts "start-end" or a single port.
func parsePortRange(value string) (relay.PortRange, error) {
	start, end, found := strings.Cut(value, "-")
	startPort, err := strconv.ParseInt(strings.TrimSpace(start), 10, 32)
	if err != nil {
		return relay.PortRange{}, fmt.Errorf("parsing port range %q: %w", value, err)
	}
	if !found {
		return relay.PortRange{Start: int32(startPort), End: int32(startPort)}, nil
	}
	endPort, err := strconv.ParseInt(strings.TrimSpace(end), 10, 32)
	if err != nil {
		return relay.PortRange{}, fmt.Errorf("parsing port range %q: %w", value, err)
	}
	if endPort < startPort {
		return relay.PortRange{}, fmt.Errorf("parsing port range %q: end is before start", value)
	}
	return relay.PortRange{Start: int32(startPort), End: int32(endPort)}, nil
}

func writeHumanResult(stdout io.Writer, result *probe.Result) {
	status := "FAIL"
	if result.Succeeded {
		status = "OK"
	}
	_, _ = fmt.Fprintf(stdout, "%s %s\n", status, result.URI)
	if result.Succeeded {
		_, _ = fmt.Fprintf(stdout, "  relayed address: %s:%d\n", result.RelayedAddress, result.RelayedPort)
	}
	if result.ErrorCode != 0 {
		_, _ = fmt.Fprintf(stdout, "  turn error code: %d\n", result.ErrorCode)
	}
	_, _ = fmt.Fprintf(stdout, "  latency: %dms\n", result.LatencyMillis)
	if result.PeerRoundTrip != nil {
		_, _ = fmt.Fprintf(stdout, "  peer round trip: %t\n", *result.PeerRoundTrip)
	}
	_, _ = fmt.Fprintf(stdout, "  %s\n", result.Message)
}

// stderrLogger sends the probe's progress to stderr so stdout stays parseable.
type stderrLogger struct{}

func (stderrLogger) Debugf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "debug: "+format+"\n", args...)
}

func (stderrLogger) Errorf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
}
