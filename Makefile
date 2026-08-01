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
# Where controller-gen's `webhook` generator writes the Mutating/Validating
# WebhookConfiguration manifests it derives from the +kubebuilder:webhook
# markers on api/v1alpha1/plant_webhook.go's SetupPlantWebhookWithManager.
WEBHOOK_DIR   := config/webhook

# The Helm chart: packages the operator, its RBAC, and the CRD (shipped in
# crds/ so Helm installs it before anything that depends on it -- see
# HELM_SYNC_CRD_SRC below for how that copy is kept from drifting) plus an
# optional sample Plant. KUBECONFORM_K8S_VERSION mirrors the same
# "1.36.1" the `manifests` CI job already hardcodes per validation step (see
# .github/workflows/ci.yaml) -- pulled into one variable here so the chart
# and Kustomize overlay targets below don't repeat the literal.
HELM                    := helm
HELM_CHART_DIR          := charts/k8s-buddy
HELM_TEST_RELEASE       := k8s-buddy-charttest
HELM_TEST_NAMESPACE     := k8s-buddy-charttest
KUBECONFORM_K8S_VERSION := 1.36.1

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

CHAOS_IMAGE_NAME := chaos-buddy
CHAOS_IMAGE       := $(IMAGE_PREFIX)/$(CHAOS_IMAGE_NAME):$(GIT_SHA)
CHAOS_IMAGE_DEV    := $(IMAGE_PREFIX)/$(CHAOS_IMAGE_NAME):dev

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

# chaos-buddy deploys into $(PLANT_NAMESPACE) alongside the Plants it
# targets, but its own kustomization does not create that namespace --
# see deploy/kustomize/chaos/kustomization.yaml's own comment for why.
# `deploy-chaos` below applies $(PLANTS_BASE) first so the namespace
# exists even on a cluster where `demo-operator` was never run.
CHAOS_BASE      := deploy/kustomize/chaos
CHAOS_BUILD_DIR := $(BUILD_DIR)/chaos

# The observability stack: kube-prometheus-stack (Prometheus, Alertmanager,
# Grafana, kube-state-metrics, node-exporter, the Prometheus Operator) and
# Loki+promtail (logs), all Helm-installed into one namespace, plus the
# committed dashboard and PrometheusRules applied as plain manifests. Chart
# versions are PINNED here, exactly -- `--version latest` (or omitting
# --version) is not reproducible, and a chart upgrade landing silently under
# a moving tag is exactly the kind of drift this project's pinned-tool
# pattern (GOLANGCI_LINT_VERSION, CONTROLLER_GEN_VERSION, ...) already
# guards against everywhere else.
OBSERVABILITY_NAMESPACE       := k8s-buddy-observability
OBSERVABILITY_VALUES_DIR      := deploy/observability
OBSERVABILITY_BUILD_DIR       := $(BUILD_DIR)/observability
KUBE_PROMETHEUS_STACK_VERSION := 88.0.1
LOKI_CHART_VERSION            := 7.2.0
PROMTAIL_CHART_VERSION        := 6.17.1

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
manifests: $(CONTROLLER_GEN) ## Regenerate config/crd/bases, config/rbac/role.yaml, and config/webhook/manifests.yaml from +kubebuilder markers
	@mkdir -p $(CRD_DIR) $(RBAC_DIR) $(WEBHOOK_DIR)
	$(CONTROLLER_GEN) crd rbac:roleName=$(RBAC_ROLE_NAME) webhook \
		$(foreach d,$(MANIFEST_DIRS),paths="$(d)") \
		output:crd:artifacts:config=$(CRD_DIR) \
		output:rbac:artifacts:config=$(RBAC_DIR) \
		output:webhook:artifacts:config=$(WEBHOOK_DIR)

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

.PHONY: docker-build-chaos
docker-build-chaos: ## Build the chaos-buddy container image, tagged with the short git SHA and :dev
	docker build \
		-f build/Dockerfile.chaos-buddy \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(CHAOS_IMAGE) \
		-t $(CHAOS_IMAGE_DEV) \
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

.PHONY: kind-load-chaos
kind-load-chaos: ## Load the built chaos-buddy image (SHA tag and :dev) into the kind cluster (k8s-buddy)
	$(KIND) load docker-image $(CHAOS_IMAGE) $(CHAOS_IMAGE_DEV) --name $(KIND_CLUSTER)

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

