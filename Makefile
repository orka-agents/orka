# Image URL to use all building/pushing image targets
IMG ?= controller:latest
AI_WORKER_IMG ?= ghcr.io/orka-agents/orka/ai-worker:latest
GENERAL_WORKER_IMG ?= ghcr.io/orka-agents/orka/general-worker:latest
ACP_CODEX_RUNTIME_IMG ?= ghcr.io/orka-agents/orka/acp-codex-runtime:latest
ACP_CLAUDE_RUNTIME_IMG ?= ghcr.io/orka-agents/orka/acp-claude-runtime:latest
WORKSPACE_PUBLISHER_IMG ?= ghcr.io/orka-agents/orka/workspace-publisher:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: ensure-ui-embed
ensure-ui-embed: ## Create stub UI embed directory if not present (for go vet/build without full UI build).
	@if [ ! -d internal/uiembed/dist ]; then \
		mkdir -p internal/uiembed/dist && \
		echo '<!doctype html><html><body>stub</body></html>' > internal/uiembed/dist/index.html; \
	fi

.PHONY: vet
vet: ensure-ui-embed ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# The default e2e setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
KIND_CLUSTER ?= orka-test-e2e
E2E_GO_TEST_TIMEOUT ?= 30m
E2E_GINKGO_FOCUS ?=
E2E_GINKGO_FOCUS_ARG = $(if $(E2E_GINKGO_FOCUS),-ginkgo.focus="$(E2E_GINKGO_FOCUS)",)

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) --config test/e2e/kind-config.yaml ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -timeout $(E2E_GO_TEST_TIMEOUT) -v -ginkgo.v $(E2E_GINKGO_FOCUS_ARG)
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: test-e2e-setup-only
test-e2e-setup-only: setup-test-e2e docker-build-all ## Set up Kind cluster and build all images without running tests.
	@echo "Loading images into Kind cluster '$(KIND_CLUSTER)'..."
	$(KIND) load docker-image $(IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(AI_WORKER_IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(GENERAL_WORKER_IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(ACP_CODEX_RUNTIME_IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(ACP_CLAUDE_RUNTIME_IMG) --name $(KIND_CLUSTER)
	$(KIND) load docker-image $(WORKSPACE_PUBLISHER_IMG) --name $(KIND_CLUSTER)

.PHONY: test-e2e-run-only
test-e2e-run-only: manifests generate fmt vet ## Run e2e tests without rebuilding images (for fast iteration).
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -timeout $(E2E_GO_TEST_TIMEOUT) -v -ginkgo.v $(E2E_GINKGO_FOCUS_ARG)

.PHONY: lint
lint: ensure-ui-embed golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: ensure-ui-embed golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Demos

.PHONY: demo-cluster-up
demo-cluster-up: ## Bootstrap a kind cluster with Orka + kontxt + agent-sandbox
	hack/demos/cluster/cluster-up.sh
	hack/demos/cluster/install-kontxt.sh
	hack/demos/cluster/install-agent-sandbox.sh
	hack/demos/cluster/install-demo-model.sh

.PHONY: demo-cluster-down
demo-cluster-down: ## Tear down the kind demo cluster
	hack/demos/cluster/cluster-down.sh

.PHONY: demo-substrate-up
demo-substrate-up: ## Bootstrap a DEDICATED kind cluster with Agent Substrate + Orka (Demo 70)
	hack/demos/cluster/install-substrate.sh

.PHONY: demo-substrate-down
demo-substrate-down: ## Tear down the Agent Substrate demo cluster (Demo 70)
	kind delete cluster --name $${KIND_CLUSTER:-orka-agent-substrate-e2e}

.PHONY: demo-cluster-up-all
demo-cluster-up-all: ## ONE substrate-flavored kind cluster that runs ALL demos (00-70)
	hack/demos/cluster/install-substrate.sh
	ORKA_DEMO_CLUSTER=$${KIND_CLUSTER:-orka-agent-substrate-e2e} hack/demos/cluster/install-kontxt.sh
	ORKA_DEMO_CLUSTER=$${KIND_CLUSTER:-orka-agent-substrate-e2e} hack/demos/cluster/install-demo-model.sh
	ORKA_DEMO_CLUSTER=$${KIND_CLUSTER:-orka-agent-substrate-e2e} hack/demos/cluster/install-agent-sandbox.sh

.PHONY: demo-cluster-up-all-down
demo-cluster-up-all-down: ## Tear down the unified demo cluster
	kind delete cluster --name $${KIND_CLUSTER:-orka-agent-substrate-e2e}

.PHONY: demo-images
demo-images: ## Build + kind-load demo-only images (kontxt-caller + sandbox runtime)
	docker build -t ghcr.io/orka-agents/orka/kontxt-caller:demo hack/demos/images/kontxt-caller
	kind load docker-image ghcr.io/orka-agents/orka/kontxt-caller:demo --name $${ORKA_DEMO_CLUSTER:-orka-demo}
	docker build -t orka-sandbox-runtime:demo -f hack/demos/images/sandbox-runtime/Dockerfile .
	kind load docker-image orka-sandbox-runtime:demo --name $${ORKA_DEMO_CLUSTER:-orka-demo}

.PHONY: demo-test
demo-test: ## Run hack/demos smoke tests (style helpers, profile dispatch, payoff cards)
	bash hack/demos/lib/test/run-all.sh

##@ UI

.PHONY: ui-install
ui-install: ## Install UI dependencies.
	cd ui && bun install

.PHONY: ui-dev
ui-dev: ## Run UI dev server.
	cd ui && bun run dev

.PHONY: ui-build
ui-build: ui-install ## Build UI and copy to embed directory.
	cd ui && bun run build
	rm -rf internal/uiembed/dist
	cp -r ui/dist internal/uiembed/dist

.PHONY: ui-lint
ui-lint: ## Run UI linter.
	cd ui && bun run lint

.PHONY: ui-test
ui-test: ## Run UI unit tests.
	cd ui && bun run test

.PHONY: ui-test-coverage
ui-test-coverage: ## Run UI unit tests with coverage.
	cd ui && bun run test:coverage

##@ Build

.PHONY: build
build: manifests generate fmt vet ui-build ## Build manager binary.
	go build -o bin/manager cmd/main.go


.PHONY: docs-cli
docs-cli: build-cli ## Generate CLI command reference docs.
	scripts/generate-cli-docs.sh

.PHONY: docs-cli-check
docs-cli-check: build-cli ## Check generated CLI command reference docs are up to date.
	scripts/generate-cli-docs.sh --check

.PHONY: build-cli
build-cli: ## Build orka CLI binary.
	go build -ldflags "-X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)" -o bin/orka ./cmd/cli/

.PHONY: build-all
build-all: build build-cli ## Build all binaries.

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-build-ai-worker
docker-build-ai-worker: ## Build docker image for the AI worker.
	$(CONTAINER_TOOL) build -t ${AI_WORKER_IMG} -f workers/ai/Dockerfile .

.PHONY: docker-build-general-worker
docker-build-general-worker: ## Build docker image for the general worker.
	$(CONTAINER_TOOL) build -t ${GENERAL_WORKER_IMG} -f workers/general/Dockerfile .

.PHONY: docker-build-acp-codex-runtime
docker-build-acp-codex-runtime: ## Build the immutable Codex ACP runtime image.
	$(CONTAINER_TOOL) build -t ${ACP_CODEX_RUNTIME_IMG} -f workers/acp/images/codex/Dockerfile .

.PHONY: docker-build-acp-claude-runtime
docker-build-acp-claude-runtime: ## Build the immutable Claude ACP runtime image.
	$(CONTAINER_TOOL) build -t ${ACP_CLAUDE_RUNTIME_IMG} -f workers/acp/images/claude/Dockerfile .

.PHONY: docker-build-workspace-publisher
docker-build-workspace-publisher: ## Build the clean-room workspace publisher image.
	$(CONTAINER_TOOL) build -t ${WORKSPACE_PUBLISHER_IMG} -f workers/publisher/Dockerfile .

.PHONY: docker-push-ai-worker
docker-push-ai-worker: ## Push docker image for the AI worker.
	$(CONTAINER_TOOL) push ${AI_WORKER_IMG}

.PHONY: docker-push-general-worker
docker-push-general-worker: ## Push docker image for the general worker.
	$(CONTAINER_TOOL) push ${GENERAL_WORKER_IMG}

.PHONY: docker-push-acp-codex-runtime
docker-push-acp-codex-runtime: ## Push the immutable Codex ACP runtime image.
	$(CONTAINER_TOOL) push ${ACP_CODEX_RUNTIME_IMG}

.PHONY: docker-push-acp-claude-runtime
docker-push-acp-claude-runtime: ## Push the immutable Claude ACP runtime image.
	$(CONTAINER_TOOL) push ${ACP_CLAUDE_RUNTIME_IMG}

.PHONY: docker-push-workspace-publisher
docker-push-workspace-publisher: ## Push the clean-room workspace publisher image.
	$(CONTAINER_TOOL) push ${WORKSPACE_PUBLISHER_IMG}

.PHONY: docker-build-all
docker-build-all: docker-build docker-build-ai-worker docker-build-general-worker docker-build-acp-codex-runtime docker-build-acp-claude-runtime docker-build-workspace-publisher ## Build all docker images.

.PHONY: docker-push-all
docker-push-all: docker-push docker-push-ai-worker docker-push-general-worker docker-push-acp-codex-runtime docker-push-acp-claude-runtime docker-push-workspace-publisher ## Push all docker images.

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: verify-acp-runtime-images
verify-acp-runtime-images: ## Require digest-pinned ACP runtime images for supported deployments.
	@for entry in \
		"ACP_CODEX_RUNTIME_IMG=$(ACP_CODEX_RUNTIME_IMG)" \
		"ACP_CLAUDE_RUNTIME_IMG=$(ACP_CLAUDE_RUNTIME_IMG)"; do \
		name="$${entry%%=*}"; ref="$${entry#*=}"; \
		if [[ ! "$${ref}" =~ ^.+@sha256:[0-9a-f]{64}$$ ]]; then \
			echo "$$name must be an immutable image reference ending in @sha256:<64 lowercase hex characters>; got '$$ref'" >&2; \
			exit 1; \
		fi; \
	done

.PHONY: deploy
deploy: verify-acp-runtime-images manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	cd config/publisher && "$(KUSTOMIZE)" edit set image docker.io/sozercan/orka-workspace-publisher=${WORKSPACE_PUBLISHER_IMG}
	@"$(KUBECTL)" create namespace orka-system --dry-run=client -o yaml | "$(KUBECTL)" apply -f -
	@"$(KUBECTL)" -n orka-system create configmap acp-runtime-images \
		--from-literal=ORKA_ACP_CODEX_RUNTIME_IMAGE="${ACP_CODEX_RUNTIME_IMG}" \
		--from-literal=ORKA_ACP_CLAUDE_RUNTIME_IMAGE="${ACP_CLAUDE_RUNTIME_IMG}" \
		--dry-run=client -o yaml | "$(KUBECTL)" apply -f -
	@if ! "$(KUBECTL)" -n orka-system get secret acp-artifact-capability >/dev/null 2>&1; then \
		secret="$$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\n')"; \
		"$(KUBECTL)" -n orka-system create secret generic acp-artifact-capability --from-literal=capability-secret="$$secret"; \
	fi
	@if ! "$(KUBECTL)" -n orka-system get secret workspace-publisher-auth >/dev/null 2>&1; then \
		bearer="$$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\n')"; \
		capability="$$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\n')"; \
		"$(KUBECTL)" -n orka-system create secret generic workspace-publisher-auth --from-literal=controller-token="$$bearer" --from-literal=operation-capability-secret="$$capability"; \
	fi
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.20.0

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.7.2
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
