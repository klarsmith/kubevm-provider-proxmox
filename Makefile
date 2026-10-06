# Targets mirror kubevm-provider-container's Makefile. `make test` never
# reaches a real Proxmox: the controller runs against internal/proxmox.Fake.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

CONTROLLER_TOOLS_VERSION := v0.21.0
GOLANGCI_LINT_VERSION    := v2.14.0
ENVTEST_K8S_VERSION      := 1.37.0

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
GOLANGCI_LINT  := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
SETUP_ENVTEST  := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25

# The KubeVM commit the vendored VirtualMachine CRD comes from. Keep in step
# with the pseudo-version in go.mod.
KUBEVM_COMMIT := c2daa3ae87c10f2da2e6e58c0d182dab40c66f7d

IMG ?= ghcr.io/klarsmith/kubevm-provider-proxmox:dev
KUBECTL ?= kubectl

##@ Help

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Generate

.PHONY: generate
generate: generate-go generate-manifests ## Generate code and manifests

.PHONY: generate-go
generate-go: ## Generate deepcopy functions
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: generate-manifests
generate-manifests: ## Generate the CRD and RBAC
	$(CONTROLLER_GEN) paths=./... crd:crdVersions=v1 rbac:roleName=manager-role \
		output:crd:dir=config/crd/bases output:rbac:dir=config/rbac

.PHONY: kubevm-crd
kubevm-crd: ## Refresh the vendored KubeVM VirtualMachine CRD from KUBEVM_COMMIT
	curl -fsSL -o config/crd/kubevm/kube-vm.io_virtualmachines.yaml \
		https://raw.githubusercontent.com/vmware-tanzu/vm-operator/$(KUBEVM_COMMIT)/external/kubevm/config/crd/bases/kube-vm.io_virtualmachines.yaml

##@ Build and test

.PHONY: build
build: ## Compile the module
	go build ./...

.PHONY: manager
manager: ## Build the manager binary
	go build -o bin/manager ./cmd/manager

.PHONY: test
test: ## Unit tests and envtest. Never reaches a real Proxmox
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path)" \
		go test ./... -count=1

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: verify
verify: ## Fail if generated code is stale
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	cp -R api "$$tmp/api" && cp -R config "$$tmp/config" && \
	$(MAKE) --no-print-directory generate >/dev/null && \
	diff -ru "$$tmp/api" api && diff -ru "$$tmp/config" config

##@ Run

.PHONY: install
install: ## Install both CRDs into the current kubectl context
	$(KUBECTL) apply -f config/crd/kubevm/ -f config/crd/bases/

.PHONY: run
run: ## Run the manager locally against the current kubectl context
	go run ./cmd/manager

.PHONY: image-build
image-build: ## Build the manager image
	docker build -t $(IMG) .

.PHONY: deploy
deploy: ## Deploy the manager with kustomize
	$(KUBECTL) apply -k config

.PHONY: undeploy
undeploy: ## Remove the manager
	$(KUBECTL) delete --ignore-not-found -k config

##@ Local test PVE (Apple Silicon)

.PHONY: mac-pve-iso
mac-pve-iso: ## Build the unattended PVE ISO (PVE_ARCH=arm64|amd64)
	hack/mac-pve/build-iso.sh

.PHONY: mac-pve-run
mac-pve-run: ## Run the local test PVE in QEMU (PVE_ARCH=arm64|amd64; foreground)
	hack/mac-pve/run.sh
