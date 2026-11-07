# Image URL to use all building/pushing image targets
IMG ?= oikade:latest
# VERSION is baked into oikadectl and oikade-host (`oikadectl version`, `oikade-host version`). Releases set it to the tag.
VERSION ?= dev

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

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# The journeys need Docker and take minutes, so they have their own target.
.PHONY: test
test: fmt vet ## Run the tests.
	go test $$(go list ./... | grep -v -e /test/journeys) -coverprofile cover.out

# The chip-tool image is tagged by its Dockerfile's hash, so it's built once per change to it.
# Building it pulls connectedhomeip's 6 GB image, which is linux/arm64 only.
CHIP_TOOL_IMG ?= oikade-chip-tool:$(shell git hash-object test/journeys/chip-tool/Dockerfile | cut -c1-12)

.PHONY: chip-tool-image
chip-tool-image: ## Build the chip-tool image for the journeys, unless it's already there.
	$(CONTAINER_TOOL) image inspect $(CHIP_TOOL_IMG) >/dev/null 2>&1 || \
		$(CONTAINER_TOOL) build -t $(CHIP_TOOL_IMG) test/journeys/chip-tool

.PHONY: test-journeys
test-journeys: chip-tool-image ## Follow a home from oikadectl init to a running host, and pair chip-tool with it. Needs Docker.
	OIKADE_CHIP_TOOL_IMAGE=$(CHIP_TOOL_IMG) go test ./test/journeys/ -v -count=1 -timeout 15m

# CONFORMANCE_PLUGIN is the command the conformance suite runs, from the repository root.
CONFORMANCE_PLUGIN ?= bin/simswitch

.PHONY: simswitch
simswitch: ## Build the simulated-relay test fixture into bin/.
	go build -o bin/simswitch ./test/fixtures/simswitch
	go build -o bin/simhome ./test/fixtures/simswitch/home

.PHONY: test-conformance
test-conformance: simswitch ## Drive a plugin through the oikade/v1 contract and the Go API (default: the simswitch fixture).
	OIKADE_CONFORMANCE_PLUGIN="$(CONFORMANCE_PLUGIN)" go test ./test/conformance/ -v -count=1
	OIKADE_CONFORMANCE_PLUGIN=bin/simhome OIKADE_CONFORMANCE_IMPORTED=simswitch go test ./test/conformance/ -v -count=1

.PHONY: test-hack
test-hack: ## Check the hack/ scripts: shellcheck, hack/plan's agent identity, merge on approval and plugin-repo steps with gh stubbed, and no raw gh writes in skills.
	shellcheck hack/plan hack/agent-token hack/linear-token hack/owner-approval hack/test/*.sh
	hack/test/agent-identity.sh
	hack/test/merge-on-approval.sh
	hack/test/plugin-repo.sh
	hack/test/no-raw-gh-writes.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: fmt vet ## Build bin/oikadectl and bin/oikade-host.
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ ./cmd/oikadectl ./cmd/oikade-host

# A release is built by GoReleaser (.goreleaser.yaml): the archives, their checksums and the image.
# This builds them all into dist/, and the image for each platform, without publishing anything.
.PHONY: release-snapshot
release-snapshot: goreleaser ## Build what a release ships into dist/, publishing nothing.
	"$(GORELEASER)" release --snapshot --clean

# release publishes the tag that's checked out, to its GitHub Release and ghcr.io. Only the release
# workflow runs it, with GITHUB_TOKEN set and a buildx builder that can push two platforms.
.PHONY: release
release: goreleaser ## Publish a release from the checked-out tag (CI only).
	"$(GORELEASER)" release --clean

# The Dockerfile compiles nothing, so the image's context is oikade-host built for this machine's
# platform, at <os>/<arch>/oikade-host as GoReleaser lays it out, with .goreleaser.yaml's flags.
DOCKER_CONTEXT ?= dist/docker
DOCKER_ARCH ?= $(shell go env GOARCH)
.PHONY: docker-build
docker-build: ## Build the oikade image for this machine's platform, from source.
	CGO_ENABLED=0 GOOS=linux GOARCH=$(DOCKER_ARCH) go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
		-o "$(DOCKER_CONTEXT)/linux/$(DOCKER_ARCH)/oikade-host" ./cmd/oikade-host
	$(CONTAINER_TOOL) build --platform linux/$(DOCKER_ARCH) -f Dockerfile -t ${IMG} "$(DOCKER_CONTEXT)"

.PHONY: docker-push
docker-push: ## Push the oikade image.
	$(CONTAINER_TOOL) push ${IMG}

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GORELEASER = $(LOCALBIN)/goreleaser

## Tool Versions
GOLANGCI_LINT_VERSION ?= v2.15.0
GORELEASER_VERSION ?= v2.20.0
.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
# bin/ is order-only: a restored CI cache leaves it newer than the binary, which
# would rebuild it every run.
$(GOLANGCI_LINT): | $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: goreleaser
goreleaser: $(GORELEASER) ## Download GoReleaser locally if necessary.
$(GORELEASER): | $(LOCALBIN)
	$(call go-install-tool,$(GORELEASER),github.com/goreleaser/goreleaser/v2,$(GORELEASER_VERSION))

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
