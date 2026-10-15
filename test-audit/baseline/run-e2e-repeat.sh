#!/usr/bin/env bash
# Phase 2 flakiness: rerun the E2E specs 5 times against the already-deployed cluster.
# Spec execution only: the cluster, images and controller are left in place by run-e2e.sh.
set -uo pipefail
cd /home/hzmi/turnfleet
OUT=/tmp/audit/flake-e2e.log
: > "$OUT"

for i in 1 2 3 4 5; do
  echo "===== e2e iteration $i  $(date -u +%H:%M:%S) =====" >> "$OUT"
  S=$(date +%s.%N)
  KUBE_CONTEXT="kind-turnfleet" KIND_CLUSTER="turnfleet" \
  COTURN_IMAGE="coturn/coturn:4.18.0-r0" \
  PROBE_BIN="/home/hzmi/turnfleet/bin/turnfleet-probe" \
  PROBE_IMG="ghcr.io/hazmi35/turnfleet-probe:dev" \
  go test -tags=e2e -count=1 -v -timeout 30m ./test/e2e/ >> "$OUT" 2>&1
  rc=$?
  E=$(date +%s.%N)
  echo "iteration $i exit=$rc seconds=$(python3 -c "print(f'{$E-$S:.1f}')")" >> "$OUT"
done
echo "DONE" >> "$OUT"
