# The oikade image: the host, for linux/arm64 and linux/amd64. It holds no Node and nothing else to
# run, because you bring your own Zigbee2MQTT and broker (OIK-155), and your code arrives by deploy.
# The operator's Deployments still run /manager from it, until OIK-160 removes them.
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go source (relies on .dockerignore to filter)
COPY . .

RUN mkdir -p data

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
# VERSION is what `oikade version`, `manager --version` and the startup logs report.
# The cache mounts keep modules and compiled packages between builds, so a rebuild
# only compiles what changed.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    sh -c 'CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags "-X main.version=${VERSION}" -o manager ./cmd && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags "-X main.version=${VERSION}" -o oikade ./cmd/oikade'

# Use distroless as minimal base image
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
# On the PATH, for docker exec.
COPY --from=builder /workspace/oikade /usr/local/bin/oikade
# Owned by the user below, which a new volume mounted here takes on.
COPY --from=builder --chown=65532:65532 /workspace/data /var/lib/oikade
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/oikade"]
CMD ["host"]
