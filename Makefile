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
BUILD_DIR    := .build
COVER_FILE   := coverage.out

# GOEXE picks up the platform's executable suffix (".exe" on Windows, empty
# elsewhere) so the prerequisite path below actually matches what `go install`
# writes, and repeated `make lint` runs are idempotent on every OS.
GOEXE                  := $(shell go env GOEXE)
GOLANGCI_LINT_VERSION   := v2.12.2
GOLANGCI_LINT           := $(TOOLS_DIR)/golangci-lint$(GOEXE)
CONTROLLER_GEN_VERSION  := v0.21.0
CONTROLLER_GEN          := $(TOOLS_DIR)/controller-gen$(GOEXE)
# Pinned to the same release as sigs.k8s.io/controller-runtime in go.mod --
# setup-envtest is published from controller-runtime's own tools/setup-envtest
# submodule, tagged in lockstep with the runtime itself.
SETUP_ENVTEST_VERSION   := v0.24.1
SETUP_ENVTEST           := $(TOOLS_DIR)/setup-envtest$(GOEXE)
# The Kubernetes control-plane version envtest boots. Must match
# envtestK8sVersion in internal/controller/suite_test.go -- there is no
# single source of truth shared between make and go test for this value.
ENVTEST_K8S_VERSION     := 1.36.2

# Where controller-gen's `object` (deepcopy) and `crd`/`rbac` generators look
# for +kubebuilder markers, and where the CRD/RBAC generators write their
# output. ./api/... itself has no .go files directly (only its v1alpha1
# subpackage does), and controller-gen v0.21's loader errors on an empty root
# rather than skipping it, so this points at the package directly.
# ./internal/controller/... is included from Task 5 onward, once
# +kubebuilder:rbac markers exist on the reconciler; `generate` (the
# deepcopy generator) only ever needs api/v1alpha1, but sharing one
# MANIFEST_DIRS var for both crd and rbac generation in the `manifests`
# target below keeps a single combined controller-gen invocation possible.
API_DIRS      := ./api/v1alpha1/...
MANIFEST_DIRS := ./api/v1alpha1/... ./internal/controller/...
CRD_DIR       := config/crd/bases
RBAC_DIR      := config/rbac
RBAC_ROLE_NAME := plant-operator-role

GIT_SHA  := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
VERSION  ?= $(GIT_SHA)

IMAGE_PREFIX := ghcr.io/sean-kramer/k8s-buddy
IMAGE_NAME   := buddy-api
IMAGE        := $(IMAGE_PREFIX)/$(IMAGE_NAME):$(GIT_SHA)
IMAGE_DEV    := $(IMAGE_PREFIX)/$(IMAGE_NAME):dev

OPERATOR_IMAGE_NAME := plant-operator
OPERATOR_IMAGE       := $(IMAGE_PREFIX)/$(OPERATOR_IMAGE_NAME):$(GIT_SHA)
OPERATOR_IMAGE_DEV    := $(IMAGE_PREFIX)/$(OPERATOR_IMAGE_NAME):dev

KIND         := kind
KIND_CLUSTER := k8s-buddy
KIND_CONFIG  := deploy/kind/kind-config.yaml

# The tracked kustomization the operator deploy targets render through a
# generated, throwaway overlay -- same reasoning as $(DEPLOY_BUILD_DIR)
# below for buddy-api's own base: pin the immutable git SHA without ever
# dirtying deploy/kustomize/operator/kustomization.yaml.
OPERATOR_BASE      := deploy/kustomize/operator
OPERATOR_BUILD_DIR := $(BUILD_DIR)/operator

# Where Plants live, and which one `demo-operator` applies. The namespace is
# a plain kustomization (no image pinning, nothing generated) so it is
# applied directly rather than through a $(BUILD_DIR) overlay.
PLANTS_BASE     := deploy/kustomize/plants
PLANT_NAMESPACE := k8s-buddy-plants
PLANT_SAMPLE    := config/samples/plant-fernie.yaml
PLANT_NAME      := fernie
PLANT_LABEL     := buddy.k8s-buddy.io/plant

