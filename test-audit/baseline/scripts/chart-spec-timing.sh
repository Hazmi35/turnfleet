#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Times the chart step's single spec with and without REUSE_IMAGES.
#
# The script behind a Session 2 claim in test-audit/REPORT.md. Kept next to
# its output so the measurement can be repeated rather than trusted.
# Why the chart step measures 46s where the derivation said ~35s.
#
# The step's own single spec, TestProbeCLIFromTheHost, reports 22-26s in all five
# CI runs against 7.5s in the one baseline sample (and 23.1s locally). This runs
# that spec verbosely three times in each of the two configurations — with and
# without REUSE_IMAGES — on one cluster, to see whether the image reuse is what
# the derivation missed.
set -uo pipefail
cd /home/hzmi/turnfleet
OUT=/tmp/audit2/chart-spec-timing.log
: > "$OUT"

run_spec() { # $1=label
  local s e rc spec
  s=$(date +%s.%N)
  KUBE_CONTEXT=kind-turnfleet KIND_CLUSTER=turnfleet COTURN_IMAGE="coturn/coturn:4.18.0-r0" \
  PROBE_BIN="$PWD/bin/turnfleet-probe" PROBE_IMG="ghcr.io/hazmi35/turnfleet-probe:dev" \
    go test -tags=e2e -count=1 -v -timeout 15m -run TestProbeCLIFromTheHost ./test/e2e/ \
    > "/tmp/audit2/chart-spec-$1.log" 2>&1
  rc=$?
  e=$(date +%s.%N)
  spec=$(grep -oE -- "--- (PASS|FAIL|SKIP): TestProbeCLIFromTheHost \([0-9.]+s\)" "/tmp/audit2/chart-spec-$1.log" | head -1)
  echo "$1 exit=$rc go-test-wall=$(python3 -c "print(f'{$e-$s:.1f}')")s spec='${spec:-<none>}'" >> "$OUT"
}

reinstall_chart() {
  make undeploy KUSTOMIZE_OVERLAY=config/e2e >> "$OUT" 2>&1
  make uninstall >> "$OUT" 2>&1
  make helm-install >> "$OUT" 2>&1
}

echo "### with REUSE_IMAGES=1 (no image build or load in the step)" >> "$OUT"
for i in 1 2 3; do reinstall_chart; run_spec "reuse-$i"; done

echo "### with REUSE_IMAGES=0 (the step rebuilds and reloads the images)" >> "$OUT"
for i in 1 2 3; do
  make docker-build >> "$OUT" 2>&1
  make load-images >> "$OUT" 2>&1
  reinstall_chart
  run_spec "noreuse-$i"
done

echo "### summary" >> "$OUT"
grep -E "^(reuse|noreuse)-[0-9] exit=" "$OUT" >> "$OUT"
echo ALLDONE >> "$OUT"
