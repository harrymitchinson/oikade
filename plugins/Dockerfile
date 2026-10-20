# Builds any plugin in plugins/ from the repository root:
#
#   docker buildx build --platform linux/arm64,linux/amd64 \
#     -f plugins/Dockerfile --build-arg PLUGIN=example-switch .
#
# PLUGIN is the directory name of a Go main package.
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26

# Go cross-compiles, so the build runs on the build machine for every target platform.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG PLUGIN=example-switch
WORKDIR /workspace
COPY go.mod go.sum ./
# go build fetches only the modules the plugin imports, so there's no go mod download step.
COPY api api
COPY contract contract
COPY internal internal
COPY sdk sdk
COPY plugins plugins
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /plugin ./plugins/${PLUGIN}

FROM gcr.io/distroless/static:nonroot
COPY --from=build /plugin /plugin
# Matching the operator's pod security context.
USER 1000:1000
ENTRYPOINT ["/plugin"]
