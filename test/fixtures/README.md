# Test fixtures

Files the end-to-end suite and the kind cluster need but cannot express in Go.

| Fixture | What it does | Used by |
| --- | --- | --- |
| `attach-secondary-address.sh` | Adds or removes a secondary address inside a kind node container's network namespace. `ExternalIPPool` binds relays to addresses that are already attached to a node, so the e2e case has to attach one first. | `test/e2e`, external-address case |
| `podip-attachment.yaml` | The Multus `NetworkAttachmentDefinition` the PodIP case names: macvlan (bridge) on the node's `eth0` with static IPAM, which takes the address from the pod's network selection (`capabilities.ips`). The address is not in the file, because the case declares it on the node from the kind network's own subnet. | `test/e2e`, `TestPodIPRelayOnAMultusAttachment` |
| `macvlan-shim.sh` | Adds or removes a macvlan interface on a kind node's `eth0` and routes the relay pod's address through it. A macvlan child and its parent cannot exchange traffic, so without it a controller pod on the relay's node could not probe the relay. | `test/e2e`, `TestPodIPRelayOnAMultusAttachment` |
| `multus-daemonset-thick.yaml` | Multus' own manifest for the thick plugin, vendored unchanged from `deployments/multus-daemonset-thick.yml` at the release `MULTUS_IMAGE` in the `Makefile` names. `make kind-multus` applies it with that pinned image in place of the `snapshot-thick` tag the file carries. Test tooling only, never shipped (`docs/versions.md`). | `make kind-up` |

The kind cluster itself is described in `hack/kind-config.yaml`: one control
plane and three workers labelled `turnfleet.hzmi.dev/relay-node=true`. That
label is what makes them candidates for relays. `make kind-up` then installs
the reference CNI plugins the node image lacks (`macvlan` and `static`, from
the pinned `containernetworking/plugins` release) into every node, and Multus,
so a local cluster and the CI cluster are the same.
