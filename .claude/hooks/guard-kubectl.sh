#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# PreToolUse hook on Bash: deny kubectl commands that do not target the
# kind-turnfleet context, and deny switching contexts. The repository rule is
# that an agent touches exactly one cluster; everything else is the human's
# job. `make` targets set the context themselves and are unaffected.
set -uo pipefail

input="$(cat)"
cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // empty')"
[ -n "$cmd" ] || exit 0

deny() {
  jq -n --arg reason "$1" \
    '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $reason}}'
  exit 0
}

# Only look at commands that actually invoke kubectl as a command word.
printf '%s' "$cmd" | grep -qE '(^|[;&|(]|[[:space:]])kubectl([[:space:]]|$)' || exit 0

if printf '%s' "$cmd" | grep -qE 'kubectl[[:space:]]+config[[:space:]]+(use-context|set-context|rename-context|delete-context)'; then
  deny "Changing the kube context is not allowed. Use --context kind-turnfleet, or ask the human."
fi

# Every kubectl call in the command must carry the allow-listed context, and
# the context must not be overridden by KUBECONFIG switching helpers.
if ! printf '%s' "$cmd" | grep -qE 'kubectl([[:space:]][^;&|]*)?[[:space:]]--context[= ]kind-turnfleet([[:space:]]|$)'; then
  deny "kubectl must run against the kind-turnfleet context (kubectl --context kind-turnfleet ...). 'make' targets already do this."
fi

# A single command may chain several kubectl calls; require all of them to be
# scoped, by counting the calls and the scoped forms.
calls="$(printf '%s' "$cmd" | grep -oE '(^|[;&|(]|[[:space:]])kubectl([[:space:]]|$)' | wc -l)"
scoped="$(printf '%s' "$cmd" | grep -oE -- '--context[= ]kind-turnfleet' | wc -l)"
if [ "$calls" -gt "$scoped" ]; then
  deny "Every kubectl call in this command must specify --context kind-turnfleet."
fi

exit 0
