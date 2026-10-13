#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Claude Code PreToolUse adapter on Bash. Claude sends the tool call as JSON on
# stdin and reads the decision as JSON on stdout; all of the policy lives in
# hack/agent/guard-kubectl.sh, which every harness calls the same way.
set -uo pipefail

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0

cmd="$(jq -r '.tool_input.command // empty' 2>/dev/null)"
[ -n "$cmd" ] || exit 0

reason="$("$root/hack/agent/guard-kubectl.sh" "$cmd" 2>&1 >/dev/null)"
status=$?
[ "$status" -eq 0 ] && exit 0

jq -n --arg reason "$reason" \
  '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $reason}}'
exit 0
