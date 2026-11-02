#!/usr/bin/env bash
# Commission `manager matter-spike` with connectedhomeip's chip-tool, both in Docker on one
# network, so mDNS works on macOS too. Then read and toggle the bridged switch, restart the spike,
# and read it again over a new CASE session (OIK-81). Needs Docker; runs on the host's arch.
set -euo pipefail

# connectedhomeip's published chip-tool, pinned by digest (tag 8862cba105e5351fcd0ff695c331214d90376070,
# 2026-10-29). It's about 6 GB.
CHIP_IMAGE=${CHIP_IMAGE:-connectedhomeip/chip-cert-bins@sha256:92951d7ad7c00357ce3e1971e53ed6e4b458188ed966cf2f210a6eb6a45011e2}
NAME=${NAME:-oikade-matter-spike}
NODE_ID=${NODE_ID:-0x1122}
ENDPOINT=2

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
cleanup() {
	docker rm -f "$NAME" "$NAME-chip-tool" >/dev/null 2>&1 || true
	docker network rm "$NAME" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
cleanup_logs() { docker logs "$NAME" 2>&1 | tail -50 || true; }
fail() { echo "FAIL: $*" >&2; cleanup_logs >&2; exit 1; }

arch=$(docker version --format '{{.Server.Arch}}')
echo "Building the manager for linux/$arch"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$work/manager" ./cmd)
mkdir -p "$work/data"

# chip-tool finds nodes by their IPv6 addresses, so the network needs IPv6.
docker network create --ipv6 "$NAME" >/dev/null
start_spike() {
	docker run -d --name "$NAME" --network "$NAME" \
		-v "$work/manager:/manager:ro" -v "$work/data:/data" \
		--entrypoint /manager "$CHIP_IMAGE" matter-spike --data-dir /data >/dev/null
	for _ in $(seq 50); do
		if docker logs "$NAME" 2>&1 | grep -qE 'Advertised operational service|Opened commissioning window'; then
			return
		fi
		sleep 0.2
	done
	fail "the spike didn't start"
}
start_spike
passcode=$(docker logs "$NAME" 2>&1 | sed -n 's/.*"passcode":\([0-9]*\).*/\1/p' | head -1)
[ -n "$passcode" ] || fail "no passcode in the spike's log"

# chip-tool browses DNS-SD through Avahi, over D-Bus.
docker run -d --name "$NAME-chip-tool" --network "$NAME" --entrypoint sh "$CHIP_IMAGE" -c \
	'mkdir -p /run/dbus && dbus-daemon --system && avahi-daemon -D --no-chroot --no-drop-root && exec sleep infinity' >/dev/null
for _ in $(seq 50); do
	docker exec "$NAME-chip-tool" avahi-daemon --check 2>/dev/null && break
	sleep 0.2
done
chip() { docker exec "$NAME-chip-tool" /root/chip-tool "$@" 2>&1; }
# onoff reads the switch's OnOff attribute.
onoff() {
	chip onoff read on-off "$NODE_ID" "$ENDPOINT" | tee -a "$work/chip-tool.log" |
		sed -nE 's/.*OnOff: (TRUE|FALSE).*/\1/p' | tail -1
}

echo "Commissioning with chip-tool pairing onnetwork"
chip pairing onnetwork "$NODE_ID" "$passcode" >"$work/pairing.log" || {
	tail -60 "$work/pairing.log"
	fail "chip-tool pairing onnetwork"
}
grep -q 'Device commissioning completed with success' "$work/pairing.log" || fail "commissioning didn't complete"
echo "PASS: commissioned"

before=$(onoff)
[ -n "$before" ] || fail "couldn't read OnOff"
chip onoff toggle "$NODE_ID" "$ENDPOINT" >>"$work/chip-tool.log" || fail "chip-tool onoff toggle"
after=$(onoff)
[ -n "$after" ] && [ "$after" != "$before" ] || fail "OnOff was $before, and $after after the toggle"
docker logs "$NAME" 2>&1 | grep -q 'Switched bridged switch' || fail "the spike didn't log the toggle"
echo "PASS: OnOff $before, then $after after toggle"

echo "Restarting the spike"
docker rm -f "$NAME" >/dev/null
start_spike
docker logs "$NAME" 2>&1 | grep -q '"fabrics":1' || fail "the spike lost its fabric"
! docker logs "$NAME" 2>&1 | grep -q 'Opened commissioning window' || fail "the spike opened its window, though it's on a fabric"
again=$(onoff)
[ -n "$again" ] || fail "couldn't read OnOff over CASE after the restart"
docker logs "$NAME" 2>&1 | grep -q 'Established CASE session' || fail "no CASE session after the restart"
echo "PASS: read OnOff ($again, the restart reset the state in memory) over CASE after the restart, with no re-commissioning"
