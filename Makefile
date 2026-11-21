# IMG is the image docker-build builds and docker-push pushes.
IMG ?= oikade:latest
# VERSION is the version make build and make docker-build bake into the binaries they build. A
# release's binaries get their tag from GoReleaser instead.
VERSION ?= dev

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL builds and pushes the images. Only docker is tested.
CONTAINER_TOOL ?= docker

# Recipes run in bash, and stop at a line that fails, or a pipe with a command that fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# help prints each target's ## text, under the ##@ category above it.
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

# The journeys need Docker and take minutes, so they have their own target. test/host's and
# test/cli's tests mostly wait on the processes they started, so they run eight at once however
# many CPUs there are (no other package has more than a few parallel tests). On a loaded machine
# test/host can run past go test's 10-minute default, so a slow test fails on its own wait instead.
.PHONY: test
test: fmt vet ## Run the tests.
	go test -parallel 8 -timeout 20m $$(go list ./... | grep -v -e /test/journeys)

# The race detector, on everything but test/host and test/cli, which take minutes under it and have
# a target of their own.
.PHONY: test-race
test-race: ## Run the tests in this process under the race detector.
	go test -race $$(go list ./... | grep -v -e /test/journeys -e /test/host -e /test/cli)

# Under -race, test/host and test/cli build the host, the CLI and the home with it too, and fail if
# any process they started reported a race (test/internal/childrace). CI runs this nightly: it takes
# about ten minutes.
.PHONY: test-race-binaries
test-race-binaries: ## Run test/host and test/cli with the race detector in every binary they start.
	go test -race -parallel 8 -timeout 30m -count=1 ./test/host ./test/cli

# test/host and test/cli run built binaries, which go test -cover can't see. This builds them with
# coverage, the home included, prints each package's share and writes what they ran in this module
# to cover-binaries.out (go tool cover -func or -html reads it; the home's own package isn't in this
# module). make test measures nothing: no number is checked, so it doesn't pay for one.
.PHONY: cover-binaries
cover-binaries: ## Measure the coverage of the binaries test/host and test/cli run.
	rm -rf $(LOCALBIN)/cover && mkdir -p $(LOCALBIN)/cover
	OIKADE_COVER=1 GOCOVERDIR=$(LOCALBIN)/cover go test ./test/host ./test/cli -count=1 -timeout 20m
	go tool covdata textfmt -i=$(LOCALBIN)/cover -pkg='oikade.dev/...' -o cover-binaries.out
	go tool covdata percent -i=$(LOCALBIN)/cover

# The chip-tool image is tagged by its Dockerfile's hash, so it's built once per change to it.
# Building it pulls connectedhomeip's 6 GB image, which is linux/arm64 only.
CHIP_TOOL_IMG ?= oikade-chip-tool:$(shell git hash-object test/journeys/chip-tool/Dockerfile | cut -c1-12)

.PHONY: chip-tool-image
chip-tool-image: ## Build the chip-tool image for the journeys, unless it's already there.
	$(CONTAINER_TOOL) image inspect $(CHIP_TOOL_IMG) >/dev/null 2>&1 || \
		$(CONTAINER_TOOL) build -t $(CHIP_TOOL_IMG) test/journeys/chip-tool

.PHONY: test-journeys
test-journeys: chip-tool-image ## Follow a home from oikade init to a running host, and pair chip-tool with it. Needs Docker.
	OIKADE_CHIP_TOOL_IMAGE=$(CHIP_TOOL_IMG) go test ./test/journeys/ -v -count=1 -timeout 15m

# CONFORMANCE_PLUGIN is the command the conformance suite runs, from the repository root.
CONFORMANCE_PLUGIN ?= bin/simswitch

.PHONY: simswitch
simswitch: ## Build the simulated-relay test fixture into bin/.
	go build -o bin/simswitch ./test/fixtures/simswitch
	go build -o bin/simhome ./test/fixtures/simswitch/home

.PHONY: test-conformance
test-conformance: simswitch ## Drive a plugin through the oikade/v1 contract, over MQTT and the host's socket (default: the simswitch fixture).
	OIKADE_CONFORMANCE_PLUGIN="$(CONFORMANCE_PLUGIN)" go test ./test/conformance/ -v -count=1
	OIKADE_CONFORMANCE_PLUGIN=bin/simhome OIKADE_CONFORMANCE_IMPORTED=simswitch go test ./test/conformance/ -v -count=1

# go test -fuzz takes one target per run, so hack/fuzz runs each in turn, for FUZZTIME. The nightly
# Fuzz workflow runs it.
FUZZTIME ?= 30s
.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME (30s) each, one at a time.
	hack/fuzz $(FUZZTIME)

.PHONY: test-hack
test-hack: ## Shellcheck the hack/ scripts and run their tests.
	shellcheck -x hack/plan hack/fuzz hack/agent-token hack/owner-approval hack/test/*.sh
	hack/test/agent-identity.sh
	hack/test/merge-on-approval.sh
	hack/test/plugin-repo.sh
	hack/test/checklist-steps.sh
	hack/test/plan-writes.sh
	hack/test/no-raw-gh-writes.sh
	hack/test/no-linear.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply its fixes.
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Check golangci-lint's configuration.
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: fmt vet ## Build bin/oikade and bin/oikade-host.
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ ./cmd/oikade ./cmd/oikade-host

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

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GORELEASER = $(LOCALBIN)/goreleaser

GOLANGCI_LINT_VERSION ?= v2.15.0
# The newest GoReleaser that builds with go.mod's Go, as CI runs with GOTOOLCHAIN=local. Raise both together.
GORELEASER_VERSION ?= v2.18.0
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

# go-install-tool installs package $(2) at version $(3) as $(1)-$(3), unless it's there, and points
# the link $(1) at it.
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
