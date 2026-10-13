# Turnfleet — build, test and release automation.
#
# Everything this file downloads is pinned: Go tool versions, container images
# by tag plus digest, and the Kubernetes version used by envtest. Go tools are
# declared as `tool` directives (root go.mod for repository tools, tools/go.mod
# for the lint toolchain) so there is a single place where their versions are
# resolved; docs/versions.md records the pins that are deliberately held.

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GO ?= go

# Identity. ORG_DOMAIN drives the API group and every label/annotation prefix.
ORG ?= hazmi35
ORG_DOMAIN ?= hzmi.dev
IMAGE_REGISTRY ?= ghcr.io/$(ORG)
IMAGE_TAG ?= dev

CONTROLLER_IMG ?= $(IMAGE_REGISTRY)/turnfleet-controller:$(IMAGE_TAG)
PROBE_IMG ?= $(IMAGE_REGISTRY)/turnfleet-probe:$(IMAGE_TAG)

# Pinned versions. Every copy of one (the Dockerfiles, the workflows,
# .custom-gcl.yml) moves together; docs/versions.md records what is held.
CONTROLLER_TOOLS_VERSION ?= v0.22.0
ENVTEST_K8S_VERSION ?= 1.37.0
KUSTOMIZE_VERSION ?= v5.8.2
# Helm renders and lints the chart. It is not a Go tool, so it is downloaded
# from the release into $(LOCALBIN) and verified against the checksum the
# release publishes next to it (see the `helm` target). Renovate reads this
# line (customManager in renovate.json).
HELM_VERSION ?= v4.3.0
KIND_IMAGE ?= kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
# The coturn data plane. COTURN_IMAGE is the tag the kind cluster is loaded
# with (a multi-arch index digest cannot be loaded with `docker save`), and
# COTURN_IMAGE_DIGEST is what that tag must resolve to: `make load-coturn`
# verifies the two before loading. Relay pods in the e2e suite reference the
# tag, because it is what the cluster has; digest pinning itself is enforced by
# the validating webhook and covered by the webhook envtest suite.
COTURN_IMAGE ?= coturn/coturn:4.18.0-r0
COTURN_IMAGE_DIGEST ?= sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3
GO_IMAGE ?= golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
RUNTIME_IMAGE ?= gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Local cluster. `make deploy` and every kubectl invocation in this file target
# this context only; the repository rule is that no other context is touched.
KIND_CLUSTER ?= turnfleet
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)
KIND_CONFIG ?= hack/kind-config.yaml
KIND ?= kind
KUBECTL ?= kubectl --context $(KUBE_CONTEXT)

# Build metadata stamped into the binaries.
VERSION_PKG ?= github.com/Hazmi35/turnfleet/internal/version
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS ?= -s -w -X $(VERSION_PKG).version=$(VERSION) -X $(VERSION_PKG).commit=$(COMMIT) -X $(VERSION_PKG).date=$(DATE)

PLATFORMS ?= linux/amd64,linux/arm64
FUZZ_TIME ?= 30s
PROBE_BIN ?= bin/turnfleet-probe
CONTROLLER_GEN_CONFIG ?= hack/crd-ref-docs.yaml

LOCALBIN ?= $(shell pwd)/bin
export PATH := $(LOCALBIN):$(PATH)

# Repository tools, resolved through `tool` directives in go.mod.
CONTROLLER_GEN = $(GO) tool controller-gen
ENVTEST = $(GO) tool setup-envtest
GOVULNCHECK = $(GO) tool govulncheck
CRD_REF_DOCS = $(GO) tool crd-ref-docs
LICENSE_CHECK = $(GO) tool licenses
KUSTOMIZE = $(GO) tool kustomize

# The chart, and the helm binary that renders and lints it.
HELM = $(LOCALBIN)/helm
LOCAL_KUSTOMIZE = $(abspath $(LOCALBIN))/kustomize
HELM_CHART ?= charts/turnfleet
# Extra `--set` arguments for `make helm-install` and `make helm-render`, e.g.
# HELM_VALUES='--set webhook.enabled=true' on a cluster with cert-manager.
HELM_VALUES ?=

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: golangci-lint ## Format Go code with the configured formatters.
	"$(LOCALBIN)/golangci-lint" fmt

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: generate
generate: ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./..."

.PHONY: manifests
manifests: ## Generate CRDs, RBAC and webhook manifests.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:generateEmbeddedObjectMeta=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: api-docs
api-docs: ## Generate docs/api.md from the API types.
	$(CRD_REF_DOCS) --source-path=api/v1alpha1 --output-path=docs/api.md --renderer=markdown --config=$(CONTROLLER_GEN_CONFIG)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint with the repository configuration.
	"$(LOCALBIN)/golangci-lint" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply the fixes it can.
	"$(LOCALBIN)/golangci-lint" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify the golangci-lint configuration.
	"$(LOCALBIN)/golangci-lint" config verify

