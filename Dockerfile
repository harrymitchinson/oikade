# The oikade image: oikade-host, for linux/arm64 and linux/amd64. It holds no Node and nothing else to
# run, because you bring your own Zigbee2MQTT and broker (OIK-155), and your code arrives by deploy.
#
# It compiles nothing: the context holds oikade-host already built, at <os>/<arch>/oikade-host, as
# GoReleaser's dockers_v2 lays it out for a release and make docker-build does locally (OIK-170).
FROM gcr.io/distroless/static:nonroot
ARG TARGETPLATFORM
WORKDIR /
# On the PATH, for docker exec.
COPY ${TARGETPLATFORM}/oikade-host /usr/local/bin/oikade-host
# Owned by the user below, which a new volume mounted here takes on. distroless has no shell to make
# it with, so it's a copy of that user's own empty home directory.
COPY --chown=65532:65532 --from=gcr.io/distroless/static:nonroot /home/nonroot /var/lib/oikade
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/oikade-host"]
