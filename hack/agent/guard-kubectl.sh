#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# kubectl guard: deny a shell command that runs kubectl against a cluster the
# agent may not touch, that runs anything but a read against the production
# cluster, or that switches the kube context.
#
# Two contexts are allow-listed, with different powers:
#
#   kind-turnfleet      any verb: every make target and the e2e suite own it.
#   jepreto-production  read-only verbs, and never a Secret: an agent may look
#                       at production, a human changes it.
#
# Every kubectl call names its context on the command line; the context is never
# switched. A verb the script cannot read is denied rather than guessed at, and
# it only sees the literal word `kubectl`: an alias or a `$VAR` invocation is
# outside what this script can see and is out of bounds regardless.
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

# Changing the kube context is never allowed.
if printf '%s' "$cmd" | grep -qE 'kubectl[[:space:]]+config[[:space:]]+(use-context|set-context|rename-context|delete-context)'; then
  deny "Changing the kube context is not allowed. Name the context on every call."
fi

# Every kubectl call must carry one of the allow-listed contexts.
if ! printf '%s' "$cmd" | grep -qE 'kubectl([[:space:]][^;&|]*)?[[:space:]]--context[= ](kind-turnfleet|jepreto-production)([[:space:]]|$)'; then
  deny "kubectl must name --context kind-turnfleet or --context jepreto-production here."
fi

# A command may chain several kubectl calls; require every one to be scoped, by
# counting the calls and the scoped forms.
calls="$(printf '%s' "$cmd" | grep -oE '(^|[;&|(]|[[:space:]])kubectl([[:space:]]|$)' | wc -l)"
scoped="$(printf '%s' "$cmd" | grep -oE -- '--context[= ](kind-turnfleet|jepreto-production)' | wc -l)"
if [ "$calls" -gt "$scoped" ]; then
  deny "Every kubectl call in this command must name an allow-listed --context."
fi

# A production call is read-only and never touches a Secret.
if printf '%s' "$cmd" | grep -qE -- '--context[= ]jepreto-production'; then
  if printf '%s' "$cmd" | grep -qiE -- '(^|[^[:alnum:]_-])secrets?([^[:alnum:]_-]|$)'; then
    deny "Reading Secrets from jepreto-production is not allowed; ask the human."
  fi

  # The verb is read after the flags a read normally carries are stripped, so
  # `kubectl --context jepreto-production get pods` and `kubectl get pods -A
  # --context jepreto-production` read the same. A verb that cannot be found is
  # denied: a guard that guesses is not a guard.
  verbs="$(printf '%s' "$cmd" \
    | sed -E \
      -e 's/--context[= ][^[:space:];&|]+//g' \
      -e 's/(^|[[:space:]])(-n|--namespace)[= ][^[:space:];&|]+/\1/g' \
      -e 's/(^|[[:space:]])(-o|--output)[= ][^[:space:];&|]+/\1/g' \
      -e 's/(^|[[:space:]])(-l|--selector)[= ][^[:space:];&|]+/\1/g' \
      -e 's/(^|[[:space:]])--field-selector[= ][^[:space:];&|]+/\1/g' \
      -e 's/(^|[[:space:]])--kubeconfig[= ][^[:space:];&|]+/\1/g' \
      -e 's/(^|[[:space:]])(-v|--v)[= ][^[:space:];&|]+/\1/g' \
      -e 's/(^|[[:space:]])-A([[:space:]]|$)/\1/g' \
      -e 's/(^|[[:space:]])--all-namespaces([[:space:]]|$)/\1/g' \
    | grep -oE 'kubectl[[:space:]]+[a-z][a-z-]*' \
    | awk '{print $2}')"
  [ -n "$verbs" ] || deny "Could not read the kubectl verb; write it as --context jepreto-production <read-only verb> ..."
  while IFS= read -r verb; do
    [ -n "$verb" ] || continue
    case "$verb" in
      get|describe|logs|top|events|explain|api-resources|api-versions|version|cluster-info|diff) ;;
      *) deny "kubectl $verb is not read-only, and jepreto-production is read-only for agents." ;;
    esac
  done <<<"$verbs"
fi

exit 0
