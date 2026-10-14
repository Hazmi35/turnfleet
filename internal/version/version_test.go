// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package version

import "testing"

// TestInfoReportsWhatTheLinkerStamped pins the one thing the linker does to this
// package: it sets version, commit and date at build time, and every accessor
// has to read those variables rather than a shadow of them. Naming Info's
// result parameters after the variables is exactly the mistake that made every
// released binary report "turnfleet-probe  (, )".
func TestInfoReportsWhatTheLinkerStamped(t *testing.T) {
	before := struct{ version, commit, date string }{version, commit, date}
	t.Cleanup(func() {
		version, commit, date = before.version, before.commit, before.date
	})

	// What the linker does at build time, done here at test time.
	version, commit, date = "v9.9.9-test", "abcdef0-test", "2026-01-02T03:04:05Z-test"

	if got := Version(); got != version {
		t.Errorf("Version() = %q, want %q", got, version)
	}
	if got := Commit(); got != commit {
		t.Errorf("Commit() = %q, want %q", got, commit)
	}
	if got := Date(); got != date {
		t.Errorf("Date() = %q, want %q", got, date)
	}

	v, c, d := Info()
	if v != version || c != commit || d != date {
		t.Errorf("Info() = (%q, %q, %q), want (%q, %q, %q)", v, c, d, version, commit, date)
	}
}

// TestInfoFallsBackToBuildInfo covers the path a `go run` or a bare `go build`
// takes: the linker stamped nothing, so the version is the default and the
// commit comes from the build's VCS settings.
func TestInfoFallsBackToBuildInfo(t *testing.T) {
	before := struct{ version, commit, date string }{version, commit, date}
	t.Cleanup(func() {
		version, commit, date = before.version, before.commit, before.date
	})

	version, commit, date = "dev", "unknown", "unknown"

	v, _, _ := Info()
	if v == "" {
		t.Error("Info() reported an empty version with the linker's default in place: the fallback did not run")
	}
}
