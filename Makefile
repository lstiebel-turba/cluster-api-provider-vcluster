
TAG ?= main
# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/loft-sh/cluster-api-provider-vcluster:$(TAG)
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.23

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

TARGETARCH ?= $(shell go env GOARCH)

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
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
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -coverprofile cover.out

.PHONY: e2e-kwok
e2e-kwok: ## End-to-end test of the flux installer on a kwok cluster (no Docker; hack/e2e-kwok.sh).
	hack/e2e-kwok.sh

INSTALLER ?= helm

.PHONY: e2e-kind
e2e-kind: ## End-to-end test on kind with a real vCluster (INSTALLER=helm|flux; hack/e2e-kind.sh).
	./hack/e2e-kind.sh $(INSTALLER)

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./main.go

.PHONY: docker-build
docker-build: manifests generate fmt vet envtest ## Build docker image with the manager.
	docker buildx build --platform linux/$(TARGETARCH) --load -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

.PHONY: combine-images
combine-images: ## Combines the manifests and pushes them
	@echo "Combining images..."
	docker push $(IMG)-amd64
	docker push $(IMG)-arm64
	docker manifest create $(IMG) \
                --amend $(IMG)-amd64 \
                --amend $(IMG)-arm64
	docker manifest push $(IMG)

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
.PHONY: controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0)

KUSTOMIZE = $(shell which kustomize || echo $(shell pwd)/bin/kustomize)
.PHONY: kustomize
kustomize: ## Download kustomize locally if necessary.
	$(call go-get-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5@v5.3.0)

ENVTEST = $(shell pwd)/bin/setup-envtest
.PHONY: envtest
envtest: ## Download envtest-setup locally if necessary.
	$(call go-get-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest@latest)

# go-get-tool will 'go get' any package $2 and install it to $1.
PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
define go-get-tool
@[ -f $(1) ] || { \
echo "Downloading $(2)" ;\
GOBIN=$(PROJECT_DIR)/bin go install $(2) ;\
}
endef

##@ Release

# The newest release series in metadata.yaml, as a development version.
VERSION ?= $(shell awk '$$2 == "major:" { m = $$3 } $$1 == "minor:" { print "v" m "." $$2 ".0-dev"; exit }' metadata.yaml)
IMAGE ?= ghcr.io/loft-sh/cluster-api-provider-vcluster:$(VERSION)
RELEASE_DIR ?= out

.PHONY: release
release: manifests kustomize ## Writes the release assets to RELEASE_DIR (out/): VERSION=v0.4.0-turba.1 IMAGE=<repo>:<tag>[@sha256:<digest>].
	VERSION="$(VERSION)" IMAGE="$(IMAGE)" RELEASE_DIR="$(RELEASE_DIR)" KUSTOMIZE="$(KUSTOMIZE)" hack/release.sh
	RELEASE_DIR="$(abspath $(RELEASE_DIR))" go test ./test/release/...

# Make sure to use the right remote TARGETARCH via make deploy-via-local-registry TARGETARCH=amd64
.PHONY: deploy-via-local-registry
deploy-via-local-registry: docker-build # Pushes the locally build image into the target cluster into a local-registry.
	# Deploy registry
	kubectl apply -f hack/local-registry/local-registry.yaml
	# Wait for registry
	@echo "Wait until local docker registry is up..."
	@until eval "kubectl wait --for=condition=ready pod -l app=docker-registry -n docker-registry" >/dev/null 2>&1; do \
		sleep 1; \
	done
	# Start port-forward in the background and store its PID
	kubectl -n docker-registry port-forward service/docker-registry-service 30115:5000 >/dev/null 2>&1 & \
	  PORT_FORWARD_PID=$$!; \
	  echo "Started port-forward with PID=$$PORT_FORWARD_PID"; \
	  # Give the port-forward a moment to initialize
	  sleep 2; \
	  # Tag the image for the local registry
	  docker tag $(IMG) localhost:30115/cluster-api-provider-vcluster:$(TAG); \
	  # Push the image to the local registry
	  docker push localhost:30115/cluster-api-provider-vcluster:$(TAG); \
	  # Kill the port-forward process
	  kill $$PORT_FORWARD_PID || true
	# Delete the existing controller
	kubectl delete po -l control-plane=capvc-controller-manager -n capvc-system --ignore-not-found
	$(KUSTOMIZE) build config/dev | kubectl apply -f -
