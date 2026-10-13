// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package probe

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v5"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/Hazmi35/turnfleet/internal/relay"
)

const (
	testSecret = "test-secret"
	testRealm  = "turnfleet.test"
	testUser   = "turnfleet-probe"

	// Relay port ranges the test servers hand out. Every test gets its own: the
	// tests run in parallel, and two servers sharing a port range would fight
	// over the same relay port.
	rangeSuccessLow    uint16 = 55000
	rangeSuccessHigh   uint16 = 55010
	rangeOnePortLow    uint16 = 55020
	rangeOnePortHigh   uint16 = 55020
	rangeWrongAddrLow  uint16 = 55030
	rangeWrongAddrHigh uint16 = 55040
	rangeEscapeLow     uint16 = 55050
	rangeEscapeHigh    uint16 = 55060
	rangeAuthLow       uint16 = 55070
	rangeAuthHigh      uint16 = 55080
	rangeCapacityLow   uint16 = 55090
	rangeCapacityHigh  uint16 = 55090
	rangeRoundTripLow  uint16 = 55100
	rangeRoundTripHigh uint16 = 55110
	rangeDeniedLow     uint16 = 55120
	rangeDeniedHigh    uint16 = 55130
)

// turnServer is a real TURN server, in-process, so the probe is tested against
// the protocol rather than against a mock of it.
type turnServer struct {
	address  string
	secret   string
	portLow  int32
	portHigh int32
}

func startTurnServer(t *testing.T, portLow, portHigh uint16, permission turn.PermissionHandler) *turnServer {
	t.Helper()

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}

	server, err := turn.NewServer(turn.ServerConfig{
		Realm:         testRealm,
		AuthHandler:   turn.LongTermTURNRESTAuthHandler(testSecret, nil),
		LoggerFactory: quietLoggerFactory(),
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: conn,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP("127.0.0.1"),
					Address:      "127.0.0.1",
					MinPort:      portLow,
					MaxPort:      portHigh,
				},
				PermissionHandler: permission,
			},
		},
	})
	if err != nil {
		t.Fatalf("starting the TURN server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	return &turnServer{
		address:  conn.LocalAddr().String(),
		secret:   testSecret,
		portLow:  int32(portLow),
		portHigh: int32(portHigh),
	}
}

func (s *turnServer) endpoint(t *testing.T) Endpoint {
	t.Helper()

	endpoint, err := ParseURI("turn:" + s.address + "?transport=udp")
	if err != nil {
		t.Fatalf("parsing the test server URI: %v", err)
	}
	return endpoint
}

func (s *turnServer) config(t *testing.T, expectAddress string, portRange *relay.PortRange) *Config {
	t.Helper()

	username, password := Credentials(s.secret, testUser, DefaultCredentialTTL, time.Now())
	return &Config{
		Endpoint:        s.endpoint(t),
		Username:        username,
		Password:        password,
		Timeout:         5 * time.Second,
		ExpectAddress:   expectAddress,
		ExpectPortRange: portRange,
	}
}

func TestParseURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		uri       string
		want      Endpoint
		wantError bool
	}{
		{
			name: "udp with an explicit transport",
			uri:  "turn:203.0.113.10:3478?transport=udp",
			want: Endpoint{Host: "203.0.113.10", Port: 3478, Transport: "udp"},
		},
		{
			name: "plain turn defaults to udp",
			uri:  "turn:203.0.113.10:3478",
			want: Endpoint{Host: "203.0.113.10", Port: 3478, Transport: "udp"},
		},
		{
			name: "turn with the default port",
			uri:  "turn:203.0.113.10?transport=udp",
			want: Endpoint{Host: "203.0.113.10", Port: DefaultPlainPort, Transport: "udp"},
		},
		{
			name: "turns",
			uri:  "turns:relay.example.com:5349?transport=tcp",
			want: Endpoint{Host: "relay.example.com", Port: 5349, Transport: "tcp", Secure: true},
		},
		{
			name: "turns defaults to its own port and tcp",
			uri:  "turns:relay.example.com",
			want: Endpoint{Host: "relay.example.com", Port: DefaultTLSPort, Transport: "tcp", Secure: true},
		},
		{
			name: "authority form",
			uri:  "turn://203.0.113.10:3478?transport=tcp",
			want: Endpoint{Host: "203.0.113.10", Port: 3478, Transport: "tcp"},
		},
		{name: "wrong scheme", uri: "http://relay.example.com:3478", wantError: true},
		{name: "no host", uri: "turn:?transport=udp", wantError: true},
		{name: "bad transport", uri: "turn:203.0.113.10:3478?transport=sctp", wantError: true},
		{name: "tls over udp", uri: "turns:203.0.113.10:5349?transport=udp", wantError: true},
		{name: "bad port", uri: "turn:203.0.113.10:notaport", wantError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseURI(tc.uri)
			if tc.wantError {
				if err == nil {
					t.Fatalf("ParseURI(%q) accepted an invalid URI and returned %+v", tc.uri, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURI(%q) failed: %v", tc.uri, err)
			}
			if got != tc.want {
				t.Errorf("ParseURI(%q) = %+v, want %+v", tc.uri, got, tc.want)
			}
		})
	}
}