# Same immutable-tag reasoning as deploy-operator above. $(PLANTS_BASE) is
# applied first (not as a Make prerequisite, so it never races
# docker-build-chaos/kind-load-chaos under `.NOTPARALLEL:`) so
# `make deploy-chaos` alone is enough to reach a working state even on a
# cluster where `demo-operator` was never run -- chaos-buddy's own
# kustomization deliberately does not create the namespace it deploys
# into; see deploy/kustomize/chaos/kustomization.yaml's own comment.
#
# chaos-buddy ships with CHAOS_DRY_RUN=true (deploy/kustomize/chaos/
# configmap.yaml) and readinessProbe/livenessProbe on /healthz, so
# `rollout status` here only proves the process started and is serving
# /metrics -- not that it has performed (or refrained from) any chaos
# action, which is what the task's own verification steps check
# separately.
.PHONY: deploy-chaos
deploy-chaos: docker-build-chaos kind-load-chaos ## Apply chaos-buddy's namespace prerequisite, RBAC, ConfigMaps, and Deployment (deploy/kustomize/chaos), pinning the image to the immutable git SHA; ships with dry-run true
	kubectl apply -k $(PLANTS_BASE)
	@mkdir -p $(CHAOS_BUILD_DIR)
	@printf '%s\n' \
		'# Generated by `make deploy-chaos`. Do not edit; do not commit.' \
		'apiVersion: kustomize.config.k8s.io/v1beta1' \
		'kind: Kustomization' \
		'resources:' \
		'  - ../../$(CHAOS_BASE)' \
		'images:' \
		'  - name: $(IMAGE_PREFIX)/$(CHAOS_IMAGE_NAME)' \
		'    newTag: "$(GIT_SHA)"' \
		> $(CHAOS_BUILD_DIR)/kustomization.yaml
	@echo "deploy-chaos: pinning $(IMAGE_PREFIX)/$(CHAOS_IMAGE_NAME) to tag $(GIT_SHA)"
	kubectl apply -k $(CHAOS_BUILD_DIR)
	kubectl -n $(PLANT_NAMESPACE) rollout status deployment/chaos-buddy --timeout=120s

.PHONY: undeploy-chaos
undeploy-chaos: ## Remove chaos-buddy's RBAC, ConfigMaps, and Deployment (deploy/kustomize/chaos) from the current kubectl context (leaves the k8s-buddy-plants namespace and any Plants in it alone)
	kubectl delete -k deploy/kustomize/chaos --ignore-not-found

