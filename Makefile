SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO ?= go
DOCKER ?= docker
HELM ?= helm
KUBECTL ?= kubectl
REGISTRY ?= 100.0.0.2:30500
IMAGE_NAME ?= julie
VERSION ?= $(shell date -u +%Y%m%d%H%M%S)
VERSION := $(VERSION)
export VERSION
IMAGE_REPOSITORY := $(REGISTRY)/$(IMAGE_NAME)
IMAGE := $(IMAGE_REPOSITORY):$(VERSION)
PLATFORMS := linux/amd64,linux/arm64
BUILD_DIR ?= build
BUILDER ?= julie-multiarch
BUILDKIT_CONFIG ?= buildkitd.toml
CHART ?= charts/julie
RELEASE ?= julie
NAMESPACE ?= julie
CREDENTIALS_SECRET ?= julie-credentials
HELM_VALUES ?=
HELM_TIMEOUT ?= 5m

.PHONY: help check test build build-amd64 build-arm64 test-compile \
        test-compile-amd64 test-compile-arm64 builder test-platforms \
        test-amd64 test-arm64 publish push lint check-deploy apply deploy

help:
	@printf '%s\n' \
	  'make check          Native race tests/vet; compile agent and test binaries for AMD64/ARM64' \
	  'make test-platforms Run tests for both platforms using Buildx (ARM64 needs emulation or a native builder)' \
	  'make test-amd64     Run Linux AMD64 tests using Buildx' \
	  'make test-arm64     Run Linux ARM64 tests using Buildx' \
	  'make publish        Test both platforms, build, and push one multi-architecture image' \
	  'make apply          Check credentials, publish the image, then deploy with Helm' \
	  'make lint           Validate the Helm chart' \
	  'Overrides: REGISTRY IMAGE_NAME VERSION NAMESPACE RELEASE CREDENTIALS_SECRET HELM_VALUES' \
	  'Image: $(IMAGE)'

check: test build test-compile

test:
	$(GO) test -race ./...
	$(GO) vet ./...

build: build-amd64 build-arm64

build-amd64 build-arm64: build-%:
	mkdir -p "$(BUILD_DIR)/$*"
	CGO_ENABLED=0 GOOS=linux GOARCH=$* $(GO) build -trimpath -ldflags='-s -w' -o "$(BUILD_DIR)/$*/julie" ./cmd/julie
	CGO_ENABLED=0 GOOS=linux GOARCH=$* $(GO) build -trimpath -ldflags='-s -w' -o "$(BUILD_DIR)/$*/julie-mcp" ./cmd/julie-mcp

test-compile: test-compile-amd64 test-compile-arm64

test-compile-amd64 test-compile-arm64: test-compile-%:
	mkdir -p "$(BUILD_DIR)/tests/$*"
	CGO_ENABLED=0 GOOS=linux GOARCH=$* $(GO) test -c -o "$(BUILD_DIR)/tests/$*/" ./...

builder:
	$(DOCKER) buildx version
	@test -f "$(BUILDKIT_CONFIG)"
	@$(DOCKER) buildx inspect "$(BUILDER)" >/dev/null 2>&1 || \
	  $(DOCKER) buildx create --name "$(BUILDER)" --driver docker-container \
	    --buildkitd-config "$(BUILDKIT_CONFIG)"
	$(DOCKER) buildx inspect "$(BUILDER)" --bootstrap

test-platforms: builder
	$(DOCKER) buildx build --builder "$(BUILDER)" --platform "$(PLATFORMS)" \
	  --target test --output type=cacheonly --progress plain .

test-amd64 test-arm64: test-%: builder
	$(DOCKER) buildx build --builder "$(BUILDER)" --platform "linux/$*" \
	  --target test --output type=cacheonly --progress plain .

publish: check test-platforms
	$(DOCKER) buildx build --builder "$(BUILDER)" --platform "$(PLATFORMS)" \
	  --target runtime --tag "$(IMAGE)" --push --progress plain .

push: publish

lint:
	$(HELM) lint "$(CHART)" --strict $(if $(HELM_VALUES),--values "$(HELM_VALUES)")

check-deploy:
	$(KUBECTL) get secret "$(CREDENTIALS_SECRET)" --namespace "$(NAMESPACE)" -o name
	$(HELM) version --short

apply: check-deploy
	$(MAKE) publish
	$(MAKE) lint
	$(HELM) upgrade --install "$(RELEASE)" "$(CHART)" \
	  --namespace "$(NAMESPACE)" \
	  $(if $(HELM_VALUES),--values "$(HELM_VALUES)") \
	  --set-string image.repository="$(IMAGE_REPOSITORY)" \
	  --set-string image.tag="$(VERSION)" \
	  --set-string credentials.existingSecret="$(CREDENTIALS_SECRET)" \
	  --wait --timeout "$(HELM_TIMEOUT)"

deploy: apply