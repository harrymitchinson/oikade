#!/usr/bin/env bash
# agent-identity: check that hack/plan writes to GitHub as oikade-agent[bot]
# inside Claude Code and as the user outside it, writes to Linear only as the
# Oikade Agent app, never as the user when a credential is missing, and counts
# a design as approved only when the user moved it out of Design Review. gh,
# security and curl are stubs on PATH, so nothing reaches GitHub, Linear or the
# Keychain. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin" "$tmp/issues" "$tmp/history"
failed=0

fail() { echo "FAIL: $*" >&2; failed=1; }

# gh logs each call with the token it ran under. repo view answers o/r, and
# deleting the label "gone" fails as GitHub does for a label a PR doesn't have.
# Every other call answers nothing.
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "${GH_TOKEN:-user}|$*" >>"$STUB_LOG"
[[ "$1 $2" == "repo view" ]] && echo o/r
[[ "$*" == *"-X DELETE "*/labels/gone ]] && { echo "gh: Label does not exist (HTTP 404)" >&2; exit 1; }
exit 0
STUB

# security answers from $KEYCHAIN_DIR/<account>, fails like the Keychain does
# when an item is missing, and stores what security -i is told to add.
cat >"$tmp/bin/security" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == -i ]]; then
	read -r line
	account=$(sed -E 's/.* -a ([^ ]+) .*/\1/' <<<"$line")
	sed -E 's/.* -w "(.*)"$/\1/' <<<"$line" >"$KEYCHAIN_DIR/$account"
	exit 0
fi
while [[ $# -gt 0 ]]; do [[ "$1" == -a ]] && account="$2"; shift; done
[[ -f "$KEYCHAIN_DIR/$account" ]] || exit 44
cat "$KEYCHAIN_DIR/$account"
STUB

# curl plays GitHub's App endpoints, Linear's token endpoint and Linear's
# GraphQL API. GraphQL calls are logged as "TOKEN|QUERY|VARIABLES". An issue
# is $STUB_DIR/issues/<ID>.json, and its history $STUB_DIR/history/<ID>.json,
# which fails without the file.
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
token="" url="${*: -1}"
for a; do
	[[ "$a" == "Authorization: Bearer "* ]] && echo "${a#Authorization: Bearer }" >"$STUB_JWT"
	[[ "$a" == @/dev/fd/* || "$a" == @/proc/* ]] && token=$(sed -nE 's/^Authorization: Bearer //p' "${a#@}")
	[[ "$a" == http* ]] && url="$a"
done
case "$url" in
*/repos/o/r/installation) echo '{"id":42}'; exit ;;
*/app/installations/42/access_tokens) echo '{"token":"ghs_agent"}'; exit ;;
*/oauth/token) cat >/dev/null; echo '{"access_token":"lin_agent","expires_in":2592000}'; exit ;;
*/graphql) ;;
*) echo '{"message":"Not Found"}'; exit 22 ;;
esac
req=$(cat)
q=$(jq -r .query <<<"$req" | tr -s ' \n\t' ' ')
v=$(jq -c .variables <<<"$req")
echo "$token|$q|$v" >>"$STUB_LINEAR_LOG"
i=$(jq -r '.i // empty' <<<"$v")
case "$q" in
*"history("*)
	[[ -f "$STUB_DIR/history/$i.json" ]] || { echo '{"errors":[{"message":"internal error"}]}'; exit; }
	jq -c '{data: {issue: {history: {nodes: .}}}}' "$STUB_DIR/history/$i.json" ;;
*"issue(id:"*) jq -c '{data: {issue: .}}' "$STUB_DIR/issues/$i.json" ;;
*"users("*) echo '{"data":{"users":{"nodes":[{"id":"u-owner","name":"Owner","app":false},{"id":"u-agent","name":"Oikade Agent","app":true}]}}}' ;;
*"team(id:"*) echo '{"data":{"team":{"id":"t1","states":{"nodes":[{"id":"s-todo","name":"Todo","type":"unstarted"},{"id":"s-dr","name":"Design Review","type":"backlog"},{"id":"s-backlog","name":"Backlog","type":"backlog"}]}}}}' ;;
*"issueLabels("*) echo '{"data":{"issueLabels":{"nodes":[{"id":"l1"}]}}}' ;;
*) echo '{"data":{"ok":{"success":true}}}' ;;
esac
STUB
chmod +x "$tmp/bin/"*