# The tracked kustomization the `deploy` target renders through a generated,
# throwaway overlay so it can pin an immutable image tag without ever
# dirtying a committed file.
DEPLOY_BASE      := deploy/kustomize/base
DEPLOY_BUILD_DIR := $(BUILD_DIR)/deploy

.PHONY: help
help: ## Show this help
	@echo "K8s Buddy -- available targets:"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format all Go source in place with gofmt
	@gofmt -l -w .

.PHONY: vet
# The second invocation, with -tags envtest, is compile-only -- it needs no
# control plane -- and exists so the envtest-gated suite in
# internal/controller (see suite_test.go) can't accumulate a compile error
# invisibly between `make test-envtest` runs: a plain `go vet ./...` never
# even looks at build-tag-gated files.
vet: ## Run go vet across the module, including the envtest-tagged suite (no-op on an empty module)
	@pkgs="$$(go list ./... 2>/dev/null)"; \
	if [ -z "$$pkgs" ]; then \
		echo "vet: no Go packages yet, skipping"; \
	else \
		go vet ./...; \
		go vet -tags envtest ./internal/controller/...; \
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

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install the pinned controller-gen version into .tools/

$(CONTROLLER_GEN):
	@mkdir -p $(TOOLS_DIR)
	@echo "Installing controller-gen $(CONTROLLER_GEN_VERSION) into $(TOOLS_DIR)..."
	@GOBIN="$$(pwd)/$(TOOLS_DIR)" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate zz_generated.deepcopy.go for every +kubebuilder:object:generate type
	$(CONTROLLER_GEN) object paths="$(API_DIRS)"

.PHONY: manifests
# A single combined controller-gen invocation, not two separate ones: crd and
# rbac are independent generators reading independent marker types
# (+kubebuilder:validation:*/+kubebuilder:object:* for crd,
# +kubebuilder:rbac:* for rbac) from the same $(MANIFEST_DIRS), so one
# invocation covering both is both faster and the pattern kubebuilder's own
# scaffolded Makefile uses. config/rbac/role.yaml is therefore GENERATED --
# never hand-edit it; fix the +kubebuilder:rbac markers on
# internal/controller/plant_controller.go and re-run this target instead.
manifests: $(CONTROLLER_GEN) ## Regenerate config/crd/bases and config/rbac/role.yaml from +kubebuilder markers
	@mkdir -p $(CRD_DIR) $(RBAC_DIR)
	$(CONTROLLER_GEN) crd rbac:roleName=$(RBAC_ROLE_NAME) \
		$(foreach d,$(MANIFEST_DIRS),paths="$(d)") \
		output:crd:artifacts:config=$(CRD_DIR) \
		output:rbac:artifacts:config=$(RBAC_DIR)

.PHONY: test
# The envtest controller suite (internal/controller/{suite,plant_controller,
# counting_client}_test.go) is gated behind the "envtest" build tag and does
# NOT run under this target: it boots a real kube-apiserver + etcd, which
# costs a real binary download and real wall-clock time neither `make test`
# nor CI's `make test-race` / `make test-cover` should have to pay by
# default. Run it explicitly via `make test-envtest`.
test: ## Run unit tests (excludes the envtest controller suite -- see test-envtest)
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

$(SETUP_ENVTEST):
	@mkdir -p $(TOOLS_DIR)
	@echo "Installing setup-envtest $(SETUP_ENVTEST_VERSION) into $(TOOLS_DIR)..."
	@GOBIN="$$(pwd)/$(TOOLS_DIR)" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

.PHONY: envtest
# The help text below deliberately doesn't interpolate $(ENVTEST_K8S_VERSION):
# `make help` extracts ## comments with awk over the raw Makefile text, not
# make-expanded text, so a $(...) reference here would print literally
# instead of as a version number. See ENVTEST_K8S_VERSION's own definition
# above for the pinned value.
envtest: $(SETUP_ENVTEST) ## Download/locate the envtest control-plane binaries via the pinned setup-envtest, and print KUBEBUILDER_ASSETS
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

