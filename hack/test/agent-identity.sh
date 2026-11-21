#!/usr/bin/env bash
# agent-identity: check that hack/plan writes to GitHub as oikade-agent[bot]
# inside Claude Code and as the user outside it, never as the user when the
# App's key is missing, writes to the plan as the Oikade Agent, and counts a
# design as approved only when the user moved it out of Design Review. gh,
# security and curl are stubs on PATH, so nothing reaches GitHub or the
# Keychain, and the plan is a scratch plan/. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
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

# curl plays GitHub's App endpoints.
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
url="${*: -1}"
for a; do
	[[ "$a" == "Authorization: Bearer "* ]] && echo "${a#Authorization: Bearer }" >"$STUB_JWT"
	[[ "$a" == http* ]] && url="$a"
done
case "$url" in
*/repos/o/r/installation) echo '{"id":42}' ;;
*/app/installations/42/access_tokens) echo '{"token":"ghs_agent"}' ;;
*) echo '{"message":"Not Found"}'; exit 22 ;;
esac
STUB
chmod +x "$tmp/bin/"*

export PATH="$tmp/bin:$PATH" STUB_LOG="$tmp/gh.log" STUB_JWT="$tmp/jwt" \
	KEYCHAIN_DIR="$tmp/keychain" AGENT_REPO=o/r PLAN_REMOTE=none PLAN_DIR="$tmp/plan-dir" \
	GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir "$KEYCHAIN_DIR"
# shellcheck source=hack/test/plan-fixture.sh
. "$root/hack/test/plan-fixture.sh"
plan_init

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

run() { : >"$STUB_LOG"; plan_mark; CLAUDECODE=1 "$root/hack/plan" "$@" >"$tmp/out" 2>"$tmp/err"; }

run label 7 +a +b -gone || fail "label on a PR failed: $(cat "$tmp/err")"
grep -qx 'ghs_agent|api repos/o/r/issues/7/labels -f labels\[\]=a -f labels\[\]=b' "$STUB_LOG" ||
	fail "label didn't add both labels in one request as the App: $(cat "$STUB_LOG")"