export PATH="$tmp/bin:$PATH" STUB_LOG="$tmp/gh.log" STUB_LINEAR_LOG="$tmp/linear.log" STUB_JWT="$tmp/jwt" \
	KEYCHAIN_DIR="$tmp/keychain" STUB_DIR="$tmp" AGENT_REPO=o/r PLAN_REMOTE=none
mkdir "$KEYCHAIN_DIR"

# --- GitHub ------------------------------------------------------------------

note() { : >"$STUB_LOG"; "$root/hack/plan" note 7 "hello" >/dev/null 2>"$tmp/err"; }

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

run() { : >"$STUB_LOG"; : >"$STUB_LINEAR_LOG"; CLAUDECODE=1 "$root/hack/plan" "$@" >"$tmp/out" 2>"$tmp/err"; }

run label 7 +a +b -gone || fail "label on a PR failed: $(cat "$tmp/err")"
grep -qx 'ghs_agent|api repos/o/r/issues/7/labels -f labels\[\]=a -f labels\[\]=b' "$STUB_LOG" ||
	fail "label didn't add both labels in one request as the App: $(cat "$STUB_LOG")"

# --- Linear ------------------------------------------------------------------

# issue ID STATE [DESCRIPTION]: write a fixture.
issue() {
	jq -n --arg i "$1" --arg s "$2" --arg d "${3:-}" '{id: ("uuid-" + $i), identifier: $i, title: "T", url: "u", description: $d,
		state: {name: $s, type: (if $s == "Todo" then "unstarted" else "backlog" end)}, project: null, parent: null,
		labels: {nodes: []}, children: {nodes: []}, inverseRelations: {nodes: []}}' >"$tmp/issues/$1.json"
}
# moved ID FROM TO ACTOR...: a history of status changes by the given actors.
moved() {
	local i="$1" out="[]" n=0
	shift
	while [[ $# -gt 0 ]]; do
		out=$(jq -c --arg f "$1" --arg t "$2" --arg a "$3" --argjson n "$n" \
			'. + [{createdAt: "2026-10-2\($n)T00:00:00Z", fromState: (if $f == "" then null else {name: $f} end),
				toState: {name: $t}, actor: {id: $a, name: $a}}]' <<<"$out")
		shift 3
		n=$((n + 1))
	done
	echo "$out" >"$tmp/history/$i.json"
}

# No Linear credentials: nothing reaches Linear, and the error names the fix.
issue OIK-1 Todo
moved OIK-1
if run note OIK-1 hi; then fail "note OIK-1 succeeded with no Linear credentials"; fi
grep -q 'Oikade Agent' "$tmp/err" || fail "the missing-credential error doesn't name the Oikade Agent: $(cat "$tmp/err")"
[[ ! -s "$STUB_LINEAR_LOG" ]] || fail "Linear was called with no credentials: $(cat "$STUB_LINEAR_LOG")"

# With them: the comment goes to Linear as the app, and the token is cached.
echo cid >"$KEYCHAIN_DIR/linear-client-id"
echo secret >"$KEYCHAIN_DIR/linear-client-secret"
run note OIK-1 hi || fail "note OIK-1 failed: $(cat "$tmp/err")"
grep -q '^lin_agent|mutation.*commentCreate' "$STUB_LINEAR_LOG" || fail "note didn't comment in Linear as the app: $(cat "$STUB_LINEAR_LOG")"
grep -q ' lin_agent$' "$KEYCHAIN_DIR/linear-access-token" || fail "the Linear token wasn't cached"
! grep -q '^|' "$STUB_LINEAR_LOG" || fail "a Linear call went out without the app's token"
! grep -q secret "$STUB_LINEAR_LOG" || fail "the client secret reached a GraphQL call"

# Labels that record the user's decisions: removing needs-review or
# agent-merged, on a PR or an issue, in any case. A refusal writes nothing.
for args in "label 7 -needs-review" "label 7 -agent-merged" "label OIK-1 -Needs-Review" "label OIK-1 +bug -agent-merged"; do
	# shellcheck disable=SC2086 # split args into words
	if run $args; then fail "plan $args succeeded"; fi
	grep -q 'only the user' "$tmp/err" || fail "plan $args didn't say why: $(cat "$tmp/err")"
	! grep -q '|api ' "$STUB_LOG" || fail "plan $args wrote to GitHub: $(cat "$STUB_LOG")"
	! grep -q 'mutation' "$STUB_LINEAR_LOG" || fail "plan $args wrote to Linear: $(cat "$STUB_LINEAR_LOG")"
done
run label OIK-1 +needs-review || fail "adding needs-review failed: $(cat "$tmp/err")"

mutated() { grep -q 'mutation' "$STUB_LINEAR_LOG"; }
may_build() { run show OIK-1; grep -E '^(May build|Approved):' "$tmp/out"; }

# Waiting in Design Review: not buildable, and an agent can't move it on.
issue OIK-1 "Design Review"
moved OIK-1
may_build | grep -q '^May build:  no' || fail "show counts a design waiting for review: $(may_build)"
if run state OIK-1 Todo; then fail "an agent moved an issue out of Design Review"; fi
! mutated || fail "a refused move wrote: $(cat "$STUB_LINEAR_LOG")"

# The user moved it to Todo: approved.
issue OIK-1 Todo
moved OIK-1 "Design Review" Todo u-owner
may_build | grep -qx 'May build:  approved' || fail "show doesn't count the user's move to Todo: $(may_build)"

# Someone else moved it: not approved, and show says who.
moved OIK-1 "Design Review" Todo u-agent
may_build | grep -q '^May build:  no' || fail "show counts the agent's move out of Design Review: $(may_build)"
may_build | grep -q 'by u-agent, not the user' || fail "show doesn't say who moved it: $(may_build)"
if run state OIK-1 "In Progress"; then fail "an agent moved an issue the agent took out of Design Review"; fi

# A move with no actor, such as an automation's, isn't the user's.
moved OIK-1 "Design Review" Todo ""
may_build | grep -q '^May build:  no' || fail "show counts a move out of Design Review with no actor: $(may_build)"

# The user sent it back to Backlog, then an agent moved it to Todo: not approved.
moved OIK-1 "" "Design Review" u-agent "Design Review" Backlog u-owner Backlog Todo u-agent
may_build | grep -q '^May build:  no' || fail "show counts a design the user sent back: $(may_build)"
issue OIK-1 Backlog
if run state OIK-1 Todo; then fail "an agent moved a design the user sent back into Todo"; fi
! mutated || fail "a refused move wrote: $(cat "$STUB_LINEAR_LOG")"

# Never in Design Review: needs no design, and an agent may move it to Todo.
moved OIK-1 "" Backlog u-agent
run state OIK-1 Todo || fail "an agent couldn't move an issue that needs no design to Todo: $(cat "$tmp/err")"
issue OIK-1 Todo
may_build | grep -qx 'May build:  needs no design' || fail "show doesn't build an issue that never needed a design: $(may_build)"

# Design: OIK-2 counts only if the user approved OIK-2.
issue OIK-1 Todo $'Design: OIK-2\n'
issue OIK-2 Todo
moved OIK-2 "Design Review" Todo u-owner
may_build | grep -qx 'May build:  builds the design approved in OIK-2' || fail "show doesn't count a Design: line the user approved: $(may_build)"
moved OIK-2 "Design Review" Todo u-agent
may_build | grep -q '^May build:  no' || fail "show counts a Design: line the agent approved: $(may_build)"

# A failed history read stops rather than reading as "never needed a design".
rm "$tmp/history/OIK-1.json"
if run show OIK-1; then fail "show succeeded with no history"; fi
grep -q "couldn't read OIK-1's history" "$tmp/err" || fail "show didn't say the history read failed: $(cat "$tmp/err")"
issue OIK-1 Backlog
if run state OIK-1 Todo; then fail "state moved an issue whose history couldn't be read"; fi
! mutated || fail "state wrote with no history: $(cat "$STUB_LINEAR_LOG")"

# On an approved design only ## Steps and below may change.
printf '## Why\nA\n\n## Steps\none\n' >"$tmp/approved.md"
printf '## Why\nB\n\n## Steps\none\n' >"$tmp/redesigned.md"
printf '## Why\nA\n\n## Steps\none\ntwo\n' >"$tmp/restepped.md"
issue OIK-1 Todo "$(cat "$tmp/approved.md")"
moved OIK-1 "Design Review" Todo u-owner
if run edit OIK-1 "$tmp/redesigned.md"; then fail "edit changed an approved design"; fi
! mutated || fail "edit wrote an approved design: $(cat "$STUB_LINEAR_LOG")"
run edit OIK-1 "$tmp/restepped.md" || fail "edit refused new Steps on an approved design: $(cat "$tmp/err")"
issue OIK-1 "Design Review" "$(cat "$tmp/approved.md")"
moved OIK-1
run edit OIK-1 "$tmp/redesigned.md" || fail "edit refused a design under review: $(cat "$tmp/err")"

(( failed == 0 )) && echo "ok: agent identity"
exit "$failed"