.PHONY: test-envtest
# Resolves KUBEBUILDER_ASSETS itself (downloading the control-plane binaries
# on first run, reusing setup-envtest's own cache thereafter) rather than
# requiring `make envtest` as a separate step first -- `make test-envtest`
# alone is always sufficient. The suite itself (see suite_test.go) also
# resolves KUBEBUILDER_ASSETS on its own if it isn't already set when
# invoked some other way (e.g. `go test -tags envtest ./internal/controller/...`
# directly), and fails loudly rather than skipping if that resolution fails.
#
# -count=1 disables go test's result cache: a suite that boots a real
# control plane must never report a stale, cached PASS from a previous run.
test-envtest: $(SETUP_ENVTEST) ## Run the envtest controller suite (boots a real kube-apiserver + etcd; internal/controller only)
	@assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)"; \
	[ -n "$$assets" ] || { echo "test-envtest: setup-envtest returned no path; aborting" >&2; exit 1; }; \
	echo "test-envtest: KUBEBUILDER_ASSETS=$$assets"; \
	KUBEBUILDER_ASSETS="$$assets" go test -tags envtest -count=1 ./internal/controller/... -v

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

.PHONY: docker-build-operator
docker-build-operator: ## Build the plant-operator container image, tagged with the short git SHA and :dev
	docker build \
		-f build/Dockerfile.plant-operator \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(OPERATOR_IMAGE) \
		-t $(OPERATOR_IMAGE_DEV) \
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

.PHONY: kind-load-operator
kind-load-operator: ## Load the built plant-operator image (SHA tag and :dev) into the kind cluster (k8s-buddy)
	$(KIND) load docker-image $(OPERATOR_IMAGE) $(OPERATOR_IMAGE_DEV) --name $(KIND_CLUSTER)

.PHONY: install-crd
install-crd: manifests ## Apply the generated Plant CRD (config/crd/bases) to the current kubectl context
	kubectl apply -f $(CRD_DIR)

.PHONY: uninstall-crd
uninstall-crd: ## Remove the Plant CRD from the current kubectl context (also deletes every Plant on the cluster)
	kubectl delete -f $(CRD_DIR) --ignore-not-found

# newTag is QUOTED in the generated overlays below (here and in
# `deploy-operator`). A git short SHA is seven hex digits, and roughly one
# commit in 27 draws seven that happen to be all decimal -- at which point
# unquoted YAML parses the tag as a NUMBER and kustomize fails the apply
# outright with "cannot unmarshal number into Go struct field
# Image.images.newTag of type string". Observed on commit 2090846, which broke
# `make deploy-operator` on a tree where nothing about the deploy path had
# changed. The bug is invisible on ~96% of commits, which is exactly what
# makes it worth pinning here rather than rediscovering.
#
# `deploy` never applies the base directly. The base's images: transformer
# defaults to the MUTABLE :dev tag, and applying a mutable tag after a
# rebuild renders a byte-identical PodSpec: the pod-template hash does not
# change, no rollout is triggered, and the cluster keeps serving the old
# image while `kubectl apply` cheerfully reports success. (Observed: this
# cluster served a four-commit-stale build through two "successful" demo
# runs.) Pinning the immutable short git SHA instead makes the PodSpec
# genuinely differ whenever the code does, so a rollout actually happens.
#
# The pin is applied through a generated overlay in $(BUILD_DIR) rather
# than `kustomize edit set image`, which would rewrite the tracked
# kustomization.yaml in place and leave the working tree dirty (and, on a
# failed run, dirty with a machine-specific SHA). The overlay is
# regenerated on every invocation so it can never go stale, and
# $(BUILD_DIR) is gitignored.
#
# Note the SHA names the commit at HEAD, not the working tree: deploying
# with uncommitted changes pins a tag whose contents are not what that SHA
# contains in git. That is the same caveat `make docker-build` already has,
# since it tags the image the same way.
.PHONY: deploy
deploy: ## Apply the Kubernetes manifests, pinning the image to the immutable git SHA
	@mkdir -p $(DEPLOY_BUILD_DIR)
	@printf '%s\n' \
		'# Generated by `make deploy`. Do not edit; do not commit.' \
		'apiVersion: kustomize.config.k8s.io/v1beta1' \
		'kind: Kustomization' \
		'resources:' \
		'  - ../../$(DEPLOY_BASE)' \
		'images:' \
		'  - name: $(IMAGE_PREFIX)/$(IMAGE_NAME)' \
		'    newTag: "$(GIT_SHA)"' \
		> $(DEPLOY_BUILD_DIR)/kustomization.yaml
	@echo "deploy: pinning $(IMAGE_PREFIX)/$(IMAGE_NAME) to tag $(GIT_SHA)"
	kubectl apply -k $(DEPLOY_BUILD_DIR)

