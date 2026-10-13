// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Command licenses verifies that every module in the build list can be
// redistributed under the project license, AGPL-3.0-only.
//
// It resolves each module's license file from the module cache, classifies it
// with github.com/google/licensecheck, and fails when a license is on the
// deny list (copyleft-incompatible or source-available) or cannot be
// classified at all. Unclassifiable licenses must be reviewed by a human and
// added to hack/licenses/exceptions.txt with a reason, which keeps the
// exception scoped to a single module instead of widening the allow list.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/licensecheck"
)

// allowedLicenses are the licenses compatible with AGPL-3.0-only distribution.
//
//nolint:gochecknoglobals // immutable lookup table
var allowedLicenses = map[string]bool{
	"0BSD":              true,
	"AGPL-3.0":          true,
	"AGPL-3.0-only":     true,
	"AGPL-3.0-or-later": true,
	"Apache-2.0":        true,
	"BSD-2-Clause":      true,
	"BSD-3-Clause":      true,
	"CC0-1.0":           true,
	"ISC":               true,
	"MIT":               true,
	"MPL-2.0":           true,
	"Unlicense":         true,
	"Zlib":              true,
}

// reasonSourceAvailable is shared by the licenses that are not open source.
const reasonSourceAvailable = "source-available, not open source"

// deniedLicenses maps a license to the reason it cannot be used.
//
//nolint:gochecknoglobals // immutable lookup table
var deniedLicenses = map[string]string{
	"BUSL-1.1":     reasonSourceAvailable,
	"Elastic-2.0":  reasonSourceAvailable,
	"GPL-2.0":      "GPL-2.0-only is incompatible with AGPL-3.0-only",
	"GPL-2.0-only": "GPL-2.0-only is incompatible with AGPL-3.0-only",
	"SSPL-1.0":     reasonSourceAvailable,
}

// licenseFilePrefixes match candidate license file names case-insensitively:
// modules ship LICENSE, License, LICENCE, LICENSE.MIT, COPYING and friends.
//
//nolint:gochecknoglobals // immutable lookup table
var licenseFilePrefixes = []string{"license", "licence", "copying", "copyright"}

type module struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *module
}

type result struct {
	path    string
	version string
	license string
	reason  string
	err     error
}

func main() {
	verbose := flag.Bool("v", false, "print every module and its detected license")
	exceptionsPath := flag.String("exceptions", "hack/licenses/exceptions.txt", "file with reviewed per-module exceptions")
	flag.Parse()

	exceptions, err := loadExceptions(*exceptionsPath)
	if err != nil {
		fail(err)
	}

	modules, err := listModules()
	if err != nil {
		fail(err)
	}

	results := make([]result, 0, len(modules))
	failed := 0
	for _, m := range modules {
		res := checkModule(m, exceptions)
		if res.err != nil {
			failed++
		}
		results = append(results, res)
	}

	slices.SortFunc(results, func(a, b result) int { return strings.Compare(a.path, b.path) })
	for _, res := range results {
		switch {
		case *verbose:
			fmt.Printf("%-70s %-14s %s\n", res.path, res.version, res.license)
		case res.err != nil:
			fmt.Printf("FAIL %s %s (%s): %v\n", res.path, res.version, res.license, res.err)
		}
	}

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d module(s) are not AGPL-3.0-only compatible\n", failed)
		os.Exit(1)
	}
	fmt.Printf("licenses: %d modules checked, all compatible with AGPL-3.0-only\n", len(results))
}

func checkModule(m module, exceptions map[string]licenseException) result {
	res := result{path: m.Path, version: m.Version, license: "UNKNOWN"}
	if m.Replace != nil && m.Replace.Path != "" {
		// Check the replacement, which is what is actually built.
		res.path = m.Path + " => " + m.Replace.Path
		m.Dir = m.Replace.Dir
		m.Version = m.Replace.Version
	}

	id, err := detectLicense(m)
	if err != nil {
		res.err = err
		return res
	}
	res.license = id

	if exc, ok := exceptions[m.Path]; ok {
		if exc.license != id {
			res.err = fmt.Errorf("exception expects license %s but %s was detected", exc.license, id)
			return res
		}
		res.reason = "exception: " + exc.reason
		return res
	}
	if reason, denied := deniedLicenses[id]; denied {
		res.err = errors.New(reason)
		return res
	}
	if !allowedLicenses[id] {
		res.err = fmt.Errorf("unclassified license %q: review AGPL-3.0-only compatibility and add it to hack/licenses/exceptions.txt", id)
		return res
	}
	return res
}

func detectLicense(m module) (string, error) {
	if m.Main {
		return "AGPL-3.0-only", nil
	}
	if m.Dir == "" {
		dir, err := downloadModule(m.Path)
		if err != nil {
			return "", err
		}
		m.Dir = dir
	}
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return "", fmt.Errorf("reading module directory %s: %w", m.Dir, err)
	}
	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		for _, prefix := range licenseFilePrefixes {
			if strings.HasPrefix(name, prefix) {
				candidates = append(candidates, entry.Name())
				break
			}
		}
	}
	slices.Sort(candidates)
	for _, name := range candidates {
		data, err := os.ReadFile(filepath.Join(m.Dir, name))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", name, err)
		}
		cov := licensecheck.Scan(data)
		best := ""
		bestSpan := 0
		for _, match := range cov.Match {
			// licensecheck v0.3.1 reports byte offsets, not per-match
			// coverage: the widest match is the best classification.
			if span := match.End - match.Start; span > bestSpan {
				best, bestSpan = match.ID, span
			}
		}
		if best != "" {
			return best, nil
		}
	}
	return "", fmt.Errorf("no license file found in %s", m.Dir)
}

func downloadModule(path string) (string, error) {
	//nolint:gosec // `path` is a module path from `go list -m all`, not user input.
	out, err := exec.Command("go", "mod", "download", "-json", path).Output()
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", path, err)
	}
	var info struct{ Dir string }
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parsing download output for %s: %w", path, err)
	}
	return info.Dir, nil
}

func listModules() ([]module, error) {
	out, err := exec.Command("go", "list", "-m", "-json", "all").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m all: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var modules []module
	for dec.More() {
		var m module
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		modules = append(modules, m)
	}
	return modules, nil
}

type licenseException struct {
	license string
	reason  string
}

func loadExceptions(path string) (map[string]licenseException, error) {
	exceptions := map[string]licenseException{}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return exceptions, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want <module>\\t<license>\\t<reason>", path, i+1)
		}
		exceptions[fields[0]] = licenseException{license: fields[1], reason: fields[2]}
	}
	return exceptions, nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
	os.Exit(1)
}
