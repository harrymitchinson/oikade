#!/usr/bin/env bash
# agent-identity: check that hack/plan writes as oikade-agent[bot] inside Claude
# Code, as the user outside it, and never as the user when the App's key is
# missing. gh, security and curl are stubs on PATH, so nothing reaches GitHub
# or the Keychain. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
failed=0

fail() { echo "FAIL: $*" >&2; failed=1; }

# gh logs each call with the token it ran under. Only repo view answers.
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "${GH_TOKEN:-user}|$*" >>"$STUB_LOG"
[[ "$1 $2" == "repo view" ]] && echo o/r
exit 0
STUB

# security answers from $KEYCHAIN_DIR/<account>, and fails like the Keychain
# does when an item is missing.
cat >"$tmp/bin/security" <<'STUB'
#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do [[ "$1" == -a ]] && account="$2"; shift; done
[[ -f "$KEYCHAIN_DIR/$account" ]] || exit 44
cat "$KEYCHAIN_DIR/$account"
STUB

# curl records the JWT and answers the installation and token endpoints.
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
for a; do [[ "$a" == "Authorization: Bearer "* ]] && echo "${a#Authorization: Bearer }" >"$STUB_JWT"; done
case "${*: -1}" in
*/repos/o/r/installation) echo '{"id":42}' ;;
*/app/installations/42/access_tokens) echo '{"token":"ghs_agent"}' ;;
*) echo '{"message":"Not Found"}'; exit 22 ;;
esac
STUB
chmod +x "$tmp/bin/"*

export PATH="$tmp/bin:$PATH" STUB_LOG="$tmp/gh.log" STUB_JWT="$tmp/jwt" KEYCHAIN_DIR="$tmp/keychain" AGENT_REPO=o/r
mkdir "$KEYCHAIN_DIR"

note() { : >"$STUB_LOG"; "$root/hack/plan" note 7 "hello" 2>"$tmp/err"; }

# No key inside Claude Code: fails, names the fix, and calls gh not at all.
if CLAUDECODE=1 note; then fail "note succeeded with no key"; fi
grep -q 'Keychain' "$tmp/err" || fail "the missing-key error doesn't name the Keychain: $(cat "$tmp/err")"
grep -q 'never fall back' "$tmp/err" || fail "the missing-key error doesn't say it won't post as the user"
[[ ! -s "$STUB_LOG" ]] || fail "gh ran with no key: $(cat "$STUB_LOG")"

# An app-id but a key that isn't one: still nothing written.
echo 123 >"$KEYCHAIN_DIR/app-id"
echo 'bm90IGEga2V5Cg==' >"$KEYCHAIN_DIR/private-key"
if CLAUDECODE=1 note; then fail "note succeeded with a bad key"; fi
[[ ! -s "$STUB_LOG" ]] || fail "gh ran with a bad key: $(cat "$STUB_LOG")"

# Outside Claude Code: the user's login, and no App token minted.
rm "$KEYCHAIN_DIR"/*
CLAUDECODE='' note || fail "note failed outside Claude Code: $(cat "$tmp/err")"
grep -qx 'user|api repos/o/r/issues/7/comments -f body=hello' "$STUB_LOG" || fail "note outside Claude Code didn't comment as the user: $(cat "$STUB_LOG")"

# With the key: the comment goes out under the App's token, signed by its key.
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$tmp/key.pem" 2>/dev/null
openssl pkey -in "$tmp/key.pem" -pubout -out "$tmp/pub.pem"
echo 123 >"$KEYCHAIN_DIR/app-id"
openssl base64 -A -in "$tmp/key.pem" >"$KEYCHAIN_DIR/private-key"
CLAUDECODE=1 note || fail "note failed with a key: $(cat "$tmp/err")"
grep -qx 'ghs_agent|api repos/o/r/issues/7/comments -f body=hello' "$STUB_LOG" || fail "note didn't comment as the App: $(cat "$STUB_LOG")"
! grep -q '^user|api' "$STUB_LOG" || fail "a write ran as the user: $(cat "$STUB_LOG")"

# The JWT names the App and verifies against its public key.
IFS=. read -r header payload sig <"$STUB_JWT"
unb64() { local s="$1"; s=$(tr -- '-_' '+/' <<<"$s"); while (( ${#s} % 4 )); do s+='='; done; openssl base64 -d -A <<<"$s"; }
[[ "$(unb64 "$payload" | jq -r .iss)" == 123 ]] || fail "the JWT's iss isn't the App ID"
unb64 "$sig" >"$tmp/sig"
printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -verify "$tmp/pub.pem" -signature "$tmp/sig" >/dev/null ||
	fail "the JWT doesn't verify against the App's public key"

(( failed == 0 )) && echo "ok: agent identity"
exit "$failed"
