#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Refuse to let an agent finish a turn while the cheap checks fail: runs
# `make verify-fast` (format, vet on changed packages, lint on new code) and
# reports the failure so the agent can fix it. A pristine tree has nothing to
# check and returns immediately.
#
# Harness-neutral contract (see hack/agent/README.md): no arguments, the verdict
# is the exit code.
#
#   verify-fast.sh
#     exit 0  the tree is clean, or the checks passed
#     exit 2  the checks failed; the tail of the output is on stderr
#     exit 3  the checks could not run (not inside a git work tree)
#
# It is not fast in wall-clock time: every invocation rebuilds the pinned
# golangci-lint and runs it — about 46s on the reference machine — so a harness
# that cancels hook handlers after a fixed budget must not call this.
set -uo pipefail

root="${TURNFLEET_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || { echo "verify-fast: not inside a git work tree" >&2; exit 3; }
cd "$root" || exit 3

# Nothing to check in a pristine tree.
if [ -z "$(git status --porcelain)" ]; then
  exit 0
fi

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

if make verify-fast >"$log" 2>&1; then
  exit 0
fi

{
  echo "make verify-fast failed. Fix it before finishing:"
  tail -n 60 "$log"
} >&2
exit 2
