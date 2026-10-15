#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Both orders of the two e2e targets, three cycles each, plus a raw go test
# against a chart-installed cluster.
#
# The script behind a Session 2 claim in test-audit/REPORT.md. Kept next to
# its output so the measurement can be repeated rather than trusted.
# Item 3's proof: the two e2e targets must pass in either order.
#
#   order A: make test-e2e (config/e2e, plain-HTTP metrics) then make test-e2e-chart
#   order B: make test-e2e-chart (chart, secure metrics) then make test-e2e
#
# Three cycles each, plus one raw `go test` against a chart-installed cluster to
# show the metrics assertion now says why it cannot run instead of failing on a
# BadRequest.
set -uo pipefail
cd /home/hzmi/turnfleet
OUT=/tmp/audit2/e2e-orders.txt
: > "$OUT"

stage() { # $1=label, $2=suite|chart|raw
  local label=$1 kind=$2 s e rc
  s=$(date +%s.%N)
  case "$kind" in
    suite) make test-e2e >> "$OUT" 2>&1 ;;
    chart) make test-e2e-chart REUSE_IMAGES=1 >> "$OUT" 2>&1 ;;
    raw)   KUBE_CONTEXT=kind-turnfleet KIND_CLUSTER=turnfleet COTURN_IMAGE="coturn/coturn:4.18.0-r0" \
             PROBE_BIN="$PWD/bin/turnfleet-probe" PROBE_IMG="ghcr.io/hazmi35/turnfleet-probe:dev" \
             go test -tags=e2e -count=1 -v -timeout 15m -run TestSaturationBecomesObservable ./test/e2e/ >> "$OUT" 2>&1 ;;
  esac
  rc=$?
  e=$(date +%s.%N)
  echo "$label [$kind] exit=$rc seconds=$(python3 -c "print(f'{$e-$s:.1f}')")" >> "$OUT"
}

{
  echo "### provisioning: images built and loaded once, so the chart target's"
  echo "### REUSE_IMAGES=1 has something to reuse in order B's first cycle"
  make kind-up
  make docker-build
  make probe-bin
  make load-images
} >> "$OUT" 2>&1

for i in 1 2 3; do
  echo "===== order A cycle $i  $(date -u +%H:%M:%S) =====" >> "$OUT"
  stage "A$i" suite
  stage "A$i" chart
  if [ "$i" = "1" ]; then
    echo "===== raw go test against the chart install  $(date -u +%H:%M:%S) =====" >> "$OUT"
    stage "A1-raw" raw
  fi
done

for i in 1 2 3; do
  echo "===== order B cycle $i  $(date -u +%H:%M:%S) =====" >> "$OUT"
  stage "B$i" chart
  stage "B$i" suite
done

echo "### results" >> "$OUT"
grep -E "^(A[0-9]|B[0-9]|A1-raw)" "$OUT" >> "$OUT" 2>/dev/null
echo ALLDONE >> "$OUT"
