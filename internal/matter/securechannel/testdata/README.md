# Handshakes recorded with chip-tool

connectedhomeip's tests have no fixed bytes for a PASE handshake: `src/protocols/secure_channel/tests/TestPASESession.cpp` (checked at commit `2dfaa494735663f37cbcd9c23ad2111380bc4bc2`) runs both sides with fresh randomness, and pins only the passcode verifier, which `internal/matter/crypto` already checks. So the external truth here is chip-tool itself: a handshake between chip-tool and Oikade, recorded with the random values Oikade's side drew. `TestPASEAgainstChipTool` replays Oikade's side and checks it against chip-tool's messages, including chip-tool's confirmation and its first encrypted message ([OIK-306](https://linear.app/oikade/issue/OIK-306)).

`pase-chip-tool.json` is chip-tool from the journeys' image (`test/journeys/chip-tool`, from `connectedhomeip/chip-cert-bins` tagged with connectedhomeip commit `8862cba105e5351fcd0ff695c331214d90376070`) commissioning Oikade's PASE responder with passcode 20202021, up to the first secure message each way. chip-tool's log in it shows that it authenticated Oikade's.

To record it again, from the repository's root (Docker, linux/arm64):

```sh
make chip-tool-image
img=oikade-chip-tool:$(git hash-object test/journeys/chip-tool/Dockerfile | cut -c1-12)
out=$(mktemp -d)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o "$out/securechannel.test" ./internal/matter/securechannel
docker run --rm --sysctl net.ipv6.conf.all.disable_ipv6=0 \
	-v "$out:/w" -v "$PWD/internal/matter/securechannel:/pkg" -w /pkg \
	-e OIKADE_CHIP_TOOL=chip-tool \
	-e OIKADE_CHIP_TOOL_SOURCE="chip-tool from connectedhomeip/chip-cert-bins:8862cba105e5351fcd0ff695c331214d90376070 (connectedhomeip commit 8862cba105e5351fcd0ff695c331214d90376070)" \
	"$img" sh -c 'mkdir -p /run/dbus && dbus-daemon --system && avahi-daemon -D --no-chroot --no-drop-root
		/w/securechannel.test -test.run TestCaptureChipToolPASE -test.v'
```

A new recording has new random values, so its bytes all change, and the tests still pass.