.PHONY: verify-fast
verify-fast: golangci-lint ## Format, vet and lint only what changed since HEAD.
	"$(LOCALBIN)/golangci-lint" fmt
	@dirs=$$(git status --porcelain | awk '{print $$2}' | grep '\.go$$' | xargs -r -n1 dirname | sort -u | sed 's|^|./|' | paste -sd, - || true); \
	if [ -n "$$dirs" ]; then $(GO) vet $$dirs; else echo "verify-fast: no changed packages to vet"; fi
	"$(LOCALBIN)/golangci-lint" run --new-from-rev=HEAD

.PHONY: verify
verify: ## Verify generated code, manifests, docs and module files produce no diff.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./..."
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:generateEmbeddedObjectMeta=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	$(CRD_REF_DOCS) --source-path=api/v1alpha1 --output-path=docs/api.md --renderer=markdown --config=$(CONTROLLER_GEN_CONFIG)
	$(GO) mod tidy
	cd tools && $(GO) mod tidy
	@hack/verify-headers.sh
	@hack/verify-harness-links.sh
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "verify: the working tree changed; run 'make generate manifests api-docs' and commit the result" >&2; \
		git status --short >&2; \
		exit 1; \
	fi

##@ Test

.PHONY: test
test: generate manifests envtest ## Run unit and envtest suites.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		$(GO) test -race -coverprofile=cover.out $$($(GO) list ./... | grep -v /test/e2e)

.PHONY: test-fuzz
test-fuzz: generate ## Fuzz the renderer and the webhook validators.
	$(GO) test -run '^$$' -fuzz '^FuzzRenderer$' -fuzztime $(FUZZ_TIME) ./internal/relay/
	$(GO) test -run '^$$' -fuzz '^FuzzWebhookValidation$$' -fuzztime $(FUZZ_TIME) ./internal/webhook/v1alpha1/

.PHONY: test-e2e
test-e2e: kind-up docker-build build install ## Run the end-to-end suite against the local kind cluster.
	$(MAKE) load-images
	$(MAKE) deploy KUSTOMIZE_OVERLAY=config/e2e
	KUBE_CONTEXT=$(KUBE_CONTEXT) KIND_CLUSTER=$(KIND_CLUSTER) COTURN_IMAGE="$(COTURN_IMAGE)" \
		PROBE_BIN="$(abspath $(PROBE_BIN))" \
		$(GO) test -tags=e2e -count=1 -timeout 30m ./test/e2e/

# The chart path through the same cluster: the controller (and the CRDs) come
# from `helm install` instead of kustomize, and the probe case from the suite
# runs against it. What `make test-e2e` deployed first is removed, because Helm
# will not adopt objects that kustomize created and the CRDs have to belong to
# the release that installs them.
.PHONY: test-e2e-chart
test-e2e-chart: kind-up docker-build build ## Install the CRDs and the controller from the chart, then probe a relay.
	$(MAKE) load-images
	$(MAKE) undeploy KUSTOMIZE_OVERLAY=config/e2e
	$(MAKE) uninstall
	$(MAKE) helm-install
	KUBE_CONTEXT=$(KUBE_CONTEXT) KIND_CLUSTER=$(KIND_CLUSTER) COTURN_IMAGE="$(COTURN_IMAGE)" \
		PROBE_BIN="$(abspath $(PROBE_BIN))" \
		$(GO) test -tags=e2e -count=1 -timeout 15m -run TestProbeCLIFromTheHost ./test/e2e/

.PHONY: envtest
envtest: ## Download the envtest control plane binaries.
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path >/dev/null
	@echo "envtest binaries ready for Kubernetes $(ENVTEST_K8S_VERSION)"

##@ Build

.PHONY: build
build: generate ## Build the controller and probe binaries.
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/turnfleet-controller ./cmd/turnfleet-controller
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/turnfleet-probe ./cmd/turnfleet-probe

.PHONY: run
run: generate ## Run the controller against the current kubeconfig.
	$(GO) run ./cmd/turnfleet-controller --leader-elect

.PHONY: docker-build
docker-build: ## Build both container images for the local platform.
	$(CONTAINER_TOOL) build \
		--build-arg GO_IMAGE=$(GO_IMAGE) --build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		-t $(CONTROLLER_IMG) -f Dockerfile .
	$(CONTAINER_TOOL) build \
		--build-arg GO_IMAGE=$(GO_IMAGE) --build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		-t $(PROBE_IMG) -f Dockerfile.probe .

CONTAINER_TOOL ?= docker

