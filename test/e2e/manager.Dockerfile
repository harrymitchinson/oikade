# The e2e manager image from a manager binary built outside Docker, so the build
# machine's Go cache applies. make docker-build-prebuilt builds the binary and
# uses its directory as the context. Releases build from the root Dockerfile;
# the final stage here must match its final stage.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
