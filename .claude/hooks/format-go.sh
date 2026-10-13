#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Claude Code PostToolUse adapter on Edit|Write|MultiEdit. Claude sends the tool
# call as JSON on stdin; the formatting itself is hack/agent/format-go.sh, which
# every harness calls the same way.
set -uo pipefail

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0

file="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
[ -n "$file" ] || exit 0

"$root/hack/agent/format-go.sh" "$file"
exit 0
