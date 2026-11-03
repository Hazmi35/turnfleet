#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# nat-standin.sh adds or removes a 1:1 NAT on the host, standing in for the
# address translation a cloud does for a public address (an Elastic IP).
#
# Traffic to <advertised> is translated to <private>, an address a kind node
# holds, and the replies leave the node as <advertised>, because the host's
# connection tracking reverses the translation. No kind node holds <advertised>:
# a relay that binds it cannot start, so a relay that answers on it was bound to
# <private> and advertises <advertised>, which is what an ExternalIPPool entry
# with a bindAddress declares.
#
# Two kinds of traffic reach <advertised>, and each needs its own rule:
#
#   * the host's own (the e2e suite's probe) goes through the nat OUTPUT chain;
#   * a pod's or a node's (the controller's probe) arrives from the kind bridge
#     and goes through the nat PREROUTING chain.
#
# Translated traffic goes back out of the kind bridge, to a node on the same
# network as the sender. Without a third rule the replies would skip the host,
# and the sender would see an answer from <private>, not <advertised>. A
# MASQUERADE on translated traffic keeps the host in the path.
#
# The host also routes <advertised> out of the kind bridge, so the host's own
# traffic leaves with the bridge address as its source and a host that filters
# on the reverse path accepts the reply.
#
# It needs root, and uses sudo without a prompt when it is not root. The rules
# are inserted at the top of their chains, ahead of the container runtime's own,
# and removed by the del action. Both actions are idempotent.
#
# Usage: nat-standin.sh <advertised> <private> <bridge-interface> [add|del]

set -euo pipefail

advertised="${1:?advertised address is required}"
private="${2:?private address is required}"
bridge="${3:?host bridge interface of the kind network is required}"
action="${4:-add}"

sudo=()
if [[ "$(id -u)" -ne 0 ]]; then
  sudo=(sudo -n)
fi

# rule applies one iptables rule once. -C asks whether it exists.
rule() {
  local table="$1"
  shift
  if ! "${sudo[@]}" iptables -t "$table" -C "$@" 2>/dev/null; then
    "${sudo[@]}" iptables -t "$table" -I "$@"
  fi
}

# unrule removes a rule if it exists.
unrule() {
  local table="$1"
  shift
  while "${sudo[@]}" iptables -t "$table" -C "$@" 2>/dev/null; do
    "${sudo[@]}" iptables -t "$table" -D "$@"
  done
}

case "$action" in
  add)
    rule nat PREROUTING -d "$advertised" -j DNAT --to-destination "$private"
    rule nat OUTPUT -d "$advertised" -j DNAT --to-destination "$private"
    rule nat POSTROUTING -d "$private" -m conntrack --ctstate DNAT -j MASQUERADE
    "${sudo[@]}" ip route replace "$advertised/32" dev "$bridge"
    echo "translating $advertised to $private through $bridge"
    ;;
  del)
    unrule nat PREROUTING -d "$advertised" -j DNAT --to-destination "$private"
    unrule nat OUTPUT -d "$advertised" -j DNAT --to-destination "$private"
    unrule nat POSTROUTING -d "$private" -m conntrack --ctstate DNAT -j MASQUERADE
    "${sudo[@]}" ip route del "$advertised/32" dev "$bridge" 2>/dev/null || true
    echo "removed the translation of $advertised"
    ;;
  *)
    echo "action must be add or del, got $action" >&2
    exit 2
    ;;
esac
