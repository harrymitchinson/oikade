#!/usr/bin/env bash
# merge-on-approval: check that hack/plan merge merges a PR held for the user
# once their Approve covers its head, including after a restack, and refuses
# it otherwise; and that hack/owner-approval decides that. git runs for real
# against a scratch remote. gh is a stub that answers from JSON fixtures,
# applying the -q filter the real gh would, and curl plays Linear. Run by
# make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
fail() { echo "FAIL: $*" >&2; failed=1; }

# gh: "pr view N" and "pr checks N" answer from pr-N.json and checks-N.json,
# "pr list --head B" from list-B.json (with / as _), "repo view" from
# repo.json, "api graphql" from graphql-N.json for the PR N it names, and
# "api PATH" from PATH with / as _. Writes (-X other than GET, or fields on a
# REST call) answer {} and are logged to writes.log. A merge fails as GitHub
# does while a required check runs, as many times as the number in
# gate-running, then fails with the message in merge-refused, if there is one.
# A fixture named fail-<name> fails the call.
mkdir "$tmp/bin" "$tmp/fx"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
fx="$STUB_FX" q="" method=GET fields=0 args=() name="" head="" n=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-q) q="$2"; shift 2 ;;
	-X) method="$2"; shift 2 ;;
	-f | -F) fields=1; [[ "$2" == n=* ]] && n="${2#n=}"; args+=("$1" "$2"); shift 2 ;;
	--head) head="$2"; shift 2 ;;
	--json | --state | --limit) shift 2 ;;
	--paginate) shift ;;
	*) args+=("$1"); shift ;;
	esac
done
set -- "${args[@]}"
case "$1 $2" in
"pr view") name="pr-$3.json" ;;
"pr checks") name="checks-$3.json" ;;
"pr list") name="list-${head//\//_}.json" ;;
"repo view") name="repo.json" ;;
"api graphql") name="graphql-$n.json"; fields=0 ;;
api*) name="api_$(sed 's#{owner}/{repo}#o/r#; s#/#_#g' <<<"$2").json" ;;
esac
if [[ "$method" != GET || ( "$1" == api && $fields == 1 ) ]]; then
	echo "$method $*" >>"$fx/writes.log"
	if [[ "$method" == PUT && "$2" == */merge ]]; then
		left=$(cat "$fx/gate-running" 2>/dev/null || echo 0)
		if ((left > 0)); then
			echo $((left - 1)) >"$fx/gate-running"
			# GitHub says "expected" before the run starts, and "queued" once it's waiting for a runner.
			if ((left % 2)); then
				echo 'gh: Repository rule violations found Required status check "Approved by the owner" is expected. (HTTP 405)' >&2
			else
				echo 'gh: Repository rule violations found Required status check "Approved by the owner" is queued. (HTTP 405)' >&2
			fi
			exit 1
		fi
		[[ ! -f "$fx/merge-refused" ]] || { cat "$fx/merge-refused" >&2; exit 1; }
		# Who held the feature's claim when it merged.
		git ls-remote --heads origin refs/heads/claim/OIK-9 | cut -f1 >"$fx/claim-at-merge"
		# Someone releases the claim mid-merge and another session claims it.
		if [[ -f "$fx/reclaim" ]]; then
			git push -q -f origin "$(git commit-tree -m reclaim "$(git rev-parse 'origin/main^{tree}')"):refs/heads/claim/OIK-9"
		fi
	fi
	echo '{}'
	exit 0
fi
[[ ! -e "$fx/fail-$name" ]] || { echo "gh: HTTP 502" >&2; exit 1; }
[[ -f "$fx/$name" ]] || { echo "gh stub: no fixture $name for: $*" >&2; exit 1; }
if [[ -n "$q" ]]; then jq -r "$q" "$fx/$name"; else cat "$fx/$name"; fi
STUB

