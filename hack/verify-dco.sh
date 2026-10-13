#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# verify-dco.sh fails when a commit in the checked range is not signed off
# (Signed-off-by: Name <email>), which the DCO requires.
#
# The range defaults to origin/main..HEAD when that ref exists, and to the full
# history otherwise, so the check works in CI and on a fresh local clone.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if [ -n "${DCO_RANGE:-}" ]; then
  range="$DCO_RANGE"
elif git rev-parse --verify --quiet origin/main >/dev/null; then
  range="origin/main..HEAD"
else
  range="HEAD"
fi

status=0
while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  if ! git log -1 --format=%B "$sha" | grep -qE '^Signed-off-by: .+ <.+@.+>$'; then
    echo "missing Signed-off-by: $(git log -1 --format='%h %s' "$sha")" >&2
    status=1
  fi
done < <(git rev-list "$range")

if [ "$status" -ne 0 ]; then
  echo "verify-dco: amend with 'git commit -s'" >&2
fi
exit "$status"
