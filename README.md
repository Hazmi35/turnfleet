# Turnfleet

Turnfleet is a Kubernetes operator for TURN relay fleets.

TURN is the fallback path for WebRTC peers that cannot establish a direct path:
symmetric NAT, CGNAT, restrictive corporate and carrier networks. The usual
deployment — N relay replicas behind one load-balanced public IP — does not
work, and fails silently. A TURN allocation is a public `(IP, port)` pair, and
peer traffic sent to that pair must reach the exact process holding the
allocation. An L4 load balancer hashes on the 5-tuple, so replicas behind one
address add processes, not capacity, while every pod reports healthy. Turnfleet
gives every relay its own public relay identity — an address plus a disjoint
port range, delivered without a load balancer — and turns capacity exhaustion
into an observable signal.

## How it works

A `TURNPool` declares the realm and shared secret, the listeners, the relay port
range and an addressing mode. The operator places one TURN process (a **relay**)
per identity on nodes labelled `turnfleet.hzmi.dev/relay-node=true`, with
`hostNetwork: true`, so the kernel delivers traffic addressed to
`(node address, port)` to the one process that owns that port. Each relay gets a
rendered coturn configuration, a stable pod name derived from its identity, and
a readiness state that comes from a real TURN `Allocate` probe. Per-relay
`turn:` URIs for ready, non-draining relays are published in the
`<pool>-turn-uris` ConfigMap for the credential issuer to hand out. Scaling
happens by adding relay addresses (nodes, or secondary IPs), never by adding
replicas behind an address.

## Capacity, in two paragraphs

Capacity is ports, not processes. A relay can hold as many concurrent
allocations as there are ports in the range it owns, so a pool's capacity is the
number of distinct relay addresses times the ports per address. `HostNetwork`
runs one relay per labelled node on the node's own address and grows with the
node count; `ExternalIPPool` binds relays to secondary addresses already
attached to nodes, one relay per address, and grows with the address count;
`PartitionedIP` lets several relays share one node address, each owning a
disjoint slice of the port range — that adds concurrency and isolation but no
capacity, because the slices are still one IP's port space.

Two things follow. First, a port range cannot be exposed through a Kubernetes
Service or a cloud load balancer: Services have no port range field at all, and
a load balancer that did support ranges would still hash on the 5-tuple and
deliver peer packets to the wrong relay. So there is no Service in the relay
data path, and the relay nodes' firewall must allow the listener ports and the
relay range directly. Second, exhaustion must be visible per relay: the operator
reads allocation counts from coturn's own metrics endpoint, publishes
`turnfleet_relay_ports_in_use` / `turnfleet_relay_ports_total`, counts TURN
`508 Insufficient Capacity` answers observed by the probe in
`turnfleet_allocation_failures_total{code="508"}`, and raises
`CapacityPressure` when a relay is genuinely full. The full model, including
why an in-cluster probe can pass while external clients cannot connect, is in
[`docs/capacity-model.md`](docs/capacity-model.md). What the operator
deliberately does not do — including the cluster shapes `HostNetwork` cannot
work on, such as nodes with no public address — is in
[`docs/limitations.md`](docs/limitations.md).

## Scaling a pool

`TURNPool` exposes the Kubernetes scale subresource — `spec.replicas` in,
`status.readyRelays` out, `status.selector` as the relay pod set — and the port
space is published as `turnfleet_relay_ports_in_use` /
`turnfleet_relay_ports_total`, per relay. `config/samples/scaling-hpa.yaml`
drives a pool from an HPA on that subresource, and `config/samples/keda/` from a
KEDA `ScaledObject`; both trigger on ports in use per relay, never on a CPU or
pod-level metric, because a relay is near its limit long before its process is
busy. Pick one or the other: KEDA's admission webhook refuses a ScaledObject for
a workload an HPA already manages.

The caveat is the whole point of the project: **scaling does not add ports to an
address.** A new relay is only useful where the addressing mode has another
address to place it on — `HostNetwork` needs another labelled relay node,
`ExternalIPPool` another already attached address, and `PartitionedIP` gains
nothing at all, because the slices are still one IP's port space. A scaler on a
pool with nowhere left to place a relay sits at `maxReplicas` while the pool
stays at `CapacityPressure`; add addresses, not replicas. Scale-down is bounded
by draining, which is why both examples slow the scale-down path.

Scaling changes the relay set, so the credential issuer must re-read
`<pool>-turn-uris` after a scale event instead of caching it.

## Quickstart on kind

Prerequisites: Docker, Go (the version pinned in `go.mod`), and `make`. `kind`
and `kubectl` are also needed; `make kind-up` uses the `kind` binary from your
`PATH`.

```sh
make kind-up            # 1 control-plane + 3 workers labelled as relay nodes
make docker-build       # build both images for your platform
make load-images        # load them into the kind cluster
make install            # install the CRDs
make deploy             # deploy the controller (config/default: no admission webhooks)
```

Or install the controller from the chart, which is the same controller with the
kustomize step done for you — CRDs, RBAC, the manager, the metrics Service and,
with `--set webhook.enabled=true` on a cluster that has cert-manager, the
admission webhooks:

```sh
make helm-install       # chart install: CRDs, RBAC, the controller, metrics
```

`make helm-install` passes the image the Makefile just built and loaded, so it is
the same dev image `make deploy` runs. It renders what `config/default` renders;
`charts/turnfleet/values.yaml` lists what is configurable, `make helm-lint`
lints it, and `make helm-drift` is the guard that checks it against `config/`
(it fails when the two stop agreeing). Installing from the chart replaces the
two steps above, so run `make install`/`make deploy` or `make helm-install`, not
both.