# curl plays Linear's GraphQL API: the feature from feature.json, the user,
# the team's statuses, and success for any write.
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
q=$(jq -r .query | tr -s ' \n\t' ' ')
case "$q" in
*"issue(id:"*) jq -c '{data: {issue: .}}' "$STUB_FX/feature.json" ;;
*"users("*) echo '{"data":{"users":{"nodes":[{"id":"u-owner","name":"Owner","app":false}]}}}' ;;
*"team(id:"*) echo '{"data":{"team":{"id":"t1","states":{"nodes":[{"id":"s-done","name":"Done","type":"completed"},{"id":"s-review","name":"In Review","type":"started"},{"id":"s-todo","name":"Todo","type":"unstarted"}]}}}}' ;;
*) echo '{"data":{"ok":{"success":true}}}' ;;
esac
STUB
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH" STUB_FX="$tmp/fx" CLAUDECODE='' \
	GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
fx() { cat >"$tmp/fx/$1"; }
fx repo.json <<<'{"nameWithOwner":"o/r"}'

# feature LABEL...: OIK-9, in FEATURE_STATE (default In Progress), with steps OIK-10 (PR #11) and OIK-11 (PR #12).
feature() {
	jq -n --arg state "${FEATURE_STATE:-In Progress}" --args '{id: "uuid-9", identifier: "OIK-9", title: "F", url: "u", description: "", priority: 0,
		state: {name: $state, type: "started"}, project: null, parent: null,
		labels: {nodes: [$ARGS.positional[] | {name: .}]}, inverseRelations: {nodes: []},
		children: {nodes: [
			{identifier: "OIK-10", title: "One", subIssueSortOrder: 1, number: 10, state: {name: "Done", type: "completed"}},
			{identifier: "OIK-11", title: "Two", subIssueSortOrder: 2, number: 11, state: {name: "In Review", type: "started"}}]}}' \
		"$@" >"$tmp/fx/feature.json"
}
feature

# The remote: main, a step 1 (PR #11) and step 2 (PR #12) stacked on it.
git init -q --bare -b main "$tmp/remote.git"
git clone -q "$tmp/remote.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
commit() { echo "$1" >"$1"; git add "$1"; git commit -qm "$1"; git rev-parse HEAD; }
mkdir hack
cp "$root/hack/owner-approval" hack/
git add hack
commit base >/dev/null
git push -q origin HEAD:main HEAD:refs/heads/claim/OIK-9
git switch -qc step1
s1=$(commit one)
git switch -qc step2
s2=$(commit two)
git push -q origin "$s1:refs/pull/11/head" "$s2:refs/pull/12/head" "$s2:refs/heads/work/OIK-11"

# Squash step 1 onto main, and restack step 2 onto it, as hack/plan does.
git switch -q main
git merge -q --squash step1 >/dev/null && git commit -qm "one (#11)"
git push -q origin main
git switch -q step2
git rebase -q --onto main "$s1"
s2r=$(git rev-parse HEAD)
git push -q -f origin "$s2r:refs/pull/12/head" "$s2r:refs/heads/work/OIK-11"
git switch -q --detach main

review() { printf '{"user":{"login":"%s"},"state":"%s","commit_id":"%s"}' "$1" "$2" "$3"; }
reviews() { local IFS=,; echo "[$*]" >"$tmp/fx/api_repos_o_r_pulls_12_reviews.json"; }
# PR #12 was stacked on work/OIK-10 until the restack based it on main.
earlier() {
	jq -n --args '{data: {repository: {pullRequest: {timelineItems: {nodes: [$ARGS.positional[] | {previousRefName: .}]}}}}}' \
		"$@" >"$tmp/fx/graphql-12.json"
}
earlier work/OIK-10
pr12() {
	fx pr-12.json <<EOF
{"state":"OPEN","headRefName":"work/OIK-11","baseRefName":"main","isDraft":false,"mergeable":"MERGEABLE",
 "headRefOid":"$s2r","labels":[$1]}
EOF
}
pr12 '{"name":"needs-review"}'
fx pr-11.json <<<'{"state":"MERGED","headRefName":"work/OIK-10","headRefOid":"'"$s1"'","baseRefName":"main"}'
fx list-work_OIK-10.json <<<'[{"number":11,"state":"MERGED","baseRefName":"main","headRefOid":"'"$s1"'"}]'
fx list-work_OIK-11.json <<<'[{"number":12,"state":"OPEN","baseRefName":"main","headRefOid":"'"$s2r"'"}]'
# gate pass|fail: Approved by the owner, as CI reports it, alongside passing tests.
gate() {
	fx checks-12.json <<EOF
[{"bucket":"pass","workflow":"Tests","name":"Run on Ubuntu"},{"bucket":"pass","workflow":"Lint","name":"Run on Ubuntu"},
 {"bucket":"$1","workflow":"Review gate","name":"Approved by the owner"}]
EOF
}