# issue ID FOLDER [DESCRIPTION]: write OIK-ID into a status folder.
issue() { plan_issue "$2" "$1" T "${3:-}" "${HISTORY:-}"; }
# moved FROM TO WHO...: the status-history front matter for the given changes.
moved() {
	local out="[]" n=0
	while [[ $# -gt 0 ]]; do
		out=$(jq -c --arg f "${1:-none}" --arg t "$2" --arg a "$3" --argjson n "$n" '. + ["2026-10-2\($n)T00:00:00Z \($f) -> \($t) by \($a)"]' <<<"$out")
		shift 3
		n=$((n + 1))
	done
	jq -cn --argjson h "$out" '{"status-history": $h}'
}

# A note on an issue is a comment of its own, by the Oikade Agent, committed.
HISTORY="" issue OIK-1 todo
run note OIK-1 hi || fail "note OIK-1 failed: $(cat "$tmp/err")"
c=$(find "$PLAN_DIR/todo/OIK-1/comments" -name '*-oikade-agent.md' | head -1)
[[ -n "$c" ]] || fail "note didn't write a comment by the Oikade Agent: $(find "$PLAN_DIR" -path '*/comments/*')"
grep -qx 'author: "Oikade Agent"' "$c" || fail "the comment doesn't name its author: $(cat "$c")"
grep -qx 'hi' "$c" || fail "the comment doesn't say hi: $(cat "$c")"
[[ "$(plan_commits)" == "OIK-1: comment" ]] || fail "note didn't commit its comment: $(plan_commits)"
! grep -q '|api ' "$STUB_LOG" || fail "a note on an issue wrote to GitHub: $(cat "$STUB_LOG")"

# Labels that record the user's decisions: removing needs-review or
# agent-merged, on a PR or an issue, in any case. A refusal writes nothing.
for args in "label 7 -needs-review" "label 7 -agent-merged" "label OIK-1 -Needs-Review" "label OIK-1 +bug -agent-merged"; do
	# shellcheck disable=SC2086 # split args into words
	if run $args; then fail "plan $args succeeded"; fi
	grep -q 'only the user' "$tmp/err" || fail "plan $args didn't say why: $(cat "$tmp/err")"
	! grep -q '|api ' "$STUB_LOG" || fail "plan $args wrote to GitHub: $(cat "$STUB_LOG")"
	[[ -z "$(plan_commits)" ]] || fail "plan $args wrote to the plan: $(plan_commits)"
done
run label OIK-1 +needs-review || fail "adding needs-review failed: $(cat "$tmp/err")"
grep -qx 'labels: \["needs-review"\]' "$(plan_file OIK-1)" || fail "needs-review isn't on OIK-1: $(cat "$(plan_file OIK-1)")"

wrote() { [[ -n "$(plan_commits)" ]]; }
may_build() { run show OIK-1; grep -E '^(May build|Approved):' "$tmp/out"; }

# Waiting in Design Review: not buildable, and an agent can't move it on.
HISTORY=$(moved "" "Design Review" "Oikade Agent") issue OIK-1 design-review
may_build | grep -q '^May build:  no' || fail "show counts a design waiting for review: $(may_build)"
if run state OIK-1 Todo; then fail "an agent moved an issue out of Design Review"; fi
! wrote || fail "a refused move wrote: $(plan_commits)"

# The user moved it to Todo: approved.
HISTORY=$(moved "" "Design Review" "Oikade Agent" "Design Review" Todo "the user") issue OIK-1 todo
may_build | grep -qx 'May build:  approved' || fail "show doesn't count the user's move to Todo: $(may_build)"
may_build | grep -qx 'Approved:   by the user' || fail "show doesn't say who approved it: $(may_build)"

# Only "the user" counts, not a person's name.
HISTORY=$(moved "Design Review" Todo Harry) issue OIK-1 todo
may_build | grep -q '^May build:  no' || fail "show counts a move by a name that isn't the user's: $(may_build)"

# Someone else moved it: not approved, and show says who.
HISTORY=$(moved "Design Review" Todo "Oikade Agent") issue OIK-1 todo
may_build | grep -q '^May build:  no' || fail "show counts the agent's move out of Design Review: $(may_build)"
may_build | grep -q 'by Oikade Agent, not the user' || fail "show doesn't say who moved it: $(may_build)"
if run state OIK-1 "In Progress"; then fail "an agent moved an issue the agent took out of Design Review"; fi

# A move by Linear's automations, from before the plan left Linear, isn't the user's.
HISTORY=$(moved "Design Review" Todo Linear) issue OIK-1 todo
may_build | grep -q '^May build:  no' || fail "show counts a move out of Design Review by Linear: $(may_build)"

# The user sent it back to Backlog, then an agent moved it to Todo: not approved.
HISTORY=$(moved "" "Design Review" "Oikade Agent" "Design Review" Backlog "the user" Backlog Todo "Oikade Agent") issue OIK-1 todo
may_build | grep -q '^May build:  no' || fail "show counts a design the user sent back: $(may_build)"
HISTORY=$(moved "" "Design Review" "Oikade Agent" "Design Review" Backlog "the user") issue OIK-1 backlog
if run state OIK-1 Todo; then fail "an agent moved a design the user sent back into Todo"; fi
! wrote || fail "a refused move wrote: $(plan_commits)"

# Never in Design Review: needs no design, and an agent may move it to Todo,
# which the history records.
HISTORY=$(moved "" Backlog "Oikade Agent") issue OIK-1 backlog
run state OIK-1 Todo || fail "an agent couldn't move an issue that needs no design to Todo: $(cat "$tmp/err")"
[[ "$(plan_status OIK-1)" == todo ]] || fail "state didn't move OIK-1's folder to todo: $(plan_status OIK-1)"
grep -q ' Backlog -> Todo by Oikade Agent"\]$' "$(plan_file OIK-1)" || fail "state didn't record the move: $(cat "$(plan_file OIK-1)")"
[[ "$(plan_moves)" == OIK-1 ]] || fail "state didn't commit the move: $(plan_commits)"
may_build | grep -qx 'May build:  needs no design' || fail "show doesn't build an issue that never needed a design: $(may_build)"

# The user moved its folder out of Design Review by hand: approved, and the
# next change records the move before its own.
HISTORY=$(moved "" "Design Review" "Oikade Agent") issue OIK-1 todo
may_build | grep -qx 'May build:  approved' || fail "show doesn't count the user's move by hand: $(may_build)"
run state OIK-1 "In Progress" || fail "an agent couldn't start a design the user approved by hand: $(cat "$tmp/err")"
grep -q 'Design Review -> Todo by the user, by hand","[^"]* Todo -> In Progress by Oikade Agent"' "$(plan_file OIK-1)" ||
	fail "the move by hand wasn't recorded before the agent's: $(cat "$(plan_file OIK-1)")"
may_build | grep -qx 'May build:  approved' || fail "the approval by hand was lost once recorded: $(may_build)"
# And by hand back to Backlog: not approved, so an agent can't move it to Todo.
HISTORY=$(moved "" "Design Review" "Oikade Agent") issue OIK-1 backlog
if run state OIK-1 Todo; then fail "an agent moved a design the user sent back by hand into Todo"; fi
# Nor once a later move has recorded the one by hand.
run state OIK-1 Triage || fail "an agent couldn't move a design the user sent back to Triage: $(cat "$tmp/err")"
grep -q 'Design Review -> Backlog by the user, by hand' "$(plan_file OIK-1)" || fail "the move by hand to Backlog wasn't recorded: $(cat "$(plan_file OIK-1)")"
if run state OIK-1 Todo; then fail "an agent moved a design the user sent back by hand into Todo, once it was recorded"; fi
may_build | grep -q '^Approved:   no' || fail "show counts a design the user sent back by hand: $(may_build)"

# approve is the user's: out of Design Review, saying the user did.
HISTORY=$(moved "" "Design Review" "Oikade Agent") issue OIK-1 design-review
run approve OIK-1 || fail "approve failed: $(cat "$tmp/err")"
[[ "$(plan_status OIK-1)" == todo ]] || fail "approve didn't move OIK-1 to Todo: $(plan_status OIK-1)"
may_build | grep -qx 'Approved:   by the user' || fail "approve doesn't count as the user's: $(may_build)"
if run approve OIK-1; then fail "approve moved an issue that isn't in Design Review"; fi

# Design: OIK-2 counts only if the user approved OIK-2.
HISTORY="" issue OIK-1 todo $'Design: OIK-2\n'
HISTORY=$(moved "Design Review" Todo "the user") issue OIK-2 todo
may_build | grep -qx 'May build:  builds the design approved in OIK-2' || fail "show doesn't count a Design: line the user approved: $(may_build)"
HISTORY=$(moved "Design Review" Todo "Oikade Agent") issue OIK-2 todo
may_build | grep -q '^May build:  no' || fail "show counts a Design: line the agent approved: $(may_build)"

# An issue the plan doesn't have stops, rather than reading as one that needs no design.
if run show OIK-77; then fail "show succeeded on an issue the plan doesn't have"; fi
grep -q "the plan has no issue OIK-77" "$tmp/err" || fail "show didn't say the plan has no OIK-77: $(cat "$tmp/err")"

# On an approved design only ## Steps and below may change.
printf '## Why\nA\n\n## Steps\none\n' >"$tmp/approved.md"
printf '## Why\nB\n\n## Steps\none\n' >"$tmp/redesigned.md"
printf '## Why\nA\n\n## Steps\none\ntwo\n' >"$tmp/restepped.md"
HISTORY=$(moved "Design Review" Todo "the user") issue OIK-1 todo "$(cat "$tmp/approved.md")"
if run edit OIK-1 "$tmp/redesigned.md"; then fail "edit changed an approved design"; fi
! wrote || fail "edit wrote an approved design: $(plan_commits)"
run edit OIK-1 "$tmp/restepped.md" || fail "edit refused new Steps on an approved design: $(cat "$tmp/err")"
grep -qx 'two' "$(plan_file OIK-1)" || fail "edit didn't write the new step: $(cat "$(plan_file OIK-1)")"
HISTORY=$(moved "" "Design Review" "Oikade Agent") issue OIK-1 design-review "$(cat "$tmp/approved.md")"
run edit OIK-1 "$tmp/redesigned.md" || fail "edit refused a design under review: $(cat "$tmp/err")"

(( failed == 0 )) && echo "ok: agent identity"
exit "$failed"
