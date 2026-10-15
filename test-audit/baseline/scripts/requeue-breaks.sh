#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# The eight-break matrix for the requeue and idempotency specs: one break per
# contract, each restored immediately, reporting which specs fail for it.
#
# The script behind a Session 2 claim in test-audit/REPORT.md. Kept next to
# its output so the measurement can be repeated rather than trusted.
# Break/restore matrix for the requeue and idempotency specs.
# Each break must fail the spec(s) that pin it, and nothing else. A break that
# fails to compile is reported as such instead of being read as "no failure".
set -uo pipefail
cd /home/hzmi/turnfleet
export KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64"
TC=internal/controller/turnpool_controller.go
ST=internal/controller/status.go
OUT=/tmp/audit2/requeue-breaks.txt
mkdir -p /tmp/audit2
: > "$OUT"

SPECS='TestReconcileOnAMissingPoolAsksForNothing|TestRequeueWhileTheAuthSecretIsMissing|TestRequeueInSteadyStateUsesTheDefaultProbeInterval|TestRequeueInSteadyStateFollowsTheSpecProbeInterval|TestRequeueWhileDrainingAScaledDownPool|TestRequeueWhileDeletingAPoolThatStillHasRelays|TestReconcileIsIdempotentOnUnchangedState|TestCarryTransitionTimes'

cp "$TC" /tmp/audit2/tc.orig
cp "$ST" /tmp/audit2/st.orig

apply_break() {
  python3 - "$1" <<'PY'
import sys
n = sys.argv[1]
tc = 'internal/controller/turnpool_controller.go'
st = 'internal/controller/status.go'

def sub(path, old, new):
    s = open(path).read()
    assert old in s, (path, old[:60])
    open(path, 'w').write(s.replace(old, new, 1))

if n == 'A':      # the Secret retry interval is retuned
    sub(tc, '\tsecretRetryInterval = 10 * time.Second', '\tsecretRetryInterval = 11 * time.Second')
elif n == 'B':    # the drain poll interval is retuned
    sub(tc, '\tdrainPollInterval = time.Second', '\tdrainPollInterval = 2 * time.Second')
elif n == 'C':    # the pool's own probe interval is ignored
    sub(tc, 'func probeInterval(pool *turnfleetv1alpha1.TURNPool) time.Duration {\n',
           'func probeInterval(pool *turnfleetv1alpha1.TURNPool) time.Duration {\n\treturn 10 * time.Second // AUDIT BREAK\n')
elif n == 'D':    # steady state asks for no work at all
    sub(tc, '\treturn ctrl.Result{RequeueAfter: r.requeueAfter(draining, pool)}, nil',
           '\t_ = draining\n\treturn ctrl.Result{}, nil // AUDIT BREAK')
elif n == 'E':    # the transition-time carry-over goes away
    sub(st, '\tcarryTransitionTimes(pool.Status, status)\n', '')
elif n == 'F':    # the carry-over applies to changed conditions too
    sub(st, '''		if prior.Status == condition.Status &&
			prior.Reason == condition.Reason &&
			prior.Message == condition.Message &&
			prior.ObservedGeneration == condition.ObservedGeneration {
			condition.LastTransitionTime = prior.LastTransitionTime
		}''',
        '\t\tcondition.LastTransitionTime = prior.LastTransitionTime // AUDIT BREAK')
elif n == 'G':    # a missing pool is reported as an error again
    sub(tc, '\t\treturn ctrl.Result{}, client.IgnoreNotFound(err)',
           '\t\treturn ctrl.Result{}, err // AUDIT BREAK')
elif n == 'H':    # a drained deleted pool keeps asking for another pass
    sub(st, '\t\tlog.V(1).Info("pool drained, finalizer removed", "pool", pool.Name, "namespace", pool.Namespace)\n\t\treturn ctrl.Result{}, nil',
           '\t\tlog.V(1).Info("pool drained, finalizer removed", "pool", pool.Name, "namespace", pool.Namespace)\n\t\treturn ctrl.Result{RequeueAfter: drainPollInterval}, nil // AUDIT BREAK')
PY
}

run_case() { # $1=label $2=break
  apply_break "$2"
  if diff -q "$TC" /tmp/audit2/tc.orig >/dev/null && diff -q "$ST" /tmp/audit2/st.orig >/dev/null; then
    echo "$1 -> BREAK NOT APPLIED" >> "$OUT"; return
  fi
  out=$(go test -count=1 -run "$SPECS" ./internal/controller/ 2>&1)
  if echo "$out" | grep -qE "build failed|\[build failed\]|cannot use|cannot find|declared and not used|undefined:"; then
    echo "$1 -> DID NOT COMPILE: $(echo "$out" | grep -m1 -E 'declared and not used|cannot find|undefined:|build failed')" >> "$OUT"
  else
    failed=$(echo "$out" | grep -oE '^--- FAIL: [A-Za-z]+' | sed 's/--- FAIL: //' | sort | tr '\n' ' ')
    echo "$1 -> failing: ${failed:-<none>}" >> "$OUT"
    echo "$out" | grep -E 'requeue_test\.go:[0-9]+:' | sed 's/^ */      /' | head -3 >> "$OUT"
  fi
  cp /tmp/audit2/tc.orig "$TC"; cp /tmp/audit2/st.orig "$ST"
}

echo "### baseline, no break" >> "$OUT"
base=$(go test -count=1 -run "$SPECS" ./internal/controller/ 2>&1)
echo "baseline -> $(echo "$base" | grep -E '^(ok|FAIL)' | head -1) / failing: $(echo "$base" | grep -cE '^--- FAIL')" >> "$OUT"

run_case "A secretRetryInterval 10s->11s"        A
run_case "B drainPollInterval 1s->2s"            B
run_case "C probeInterval ignores the pool's own" C
run_case "D steady state asks for nothing"       D
run_case "E carryTransitionTimes removed"        E
run_case "F carry-over also on changed condition" F
run_case "G missing pool returns an error"       G
run_case "H drained deletion still requeues"     H

echo "### implementation restored; leftover diff (want empty):" >> "$OUT"
git diff --stat "$TC" "$ST" >> "$OUT" 2>&1
cat "$OUT"
