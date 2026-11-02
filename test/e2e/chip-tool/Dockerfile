# check=skip=FromPlatformFlagConstDisallowed
# chip-tool for the e2e tests, without the rest of connectedhomeip's 6 GB certification image.
# That image is published for linux/arm64 only (tag 8862cba105e5351fcd0ff695c331214d90376070,
# 2026-10-29), so this one is too. chip-tool browses DNS-SD through Avahi over D-Bus, so the
# image has both daemons.
FROM --platform=linux/arm64 connectedhomeip/chip-cert-bins@sha256:92951d7ad7c00357ce3e1971e53ed6e4b458188ed966cf2f210a6eb6a45011e2 AS src

# The Ubuntu release the certification image is built on, so chip-tool's libraries match.
FROM --platform=linux/arm64 ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852
RUN apt-get update && apt-get install -y --no-install-recommends \
		avahi-daemon dbus libavahi-client3 libevent-core-2.1-7t64 libevent-pthreads-2.1-7t64 \
		libglib2.0-0t64 libpcsclite1 libssl3t64 \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=src /root/apps/chip-tool /usr/local/bin/chip-tool
RUN chip-tool 2>&1 | grep -q 'Usage:'
CMD ["sh", "-c", "mkdir -p /run/dbus && dbus-daemon --system && avahi-daemon -D --no-chroot --no-drop-root && exec sleep infinity"]
