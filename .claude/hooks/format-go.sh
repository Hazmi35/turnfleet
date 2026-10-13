#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# PostToolUse hook: format a Go file right after Claude edits it, using the
# same formatters `make fmt` uses (gofumpt + gci via the custom golangci-lint
# build). Falls back to gofmt when the lint binary has not been built yet.
# Never fails the tool call: formatting is best-effort, `make verify` is the
# gate.
set -uo pipefail

input="$(cat)"
file="$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty')"

case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$file" ] || exit 0

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0

if [ -x "$root/bin/golangci-lint" ]; then
  (cd "$root" && bin/golangci-lint fmt "$file" >/dev/null 2>&1) || true
elif command -v gofmt >/dev/null 2>&1; then
  gofmt -w "$file" >/dev/null 2>&1 || true
fi

exit 0
