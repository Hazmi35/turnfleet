#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# The node-constraint experiment that worked: --memory and --memory-swap
# together, with the limits verified in force before any cycle ran.
#
# The script behind a Session 2 claim in test-audit/REPORT.md. Kept next to
# its output so the measurement can be repeated rather than trusted.
# Item 2, variant V6: the rollout step with the kind nodes genuinely
# resource-limited.
#
# The first attempt failed: `docker update --memory` is rejected unless
# memory-swap is set too ("Memory limit should be smaller than already set
# memoryswap limit"), and docker rejects the whole invocation, so neither the
# CPU nor the memory limit took effect and those 40 cycles were unconstrained.
# This one applies both and verifies they are in force before cycling.
set -uo pipefail
cd /home/hzmi/turnfleet
OUT=/tmp/audit2/flake-hunt-v6.log
NODES="turnfleet-control-plane turnfleet-worker turnfleet-worker2 turnfleet-worker3"
: > "$OUT"

echo "### before" >> "$OUT"
for n in $NODES; do docker inspect "$n" --format '{{.Name}} NanoCpus={{.HostConfig.NanoCpus}} Memory={{.HostConfig.Memory}} MemorySwap={{.HostConfig.MemorySwap}}' >> "$OUT"; done

for n in $NODES; do
  docker update --cpus=0.4 --memory=1500m --memory-swap=1500m "$n" >> "$OUT" 2>&1 || echo "update failed for $n" >> "$OUT"
done

echo "### after (must show non-zero NanoCpus and Memory)" >> "$OUT"
applied=0
for n in $NODES; do
  line=$(docker inspect "$n" --format '{{.Name}} NanoCpus={{.HostConfig.NanoCpus}} Memory={{.HostConfig.Memory}} MemorySwap={{.HostConfig.MemorySwap}}')
  echo "$line" >> "$OUT"
  case "$line" in
    *"NanoCpus=0 "*) ;;
    *) if echo "$line" | grep -qv "Memory=0"; then applied=$((applied+1)); fi ;;
  esac
done

if [ "$applied" -ne 4 ]; then
  echo "### ABORT: the limits did not take effect on all four nodes ($applied/4)" >> "$OUT"
  for n in $NODES; do docker update --cpu-quota=-1 --memory=0 "$n" >/dev/null 2>&1 || true; done
  echo ALLDONE >> "$OUT"
  exit 1
fi
echo "### limits in force on all four nodes" >> "$OUT"

for i in $(seq 1 20); do
  if ! kubectl --context kind-turnfleet get nodes >/dev/null 2>&1; then
    echo "V6 cycle=$i cluster unreachable, stopping" >> "$OUT"; break
  fi
  s=$(date +%s.%N)
  make deploy KUSTOMIZE_OVERLAY=config/e2e >> "$OUT" 2>&1
  rc=$?
  e=$(date +%s.%N)
  secs=$(python3 -c "print(f'{$e-$s:.1f}')")
  if [ "$rc" = "0" ]; then
    echo "V6 cycle=$i ok seconds=$secs" >> "$OUT"
  else
    echo "V6 cycle=$i FAILED exit=$rc seconds=$secs" >> "$OUT"
    mkdir -p /tmp/audit2/flake-dumps
    cp "$OUT" "/tmp/audit2/flake-dumps/failure-V6-$i.log"
  fi
done

for n in $NODES; do docker update --cpu-quota=-1 --memory=0 --memory-swap=0 "$n" >/dev/null 2>&1 || true; done
echo "### limits cleared" >> "$OUT"
grep -E "^V6 cycle=" "$OUT" | tail -3 >> "$OUT"
echo ALLDONE >> "$OUT"
