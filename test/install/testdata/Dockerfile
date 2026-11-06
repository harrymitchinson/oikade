# A machine with systemd, as fresh as an OS image gets: oikade install is all that's done to it.
FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus sudo >/dev/null
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
