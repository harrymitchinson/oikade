#!/usr/bin/env bash
# merge-on-approval: check that hack/plan merge merges a PR held for the user
# once their Approve covers its head, including after a restack, and refuses
# it otherwise; and that hack/owner-approval decides that. git runs for real
# against a scratch remote. gh is a stub that answers from JSON fixtures,
# applying the -q filter the real gh would. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
fail() { echo "FAIL: $*" >&2; failed=1; }

# gh: "pr view N", "issue view N" and "pr checks N" answer from pr-N.json,
# issue-N.json and checks-N.json, "repo view" from repo.json, and "api PATH"
# from PATH with / as _. Writes (-X other than GET, or fields) answer {} and
# are logged to writes.log. A fixture named fail-<name> fails the call.
mkdir "$tmp/bin" "$tmp/fx"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
fx="$STUB_FX" q="" method=GET fields=0 args=() name=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-q) q="$2"; shift 2 ;;
	-X) method="$2"; shift 2 ;;
	-f | -F) fields=1; args+=("$1" "$2"); shift 2 ;;
	--json) shift 2 ;;
	--paginate) shift ;;
	*) args+=("$1"); shift ;;
	esac
done
set -- "${args[@]}"
case "$1 $2" in
"pr view" | "issue view") name="${1}-$3.json" ;;
"pr checks") name="checks-$3.json" ;;
"repo view") name="repo.json" ;;
api*) name="api_$(sed 's#{owner}/{repo}#o/r#; s#/#_#g' <<<"$2").json" ;;
esac
if [[ "$method" != GET || ( "$1" == api && $fields == 1 ) ]]; then
	echo "$method $*" >>"$fx/writes.log"
	echo '{}'
	exit 0
fi
[[ ! -e "$fx/fail-$name" ]] || { echo "gh: HTTP 502" >&2; exit 1; }
[[ -f "$fx/$name" ]] || { echo "gh stub: no fixture $name for: $*" >&2; exit 1; }
if [[ -n "$q" ]]; then jq -r "$q" "$fx/$name"; else cat "$fx/$name"; fi
STUB
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" STUB_FX="$tmp/fx" PLAN_PROJECT_OWNER=o CLAUDECODE='' \
	GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
fx() { cat >"$tmp/fx/$1"; }
fx repo.json <<<'{"nameWithOwner":"o/r"}'

# The remote: main, a step 1 (PR #11) and step 2 (PR #12) stacked on it.
git init -q --bare -b main "$tmp/remote.git"
git clone -q "$tmp/remote.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
commit() { echo "$1" >"$1"; git add "$1"; git commit -qm "$1"; git rev-parse HEAD; }
mkdir hack
cp "$root/hack/owner-approval" hack/
git add hack
commit base >/dev/null
git push -q origin HEAD:main
git switch -qc step1
s1=$(commit one)
git switch -qc step2
s2=$(commit two)
git push -q origin "$s1:refs/pull/11/head" "$s2:refs/pull/12/head" "$s2:refs/heads/work/9-2" main:refs/heads/work/9

# Squash step 1 onto main, and restack step 2 onto it, as hack/plan does.
git switch -q main
git merge -q --squash step1 >/dev/null && git commit -qm "one (#11)"
git push -q origin main
git switch -q step2
git rebase -q --onto main "$s1"
s2r=$(git rev-parse HEAD)
git push -q -f origin "$s2r:refs/pull/12/head" "$s2r:refs/heads/work/9-2"
git switch -q --detach main

review() { printf '{"user":{"login":"%s"},"state":"%s","commit_id":"%s"}' "$1" "$2" "$3"; }
reviews() { local IFS=,; echo "[$*]" >"$tmp/fx/api_repos_o_r_pulls_12_reviews.json"; }
fx pr-12.json <<EOF
{"state":"OPEN","headRefName":"work/9-2","baseRefName":"main","isDraft":false,"mergeable":"MERGEABLE",
 "headRefOid":"$s2r","labels":[{"name":"needs-review"}],"body":"Part of #9"}
EOF
fx pr-11.json <<<'{"state":"MERGED","headRefName":"work/9-1","headRefOid":"'"$s1"'","baseRefName":"main"}'
fx issue-9.json <<<'{"labels":[],"body":"## Steps\n- [x] One (PR #11)\n- [ ] Two (PR #12)\n"}'
fx checks-12.json <<'EOF'
[{"bucket":"pass","workflow":"Tests","name":"Run on Ubuntu"},{"bucket":"pass","workflow":"Lint","name":"Run on Ubuntu"},
 {"bucket":"pass","workflow":"Review gate","name":"Approved by the owner"}]
EOF
fx api_repos_o_r_pulls_12_files.json <<<'[{"filename":"hack/plan"}]'
fx api_repos_o_r_issues_9_dependencies_blocked_by.json <<<'[]'

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
chmod +x "$tmp/rigged/hack/"*
merge() { : >"$tmp/fx/writes.log"; "$plan" merge 9 12 >"$tmp/out" 2>&1; }
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

merge || fail "merge refused an Approve that covers the head: $(cat "$tmp/out")"
grep -qx "PUT api repos/o/r/pulls/12/merge -f merge_method=squash -f sha=$s2r" "$tmp/fx/writes.log" ||
	fail "merge didn't squash-merge the head: $(cat "$tmp/fx/writes.log")"
! grep -q 'agent-merged' "$tmp/fx/writes.log" || fail "merge labelled an approved PR agent-merged"
grep -q 'on your Approve' "$tmp/fx/writes.log" || fail "merge's comment doesn't say it merged on the Approve"

# A feature labelled needs-review holds a PR that's otherwise low risk.
fx pr-12.json <<EOF
{"state":"OPEN","headRefName":"work/9-2","baseRefName":"main","isDraft":false,"mergeable":"MERGEABLE",
 "headRefOid":"$s2r","labels":[],"body":"Part of #9"}
EOF
fx issue-9.json <<<'{"labels":[{"name":"needs-review"}],"body":"## Steps\n- [x] One (PR #11)\n- [ ] Two (PR #12)\n"}'
fx api_repos_o_r_pulls_12_files.json <<<'[{"filename":"internal/rule/rule.go"}]'
reviews
if merge; then fail "merge succeeded on a feature labelled needs-review with no Approve"; fi
grep -q '#9 has the needs-review label' "$tmp/out" || fail "merge didn't name the feature's hold: $(cat "$tmp/out")"
reviews "$(review o APPROVED "$s2r")"
merge || fail "merge refused an approved PR on a feature labelled needs-review: $(cat "$tmp/out")"

# With no hold at all it merges as before, labelled agent-merged.
fx issue-9.json <<<'{"labels":[],"body":"## Steps\n- [x] One (PR #11)\n- [ ] Two (PR #12)\n"}'
reviews
merge || fail "merge refused a low-risk PR: $(cat "$tmp/out")"
grep -q 'labels\[\]=agent-merged' "$tmp/fx/writes.log" || fail "a low-risk merge isn't labelled agent-merged: $(cat "$tmp/fx/writes.log")"

(( failed == 0 )) && echo "ok: merge on approval"
exit "$failed"