# hack/owner-approval on its own.
approval() { OWNER=o "$root/hack/owner-approval" 12 "${1:-$s2r}" main >"$tmp/out" 2>&1; }
expect() {
	local want="$1" what="$2" got=0
	approval "${3:-}" || got=$?
	[[ "$got" == "$want" ]] || fail "$what: exit $got, want $want ($(cat "$tmp/out"))"
}
reviews
expect 1 "no review"
reviews "$(review o APPROVED "$s2r")"
expect 0 "Approve of the head"
reviews "$(review o APPROVED "$s2")"
expect 0 "Approve while stacked, then a restack"
earlier
expect 1 "Approve of an older commit, with no earlier base"
earlier work/OIK-10
reviews "$(review o APPROVED "$s2")" "$(review o COMMENTED "$s2r")"
expect 0 "a comment after the Approve"
reviews "$(review o APPROVED "$s2r")" "$(review o CHANGES_REQUESTED "$s2r")"
expect 1 "Request changes after the Approve"
reviews "$(review 'oikade-agent[bot]' APPROVED "$s2r")"
expect 1 "someone else's Approve"
reviews "$(review o APPROVED "$s2")"
git switch -q -c more "$s2r"
s3=$(commit three)
git push -q origin "$s3:refs/heads/more"
git switch -q --detach main
expect 1 "a commit after the Approve" "$s3"
touch "$tmp/fx/fail-api_repos_o_r_pulls_12_reviews.json"
expect 2 "the reviews can't be read"
rm "$tmp/fx/fail-api_repos_o_r_pulls_12_reviews.json"
touch "$tmp/fx/fail-graphql-12.json"
expect 2 "the earlier bases can't be read"
rm "$tmp/fx/fail-graphql-12.json"

# Whitespace is a change: re-indenting a YAML key after the Approve moves it.
git switch -q -c indented main
printf 'a:\n  c: 2\n' >conf.yaml && git add conf.yaml && git commit -qm conf
indented=$(git rev-parse HEAD)
git switch -q -c dedented main
printf 'a:\nc: 2\n' >conf.yaml && git add conf.yaml && git commit -qm conf
dedented=$(git rev-parse HEAD)
git push -q origin "$indented:refs/heads/indented" "$dedented:refs/heads/dedented"
git switch -q --detach main
reviews "$(review o APPROVED "$indented")"
expect 1 "a whitespace-only change after the Approve" "$dedented"

# hack/plan merge. It runs main's hack/owner-approval, so a copy that
# approves everything, as a PR's branch could have, changes nothing.
plan="$tmp/rigged/hack/plan"
mkdir -p "$tmp/rigged/hack"
cp "$root/hack/plan" "$root/hack/agent-token" "$tmp/rigged/hack/"
printf '#!/usr/bin/env bash\necho rigged\n' >"$tmp/rigged/hack/owner-approval"
printf '#!/usr/bin/env bash\necho lin_test\n' >"$tmp/rigged/hack/linear-token"
chmod +x "$tmp/rigged/hack/"*
merge() { : >"$tmp/fx/writes.log"; "$plan" merge OIK-9 12 >"$tmp/out" 2>&1; }
gate fail
reviews
if merge; then fail "merge succeeded with no Approve"; fi
grep -q 'merges once the user approves it' "$tmp/out" || fail "merge didn't say it waits for the Approve: $(cat "$tmp/out")"
! grep -q '^PUT' "$tmp/fx/writes.log" || fail "merge merged with no Approve: $(cat "$tmp/fx/writes.log")"

