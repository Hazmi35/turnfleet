# Turnfleet — build, test and release automation.
#
# Everything this file downloads is pinned: Go tool versions, container images
# by tag plus digest, and the Kubernetes version used by envtest. The provenance
# of every pin lives in docs/versions.md. Go tools are declared as `tool`
# directives (root go.mod for repository tools, tools/go.mod for the lint
# toolchain) so there is a single place where their versions are resolved.

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GO ?= go

# Identity. ORG_DOMAIN drives the API group and every label/annotation prefix.
ORG ?= hzmi
ORG_DOMAIN ?= hzmi.dev
IMAGE_REGISTRY ?= ghcr.io/$(ORG)
IMAGE_TAG ?= dev

CONTROLLER_IMG ?= $(IMAGE_REGISTRY)/turnfleet-controller:$(IMAGE_TAG)
PROBE_IMG ?= $(IMAGE_REGISTRY)/turnfleet-probe:$(IMAGE_TAG)

# Pinned versions. Bump together with docs/versions.md.
CONTROLLER_TOOLS_VERSION ?= v0.22.0
ENVTEST_K8S_VERSION ?= 1.37.0
GOLANGCI_LINT_VERSION ?= v2.14.0
KUSTOMIZE_VERSION ?= v5.8.2
KIND_IMAGE ?= kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
COTURN_IMAGE ?= coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3
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
VERSION_PKG ?= github.com/hzmi/turnfleet/internal/version
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS ?= -s -w -X $(VERSION_PKG).version=$(VERSION) -X $(VERSION_PKG).commit=$(COMMIT) -X $(VERSION_PKG).date=$(DATE)

PLATFORMS ?= linux/amd64,linux/arm64
FUZZ_TIME ?= 30s
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
	@dirs=$$(git status --porcelain | awk '{print $$2}' | grep '\.go$$' | xargs -r -n1 dirname | sort -u | sed 's|^|./|' | paste -sd, -); \
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
	@hack/verify-no-claude-md.sh
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
test-e2e: kind-up install ## Run the end-to-end suite against the local kind cluster.
	$(MAKE) load-images
	$(MAKE) deploy
	KUBE_CONTEXT=$(KUBE_CONTEXT) KIND_CLUSTER=$(KIND_CLUSTER) COTURN_IMAGE="$(COTURN_IMAGE)" \
		$(GO) test -tags=e2e -count=1 -timeout 30m ./test/e2e/

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
load-images: ## Load the locally built images into the kind cluster.
	$(KIND) load docker-image $(CONTROLLER_IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(PROBE_IMG) --name $(KIND_CLUSTER)

.PHONY: install
install: manifests ## Install the CRDs into the kind cluster.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: ## Remove the CRDs from the kind cluster.
	-$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found -f -

.PHONY: deploy
deploy: ## Deploy the controller into the kind cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(CONTROLLER_IMG)
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -
	$(KUBECTL) -n turnfleet-system rollout status deployment/turnfleet-controller --timeout=180s

.PHONY: undeploy
undeploy: ## Remove the controller from the kind cluster.
	-$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found -f -

##@ Quality

.PHONY: vuln
vuln: ## Scan the module for known vulnerabilities.
	$(GOVULNCHECK) ./...

.PHONY: licenses
licenses: ## Fail if a dependency is incompatible with AGPL-3.0-only.
	$(LICENSE_CHECK) -v

.PHONY: dco
dco: ## Check that every commit in the branch is signed off.
	@hack/verify-dco.sh

.PHONY: ci
ci: verify lint test vuln licenses dco ## Run everything CI runs.

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