.PHONY: undeploy
undeploy: ## Remove the Kubernetes manifests (deploy/kustomize/base) from the current kubectl context
	kubectl delete -k deploy/kustomize/base --ignore-not-found

# Same immutable-tag reasoning as `deploy` above, applied to the operator's
# own image: an operator Deployment left pointing at a mutable :dev tag would
# silently keep running a stale binary after a rebuild for exactly the same
# reason buddy-api's own deploy target avoids it.
# docker-build-operator and kind-load-operator are prerequisites, not
# instructions in a README. Without them `make deploy-operator` on a fresh
# cluster applies a Deployment referencing an image tag that exists nowhere
# -- imagePullPolicy: IfNotPresent cannot save a tag the node has never
# seen, and the target's own `rollout status` then sits in ImagePullBackOff
# for the full 120s before failing. Ordering is guaranteed by the
# .NOTPARALLEL: below; kind-load-operator needs docker-build-operator's
# image, and the apply needs kind-load-operator to have put it on the nodes.
.PHONY: deploy-operator
deploy-operator: install-crd docker-build-operator kind-load-operator ## Apply the operator's RBAC and Deployment (deploy/kustomize/operator), pinning the image to the immutable git SHA
	@mkdir -p $(OPERATOR_BUILD_DIR)
	@printf '%s\n' \
		'# Generated by `make deploy-operator`. Do not edit; do not commit.' \
		'apiVersion: kustomize.config.k8s.io/v1beta1' \
		'kind: Kustomization' \
		'resources:' \
		'  - ../../$(OPERATOR_BASE)' \
		'images:' \
		'  - name: $(IMAGE_PREFIX)/$(OPERATOR_IMAGE_NAME)' \
		'    newTag: "$(GIT_SHA)"' \
		> $(OPERATOR_BUILD_DIR)/kustomization.yaml
	@echo "deploy-operator: pinning $(IMAGE_PREFIX)/$(OPERATOR_IMAGE_NAME) to tag $(GIT_SHA)"
	kubectl apply -k $(OPERATOR_BUILD_DIR)
	kubectl -n k8s-buddy-system rollout status deployment/plant-operator --timeout=120s

.PHONY: undeploy-operator
undeploy-operator: ## Remove the operator's RBAC and Deployment (deploy/kustomize/operator) from the current kubectl context
	kubectl delete -k deploy/kustomize/operator --ignore-not-found

.PHONY: status
status: ## Show buddy-api pods/services/PDB and rollout status in the k8s-buddy namespace
	kubectl -n k8s-buddy get pods,svc,pdb -o wide
	kubectl -n k8s-buddy rollout status deployment/buddy-api

.PHONY: logs
logs: ## Tail logs from every buddy-api pod in the k8s-buddy namespace
	kubectl -n k8s-buddy logs -l app.kubernetes.io/name=buddy-api --all-containers --tail=200 -f

