REGISTRY ?= ghcr.io/sympozium-ai/llmfit-dra
TAG ?= dev
IMAGE = $(REGISTRY):$(TAG)
KIND_CLUSTER ?= tailnet
NAMESPACE ?= llmfit-dra

.PHONY: help build test fmt vet docker-build image kind-load sideload kind-reload \
        deploy deploy-helm deploy-local undeploy undeploy-helm helm-lint \
        pull-secret scenarios scenarios-cpu

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

build: ## Build the driver binary
	CGO_ENABLED=0 go build -o bin/llmfit-dra ./cmd/llmfit-dra

test: ## Run unit tests
	go test ./...

fmt: ## Run gofmt
	gofmt -w .

vet: ## Run go vet
	go vet ./...

# llmfit is built from the pinned submodule inside the Dockerfile.
docker-build: ## Build the driver image ($(REGISTRY):$(TAG))
	git submodule update --init
	docker build -t $(IMAGE) .

image: docker-build ## Alias for docker-build

##@ Image loading

kind-load: docker-build ## Build and load the image into a local kind cluster
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

# Side-load into a REMOTE kind node reachable only via kubectl: stream the
# image archive through a privileged pod into the node's containerd.
sideload: docker-build ## Build and stream the image into a remote kind node
	kubectl -n kube-system run image-loader --restart=Never \
	  --image=docker.io/library/busybox:1.36 \
	  --overrides='{"spec":{"nodeName":"kind-control-plane","hostPID":true,"containers":[{"name":"image-loader","image":"docker.io/library/busybox:1.36","command":["sleep","900"],"securityContext":{"privileged":true}}]}}' || true
	kubectl -n kube-system wait --for=condition=Ready pod/image-loader --timeout=90s
	docker save $(IMAGE) | kubectl exec -i -n kube-system image-loader -- \
	  nsenter -t 1 -m -- ctr -n k8s.io images import -
	kubectl -n kube-system delete pod image-loader --wait=false

kind-reload: sideload ## Build, load, and restart the DaemonSet on the new image
	kubectl -n $(NAMESPACE) rollout restart daemonset/llmfit-dra
	kubectl -n $(NAMESPACE) rollout status daemonset/llmfit-dra --timeout=180s

##@ Deploy

# Helm is the recommended install path; deploy/ manifests remain for raw
# kubectl workflows and stay in lockstep with the chart.
deploy-helm: ## Install/upgrade via the Helm chart (released image tags)
	helm upgrade --install llmfit-dra charts/llmfit-dra -n $(NAMESPACE) --create-namespace

deploy-local: sideload ## Local dev loop: build image, load it, helm install with TAG
	helm upgrade --install llmfit-dra charts/llmfit-dra -n $(NAMESPACE) --create-namespace \
	  --set image.repository=$(REGISTRY) \
	  --set image.tag=$(TAG) \
	  --set image.pullPolicy=IfNotPresent
	kubectl -n $(NAMESPACE) rollout restart daemonset/llmfit-dra
	kubectl -n $(NAMESPACE) rollout status daemonset/llmfit-dra --timeout=180s

undeploy-helm: ## Uninstall the Helm release
	helm uninstall llmfit-dra -n $(NAMESPACE)

deploy: ## Apply the raw manifests (kubectl path)
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/admission.yaml
	kubectl apply -f deploy/deviceclass.yaml
	kubectl apply -f deploy/daemonset.yaml

undeploy: ## Delete the raw manifests
	kubectl delete -f deploy/daemonset.yaml --ignore-not-found
	kubectl delete -f deploy/deviceclass.yaml --ignore-not-found
	kubectl delete -f deploy/admission.yaml --ignore-not-found
	kubectl delete -f deploy/rbac.yaml --ignore-not-found

helm-lint: ## Lint the Helm chart
	helm lint charts/llmfit-dra

# Create/refresh the GHCR pull secret in every namespace that pulls
# private sympozium-ai images. Requires a token with read:packages:
# GITHUB_TOKEN from the environment, falling back to `gh auth token`
# (which needs `gh auth refresh -s read:packages`). The token travels via
# the environment and stdin — never in process arguments.
GHCR_USER ?= AlexsJones
PULL_SECRET_NAMESPACES ?= llmfit-dra sympozium-system
pull-secret: export GHCR_TOKEN := $(or $(GITHUB_TOKEN),$(shell gh auth token))
pull-secret: ## Create/refresh the ghcr-pull secret (token via env/stdin)
	@for ns in $(PULL_SECRET_NAMESPACES); do \
	  printf '{"auths":{"ghcr.io":{"auth":"%s"}}}' \
	    "$$(printf '%s:%s' '$(GHCR_USER)' "$$GHCR_TOKEN" | base64 -w0)" | \
	  kubectl -n $$ns create secret generic ghcr-pull \
	    --type=kubernetes.io/dockerconfigjson \
	    --from-file=.dockerconfigjson=/dev/stdin \
	    --dry-run=client -o yaml | kubectl apply -f -; \
	done

##@ Test (live cluster)

scenarios: ## Run the end-to-end scenario suite
	./hack/scenarios.sh

# The CI run, reproduced locally: probe sees no sysfs → cpu0-only inventory.
scenarios-cpu: ## Run the suite in CPU-only mode (reproduces CI)
	./hack/scenarios-cpu.sh
