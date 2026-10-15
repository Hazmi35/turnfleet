#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# The rollout-flake hunt: 100 bare cycles, 20 after an image import, 20 with
# two CPU hogs on a 2-vCPU box.
#
# The script behind a Session 2 claim in test-audit/REPORT.md. Kept next to
# its output so the measurement can be repeated rather than trusted.
# Item 2: try to reproduce the 8-in-50 rollout flake locally.
#
# The failing step is `make deploy`'s rollout wait. Each cycle restarts the
# controller Deployment and waits up to 180s; `await-rollout` dumps the
# deployment, the pods, the nodes and the last 40 events when it times out, so a
# failure is captured with its evidence.
#
# Three variants, cheapest first:
#   V1 bare                — the step on its own
#   V2 after an image load — CI runs it right after three images are imported
#                            into four nodes, so containerd is busy
#   V3 with two CPU hogs   — a 2-vCPU box running a 4-node cluster is already
#                            starved relative to a 4-vCPU runner; this doubles
#                            down on it
set -uo pipefail
cd /home/hzmi/turnfleet
OUT=/tmp/audit2/flake-hunt.log
DUMPS=/tmp/audit2/flake-dumps
mkdir -p "$DUMPS"
: > "$OUT"

hogs() { # $1 = start|stop
  if [ "$1" = "start" ]; then
    for _ in 1 2; do ( command -v yes >/dev/null && exec yes > /dev/null ) & echo $! >> /tmp/audit2/hogs.pid; done
  else
    if [ -f /tmp/audit2/hogs.pid ]; then
      while read -r pid; do kill "$pid" 2>/dev/null || true; done < /tmp/audit2/hogs.pid
      rm -f /tmp/audit2/hogs.pid
    fi
  fi
}

cycle() { # $1=variant $2=index $3=pre (none|load)
  local v=$1 n=$2 pre=$3 s e rc secs
  s=$(date +%s.%N)
  if [ "$pre" = "load" ]; then
    make load-images >> "$OUT" 2>&1 || true
  fi
  make deploy KUSTOMIZE_OVERLAY=config/e2e >> "$OUT" 2>&1
  rc=$?
  e=$(date +%s.%N)
  secs=$(python3 -c "print(f'{$e-$s:.1f}')")
  if [ "$rc" = "0" ]; then
    echo "$v cycle=$n ok seconds=$secs" >> "$OUT"
  else
    echo "$v cycle=$n FAILED exit=$rc seconds=$secs  (dump follows above)" >> "$OUT"
    cp "$OUT" "$DUMPS/failure-$v-$n.log"
    {
      echo "=== failure $v/$n captured $DUMPS/failure-$v-$n.log at $(date -u +%H:%M:%S) ==="
      grep -n "await-rollout\|pending termination\|exit=$rc" "$OUT" | tail -5
    } >> "$OUT"
  fi
}

echo "### V1: 100 bare cycles" >> "$OUT"
for i in $(seq 1 100); do cycle V1 "$i" none; done

echo "### V2: 20 cycles after an image load (I/O pressure, like CI)" >> "$OUT"
for i in $(seq 1 20); do cycle V2 "$i" load; done

echo "### V3: 20 cycles with two CPU hogs" >> "$OUT"
hogs start
for i in $(seq 1 20); do cycle V3 "$i" none; done
hogs stop

echo "### summary" >> "$OUT"
grep -cE " ok seconds=" "$OUT" | sed 's/^/cycles ok: /' >> "$OUT"
grep -cE " FAILED exit=" "$OUT" | sed 's/^/cycles failed: /' >> "$OUT"
grep -E " FAILED exit=" "$OUT" >> "$OUT"
echo ALLDONE >> "$OUT"