.PHONY: docker-buildx
docker-buildx: ## Build and push both images for $(PLATFORMS).
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) \
		--build-arg GO_IMAGE=$(GO_IMAGE) --build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		--provenance=true --sbom=true \
		-t $(CONTROLLER_IMG) -f Dockerfile .
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) \
		--build-arg GO_IMAGE=$(GO_IMAGE) --build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		--provenance=true --sbom=true \
		-t $(PROBE_IMG) -f Dockerfile.probe .

##@ Cluster

.PHONY: kind-up
kind-up: ## Create the local kind cluster if it does not exist.
	@if ! $(KIND) get clusters | grep -qx "$(KIND_CLUSTER)"; then \
		$(KIND) create cluster --config $(KIND_CONFIG) --image $(KIND_IMAGE) --wait 5m; \
	else \
		echo "kind cluster $(KIND_CLUSTER) already exists"; \
	fi

.PHONY: kind-down
kind-down: ## Delete the local kind cluster.
	-$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: load-images
load-images: load-coturn ## Load the controller, probe and coturn images into the kind cluster.
	$(KIND) load docker-image $(CONTROLLER_IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(PROBE_IMG) --name $(KIND_CLUSTER)

.PHONY: load-coturn
load-coturn: ## Pull the pinned coturn image and load it into the kind cluster.
	$(CONTAINER_TOOL) pull $(COTURN_IMAGE)
	@digest="$$($(CONTAINER_TOOL) image inspect $(COTURN_IMAGE) --format '{{index .RepoDigests 0}}')"; \
	case "$$digest" in \
		*$(COTURN_IMAGE_DIGEST)) echo "coturn image matches the pin: $$digest" ;; \
		*) echo "coturn image $$COTURN_IMAGE resolves to $$digest, which is not the pinned $(COTURN_IMAGE_DIGEST)" >&2; exit 1 ;; \
	esac
	$(KIND) load docker-image $(COTURN_IMAGE) --name $(KIND_CLUSTER)

.PHONY: install
install: manifests ## Install the CRDs into the kind cluster.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: ## Remove the CRDs from the kind cluster.
	-$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found -f -

# KUSTOMIZE_OVERLAY selects what `make deploy` applies. config/default runs the
# controller without admission webhooks (nothing else to install); use
# config/with-webhook on a cluster that has cert-manager, which is where the
# webhooks and their serving certificates live.
KUSTOMIZE_OVERLAY ?= config/default

.PHONY: deploy
deploy: ## Deploy the controller into the kind cluster.
	# The image is set on the cluster rather than in config/manager, so running
	# the target never leaves the working tree dirty.
	$(KUSTOMIZE) build $(KUSTOMIZE_OVERLAY) | $(KUBECTL) apply -f -
	$(KUBECTL) -n turnfleet-system set image deployment/turnfleet-controller-manager manager=$(CONTROLLER_IMG)
	# The tag is constant (dev), so a freshly loaded image needs a new pod:
	# without this, a running pod keeps the image it started with and the
	# cluster silently tests the previous build.
	$(KUBECTL) -n turnfleet-system rollout restart deployment/turnfleet-controller-manager
	$(KUBECTL) -n turnfleet-system rollout status deployment/turnfleet-controller-manager --timeout=180s

.PHONY: undeploy
undeploy: ## Remove the controller from the kind cluster.
	-$(KUSTOMIZE) build $(KUSTOMIZE_OVERLAY) | $(KUBECTL) delete --ignore-not-found -f -

##@ Helm

# The chart installs the controller, never the relays: relays come from the
# planner. charts/turnfleet renders what config/default and config/with-webhook
# render, which `make helm-drift` holds it to.

# helm is not a Go tool, so its version is a Makefile pin and the release is
# downloaded into $(LOCALBIN) with the checksum the release publishes next to
# it. HELM_VERSION is what Renovate moves (customManager in renovate.json).
.PHONY: helm
helm: ## Download the pinned helm binary into bin/.
	@mkdir -p "$(LOCALBIN)"
	@if [ -x "$(HELM)" ] && "$(HELM)" version --short 2>/dev/null | grep -qF "$(HELM_VERSION)"; then \
		echo "helm $(HELM_VERSION) is already in $(LOCALBIN)"; \
	else \
	os="$$($(GO) env GOOS)"; arch="$$($(GO) env GOARCH)"; \
	archive="helm-$(HELM_VERSION)-$$os-$$arch.tar.gz"; \
	url="https://get.helm.sh/$$archive"; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	curl -fsSL -o "$$tmp/$$archive" "$$url"; \
	curl -fsSL -o "$$tmp/$$archive.sha256sum" "$$url.sha256sum"; \
	(cd "$$tmp" && sha256sum --check --strict "$$archive.sha256sum"); \
	tar -xzf "$$tmp/$$archive" -C "$$tmp"; \
	install -m 0755 "$$tmp/$$os-$$arch/helm" "$(HELM)"; \
	fi
	@"$(HELM)" version --short

