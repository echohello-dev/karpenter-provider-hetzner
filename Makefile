.PHONY: build test lint lint-chart generate generate-verify docker-build docker-push test-envtest verify-modules

BINARY         := karpenter-provider-hetzner
IMAGE          := ghcr.io/echohello-dev/karpenter-provider-hetzner
TAG            ?= latest
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0

build:
	go build -o bin/$(BINARY) ./cmd/controller

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

lint-chart:
	helm lint charts/karpenter-provider-hetzner

generate:
	$(CONTROLLER_GEN) object paths="./pkg/apis/..."
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:dir=charts/karpenter-provider-hetzner/crds

generate-verify: generate
	@if [ -n "$$(git status --porcelain pkg/apis charts/karpenter-provider-hetzner/crds)" ]; then \
		echo "generated files are out of date; run 'make generate' and commit"; \
		git --no-pager diff -- pkg/apis charts/karpenter-provider-hetzner/crds; \
		exit 1; \
	fi

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push: docker-build
	docker push $(IMAGE):$(TAG)

verify-modules:
	@go mod verify

ci: lint vet test build generate-verify verify-modules

vet:
	go vet ./...

# Run envtest-backed controller tests against a kubebuilder envtest cluster.
ENVTEST         := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
ENVTEST_K8S_VERSION ?= 1.34.0
test-envtest:
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -race -count=1 ./pkg/controllers/...

help:
	@echo "Available targets:"
	@echo "  build         - Build controller binary into ./bin/$(BINARY)"
	@echo "  test          - Run unit tests with race detector"
	@echo "  test-envtest  - Run envtest-backed controller tests"
	@echo "  lint          - Run golangci-lint"
	@echo "  lint-chart    - Lint the Helm chart"
	@echo "  vet           - Run go vet"
	@echo "  generate      - Regenerate CRDs and deep-copy methods"
	@echo "  generate-verify - Verify generated files are up to date (used in CI)"
	@echo "  docker-build  - Build docker image"
	@echo "  docker-push   - Build and push docker image"
	@echo "  ci            - Run all CI checks"
	@echo "  help          - Show this help"