# observability-install's ordering is load-bearing, not incidental:
#
#   1. The namespace, applied ALONE and first -- Helm does not create
#      namespaces with the PSA labels deploy/observability/namespace.yaml
#      carries, so it has to exist (with those labels) before either Helm
#      release lands a single pod in it.
#   2. kube-prometheus-stack -- brings the ServiceMonitor/PrometheusRule
#      CRDs and the Prometheus Operator that watches for them. Installed
#      before Loki/promtail only because it is also the one that matters for
#      steps 4-5 below; Loki has no ordering dependency on it.
#   3. Loki, then promtail -- promtail's --set below hardcodes the "loki"
#      release's own Service DNS name (see values-loki.yaml's own comment on
#      why gateway.enabled: false means Grafana/promtail talk to Loki's
#      singleBinary Service directly), so Loki has to already exist as a
#      release name promtail's config can point at; in practice `helm
#      upgrade --install` doesn't require the Service to already be UP, only
#      that this Makefile's own hardcoded URL agrees with what step 3
#      creates.
#   4. `kubectl apply -k deploy/observability` -- re-applies the namespace
#      (idempotent) and applies prometheus-rbac.yaml (references the
#      plant-operator-metrics-reader-role ClusterRole, which requires
#      deploy/kustomize/operator to already be deployed -- run `make
#      deploy-operator` first on a cluster where it isn't) and the two
#      static ServiceMonitors (reference the CRD step 2 just installed) and
#      the dashboard ConfigMap.
#   5. The PrometheusRules -- observability/rules/*.yaml are committed in
#      PLAIN Prometheus rule-file format (top-level `groups:`, exactly what
#      `promtool check rules` validates directly) rather than as
#      Kubernetes PrometheusRule custom resources, so the single committed
#      file is both the promtool-checkable source AND (via the thin
#      generated wrapper below) the cluster object -- never two documents
#      that could drift. The wrapper is regenerated into
#      $(OBSERVABILITY_BUILD_DIR) on every invocation (gitignored, same
#      pattern as $(DEPLOY_BUILD_DIR)/$(OPERATOR_BUILD_DIR) above) by
#      indenting the rule file's own `groups:` block under `spec:`.
.PHONY: observability-install
observability-install: ## Install kube-prometheus-stack + Loki + promtail (pinned chart versions) plus the dashboard, PrometheusRules, and RBAC (run after deploy-operator and deploy-chaos)
	@echo "observability-install: creating namespace $(OBSERVABILITY_NAMESPACE) (PSA privileged -- see deploy/observability/namespace.yaml for why)"
	kubectl apply -f $(OBSERVABILITY_VALUES_DIR)/namespace.yaml
	@echo "observability-install: adding/updating the prometheus-community and grafana chart repos"
	@helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
	@helm repo add grafana https://grafana.github.io/helm-charts >/dev/null 2>&1 || true
	helm repo update prometheus-community grafana
	@echo "observability-install: installing kube-prometheus-stack $(KUBE_PROMETHEUS_STACK_VERSION)"
	helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
		--version $(KUBE_PROMETHEUS_STACK_VERSION) \
		--namespace $(OBSERVABILITY_NAMESPACE) \
		-f $(OBSERVABILITY_VALUES_DIR)/values-kube-prometheus-stack.yaml \
		--wait --timeout 5m
	@echo "observability-install: installing loki $(LOKI_CHART_VERSION)"
	helm upgrade --install loki grafana/loki \
		--version $(LOKI_CHART_VERSION) \
		--namespace $(OBSERVABILITY_NAMESPACE) \
		-f $(OBSERVABILITY_VALUES_DIR)/values-loki.yaml \
		--wait --timeout 5m
	@echo "observability-install: installing promtail $(PROMTAIL_CHART_VERSION), pointed at the loki release just installed"
	helm upgrade --install promtail grafana/promtail \
		--version $(PROMTAIL_CHART_VERSION) \
		--namespace $(OBSERVABILITY_NAMESPACE) \
		--set "config.clients[0].url=http://loki.$(OBSERVABILITY_NAMESPACE).svc.cluster.local:3100/loki/api/v1/push" \
		--wait --timeout 5m
	@echo "observability-install: applying namespace/RBAC/dashboard/ServiceMonitors (deploy/observability)"
	kubectl apply -k $(OBSERVABILITY_VALUES_DIR)
	@echo "observability-install: generating and applying PrometheusRules from observability/rules/*.yaml"
	@mkdir -p $(OBSERVABILITY_BUILD_DIR)
	@{ \
		echo "# Generated by \`make observability-install\` from observability/rules/slo.yaml. Do not edit; do not commit."; \
		echo "apiVersion: monitoring.coreos.com/v1"; \
		echo "kind: PrometheusRule"; \
		echo "metadata:"; \
		echo "  name: buddy-api-slo"; \
		echo "  namespace: $(OBSERVABILITY_NAMESPACE)"; \
		echo "  labels:"; \
		echo "    app.kubernetes.io/name: k8s-buddy-observability"; \
		echo "    app.kubernetes.io/instance: k8s-buddy-observability"; \
		echo "    app.kubernetes.io/component: observability"; \
		echo "    app.kubernetes.io/part-of: k8s-buddy"; \
		echo "    app.kubernetes.io/managed-by: kustomize"; \
		echo "spec:"; \
		sed 's/^/  /' observability/rules/slo.yaml; \
	} > $(OBSERVABILITY_BUILD_DIR)/prometheusrule-slo.yaml
	@{ \
		echo "# Generated by \`make observability-install\` from observability/rules/operational.yaml. Do not edit; do not commit."; \
		echo "apiVersion: monitoring.coreos.com/v1"; \
		echo "kind: PrometheusRule"; \
		echo "metadata:"; \
		echo "  name: k8s-buddy-operational"; \
		echo "  namespace: $(OBSERVABILITY_NAMESPACE)"; \
		echo "  labels:"; \
		echo "    app.kubernetes.io/name: k8s-buddy-observability"; \
		echo "    app.kubernetes.io/instance: k8s-buddy-observability"; \
		echo "    app.kubernetes.io/component: observability"; \
		echo "    app.kubernetes.io/part-of: k8s-buddy"; \
		echo "    app.kubernetes.io/managed-by: kustomize"; \
		echo "spec:"; \
		sed 's/^/  /' observability/rules/operational.yaml; \
	} > $(OBSERVABILITY_BUILD_DIR)/prometheusrule-operational.yaml
	kubectl apply -f $(OBSERVABILITY_BUILD_DIR)/prometheusrule-slo.yaml -f $(OBSERVABILITY_BUILD_DIR)/prometheusrule-operational.yaml
	@echo
	@echo "observability-install: done. Grafana: http://localhost:30300 (see \`make grafana-port-forward\` for the admin password)."

