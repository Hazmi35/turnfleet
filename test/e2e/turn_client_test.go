//go:build e2e

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package e2e

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"

	"github.com/Hazmi35/turnfleet/internal/probe"
)

// turnClient is a TURN client used to hold allocations, which is how the suite
// creates the state a relay must protect: a peer that is being served.
type turnClient struct {
	conn   net.PacketConn
	client *turn.Client
}

// dialTurn opens a TURN client to a relay listener with credentials derived from
// the pool's shared secret.
func dialTurn(t *testing.T, address string, port int32) *turnClient {
	t.Helper()

	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatalf("opening a udp socket: %v", err)
	}

	username, password := probe.Credentials(sharedSecret, "turnfleet-e2e", time.Hour, time.Now())
	client, err := turn.NewClient(&turn.ClientConfig{
		Conn:           conn,
		STUNServerAddr: net.JoinHostPort(address, strconv.Itoa(int(port))),
		TURNServerAddr: net.JoinHostPort(address, strconv.Itoa(int(port))),
		Username:       username,
		Password:       password,
		LoggerFactory:  quietLoggerFactory(),
	})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("creating a TURN client for %s:%d: %v", address, port, err)
	}
	if err := client.Listen(); err != nil {
		client.Close()
		_ = conn.Close()
		t.Fatalf("starting the TURN client for %s:%d: %v", address, port, err)
	}
	return &turnClient{conn: conn, client: client}
}

func (c *turnClient) allocate(ctx context.Context) (net.PacketConn, error) {
	return c.client.AllocateWithContext(ctx)
}

func (c *turnClient) close() {
	c.client.Close()
	_ = c.conn.Close()
}

// quietLoggerFactory silences the TURN client's own logging so the suite's
// output is its own.
func quietLoggerFactory() logging.LoggerFactory {
	return &logging.DefaultLoggerFactory{
		Writer:          io.Discard,
		DefaultLogLevel: logging.LogLevelDisabled,
		ScopeLevels:     map[string]logging.LogLevel{},
	}
}