// TestCredentials pins the TURN REST API derivation against a value computed
// independently of this implementation: the username carries the expiry and the
// password is base64(HMAC-SHA1(secret, username)). Getting this wrong would make
// every probe fail with 401 and every client credential the issuer hands out
// useless.
func TestCredentials(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	username, password := Credentials("test-secret", testUser, 5*time.Minute, now)

	if username != "1700000300:turnfleet-probe" {
		t.Errorf("username = %q, want %q", username, "1700000300:turnfleet-probe")
	}
	// The literal is a published test vector, not a credential.
	//nolint:gosec // published HMAC test vector for a throwaway secret.
	if password != "QS7At7t+jGJFlhMXnj4SxxVYkCY=" {
		t.Errorf("password = %q, want %q", password, "QS7At7t+jGJFlhMXnj4SxxVYkCY=")
	}

	other, otherPassword := Credentials("another-secret", testUser, 5*time.Minute, now)
	if other != username {
		t.Errorf("the username depends on the secret: %q vs %q", other, username)
	}
	if otherPassword == password {
		t.Error("a different secret produced the same password")
	}

	_, laterPassword := Credentials("test-secret", testUser, 10*time.Minute, now)
	if laterPassword == password {
		t.Error("a different expiry produced the same password")
	}
}

func TestAllocateSucceeds(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeSuccessLow, rangeSuccessHigh, nil)
	range55000to55010 := relay.PortRange{Start: int32(rangeSuccessLow), End: int32(rangeSuccessHigh)}

	result := Allocate(context.Background(), server.config(t, "127.0.0.1", &range55000to55010))

	if !result.Succeeded {
		t.Fatalf("probe failed: %+v", result)
	}
	if result.RelayedAddress != "127.0.0.1" {
		t.Errorf("relayed address = %q, want 127.0.0.1", result.RelayedAddress)
	}
	if !range55000to55010.Contains(result.RelayedPort) {
		t.Errorf("relayed port = %d, want a port in %s", result.RelayedPort, range55000to55010)
	}
	if result.ErrorCode != 0 {
		t.Errorf("error code = %d, want 0 on success", result.ErrorCode)
	}
	if result.LatencyMillis < 0 {
		t.Errorf("latency = %d, want a non-negative duration", result.LatencyMillis)
	}
}

// TestAllocateReleasesTheAllocation is the property that keeps the probe from
// consuming the capacity it measures: a one-port relay must serve the probe over
// and over.
//
// Releasing is asynchronous by design — the client sends Refresh with lifetime
// zero and does not wait for the response — so the relay frees the port a moment
// after the probe returns. The test therefore polls: what it proves is that the
// port is released, not that it is released instantly.
func TestAllocateReleasesTheAllocation(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeOnePortLow, rangeOnePortHigh, nil)
	onePort := relay.PortRange{Start: int32(rangeOnePortLow), End: int32(rangeOnePortHigh)}

	for round := range 3 {
		var last Result
		err := wait.PollUntilContextTimeout(context.Background(), 20*time.Millisecond, 5*time.Second, true,
			func(ctx context.Context) (bool, error) {
				last = Allocate(ctx, server.config(t, "127.0.0.1", &onePort))
				return last.Succeeded, nil
			})
		if err != nil {
			t.Fatalf("round %d: the previous probe never released its allocation: %+v", round+1, last)
		}
	}
}

func TestAllocateDetectsWrongAddress(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeWrongAddrLow, rangeWrongAddrHigh, nil)
	portRange := relay.PortRange{Start: int32(rangeWrongAddrLow), End: int32(rangeWrongAddrHigh)}

	result := Allocate(context.Background(), server.config(t, "203.0.113.10", &portRange))

	if result.Succeeded {
		t.Fatalf("probe passed although the relay advertised %s instead of 203.0.113.10", result.RelayedAddress)
	}
	if !strings.Contains(result.Message, "want 203.0.113.10") {
		t.Errorf("message %q does not say which address was expected", result.Message)
	}
}