.PHONY: observability-uninstall
observability-uninstall: ## Remove the observability stack: the three Helm releases, deploy/observability, the generated PrometheusRules, and the namespace itself (leaves kube-prometheus-stack's CRDs installed -- Helm's own convention, since another release could still reference them)
	-helm uninstall promtail --namespace $(OBSERVABILITY_NAMESPACE)
	-helm uninstall loki --namespace $(OBSERVABILITY_NAMESPACE)
	-helm uninstall kube-prometheus-stack --namespace $(OBSERVABILITY_NAMESPACE)
	kubectl delete -f $(OBSERVABILITY_BUILD_DIR)/prometheusrule-slo.yaml -f $(OBSERVABILITY_BUILD_DIR)/prometheusrule-operational.yaml --ignore-not-found 2>/dev/null || true
	kubectl delete -k $(OBSERVABILITY_VALUES_DIR) --ignore-not-found
	kubectl delete namespace $(OBSERVABILITY_NAMESPACE) --ignore-not-found

.PHONY: grafana-port-forward
grafana-port-forward: ## Print the generated Grafana admin password, then port-forward Grafana to localhost:3000 (it is also already reachable at localhost:30300 via NodePort, no port-forward required)
	@echo "Grafana is reachable directly at http://localhost:30300 (NodePort -- see deploy/kind/kind-config.yaml's extraPortMappings)."
	@echo
	@echo "Admin username: admin"
	@echo -n "Admin password: "
	@kubectl -n $(OBSERVABILITY_NAMESPACE) get secret kube-prometheus-stack-grafana \
		-o jsonpath='{.data.admin-password}' | base64 -d
	@echo
	@echo
	@echo "Port-forwarding to http://localhost:3000 as well (Ctrl-C to stop)..."
	kubectl -n $(OBSERVABILITY_NAMESPACE) port-forward svc/kube-prometheus-stack-grafana 3000:80

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

.PHONY: helm-sync-crd
# Copies, never regenerates: the CRD is already generated by `make
# manifests` into $(CRD_DIR); this target's only job is keeping the chart's
# own copy byte-identical to it, the same way a hand-maintained mirror of a
# generated file is kept honest everywhere else in this repo -- by a diff
# gate, not by trusting nobody forgets to re-copy it. See the `manifests`
# CI job (.github/workflows/ci.yaml) for that gate: `make helm-sync-crd`
# followed by `git diff --exit-code charts/k8s-buddy/crds`.
helm-sync-crd: manifests ## Copy the generated Plant CRD (config/crd/bases) into charts/k8s-buddy/crds, so the chart can never ship a stale copy
	@mkdir -p $(HELM_CHART_DIR)/crds
	cp $(CRD_DIR)/buddy.k8s-buddy.io_plants.yaml $(HELM_CHART_DIR)/crds/buddy.k8s-buddy.io_plants.yaml

.PHONY: helm-lint
helm-lint: helm-sync-crd ## Lint the k8s-buddy Helm chart (charts/k8s-buddy)
	$(HELM) lint $(HELM_CHART_DIR)

