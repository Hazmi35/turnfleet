// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package probe

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // TURN REST API credentials are defined over HMAC-SHA1 (draft-uberti-behave-turn-rest); this is not a free choice of hash.
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults for probes and credentials.
const (
	// DefaultTimeout bounds a single probe.
	DefaultTimeout = 3 * time.Second

	// DefaultCredentialTTL is how long a probe credential is valid. It only has
	// to outlive the probe itself.
	DefaultCredentialTTL = 5 * time.Minute

	// DefaultUser is the user name embedded in the credential, so a relay's
	// logs distinguish probe allocations from client ones.
	DefaultUser = "turnfleet-probe"

	// DefaultPlainPort and DefaultTLSPort are the TURN URI default ports.
	DefaultPlainPort int32 = 3478
	DefaultTLSPort   int32 = 5349

	// TransportUDP, TransportTCP and TransportTLS are the client transports a
	// TURN URI can name.
	TransportUDP = "udp"
	TransportTCP = "tcp"
	TransportTLS = "tls"

	// schemeTurn and schemeTurns are the TURN URI schemes.
	schemeTurn  = "turn"
	schemeTurns = "turns"
)

// Credentials derives the TURN REST API credential pair for a relay.
//
// The username carries the expiry and the password is the base64 HMAC-SHA1 of
// the username under the shared secret, which is exactly what coturn's
// --use-auth-secret expects and what the credential issuer in front of the pool
// produces. Deriving it here, from the same secret, is what makes the probe a
// real check of the pool's auth rather than a port scan.
func Credentials(secret, user string, ttl time.Duration, now time.Time) (username, password string) {
	username = strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + user
	mac := hmac.New(sha1.New, []byte(secret))
	// hash.Hash.Write never returns an error.
	_, _ = mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Endpoint is where and how to reach a relay listener.
type Endpoint struct {
	// Host is the relay address clients use.
	Host string

	// Port is the listener port.
	Port int32

	// Transport is "udp" or "tcp": the transport to the listener, not the
	// relayed transport.
	Transport string

	// Secure reports whether the URI used the turns: scheme.
	Secure bool
}

// Address returns the host:port to dial.
func (e Endpoint) Address() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port)))
}

// String renders the endpoint back as a TURN URI.
func (e Endpoint) String() string {
	scheme := schemeTurn
	if e.Secure {
		scheme = schemeTurns
	}
	return fmt.Sprintf("%s:%s?transport=%s", scheme, e.Address(), e.Transport)
}

// ParseURI parses a TURN URI as published in the pool's turnURIs.
//
// Accepted forms are `turn:host:port?transport=udp`, `turn:host:port` (udp by
// default), `turn://host:port?transport=tcp` and `turns:host:port?transport=tcp`
// (port defaults to 5349 for turns:).
func ParseURI(raw string) (Endpoint, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parsing TURN URI %q: %w", raw, err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != schemeTurn && scheme != schemeTurns {
		return Endpoint{}, fmt.Errorf("parsing TURN URI %q: scheme must be turn: or turns:, not %q", raw, parsed.Scheme)
	}
	endpoint := Endpoint{Secure: scheme == schemeTurns, Port: DefaultPlainPort}
	if endpoint.Secure {
		endpoint.Port = DefaultTLSPort
	}

	hostPort := parsed.Opaque
	if hostPort == "" {
		hostPort = parsed.Host
	}
	if hostPort == "" {
		return Endpoint{}, fmt.Errorf("parsing TURN URI %q: no host", raw)
	}

	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		// No port in the URI: keep the scheme default.
		endpoint.Host = hostPort
	} else {
		parsedPort, convErr := strconv.ParseInt(port, 10, 32)
		if convErr != nil {
			return Endpoint{}, fmt.Errorf("parsing TURN URI %q: invalid port %q: %w", raw, port, convErr)
		}
		endpoint.Host = host
		endpoint.Port = int32(parsedPort)
	}
	if endpoint.Host == "" {
		return Endpoint{}, fmt.Errorf("parsing TURN URI %q: no host", raw)
	}

	endpoint.Transport = strings.ToLower(parsed.Query().Get("transport"))
	if endpoint.Transport == "" {
		endpoint.Transport = TransportUDP
		if endpoint.Secure {
			endpoint.Transport = TransportTCP
		}
	}
	switch endpoint.Transport {
	case TransportUDP:
		if endpoint.Secure {
			return Endpoint{}, errors.New("a turns: URI carries TLS, which cannot run over udp")
		}
	case TransportTCP:
	default:
		return Endpoint{}, fmt.Errorf("parsing TURN URI %q: transport must be udp or tcp, not %q", raw, endpoint.Transport)
	}
	return endpoint, nil
}
