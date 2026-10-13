// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package probe performs a real TURN Allocate against a relay listener.
//
// It exists because the failure Turnfleet removes is silent: signalling
// succeeds, the relay candidate never forms, and every pod looks healthy. A
// probe that only checks "is the port open" cannot see that, so the probe
// allocates for real, using credentials derived from the pool's shared secret,
// and asserts that the relayed transport address is the one the relay is
// supposed to own.
//
// The same code backs the controller's periodic check and the standalone
// turnfleet-probe CLI, so what an operator runs by hand is what the controller
// trusts.
//
// An in-cluster probe proves the relay's configuration and capacity; it cannot
// prove external reachability (hairpin NAT and firewalls in front of the relay
// node are invisible from inside). Running turnfleet-probe from outside the
// cluster is the authoritative check; see docs/capacity-model.md.
package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"

	"github.com/hzmi/turnfleet/internal/relay"
)

// Config describes one probe.
type Config struct {
	// Endpoint is the relay listener to probe.
	Endpoint Endpoint

	// Username and Password are the TURN REST API credentials. Use Credentials
	// to derive them from the pool's shared secret.
	Username string
	Password string

	// Timeout bounds the whole probe. Defaults to DefaultTimeout.
	Timeout time.Duration

	// ExpectAddress is the address the relay must advertise in
	// XOR-RELAYED-ADDRESS.
	ExpectAddress string

	// ExpectPortRange is the port range the relayed port must fall in. It is the
	// range the relay owns, not the pool's: a relay that hands out a port
	// outside its slice is relaying on another relay's capacity.
	ExpectPortRange *relay.PortRange

	// PeerAddress, when set, makes the probe create a permission for that peer
	// and send a datagram through the allocation.
	PeerAddress string

	// EchoListen, when set, starts a UDP echo server on that address and uses it
	// as the peer, so the round trip is self-contained.
	EchoListen string

	// Logger receives the probe's progress logs.
	Logger Logger
}

// Logger is the small logging surface the probe uses.
type Logger interface {
	// Debugf logs a step of the probe.
	Debugf(format string, args ...any)
	// Errorf logs a failure that affects the result.
	Errorf(format string, args ...any)
}

type discardLogger struct{}

func (discardLogger) Debugf(string, ...any) {}
func (discardLogger) Errorf(string, ...any) {}

// Result is the outcome of a probe. It is the JSON payload of the CLI and the
// source of the controller's relay status.
type Result struct {
	// Succeeded reports whether the relay allocated and returned the expected
	// relayed address and range.
	Succeeded bool `json:"succeeded"`

	// URI is the listener that was probed.
	URI string `json:"uri"`

	// RelayedAddress and RelayedPort are what the relay answered with.
	RelayedAddress string `json:"relayedAddress,omitempty"`
	RelayedPort    int32  `json:"relayedPort,omitempty"`

	// LatencyMillis is how long the probe took, in milliseconds.
	LatencyMillis int64 `json:"latencyMillis"`

	// ErrorCode is the TURN error code the relay answered with, when it refused
	// the allocation. 508 means Insufficient Capacity: the relay's port range is
	// exhausted, which is the saturation signal.
	ErrorCode int `json:"errorCode,omitempty"`

	// Message explains the result in one sentence.
	Message string `json:"message"`

	// PeerRoundTrip reports the outcome of the optional permission and send
	// round trip, when one was requested.
	PeerRoundTrip *bool `json:"peerRoundTrip,omitempty"`
}

// Allocate probes one relay and returns what happened.
//
// The probe always releases the allocation it created (a Refresh with lifetime
// zero), so it does not consume the capacity it measures.
func Allocate(ctx context.Context, cfg *Config) Result {
	start := time.Now()
	result := Result{URI: cfg.Endpoint.String()}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger := cfg.Logger
	if logger == nil {
		logger = discardLogger{}
	}

	conn, err := dial(ctx, cfg.Endpoint)
	if err != nil {
		return finish(&result, start, err, 0, "cannot reach the relay listener")
	}
	defer func() { _ = conn.Close() }()

	client, err := turn.NewClient(&turn.ClientConfig{
		Conn:           conn,
		STUNServerAddr: cfg.Endpoint.Address(),
		TURNServerAddr: cfg.Endpoint.Address(),
		Username:       cfg.Username,
		Password:       cfg.Password,
		LoggerFactory:  quietLoggerFactory(),
	})
	if err != nil {
		return finish(&result, start, err, 0, "cannot create the TURN client")
	}
	defer client.Close()

	if err := client.Listen(); err != nil {
		return finish(&result, start, err, 0, "cannot start the TURN client")
	}

	logger.Debugf("allocating on %s as %s", cfg.Endpoint.String(), cfg.Username)

	alloc, err := allocate(ctx, client, cfg.Endpoint)
	if err != nil {
		return finish(&result, start, err, turnErrorCode(err), "the relay refused the allocation")
	}
	// Closing the allocation sends Refresh with lifetime 0: the port goes back
	// to the relay immediately, so the probe does not hold the capacity it is
	// measuring.
	defer func() { _ = alloc.close() }()

	relayed, err := relayedUDPAddr(alloc.localAddr())
	if err != nil {
		return finish(&result, start, err, 0, "the relay answered with an address the probe cannot read")
	}
	result.RelayedAddress = relayed.IP.String()
	// A port from net is 0-65535.
	//nolint:gosec // a UDP port always fits in int32.
	result.RelayedPort = int32(relayed.Port)
	logger.Debugf("relayed address is %s:%d", result.RelayedAddress, result.RelayedPort)

	if err := checkExpectations(cfg, relayed); err != nil {
		return finish(&result, start, err, 0, "the relay answered with an address it does not own")
	}

	if cfg.PeerAddress != "" || cfg.EchoListen != "" {
		if alloc.packet == nil {
			return finish(&result, start,
				errors.New("a peer round trip needs a udp allocation"),
				0, "the probe cannot send through a non-udp allocation")
		}
		roundTrip, err := peerRoundTrip(ctx, client, alloc.packet, cfg, logger)
		result.PeerRoundTrip = &roundTrip
		if err != nil {
			return finish(&result, start, err, turnErrorCode(err), "the relayed path did not complete a round trip")
		}
	}

	result.Succeeded = true
	result.LatencyMillis = time.Since(start).Milliseconds()
	result.Message = "relay allocated and advertised the expected relayed address"
	return result
}