reviews "$(review 'oikade-agent[bot]' APPROVED "$s2r")"
if merge; then fail "merge succeeded on the bot's Approve"; fi

touch "$tmp/fx/fail-api_repos_o_r_pulls_12_reviews.json"
reviews "$(review o APPROVED "$s2")"
if merge; then fail "merge succeeded when the Approve couldn't be read"; fi
grep -q "couldn't read PR #12's reviews" "$tmp/out" || fail "merge didn't say why: $(cat "$tmp/out")"
! grep -q '^PUT' "$tmp/fx/writes.log" || fail "merge merged when the Approve couldn't be read"
rm "$tmp/fx/fail-api_repos_o_r_pulls_12_reviews.json"

# Approved, but the gate hasn't rerun yet: the checks still stop it.
if merge; then fail "merge succeeded while Approved by the owner still failed"; fi
grep -q "checks haven't all passed" "$tmp/out" || fail "merge didn't wait for the gate: $(cat "$tmp/out")"
gate pass
merge || fail "merge refused an Approve that covers the head: $(cat "$tmp/out")"
grep -qx "PUT api repos/o/r/pulls/12/merge -f merge_method=squash -f sha=$s2r" "$tmp/fx/writes.log" ||
	fail "merge didn't squash-merge the head: $(cat "$tmp/fx/writes.log")"
! grep -q 'agent-merged' "$tmp/fx/writes.log" || fail "merge labelled an approved PR agent-merged"
grep -q 'on your Approve' "$tmp/fx/writes.log" || fail "merge's comment doesn't say it merged on the Approve"

# A feature labelled needs-review holds a PR that's otherwise low risk.
pr12 ''
feature needs-review
reviews
gate fail
if merge; then fail "merge succeeded on a feature labelled needs-review with no Approve"; fi
grep -q 'OIK-9 has the needs-review label' "$tmp/out" || fail "merge didn't name the feature's hold: $(cat "$tmp/out")"
reviews "$(review o APPROVED "$s2r")"
gate pass
merge || fail "merge refused an approved PR on a feature labelled needs-review: $(cat "$tmp/out")"

# With no hold at all it merges labelled agent-merged, whatever it changes:
# no path is held for the user. The label reruns the gate,
# so GitHub refuses the merge until that run ends, and merge retries.
feature
reviews
export PLAN_MERGE_TRIES=3 PLAN_MERGE_SLEEP=0
echo 2 >"$tmp/fx/gate-running"
merge || fail "merge refused a low-risk PR while the gate reran: $(cat "$tmp/out")"
grep -q 'labels\[\]=agent-merged' "$tmp/fx/writes.log" || fail "a low-risk merge isn't labelled agent-merged: $(cat "$tmp/fx/writes.log")"
[[ $(grep -c '^PUT' "$tmp/fx/writes.log") == 3 ]] || fail "merge didn't retry until the gate ended: $(cat "$tmp/fx/writes.log")"

echo 5 >"$tmp/fx/gate-running"
if merge; then fail "merge succeeded while the gate never ended"; fi
grep -q 'is expected' "$tmp/out" || fail "merge didn't pass on GitHub's reason: $(cat "$tmp/out")"
[[ $(grep -c '^PUT' "$tmp/fx/writes.log") == 3 ]] || fail "merge didn't stop after PLAN_MERGE_TRIES: $(cat "$tmp/fx/writes.log")"
grep -q 'DELETE .*labels/agent-merged' "$tmp/fx/writes.log" || fail "merge left agent-merged on a PR it didn't merge"