A release publishes the same chart as an OCI artifact on ghcr.io, at the
release's version, so a consumer with no checkout installs the released chart:

```sh
helm install turnfleet oci://ghcr.io/hazmi35/turnfleet-chart/turnfleet \
  --version 0.1.1 --namespace turnfleet-system --create-namespace
```

Installed without `--set`, that chart runs the released
`turnfleet-controller:0.1.1`, which is the image the release published. The
release notes carry the chart's digest and the commands that verify it.

Upgrading is one step more than installing. Helm installs the files under
`crds/` on the first install and never updates an existing CRD, so a release
that changes the schema needs `make helm-crds-apply` before `helm upgrade`; the
kustomize path gets the same thing from `make install`.

Then create a pool. A minimal `HostNetwork` pool with three relays:

```yaml
apiVersion: turnfleet.hzmi.dev/v1alpha1
kind: TURNPool
metadata:
  name: demo
  namespace: default
spec:
  replicas: 3
  dataplane:
    image: coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3
  addressing:
    mode: HostNetwork
  relayPortRange:
    start: 49152
    end: 49200
  listeners:
    - name: turn
      protocol: udp
      port: 3478
  auth:
    realm: turnfleet.example.com
    secretRef:
      name: turn-shared-secret
      key: secret
```

```sh
kubectl --context kind-turnfleet -n default create secret generic turn-shared-secret \
  --from-literal=secret="$(openssl rand -hex 32)"
kubectl --context kind-turnfleet -n default apply -f pool.yaml
kubectl --context kind-turnfleet -n default get turnpools -w
kubectl --context kind-turnfleet -n default get cm demo-turn-uris -o jsonpath='{.data}'
```

Check a relay by hand, the way an external client would:

```sh
kubectl --context kind-turnfleet -n default get secret turn-shared-secret \
  -o jsonpath='{.data.secret}' | base64 -d > /tmp/turn-secret
make build
bin/turnfleet-probe --uri "turn:<relay-address>:3478?transport=udp" \
  --secret-file /tmp/turn-secret --expect-address <relay-address> --json
```

To have the controller report that check instead of leaving it in a terminal,
apply the vantage runner next to the pool (`config/vantage` is written for this
`demo` pool):

```sh
kubectl --context kind-turnfleet -n default apply -f config/vantage/runner.yaml
kubectl --context kind-turnfleet -n default create job --from=cronjob/vantage-runner vantage-runner-once
kubectl --context kind-turnfleet -n default get turnpool demo \
  -o jsonpath='{.status.conditions[?(@.type=="ReachableFromVantage")]}'
```

The runner's pod is on the cluster's network, so this exercises the delivery
path and the condition, not external reachability: for that the same command has
to run from outside the cluster
([`docs/adr/0015-vantage-result-delivery.md`](docs/adr/0015-vantage-result-delivery.md)).

`make test-e2e` runs the full suite against this cluster, including the
saturation and peer-policy cases.

## Repository layout

| Path | What lives there |
| --- | --- |
| `api/v1alpha1` | `TURNPool` types and their markers. |
| `internal/relay` | The planner (relay identities per addressing mode) and the coturn config renderer. |
| `internal/probe` | The TURN `Allocate` probe, shared by the controller and the CLI. |
| `internal/vantage` | The result a vantage runner publishes and where the controller reads it. |
| `internal/controller`, `internal/webhook` | Reconciliation and admission. |
| `cmd/` | `turnfleet-controller` and `turnfleet-probe`. |
| `config/vantage` | The vantage runner the operator applies per pool and does not schedule. |
| `charts/turnfleet` | The Helm chart: the same controller `config/` deploys, with the webhooks optional. Held against `config/` by `make helm-drift`. |
| `config/` | The kustomize manifests the chart mirrors: `config/default` (no admission webhooks) and `config/with-webhook` (cert-manager certificates). |
| `docs/` | Architecture, capacity model, ADRs, pinned versions, known limitations, generated API reference. |
| `test/e2e` | The kind end-to-end suite. |
| `.agents/` | The skills and subagent definitions every agent harness shares; `.claude/` and `.omp/` hold the per-harness wiring and symlinks, `hack/agent/` the hook policy. |

`AGENTS.md` is the single instruction file for coding agents, and every harness
here reads it.

The agent harness is harness-agnostic: the skills, the subagent definitions and
the hook policy live once under `.agents/` and `hack/agent/`, and `.claude/`
(Claude Code) and `.omp/` (omp) only wire them in — see `docs/adr/0010`. Editing
an alias instead of the canonical file fails `make verify`.

## Development

```sh
make help        # every target
make ci          # what CI runs: verify, helm-verify, lint, test, vuln, licenses, dco
make test-e2e    # the kind suite
make test-e2e-chart  # install the controller from the chart, then probe a relay
make helm-verify # lint the chart and check it against config/
```

Commits are Conventional Commits and must be signed off (`git commit -s`).

## License

AGPL-3.0-only. See [LICENSE](LICENSE). The TURN data plane is the upstream
`coturn/coturn` image, unmodified, under its own BSD-3-Clause license; nothing
from coturn is vendored into this repository.

The Go module is `github.com/Hazmi35/turnfleet` and the API group is
`turnfleet.hzmi.dev` (version `v1alpha1`, kind `TURNPool`). The project's own
labels, annotations and finalizers — the relay-node label, the relay-address and
config-hash annotations, the drain finalizer — carry the `turnfleet.hzmi.dev/`
prefix; the objects a pool owns also carry the standard `app.kubernetes.io/*`
labels.
