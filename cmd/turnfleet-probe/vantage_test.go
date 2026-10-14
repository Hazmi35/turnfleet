// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/vantage"
)

// TestValidateVantageOptions covers the flag combinations the runner refuses,
// because each of them would silently change what is probed.
func TestValidateVantageOptions(t *testing.T) {
	tests := map[string]struct {
		opts    options
		wantErr bool
	}{
		"the runner as config/vantage applies it": {
			opts: options{urisConfigmap: "demo-turn-uris", publishConfigmap: "demo-vantage", pool: "demo"},
		},
		"probing a list without publishing": {
			opts: options{urisConfigmap: "demo-turn-uris"},
		},
		"one URI, published": {
			opts: options{uri: "turn:203.0.113.10:3478?transport=udp", publishConfigmap: "demo-vantage", pool: "demo"},
		},
		"a URI and a list": {
			opts:    options{uri: "turn:203.0.113.10:3478?transport=udp", urisConfigmap: "demo-turn-uris"},
			wantErr: true,
		},
		"an expectation that only fits one URI": {
			opts:    options{urisConfigmap: "demo-turn-uris", expectAddress: "203.0.113.10"},
			wantErr: true,
		},
		"a port range a runner cannot know": {
			opts:    options{urisConfigmap: "demo-turn-uris", expectPortRange: "49152-49199"},
			wantErr: true,
		},
		"a peer for a list": {
			opts:    options{urisConfigmap: "demo-turn-uris", peerAddress: "203.0.113.9:5000"},
			wantErr: true,
		},
		"publishing without naming the pool": {
			opts:    options{publishConfigmap: "demo-vantage"},
			wantErr: true,
		},
		"naming a pool nothing is published for": {
			opts:    options{urisConfigmap: "demo-turn-uris", pool: "demo"},
			wantErr: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateVantageOptions(&test.opts)
			if test.wantErr && err == nil {
				t.Error("validateVantageOptions accepted a combination it must refuse")
			}
			if !test.wantErr && err != nil {
				t.Errorf("validateVantageOptions refused a valid combination: %v", err)
			}
		})
	}
}

// TestWriteVantageResult covers the two shapes the runner reports in: the
// payload the controller reads, and a line per listener for a human.
func TestWriteVantageResult(t *testing.T) {
	payload := &vantage.Payload{
		Pool:       "demo",
		ObservedAt: mustParseTime(t, "2026-10-13T21:04:05Z"),
		Results: []probe.Result{
			{URI: "turn:203.0.113.10:3478?transport=udp", Succeeded: true, Message: "ok", RelayedAddress: "203.0.113.10", RelayedPort: 49152},
			{URI: "turn:203.0.113.11:3478?transport=udp", Message: "cannot reach the relay listener"},
		},
	}

	var jsonOutput bytes.Buffer
	if err := writeVantageResult(&jsonOutput, payload, true); err != nil {
		t.Fatalf("writing the JSON result: %v", err)
	}
	decoded := &vantage.Payload{}
	if err := json.Unmarshal(jsonOutput.Bytes(), decoded); err != nil {
		t.Fatalf("the JSON output is not a payload: %v\n%s", err, jsonOutput.String())
	}
	if len(decoded.Results) != 2 || len(decoded.Failures()) != 1 {
		t.Errorf("the JSON output holds %d results and %d failures, want 2 and 1", len(decoded.Results), len(decoded.Failures()))
	}

	var human bytes.Buffer
	if err := writeVantageResult(&human, payload, false); err != nil {
		t.Fatalf("writing the human result: %v", err)
	}
	for _, want := range []string{"OK turn:203.0.113.10:3478?transport=udp", "FAIL turn:203.0.113.11:3478?transport=udp", "1 of 2 listener URI(s) answered"} {
		if !strings.Contains(human.String(), want) {
			t.Errorf("the human output does not contain %q:\n%s", want, human.String())
		}
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}