// allocation is the slice of the pion client's allocation API the probe needs.
// The udp and tcp allocation types expose the relayed address under different
// names, and the tcp one is in an internal package, so the probe wraps both
// behind function values instead of naming their types.
type allocation struct {
	localAddr func() net.Addr
	close     func() error
	deadline  func(time.Time) error

	// packet is the udp allocation as a packet connection, for the optional peer
	// round trip. It is nil for a tcp or tls transport.
	packet net.PacketConn
}

// allocate performs the TURN Allocate for the probe's transport.
//
// The relayed transport follows the client transport, as RFC 5766 requires: a
// udp listener gets a udp allocation, and a tcp or tls listener gets a tcp one.
// Both allocate their relay port from the range coturn was configured with, so
// the capacity check is the same for both.
func allocate(ctx context.Context, client *turn.Client, endpoint Endpoint) (*allocation, error) {
	switch endpoint.Transport {
	case TransportUDP:
		conn, err := client.AllocateWithContext(ctx)
		if err != nil {
			return nil, err
		}
		return &allocation{
			localAddr: conn.LocalAddr,
			close:     conn.Close,
			deadline:  conn.SetDeadline,
			packet:    conn,
		}, nil
	case TransportTCP, TransportTLS:
		alloc, err := client.AllocateTCPWithContext(ctx)
		if err != nil {
			return nil, err
		}
		return &allocation{
			localAddr: alloc.Addr,
			close:     alloc.Close,
			deadline:  alloc.SetDeadline,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", endpoint.Transport)
	}
}

// relayedUDPAddr reads the relayed transport address the relay handed out.
func relayedUDPAddr(addr net.Addr) (*net.UDPAddr, error) {
	if udp, ok := addr.(*net.UDPAddr); ok && udp != nil {
		return udp, nil
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, fmt.Errorf("relay returned an unusable relayed address %v: %w", addr, err)
	}
	parsedPort, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("relay returned an unusable relayed port %v: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("relay returned an unusable relayed address %v", addr)
	}
	return &net.UDPAddr{IP: ip, Port: int(parsedPort)}, nil
}

// dial opens the client transport to the relay listener.
func dial(ctx context.Context, endpoint Endpoint) (net.PacketConn, error) {
	dialer := &net.Dialer{Timeout: DefaultTimeout}
	switch endpoint.Transport {
	case TransportUDP:
		// An unconnected socket: the TURN client addresses the server itself with
		// WriteTo, and a connected socket would reject every write but the first.
		conn, err := net.ListenPacket(udpNetwork(endpoint.Host), ":0")
		if err != nil {
			return nil, fmt.Errorf("opening a udp socket for %s: %w", endpoint.Address(), err)
		}
		return conn, nil
	case TransportTCP:
		conn, err := dialer.DialContext(ctx, "tcp", endpoint.Address())
		if err != nil {
			return nil, fmt.Errorf("dialing tcp %s: %w", endpoint.Address(), err)
		}
		return turn.NewSTUNConn(conn), nil
	case TransportTLS:
		conn, err := tls.DialWithDialer(dialer, "tcp", endpoint.Address(), &tls.Config{
			MinVersion: tls.VersionTLS12,
			// A relay serves a certificate for its own address; the probe proves
			// the data path, and the pool's certificate material is checked where
			// it is provisioned.
			//nolint:gosec // the relay's serving certificate is not trusted by a public CA.
			InsecureSkipVerify: true,
		})
		if err != nil {
			return nil, fmt.Errorf("dialing tls %s: %w", endpoint.Address(), err)
		}
		return turn.NewSTUNConn(conn), nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", endpoint.Transport)
	}
}

// udpNetwork picks the address family to open the client socket in, so a relay
// with an IPv6 address is reachable too.
func udpNetwork(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "udp6"
	}
	return "udp4"
}

