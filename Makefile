SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------
# K8s Buddy — single entry point for humans and CI.
#
# CI calls only the targets below (never raw go/docker/kubectl commands), so
# local development and CI cannot drift out of sync.
# ---------------------------------------------------------------------------

CURRENT_MODULE := $(shell head -1 go.mod | awk '{print $$2}')
BIN_DIR      := bin
TOOLS_DIR    := .tools
COVER_FILE   := coverage.out

# GOEXE picks up the platform's executable suffix (".exe" on Windows, empty
# elsewhere) so the prerequisite path below actually matches what `go install`
# writes, and repeated `make lint` runs are idempotent on every OS.
GOEXE                  := $(shell go env GOEXE)
GOLANGCI_LINT_VERSION   := v2.12.2
GOLANGCI_LINT           := $(TOOLS_DIR)/golangci-lint$(GOEXE)

GIT_SHA  := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
VERSION  ?= $(GIT_SHA)

IMAGE_PREFIX := ghcr.io/sean-kramer/k8s-buddy
IMAGE_NAME   := buddy-api
IMAGE        := $(IMAGE_PREFIX)/$(IMAGE_NAME):$(GIT_SHA)
IMAGE_DEV    := $(IMAGE_PREFIX)/$(IMAGE_NAME):dev

KIND         := kind
KIND_CLUSTER := k8s-buddy
KIND_CONFIG  := deploy/kind/kind-config.yaml

.PHONY: help
help: ## Show this help
	@echo "K8s Buddy -- available targets:"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format all Go source in place with gofmt
	@gofmt -l -w .

.PHONY: vet
vet: ## Run go vet across the module (no-op on an empty module)
	@pkgs="$$(go list ./... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "vet: no Go packages yet, skipping"; \
	else \
		go vet ./...; \
	fi

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint using the pinned version in .tools/
	@pkgs="$$(go list ./... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "lint: no Go packages yet, skipping"; \
	else \
		$(GOLANGCI_LINT) run ./...; \
	fi

$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_DIR)
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) into $(TOOLS_DIR)..."
	@GOBIN="$$(pwd)/$(TOOLS_DIR)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: test
test: ## Run unit tests (no-op on an empty module)
	@pkgs="$$(go list ./... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "test: no Go packages yet, skipping"; \
	else \
		go test ./...; \
	fi

.PHONY: test-race
test-race: ## Run unit tests with the race detector (needs cgo; not run on this Windows dev box -- CI runs it on ubuntu-latest)
	@pkgs="$$(go list ./... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "test-race: no Go packages yet, skipping"; \
	else \
		go test -race ./...; \
	fi

.PHONY: test-cover
test-cover: ## Run unit tests with a coverage profile (no-op on an empty module)
	@pkgs="$$(go list ./... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "test-cover: no Go packages yet, skipping"; \
		: > $(COVER_FILE); \
	else \
		go test ./... -coverprofile=$(COVER_FILE) -covermode=atomic; \
		go tool cover -func=$(COVER_FILE); \
	fi

.PHONY: build
build: ## Build every ./cmd/* binary into bin/ (no-op until cmd/ exists)
	@mkdir -p $(BIN_DIR)
	@pkgs="$$(go list ./cmd/... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "build: no cmd/ packages yet, skipping"; \
	else \
		go build -trimpath -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o $(BIN_DIR)/ ./cmd/...; \
	fi

.PHONY: docker-build
docker-build: ## Build the buddy-api container image, tagged with the short git SHA and :dev
	docker build \
		-f build/Dockerfile.buddy-api \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE) \
		-t $(IMAGE_DEV) \
		.

.PHONY: kind-up
kind-up: ## Create the kind cluster (k8s-buddy) if it does not already exist
	@if $(KIND) get clusters 2>/dev/null | grep -qx '$(KIND_CLUSTER)'; then \
		echo "kind-up: cluster '$(KIND_CLUSTER)' already exists, skipping"; \
	else \
		$(KIND) create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG); \
	fi

.PHONY: kind-down
kind-down: ## Delete the kind cluster (k8s-buddy), succeeding even if it does not exist
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: kind-load
kind-load: ## Load the built buddy-api image (SHA tag and :dev) into the kind cluster (k8s-buddy)
	$(KIND) load docker-image $(IMAGE) $(IMAGE_DEV) --name $(KIND_CLUSTER)

.PHONY: deploy
deploy: ## Apply the Kubernetes manifests (deploy/kustomize/base) to the current kubectl context
	kubectl apply -k deploy/kustomize/base

.PHONY: undeploy
undeploy: ## Remove the Kubernetes manifests (deploy/kustomize/base) from the current kubectl context
	kubectl delete -k deploy/kustomize/base --ignore-not-found

.PHONY: status
status: ## Show buddy-api pods/services/PDB and rollout status in the k8s-buddy namespace
	kubectl -n k8s-buddy get pods,svc,pdb -o wide
	kubectl -n k8s-buddy rollout status deployment/buddy-api

.PHONY: logs
logs: ## Tail logs from every buddy-api pod in the k8s-buddy namespace
	kubectl -n k8s-buddy logs -l app.kubernetes.io/name=buddy-api --all-containers --tail=200 -f

.PHONY: demo
demo: kind-up docker-build kind-load deploy ## Full self-healing demo: kind-up -> docker-build -> kind-load -> deploy -> wait for rollout -> hack/demo.sh
	kubectl -n k8s-buddy rollout status deployment/buddy-api --timeout=120s
	bash hack/demo.sh

.PHONY: clean
clean: ## Remove build artifacts and coverage output (leaves .tools/ intact; see `tools-clean`)
	@rm -rf $(BIN_DIR) $(COVER_FILE)

.PHONY: tools
tools: $(GOLANGCI_LINT) ## Install/update pinned local developer tooling into .tools/

.PHONY: tools-clean
tools-clean: ## Remove downloaded local tooling
	@rm -rf $(TOOLS_DIR)

.PHONY: rename-module
rename-module: ## Rewrite the module path everywhere (usage: make rename-module MODULE=github.com/you/repo)
	@if [ -z "$(MODULE)" ]; then \
		echo "ERROR: MODULE is not set. Usage: make rename-module MODULE=github.com/you/repo" >&2; \
		exit 1; \
	fi
	@old="$(CURRENT_MODULE)"; \
	new="$(MODULE)"; \
	echo "Renaming module from $$old to $$new..."; \
	find . -type f -name '*.go' \
		-not -path './.git/*' \
		-not -path './.tools/*' \
		-not -path './bin/*' \
		-print0 | xargs -0 -r sed -i "s|$$old|$$new|g"; \
	sed -i "s|^module .*|module $$new|" go.mod; \
	go mod tidy
	@echo "Module renamed to $(MODULE). Review the diff, then re-run 'make build test'."
