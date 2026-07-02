IMAGE ?= ghcr.io/sympozium-ai/llmfit-dra:dev
KIND_CLUSTER ?= tailnet

.PHONY: build test image kind-load deploy undeploy scenarios fmt vet

build:
	CGO_ENABLED=0 go build -o bin/llmfit-dra ./cmd/llmfit-dra

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

# llmfit is built from the pinned submodule inside the Dockerfile.
image:
	git submodule update --init
	docker build -t $(IMAGE) .

kind-load: image
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

# Side-load into a REMOTE kind node reachable only via kubectl: stream the
# image archive through a privileged pod into the node's containerd.
sideload: image
	kubectl -n kube-system run image-loader --restart=Never \
	  --image=docker.io/library/busybox:1.36 \
	  --overrides='{"spec":{"nodeName":"kind-control-plane","hostPID":true,"containers":[{"name":"image-loader","image":"docker.io/library/busybox:1.36","command":["sleep","900"],"securityContext":{"privileged":true}}]}}' || true
	kubectl -n kube-system wait --for=condition=Ready pod/image-loader --timeout=90s
	docker save $(IMAGE) | kubectl exec -i -n kube-system image-loader -- \
	  nsenter -t 1 -m -- ctr -n k8s.io images import -
	kubectl -n kube-system delete pod image-loader --wait=false

# Create/refresh the GHCR pull secret in every namespace that pulls
# private sympozium-ai images. Requires a token with read:packages:
# GITHUB_TOKEN from the environment, falling back to `gh auth token`
# (which needs `gh auth refresh -s read:packages`). The token travels via
# the environment and stdin — never in process arguments.
GHCR_USER ?= AlexsJones
PULL_SECRET_NAMESPACES ?= llmfit-dra sympozium-system
pull-secret: export GHCR_TOKEN := $(or $(GITHUB_TOKEN),$(shell gh auth token))
pull-secret:
	@for ns in $(PULL_SECRET_NAMESPACES); do \
	  printf '{"auths":{"ghcr.io":{"auth":"%s"}}}' \
	    "$$(printf '%s:%s' '$(GHCR_USER)' "$$GHCR_TOKEN" | base64 -w0)" | \
	  kubectl -n $$ns create secret generic ghcr-pull \
	    --type=kubernetes.io/dockerconfigjson \
	    --from-file=.dockerconfigjson=/dev/stdin \
	    --dry-run=client -o yaml | kubectl apply -f -; \
	done

deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/admission.yaml
	kubectl apply -f deploy/deviceclass.yaml
	kubectl apply -f deploy/daemonset.yaml

undeploy:
	kubectl delete -f deploy/daemonset.yaml --ignore-not-found
	kubectl delete -f deploy/deviceclass.yaml --ignore-not-found
	kubectl delete -f deploy/admission.yaml --ignore-not-found
	kubectl delete -f deploy/rbac.yaml --ignore-not-found

scenarios:
	./hack/scenarios.sh

# The CI run, reproduced locally: probe sees no sysfs → cpu0-only inventory.
scenarios-cpu:
	./hack/scenarios-cpu.sh
