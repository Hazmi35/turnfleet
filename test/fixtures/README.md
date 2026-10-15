# Test fixtures

Helpers the end-to-end suite needs but cannot express in Go, because they act
outside the cluster.

| Fixture | What it does | Used by |
| --- | --- | --- |
| `attach-secondary-address.sh` | Adds or removes a secondary address inside a kind node container's network namespace. `ExternalIPPool` binds relays to addresses that are already attached to a node, so the e2e case has to attach one first. | `test/e2e`, external-address case |

The kind cluster itself is described in `hack/kind-config.yaml`: one control
plane and three workers labelled `turnfleet.hzmi.dev/relay-node=true`. That
label is what makes them candidates for relays.