.PHONY: helm-lint
helm-lint: helm ## Lint the chart.
	"$(HELM)" lint --strict $(HELM_CHART)

.PHONY: helm-render
helm-render: helm ## Render the chart with the image the Makefile builds.
	"$(HELM)" template turnfleet $(HELM_CHART) --namespace turnfleet-system --include-crds \
		--set image.repository=$(IMAGE_REGISTRY)/turnfleet-controller --set image.tag=$(IMAGE_TAG) \
		$(HELM_VALUES)

.PHONY: helm-crds
helm-crds: ## Copy the generated CRDs from config/crd/bases into the chart.
	rm -f $(HELM_CHART)/crds/*.yaml
	cp config/crd/bases/*.yaml $(HELM_CHART)/crds/

.PHONY: helm-drift
helm-drift: helm kustomize ## Fail when the chart and config/ no longer render the same controller.
	# The kustomize binary is the one the `kustomize` target installs from
	# KUSTOMIZE_VERSION, the same pin `$(KUSTOMIZE) build` resolves.
	$(GO) run ./hack/helm-drift -helm "$(HELM)" -kustomize "$(LOCAL_KUSTOMIZE)" -chart $(HELM_CHART)

.PHONY: helm-verify
helm-verify: helm-lint helm-drift ## Lint the chart and check it against config/.

.PHONY: helm-install
helm-install: helm ## Install the controller from the chart into the kind cluster.
	# The image comes from the Makefile, the way `make deploy` sets it on the
	# cluster: the chart's own default is the release repository and the chart
	# version, which is not what a local kind cluster has loaded.
	$(HELM) upgrade --install turnfleet $(HELM_CHART) \
		--kube-context $(KUBE_CONTEXT) \
		--namespace turnfleet-system --create-namespace \
		--set image.repository=$(IMAGE_REGISTRY)/turnfleet-controller --set image.tag=$(IMAGE_TAG) \
		$(HELM_VALUES)
	# Same reason as `make deploy`: the tag is constant, so a freshly loaded
	# image needs a new pod.
	$(KUBECTL) -n turnfleet-system rollout restart deployment/turnfleet-controller-manager
	$(KUBECTL) -n turnfleet-system rollout status deployment/turnfleet-controller-manager --timeout=180s

.PHONY: helm-uninstall
helm-uninstall: helm ## Remove the chart from the kind cluster.
	-$(HELM) uninstall turnfleet --kube-context $(KUBE_CONTEXT) --namespace turnfleet-system

##@ Quality

# Some quality targets walk the whole dependency graph, which needs go.sum
# entries that `go mod tidy` deliberately prunes (the test graph, the module
# list). The Go tooling adds those entries while it runs, so these targets
# restore the file afterwards: a quality target must not leave the working tree
# dirty, or the next `make verify` fails for a reason that has nothing to do with
# the change under review.
# $1 is the command to run with go.sum held constant.
define with_clean_gosum
@cp go.sum "$(LOCALBIN)/go.sum.base"
@$(1) ; status=$$?; cp "$(LOCALBIN)/go.sum.base" go.sum; rm -f "$(LOCALBIN)/go.sum.base"; exit $$status
endef

.PHONY: vuln
vuln: $(LOCALBIN) ## Scan the module for known vulnerabilities.
	$(call with_clean_gosum,$(GOVULNCHECK) ./...)

.PHONY: licenses
licenses: $(LOCALBIN) ## Fail if a dependency is incompatible with AGPL-3.0-only.
	$(call with_clean_gosum,$(LICENSE_CHECK) -v)

.PHONY: dco
dco: ## Check that every commit in the branch is signed off.
	@hack/verify-dco.sh

.PHONY: ci
ci: verify helm-verify lint test vuln licenses dco ## Run everything CI runs.

##@ Tooling

.PHONY: golangci-lint
golangci-lint: ## Build the pinned golangci-lint with the module plugins.
	@mkdir -p "$(LOCALBIN)"
	@cd tools && GOBIN=$(abspath $(LOCALBIN)) $(GO) build -o $(abspath $(LOCALBIN))/golangci-lint-base github.com/golangci/golangci-lint/v2/cmd/golangci-lint
	@"$(LOCALBIN)/golangci-lint-base" custom --destination "$(LOCALBIN)" --name golangci-lint-custom
	@mv -f "$(LOCALBIN)/golangci-lint-custom" "$(LOCALBIN)/golangci-lint"
	@rm -f "$(LOCALBIN)/golangci-lint-base"
	@"$(LOCALBIN)/golangci-lint" version

.PHONY: kustomize
kustomize: ## Build the pinned kustomize binary into bin/.
	@mkdir -p "$(LOCALBIN)"
	@GOBIN=$(abspath $(LOCALBIN)) $(GO) install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)