func TestAllocateDetectsPortOutsideTheRelayRange(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeEscapeLow, rangeEscapeHigh, nil)
	elsewhere := relay.PortRange{Start: 60000, End: 60010}

	result := Allocate(context.Background(), server.config(t, "127.0.0.1", &elsewhere))

	if result.Succeeded {
		t.Fatalf("probe passed although port %d is outside %s", result.RelayedPort, elsewhere)
	}
	if !strings.Contains(result.Message, "outside the relay's range") {
		t.Errorf("message %q does not explain that the port escaped the relay's range", result.Message)
	}
}

func TestAllocateReportsAuthFailure(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeAuthLow, rangeAuthHigh, nil)
	cfg := server.config(t, "127.0.0.1", nil)
	cfg.Password = "definitely-not-the-password"

	result := Allocate(context.Background(), cfg)

	if result.Succeeded {
		t.Fatal("probe passed with the wrong credential")
	}
	// The code is what the server answered: coturn rejects a bad integrity
	// check with 401, the pion server used here with 400. Either way the probe
	// must report the refusal with a TURN error code rather than a generic
	// failure.
	if result.ErrorCode != 401 && result.ErrorCode != 400 {
		t.Errorf("error code = %d, want a TURN authentication error (401 from coturn, 400 from the test server)", result.ErrorCode)
	}
	if !strings.Contains(result.Message, "refused the allocation") {
		t.Errorf("message %q does not say the relay refused the allocation", result.Message)
	}
}

// TestAllocateReportsInsufficientCapacity is the saturation signal: coturn
// answers 508 when the relay's port range is exhausted, and that is what the
// controller counts in turnfleet_allocation_failures_total{code="508"}.
func TestAllocateReportsInsufficientCapacity(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeCapacityLow, rangeCapacityHigh, nil)
	onePort := relay.PortRange{Start: int32(rangeCapacityLow), End: int32(rangeCapacityHigh)}
	cfg := server.config(t, "127.0.0.1", &onePort)

	_, release := holdAllocation(t, server, cfg)
	defer release()

	result := Allocate(context.Background(), cfg)

	if result.Succeeded {
		t.Fatal("probe passed although the only relay port was taken")
	}
	if result.ErrorCode != 508 {
		t.Errorf("error code = %d, want 508 (Insufficient Capacity): %s", result.ErrorCode, result.Message)
	}
}

// holdAllocation occupies the single relay port with a raw TURN client, so the
// probe has nothing left to allocate.
func holdAllocation(t *testing.T, server *turnServer, cfg *Config) (allocation net.PacketConn, release func()) {
	t.Helper()

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the holder client: %v", err)
	}
	client, err := turn.NewClient(&turn.ClientConfig{
		Conn:           conn,
		TURNServerAddr: server.address,
		Username:       cfg.Username,
		Password:       cfg.Password,
		LoggerFactory:  quietLoggerFactory(),
	})
	if err != nil {
		t.Fatalf("creating the holder client: %v", err)
	}
	if err := client.Listen(); err != nil {
		t.Fatalf("starting the holder client: %v", err)
	}

	held, err := client.AllocateWithContext(context.Background())
	if err != nil {
		t.Fatalf("the holder could not allocate: %v", err)
	}

	return held, func() {
		_ = held.Close()
		client.Close()
		_ = conn.Close()
	}
}

func TestAllocatePeerRoundTrip(t *testing.T) {
	t.Parallel()

	server := startTurnServer(t, rangeRoundTripLow, rangeRoundTripHigh, nil)
	cfg := server.config(t, "127.0.0.1", nil)
	cfg.EchoListen = "127.0.0.1:0"

	result := Allocate(context.Background(), cfg)

	if !result.Succeeded {
		t.Fatalf("probe with a round trip failed: %+v", result)
	}
	if result.PeerRoundTrip == nil || !*result.PeerRoundTrip {
		t.Errorf("peer round trip was not reported as successful: %+v", result.PeerRoundTrip)
	}
}

// TestAllocateRefusesDeniedPeer mirrors the peer-policy e2e case: a relay that
// is configured to refuse a peer must refuse the permission, and the probe must
// report that rather than a generic failure.
func TestAllocateRefusesDeniedPeer(t *testing.T) {
	t.Parallel()

	deny := func(_ net.Addr, peerIP net.IP) bool { return !peerIP.IsLoopback() }
	server := startTurnServer(t, rangeDeniedLow, rangeDeniedHigh, deny)
	cfg := server.config(t, "127.0.0.1", nil)
	cfg.EchoListen = "127.0.0.1:0"

	result := Allocate(context.Background(), cfg)

	if result.Succeeded {
		t.Fatal("probe passed although the relay refuses the peer")
	}
	if result.PeerRoundTrip == nil || *result.PeerRoundTrip {
		t.Errorf("round trip = %v, want false", result.PeerRoundTrip)
	}
	if !strings.Contains(result.Message, "permission") {
		t.Errorf("message %q does not mention the refused permission", result.Message)
	}
}