# `demo`'s prerequisites (kind-up, docker-build, kind-load, deploy) are a
# strict sequential pipeline -- kind-load needs docker-build's image, deploy
# needs kind-load to have already loaded it into the cluster -- so `make -j
# demo` would race them against each other. `.NOTPARALLEL:` with no target
# list disables parallelism for the whole file, which is the only form
# GNU Make 3.81 understands (the target-scoped form is a later GNU Make
# extension); nothing else in this Makefile benefits from -j anyway.
.NOTPARALLEL:

.PHONY: demo
demo: kind-up docker-build kind-load deploy ## Demo A -- static manifests (Plan 1 path): kind-up -> build -> load -> deploy -> hack/demo.sh chaos+recovery
	kubectl -n k8s-buddy rollout status deployment/buddy-api --timeout=120s
	bash hack/demo.sh

# The headline path. `demo` above proves self-healing with static manifests;
# this one proves the actual thesis of the project -- that the plant is a
# Custom Resource reconciled by an operator, not a hardcoded Deployment --
# and it has to be reachable in one command or it is not the headline.
#
# Prerequisites cover BOTH images: the operator's own, and buddy-api's,
# because the Plant the sample creates runs buddy-api and the sample
# deliberately leaves spec.image unset so the CRD's default (:dev) applies.
# deploy-operator itself pulls in install-crd, docker-build-operator, and
# kind-load-operator, so they are not repeated here.
.PHONY: demo-operator
demo-operator: kind-up docker-build kind-load deploy-operator ## Demo B -- Plant CRD + operator (RECOMMENDED): kind-up -> build both -> load both -> CRD -> operator -> apply a Plant -> wait -> kubectl get plants
	kubectl apply -k $(PLANTS_BASE)
	kubectl apply -f $(PLANT_SAMPLE)
	@echo "demo-operator: waiting for plant/$(PLANT_NAME) in $(PLANT_NAMESPACE) to report status.readyReplicas == spec.replicas..."
	@for i in $$(seq 1 60); do \
		desired="$$(kubectl -n $(PLANT_NAMESPACE) get plant $(PLANT_NAME) -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"; \
		ready="$$(kubectl -n $(PLANT_NAMESPACE) get plant $(PLANT_NAME) -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"; \
		if [ -n "$$desired" ] && [ -n "$$ready" ] && [ "$$ready" = "$$desired" ]; then \
			echo "demo-operator: $(PLANT_NAME) is ready ($$ready/$$desired)"; \
			break; \
		fi; \
		if [ "$$i" -eq 60 ]; then \
			echo "demo-operator: $(PLANT_NAME) never reached status.readyReplicas == spec.replicas" >&2; \
			kubectl -n $(PLANT_NAMESPACE) describe plant $(PLANT_NAME) >&2 || true; \
			exit 1; \
		fi; \
		echo "  waiting: readyReplicas=$${ready:-<unset>} desiredReplicas=$${desired:-<unset>} (attempt $$i/60)"; \
		sleep 5; \
	done
	@echo
	kubectl -n $(PLANT_NAMESPACE) get plants
	@echo
	@echo "The six children this Plant owns:"
	kubectl -n $(PLANT_NAMESPACE) get deploy,svc,cm,pdb,sa,netpol -l $(PLANT_LABEL)=$(PLANT_NAME)

.PHONY: undeploy-plants
undeploy-plants: ## Remove the sample Plants and the k8s-buddy-plants namespace from the current kubectl context
	kubectl delete -f $(PLANT_SAMPLE) --ignore-not-found
	kubectl delete -k $(PLANTS_BASE) --ignore-not-found

.PHONY: clean
clean: ## Remove build artifacts and coverage output (leaves .tools/ intact; see `tools-clean`)
	@rm -rf $(BIN_DIR) $(BUILD_DIR) $(COVER_FILE)

.PHONY: tools
tools: $(GOLANGCI_LINT) $(CONTROLLER_GEN) $(SETUP_ENVTEST) ## Install/update pinned local developer tooling into .tools/

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
