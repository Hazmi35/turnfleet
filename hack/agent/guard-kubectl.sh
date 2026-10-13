#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# kubectl guard: deny a shell command that runs kubectl against any cluster
# other than kind-turnfleet, or that switches the kube context. The repository
# rule is that an agent touches exactly one cluster; everything else is the
# human's job. `make` targets set the context themselves and never call kubectl
# directly.
#
# Harness-neutral contract (see hack/agent/README.md): the command arrives as
# the single argument, the verdict is the exit code.
#
#   guard-kubectl.sh '<shell command>'
#     exit 0  the command may run
#     exit 2  the command must not run; the reason is on stderr
set -uo pipefail

cmd="${1-}"
[ -n "$cmd" ] || exit 0

deny() {
  printf '%s\n' "$1" >&2
  exit 2
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
