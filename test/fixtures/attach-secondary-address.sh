#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# attach-secondary-address.sh adds or removes a secondary address inside a kind
# node container's network namespace.
#
# The ExternalIPPool addressing mode binds relays to secondary addresses that are
# already attached to a node; kind nodes have none, so the e2e suite attaches
# them with this script. The address lives in the node's network namespace, which
# is the namespace a relay pod shares through hostNetwork, so a relay can bind it
# and the kind bridge routes to it from the host.
#
# Usage: attach-secondary-address.sh <node-container> <address> [add|del]

set -euo pipefail

node="${1:?node container name is required}"
address="${2:?address is required}"
action="${3:-add}"

case "$action" in
  add)
    # Idempotent: adding an address that is already there is not an error.
    if docker exec "$node" ip -o addr show dev eth0 | grep -qF "$address/"; then
      echo "$address is already attached to $node"
      exit 0
    fi
    docker exec "$node" ip addr add "$address/32" dev eth0
    echo "attached $address to $node"
    ;;
  del)
    docker exec "$node" ip addr del "$address/32" dev eth0 2>/dev/null || true
    echo "detached $address from $node"
    ;;
  *)
    echo "action must be add or del, got $action" >&2
    exit 2
    ;;
esac
