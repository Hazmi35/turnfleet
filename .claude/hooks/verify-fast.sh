#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Claude Code Stop adapter. `stop_hook_active` guards against looping on a
# condition the agent cannot fix; the check itself is
# hack/agent/verify-fast.sh, which every harness calls the same way. Its exit 2
# is Claude's "send stderr back to the model and keep working".
set -uo pipefail

input="$(cat)"
if [ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false' 2>/dev/null)" = "true" ]; then
  exit 0
fi

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0

exec "$root/hack/agent/verify-fast.sh"
