#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Helpers shared by the release scripts the release workflow runs. Sourced,
# never executed: the scripts take arguments and print a file, so every step the
# workflow takes can also be run by hand against a published digest.
set -euo pipefail

die() { printf '%s\n' "$*" >&2; exit 1; }

# repo_root is the checkout the sourcing script lives in. The workflow checks
# out the tagged commit before it calls any of these, so the Makefile and the
# scripts are the ones that tag carries.
repo_root() {
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

require_tool() {
  local tool="$1"
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
}

# makefile_pin prints the value make resolves NAME to in the Makefile.
#
# The Makefile holds one copy of every pin (docs/versions.md), and its values are
# written in terms of each other (`IMAGE_REGISTRY` is `ghcr.io/$(ORG)`), so this
# asks make to evaluate the pin instead of parsing the line. A generated
# makefile includes the repository's own and prints the one variable, which
# keeps make out of the loop for every other target.
makefile_pin() {
  local name="$1" root generated value status
  case "$name" in
    ''|*[!A-Za-z0-9_]*) die "makefile_pin: '$name' is not a variable name" ;;
  esac
  root="$(repo_root)"
  generated="$(mktemp)"
  # "$(%s)" is make's expansion of the pin, and the '$' is passed in as an
  # argument so that it reads as make syntax rather than as shell syntax.
  printf 'include %s/Makefile\npin-%s:\n\t@printf "%%s\\n" "%s(%s)"\n' \
    "$root" "$name" '$' "$name" >"$generated"
  status=0
  value="$(make -s -C "$root" -f "$generated" "pin-$name")" || status=$?
  rm -f "$generated"
  [ "$status" -eq 0 ] || die "makefile_pin: make could not resolve $name in the Makefile"
  [ -n "$value" ] || die "makefile_pin: the Makefile resolves $name to nothing"
  printf '%s\n' "$value"
}
