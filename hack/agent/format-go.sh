#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Format Go files that an agent has just written, with the same formatters
# `make fmt` uses (gofumpt + gci, through the custom golangci-lint build). Falls
# back to gofmt when that binary has not been built yet.
#
# Harness-neutral contract (see hack/agent/README.md): paths arrive as
# arguments. Formatting is best-effort and never fails the caller's tool call;
# `make verify` is the gate.
#
#   format-go.sh <file>...
#     exit 0  always
set -uo pipefail

root="${TURNFLEET_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0

for file in "$@"; do
  case "$file" in
    *.go) ;;
    *) continue ;;
  esac
  [ -f "$file" ] || continue

  if [ -x "$root/bin/golangci-lint" ]; then
    (cd "$root" && bin/golangci-lint fmt "$file" >/dev/null 2>&1) || true
  elif command -v gofmt >/dev/null 2>&1; then
    gofmt -w "$file" >/dev/null 2>&1 || true
  fi
done

exit 0
