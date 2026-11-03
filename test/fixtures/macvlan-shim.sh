#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# macvlan-shim.sh adds or removes a macvlan interface on a kind node's eth0, so
# the node can reach a pod's macvlan address on the same node.
#
# A macvlan child (the pod's net1) and its parent (the node's eth0) cannot
# exchange traffic: the kernel delivers a frame between two macvlan interfaces
# of one parent, and never between a child and the parent itself. The
# controller probes a relay from its own pod, which may run on the relay's
# node, and reaches the relay through the node's routing table, so without this
# shim that probe never answers although the host and every other node reach
# the relay. The shim is the usual fix: a second child of the same parent in
# the node's namespace, with the pod's address routed through it.
#
# Usage: macvlan-shim.sh <node-container> <shim-address> <pod-address> [add|del]

set -euo pipefail

node="${1:?node container name is required}"
shim_address="${2:?shim address is required}"
pod_address="${3:?pod address is required}"
action="${4:-add}"

shim="turnfleet-shim"

case "$action" in
  add)
    # Idempotent: a shim left by an earlier run is replaced.
    docker exec "$node" ip link del "$shim" 2>/dev/null || true
    docker exec "$node" ip link add "$shim" link eth0 type macvlan mode bridge
    docker exec "$node" ip addr add "$shim_address/32" dev "$shim"
    docker exec "$node" ip link set "$shim" up
    docker exec "$node" ip route add "$pod_address/32" dev "$shim"
    echo "added $shim ($shim_address) on $node, routing $pod_address through it"
    ;;
  del)
    docker exec "$node" ip link del "$shim" 2>/dev/null || true
    echo "removed $shim from $node"
    ;;
  *)
    echo "action must be add or del, got $action" >&2
    exit 2
    ;;
esac