// checkExpectations asserts that the relay advertised an address it owns.
func checkExpectations(cfg *Config, relayed *net.UDPAddr) error {
	if cfg.ExpectAddress != "" {
		expected, err := net.ResolveUDPAddr("udp", net.JoinHostPort(cfg.ExpectAddress, "0"))
		if err != nil {
			return fmt.Errorf("resolving expected address %q: %w", cfg.ExpectAddress, err)
		}
		if !relayed.IP.Equal(expected.IP) {
			return fmt.Errorf("relayed address is %s, want %s: peers cannot reach the advertised address",
				relayed.IP, expected.IP)
		}
	}
	// A port from net is always 0-65535.
	//nolint:gosec // a UDP port always fits in int32.
	relayedPort := int32(relayed.Port)
	if cfg.ExpectPortRange != nil && !cfg.ExpectPortRange.Contains(relayedPort) {
		return fmt.Errorf("relayed port %d is outside the relay's range %s: this relay is relaying on capacity it does not own",
			relayed.Port, cfg.ExpectPortRange)
	}
	return nil
}

// peerRoundTrip creates a permission for a peer and sends a datagram through the
// allocation. It proves the relayed path works end to end, which is what a peer
// eventually depends on.
func peerRoundTrip(ctx context.Context, client *turn.Client, allocation net.PacketConn, cfg *Config, logger Logger) (bool, error) {
	peer := cfg.PeerAddress
	var echo net.PacketConn
	if cfg.EchoListen != "" {
		conn, err := net.ListenPacket("udp", cfg.EchoListen)
		if err != nil {
			return false, fmt.Errorf("starting the echo peer on %s: %w", cfg.EchoListen, err)
		}
		echo = conn
		defer func() { _ = echo.Close() }()
		peer = conn.LocalAddr().String()
	}

	peerAddr, err := net.ResolveUDPAddr("udp", peer)
	if err != nil {
		return false, fmt.Errorf("resolving peer %q: %w", peer, err)
	}

	payload := []byte("turnfleet-probe")
	done := make(chan error, 1)
	if echo != nil {
		go func() {
			buf := make([]byte, 1500)
			if err := echo.SetReadDeadline(time.Now().Add(DefaultTimeout)); err != nil {
				done <- err
				return
			}
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				done <- fmt.Errorf("echo peer read: %w", err)
				return
			}
			if _, err := echo.WriteTo(buf[:n], from); err != nil {
				done <- fmt.Errorf("echo peer write: %w", err)
				return
			}
			done <- nil
		}()
	}

	logger.Debugf("creating a permission for peer %s", peerAddr)
	if err := client.CreatePermission(peerAddr); err != nil {
		// A denied peer is refused here, which is the peer-policy check.
		return false, fmt.Errorf("creating a permission for %s: %w", peerAddr, err)
	}

	deadline := time.Now().Add(DefaultTimeout)
	if err := allocation.SetDeadline(deadline); err != nil {
		return false, fmt.Errorf("setting the allocation deadline: %w", err)
	}
	if _, err := allocation.WriteTo(payload, peerAddr); err != nil {
		return false, fmt.Errorf("sending to %s through the allocation: %w", peerAddr, err)
	}

	buf := make([]byte, 1500)
	n, from, err := allocation.ReadFrom(buf)
	if err != nil {
		return false, fmt.Errorf("waiting for the peer's datagram from %s: %w", peerAddr, err)
	}
	if !bytes.Equal(buf[:n], payload) {
		return false, fmt.Errorf("peer echoed %q from %s, want %q", buf[:n], from, payload)
	}

	if echo != nil {
		select {
		case err := <-done:
			if err != nil {
				return false, err
			}
		case <-ctx.Done():
			return false, fmt.Errorf("echo peer did not answer: %w", ctx.Err())
		}
	}
	return true, nil
}

// turnErrorCode extracts the TURN error code from an error returned by the pion
// client. coturn answers 508 when the relay's port range is exhausted, which is
// the code that turns into CapacityPressure.
func turnErrorCode(err error) int {
	if turnErr, ok := errors.AsType[*stun.TurnError](err); ok {
		return int(turnErr.ErrorCodeAttr.Code)
	}
	return 0
}

// TurnErrorCode exposes the TURN error code carried by an error, for callers
// that need it (the controller counts 508 in turnfleet_allocation_failures_total).
func TurnErrorCode(err error) int { return turnErrorCode(err) }

func finish(result *Result, start time.Time, err error, code int, explanation string) Result {
	result.LatencyMillis = time.Since(start).Milliseconds()
	result.ErrorCode = code
	if err != nil {
		result.Message = fmt.Sprintf("%s: %v", explanation, err)
	} else {
		result.Message = explanation
	}
	return *result
}

// quietLoggerFactory silences the TURN client's own logging: the probe reports
// through its Result, and the controller's logs must not be filled with the
// client's debug output.
func quietLoggerFactory() logging.LoggerFactory {
	return &logging.DefaultLoggerFactory{
		Writer:          discardWriter{},
		DefaultLogLevel: logging.LogLevelDisabled,
		ScopeLevels:     map[string]logging.LogLevel{},
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
