// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package version carries the build metadata stamped into the Turnfleet
// binaries with -ldflags -X at build time.
package version

import "runtime/debug"

// These values are overridden at build time; see LDFLAGS in the Makefile.
//
//nolint:gochecknoglobals // set by the linker
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Version returns the semantic version or git description of the build.
func Version() string { return version }

// Commit returns the git revision the binary was built from.
func Commit() string { return commit }

// Date returns the build timestamp in RFC 3339 form.
func Date() string { return date }

// Info returns the version, commit and date, falling back to the module build
// info when the binary was built without the Makefile (for example `go run`).
func Info() (version, commit, date string) {
	v, c, d := version, commit, date
	if bi, ok := debug.ReadBuildInfo(); ok && v == "dev" {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && c == "unknown" {
				c = s.Value
			}
		}
	}
	return v, c, d
}
