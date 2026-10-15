// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package version

import (
	"runtime/debug"
	"testing"
)

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

	// The state a build without -ldflags leaves behind, which is what the
	// package ships with.
	version, commit, date = "dev", "unknown", "unknown"

	v, c, d := Info()

	// The version half can only be observed where the build info carries a
	// usable module version; the test binary's main module reports "(devel)",
	// which the fallback deliberately ignores. Both outcomes are pinned so that
	// dropping the lookup fails where it can be observed.
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("this binary carries no build info: the fallback cannot be observed")
	}
	switch usable := bi.Main.Version != "" && bi.Main.Version != "(devel)"; {
	case usable && v != bi.Main.Version:
		t.Errorf("Info() version = %q, want the module version %q the build info carries", v, bi.Main.Version)
	case !usable && v != "dev":
		t.Errorf("Info() version = %q, want the %q default: the build info carries no usable module version (%q)",
			v, "dev", bi.Main.Version)
	}

	// The date has no fallback at all, so it must come back untouched.
	if d != "unknown" {
		t.Errorf("Info() date = %q, want %q: nothing falls back to the build info for the date", d, "unknown")
	}

	// The commit is the half that does something observable here: it is read
	// out of the build's VCS settings. Read the same settings and hold Info()
	// to them, so that dropping the lookup fails this test.
	var revision string
	for _, setting := range bi.Settings {
		if setting.Key == "vcs.revision" {
			revision = setting.Value
		}
	}
	switch {
	case revision == "":
		// A `go test` binary is not VCS-stamped: its build info carries only
		// -buildmode, -compiler, CGO_*, GOARCH and GOOS. The commit fallback is
		// therefore unreachable from this layer and only a binary built with
		// `go build` can exercise it. Said out loud instead of skipped, so the
		// gap is visible.
		t.Log("this binary carries no vcs.revision setting: the commit fallback is unreachable from a test binary")
	case c != revision:
		t.Errorf("Info() commit = %q, want the build's vcs.revision %q", c, revision)
	}
}