# Renders with the optional sample Plant turned on (plant.enabled=true, and
# its own namespace) so every template in the chart -- not just the ones a
# bare `helm template` would render by default -- is exercised by
# kubeconform. `-skip Plant`: kubeconform has no schema for the Plant CRD
# (a third-party/project-local CRD, not a built-in Kubernetes type), the
# same reason no kubeconform step in .github/workflows/ci.yaml's own
# `manifests` job ever validates a rendered Plant object either.
.PHONY: helm-template
helm-template: helm-sync-crd ## Render the k8s-buddy chart (incl. the optional sample Plant) and validate it against real Kubernetes schemas with kubeconform -strict
	@set -euo pipefail; \
	$(HELM) template k8s-buddy $(HELM_CHART_DIR) -n k8s-buddy-system \
		--set plant.enabled=true --set plant.namespace.create=true --set plant.namespace.name=k8s-buddy-plants \
		| kubeconform -strict -summary -skip Plant -kubernetes-version $(KUBECONFORM_K8S_VERSION) -

.PHONY: helm-test
# Requires the helm-unittest plugin: `helm plugin install
# https://github.com/helm-unittest/helm-unittest`. Covers (charts/k8s-buddy/
# tests/*_test.yaml): the Deployment's image/securityContext/replicaCount,
# the optional Plant's enabled/disabled rendering, RBAC name scoping (no
# collision between two releases or with the kustomize path's own
# plant-operator-role), and -- the ones that prove values.schema.json is
# load-bearing, not decorative -- that an invalid resourceProfile, species,
# or replicaCount is REJECTED before a single template renders.
helm-test: helm-sync-crd ## Run the chart's helm-unittest suite (charts/k8s-buddy/tests)
	$(HELM) unittest $(HELM_CHART_DIR)

.PHONY: helm-install-dry
# --dry-run=client (not the deprecated bare --dry-run): renders and
# schema-validates the install exactly like a real `helm install` would,
# without ever contacting a Kubernetes API server -- deliberate, so this
# target runs identically here, in CI's cluster-less `manifests` job, and
# against a real cluster. A scratch release name/namespace, never the live
# k8s-buddy-system this cluster's real operator runs in -- see
# templates/clusterrole.yaml's own comment for why every cluster-scoped
# object this chart creates is release-scoped by name, which is what makes
# this (and a REAL `helm install`, without --dry-run) safe to run against a
# cluster that already has the kustomize path's operator installed.
helm-install-dry: helm-sync-crd ## helm install --dry-run=client into a scratch namespace/release, proving the chart installs cleanly without touching the cluster
	$(HELM) install $(HELM_TEST_RELEASE) $(HELM_CHART_DIR) -n $(HELM_TEST_NAMESPACE) --create-namespace \
		--set plant.enabled=true --dry-run=client --debug

.PHONY: helm-rbac-drift-check
# Requires PyYAML (`pip install pyyaml`) -- see hack/check-helm-rbac-drift.py's
# own header comment for the full reasoning: config/rbac/role.yaml is
# generated and drift-gated against its +kubebuilder:rbac markers by the
# `lint` CI job already; this is that same gate's counterpart for the
# chart's hand-maintained mirror of those rules.
helm-rbac-drift-check: ## Assert charts/k8s-buddy's ClusterRole rules have not drifted from generated config/rbac/role.yaml
	@mkdir -p $(BUILD_DIR)
	$(HELM) template k8s-buddy $(HELM_CHART_DIR) --show-only templates/clusterrole.yaml > $(BUILD_DIR)/chart-clusterrole.yaml
	python3 hack/check-helm-rbac-drift.py $(BUILD_DIR)/chart-clusterrole.yaml

.PHONY: kustomize-build-overlays
kustomize-build-overlays: ## Build both Kustomize overlays (deploy/kustomize/overlays/{dev,prod}) and validate each with kubeconform -strict
	@set -euo pipefail; \
	echo "kustomize-build-overlays: validating deploy/kustomize/overlays/dev"; \
	kubectl kustomize deploy/kustomize/overlays/dev | kubeconform -strict -summary -kubernetes-version $(KUBECONFORM_K8S_VERSION) -; \
	echo "kustomize-build-overlays: validating deploy/kustomize/overlays/prod"; \
	kubectl kustomize deploy/kustomize/overlays/prod | kubeconform -strict -summary -kubernetes-version $(KUBECONFORM_K8S_VERSION) -

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
