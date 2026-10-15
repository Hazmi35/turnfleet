#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# The first node-constraint attempt, kept as the record of a mistake: docker
# rejects --memory without --memory-swap and rejects the whole invocation, so
# no CPU or memory limit took effect and its 40 cycles were unconstrained.
#
# The script behind a Session 2 claim in test-audit/REPORT.md. Kept next to
# its output so the measurement can be repeated rather than trusted.
# Item 2, variant V4: the rollout step with the kind nodes resource-limited.
#
# CI runs four kind nodes on a 4-vCPU/16GiB runner and starts the rollout while
# three images are being imported into all four nodes. This box has 2 vCPU, so
# the untroubled variant (V1-V3 in flake-hunt.sh) is already starved relative to
# CI per node. Constraining the node containers is the one shape local has not
# tried: it starves the kubelet itself, which is what would leave an old pod
# unable to finish terminating.
#
# Reverts the limits at the end; `make kind-down && make kind-up` is the escape
# hatch if a node wedges.
set -uo pipefail
cd /home/hzmi/turnfleet
OUT=/tmp/audit2/flake-hunt-constrained.log
NODES="turnfleet-control-plane turnfleet-worker turnfleet-worker2 turnfleet-worker3"
: > "$OUT"

echo "### node container settings before" >> "$OUT"
for n in $NODES; do docker inspect "$n" --format '{{.Name}} NanoCpus={{.HostConfig.NanoCpus}} Memory={{.HostConfig.Memory}}' >> "$OUT"; done

apply_limits() { # $1=cpu seconds, $2=memory
  for n in $NODES; do docker update --cpus="$1" --memory="$2" "$n" >/dev/null; done
  echo "### limits applied: cpus=$1 memory=$2" >> "$OUT"
  for n in $NODES; do docker inspect "$n" --format '{{.Name}} NanoCpus={{.HostConfig.NanoCpus}} Memory={{.HostConfig.Memory}}' >> "$OUT"; done
}

clear_limits() {
  for n in $NODES; do docker update --cpus=0 --memory=0 "$n" >/dev/null 2>&1 || true; done
  echo "### limits cleared" >> "$OUT"
}

cycle() { # $1=label $2=index
  local s e rc secs
  s=$(date +%s.%N)
  make deploy KUSTOMIZE_OVERLAY=config/e2e >> "$OUT" 2>&1
  rc=$?
  e=$(date +%s.%N)
  secs=$(python3 -c "print(f'{$e-$s:.1f}')")
  if [ "$rc" = "0" ]; then
    echo "$1 cycle=$2 ok seconds=$secs" >> "$OUT"
  else
    echo "$1 cycle=$2 FAILED exit=$rc seconds=$secs" >> "$OUT"
    mkdir -p /tmp/audit2/flake-dumps
    cp "$OUT" "/tmp/audit2/flake-dumps/failure-$1-$2.log"
  fi
}

echo "### V4: 20 cycles with cpus=0.5 memory=2g" >> "$OUT"
apply_limits 0.5 2g
for i in $(seq 1 20); do
  if ! kubectl --context kind-turnfleet get nodes >/dev/null 2>&1; then
    echo "V4 cycle=$i cluster unreachable, stopping" >> "$OUT"; break
  fi
  cycle V4 "$i"
done

echo "### V5: 20 cycles with cpus=0.3 memory=1.5g" >> "$OUT"
apply_limits 0.3 1.5g
for i in $(seq 1 20); do
  if ! kubectl --context kind-turnfleet get nodes >/dev/null 2>&1; then
    echo "V5 cycle=$i cluster unreachable, stopping" >> "$OUT"; break
  fi
  cycle V5 "$i"
done

clear_limits
echo "### summary" >> "$OUT"
grep -cE " ok seconds=" "$OUT" | sed 's/^/ok cycles: /' >> /tmp/audit2/v4v5-summary.txt
grep -E "FAILED exit=" "$OUT" >> /tmp/audit2/v4v5-summary.txt
grep -E "^(V4|V5) cycle=" "$OUT" | tail -6 >> /tmp/audit2/v4v5-summary.txt
echo ALLDONE >> "$OUT"