rm "$tmp/fx/gate-running"
echo 'gh: Pull Request is not mergeable (HTTP 405)' >"$tmp/fx/merge-refused"
if merge; then fail "merge succeeded when GitHub refused it"; fi
[[ $(grep -c '^PUT' "$tmp/fx/writes.log") == 1 ]] || fail "merge retried a refusal that isn't a running check: $(cat "$tmp/fx/writes.log")"
grep -q 'not mergeable' "$tmp/out" || fail "merge didn't say why GitHub refused it: $(cat "$tmp/out")"
rm "$tmp/fx/merge-refused"

# Nobody holds the claim, as once the session that built the steps released
# it: merge takes it while it runs and gives it back, whether or not it merged.
claimed() { git ls-remote --exit-code --heads origin refs/heads/claim/OIK-9 >/dev/null; }
git push -q origin --delete claim/OIK-9
echo 'gh: Pull Request is not mergeable (HTTP 405)' >"$tmp/fx/merge-refused"
if merge; then fail "merge succeeded when GitHub refused it, unclaimed"; fi
! claimed || fail "a refused merge kept the claim it took"
rm "$tmp/fx/merge-refused" "$tmp/fx/claim-at-merge"
merge || fail "merge refused a low-risk PR nobody had claimed: $(cat "$tmp/out")"
grep -q "nobody held OIK-9's claim" "$tmp/out" || fail "merge didn't say it took the claim: $(cat "$tmp/out")"
[[ -s "$tmp/fx/claim-at-merge" ]] || fail "merge merged without holding the claim"
! claimed || fail "merge kept the claim it took"
# A feature the user has stopped isn't claimed to merge.
FEATURE_STATE=Backlog feature
if merge; then fail "merge succeeded, unclaimed, on a feature in Backlog"; fi
grep -q "may not be built" "$tmp/out" || fail "merge didn't say the feature may not be built: $(cat "$tmp/out")"
! claimed || fail "a merge refused by the gate left a claim"
! grep -q '^PUT' "$tmp/fx/writes.log" || fail "merge merged a feature in Backlog"
feature
# A claim another session took while the merge ran isn't the merge's to give back.
touch "$tmp/fx/reclaim"
merge || fail "merge refused a low-risk PR nobody had claimed: $(cat "$tmp/out")"
rm "$tmp/fx/reclaim"
claimed || fail "merge deleted a claim another session took while it ran"
git push -q origin --delete claim/OIK-9

# Someone else's claim isn't given back.
git push -q origin main:refs/heads/claim/OIK-9
merge || fail "merge refused a claimed low-risk PR: $(cat "$tmp/out")"
claimed || fail "merge gave back a claim it didn't take"

# A PR behind main merges when main hasn't touched its files since it
# branched, and says so. Once main changes one of them, it's refused.
git switch -q --detach origin/main
commit other >/dev/null
git push -q origin HEAD:main
merge || fail "merge refused a PR behind main that main hadn't touched: $(cat "$tmp/out")"
grep -q 'behind main' "$tmp/out" || fail "merge didn't say it merged behind main: $(cat "$tmp/out")"
grep -qx "PUT api repos/o/r/pulls/12/merge -f merge_method=squash -f sha=$s2r" "$tmp/fx/writes.log" ||
	fail "merge didn't squash-merge the head behind main: $(cat "$tmp/fx/writes.log")"
grep -q 'older main' "$tmp/fx/writes.log" || fail "merge's comment doesn't say it merged behind main: $(cat "$tmp/fx/writes.log")"
echo changed >two && git add two && git commit -qm "two on main"
git push -q origin HEAD:main
if merge; then fail "merge succeeded behind a main that changed its files"; fi
grep -q 'main has since changed files it changes too: two\.' "$tmp/out" || fail "merge didn't name the overlap: $(cat "$tmp/out")"
! grep -q '^PUT' "$tmp/fx/writes.log" || fail "merge merged behind a main that changed its files"

(( failed == 0 )) && echo "ok: merge on approval"
exit "$failed"
