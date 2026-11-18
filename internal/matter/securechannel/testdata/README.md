# Handshakes recorded with chip-tool

connectedhomeip's tests have no fixed bytes for a PASE or CASE handshake, or for a whole encrypted message: `src/protocols/secure_channel/tests/TestPASESession.cpp` and `TestCASESession.cpp`, and `src/transport/tests/TestSecureSession.cpp` (checked at commit `2dfaa494735663f37cbcd9c23ad2111380bc4bc2`), run both sides with fresh randomness, and pin only the passcode verifier, which `internal/matter/crypto` already checks. So the external truth here is chip-tool itself: handshakes between chip-tool and Oikade, recorded with the secrets Oikade's side drew. The tests in `chiptool_test.go` replay Oikade's side and check it against chip-tool's messages, including chip-tool's confirmations, signatures and encrypted messages (OIK-306).

Both are chip-tool from the journeys' image (`test/journeys/chip-tool`, from `connectedhomeip/chip-cert-bins` tagged with connectedhomeip commit `8862cba105e5351fcd0ff695c331214d90376070`). chip-tool's log in each shows that it authenticated Oikade's messages.

- `pase-chip-tool.json` is chip-tool commissioning Oikade's PASE responder with passcode 20202021, up to the first secure message each way (`TestCaptureChipToolPASE`).
- `case-chip-tool.json` is chip-tool commissioning a whole Oikade node, which ends in a full CASE, then expiring that session and reading from the node, which resumes it (`TestCaptureChipToolCASE`). It has every datagram on the node's port, sniffed, the ephemeral key of each CASE the node answered, and the fabric chip-tool put the node on.

To record them again, from the repository's root (Docker, linux/arm64). The CASE capture sniffs, so it runs as root, and chip-tool finds the node by DNS-SD on an IPv6 network:

```sh
make chip-tool-image
img=oikade-chip-tool:$(git hash-object test/journeys/chip-tool/Dockerfile | cut -c1-12)
out=$(mktemp -d)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o "$out/securechannel.test" ./internal/matter/securechannel
docker network create --ipv6 --subnet fd6f:696b:6165::/64 oikade-capture
docker run --rm --network oikade-capture \
	-v "$out:/w" -v "$PWD/internal/matter/securechannel:/pkg" -w /pkg \
	-e OIKADE_CHIP_TOOL=chip-tool \
	-e OIKADE_CHIP_TOOL_SOURCE="chip-tool from connectedhomeip/chip-cert-bins:8862cba105e5351fcd0ff695c331214d90376070 (connectedhomeip commit 8862cba105e5351fcd0ff695c331214d90376070)" \
	"$img" sh -c 'mkdir -p /run/dbus && dbus-daemon --system && avahi-daemon -D --no-chroot --no-drop-root
		/w/securechannel.test -test.run "TestCaptureChipTool(PASE|CASE)" -test.v'
docker network rm oikade-capture
```

A new recording has new random values, so its bytes all change, and the tests still pass.
