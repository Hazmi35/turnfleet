#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Stop hook: refuse to finish a turn while the cheap checks fail. This runs
# `make verify-fast` (format, vet on changed packages, lint on new code) and
# sends Claude back to work with the failure output when it does not pass.
# `stop_hook_active` guards against looping on a condition that cannot be fixed.
set -uo pipefail

input="$(cat)"
if [ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false')" = "true" ]; then
  exit 0
fi

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0
cd "$root" || exit 0

# Nothing to check in a pristine tree.
if [ -z "$(git status --porcelain)" ]; then
  exit 0
fi

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

if ! make verify-fast >"$log" 2>&1; then
  {
    echo "make verify-fast failed. Fix it before finishing:"
    tail -n 60 "$log"
  } >&2
  exit 2
fi

exit 0
