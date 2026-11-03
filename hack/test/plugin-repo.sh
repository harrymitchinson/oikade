#!/usr/bin/env bash
# plugin-repo: check that a step whose sub-issue says "Repo: OWNER/NAME" is
# shown, opened and merged in that repo, not this one, and that repo init
# starts only an empty repo. git runs for real against scratch bare repos. gh
# is a stub that answers from JSON fixtures keyed by GH_REPO, and curl plays
# Linear. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
fail() { echo "FAIL: $*" >&2; failed=1; }

# gh: "pr view N", "pr checks N" and "pr list --head B" answer from
# pr-REPO-N.json, checks-REPO-N.json and list-REPO-B.json, where REPO is
# GH_REPO with / as _. "repo view" answers from repo.json, and "repo view R"
# from repo-R.json. Writes (-X other than GET, or fields on api) answer {} and
# are logged to writes.log with the GH_REPO they ran under.
mkdir "$tmp/bin" "$tmp/fx"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
fx="$STUB_FX" q="" method=GET fields=0 args=() name="" head=""
r="${GH_REPO//\//_}"
while [[ $# -gt 0 ]]; do
	case "$1" in
	-q) q="$2"; shift 2 ;;
	-X) method="$2"; shift 2 ;;
	-f | -F) fields=1; args+=("$1" "$2"); shift 2 ;;
	--head) head="$2"; shift 2 ;;
	--json | --state | --limit | --search) shift 2 ;;
	--paginate) shift ;;
	*) args+=("$1"); shift ;;
	esac
done
set -- "${args[@]}"
case "$1 ${2:-}" in
"pr view") name="pr-$r-$3.json" ;;
"pr checks") name="checks-$r-$3.json" ;;
"pr list") name="list-$r-${head//\//_}.json" ;;
"repo view") name="repo${3:+-${3//\//_}}.json" ;;
api*) name="api_$(tr / _ <<<"$2").json" ;;
esac
if [[ "$method" != GET || ( "$1" == api && $fields == 1 ) ]]; then
	echo "[$GH_REPO] $method $*" >>"$fx/writes.log"
	echo '{}'
	exit 0
fi
[[ -f "$fx/$name" ]] || { echo "gh stub: no fixture $name for: $*" >&2; exit 1; }
if [[ -n "$q" ]]; then jq -r "$q" "$fx/$name"; else cat "$fx/$name"; fi
STUB
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
q=$(jq -r .query | tr -s ' \n\t' ' ')
case "$q" in
*"issue(id:"*) jq -c '{data: {issue: .}}' "$STUB_FX/feature.json" ;;
*"users("*) echo '{"data":{"users":{"nodes":[{"id":"u-owner","name":"Owner","app":false}]}}}' ;;
*"team(id:"*) echo '{"data":{"team":{"id":"t1","states":{"nodes":[{"id":"s-done","name":"Done","type":"completed"},{"id":"s-review","name":"In Review","type":"started"},{"id":"s-prog","name":"In Progress","type":"started"},{"id":"s-todo","name":"Todo","type":"unstarted"}]}}}}' ;;
*"history"*) echo '{"data":{"issue":{"history":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}' ;;
*) echo '{"data":{"ok":{"success":true}}}' ;;
esac
STUB
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH" STUB_FX="$tmp/fx" CLAUDECODE='' PLAN_GITHUB="$tmp/github" \
	GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
fx() { cat >"$tmp/fx/$1"; }
fx repo.json <<<'{"nameWithOwner":"o/r"}'

# OIK-20: step OIK-21 here, done, and step OIK-22 in o/r-plug. Its name
# starts with this repo's, o/r, so nothing may take it for a path under o/r.
jq -n '{id: "uuid-20", identifier: "OIK-20", title: "F", url: "u", description: "", priority: 0,
	state: {name: "In Progress", type: "started"}, project: null, parent: null,
	labels: {nodes: []}, inverseRelations: {nodes: []},
	children: {nodes: [
		{identifier: "OIK-21", title: "Here", description: "Scope: here.", subIssueSortOrder: 1, number: 21, state: {name: "Done", type: "completed"}},
		{identifier: "OIK-22", title: "There", description: "Scope: a plugin.\n**Repo:** `o/r-plug` (its own)\nAcceptance: it works.", subIssueSortOrder: 2, number: 22, state: {name: "In Progress", type: "started"}}]}}' \
	>"$tmp/fx/feature.json"

# This repo, claimed, and the plugin's repo with a main.
git init -q --bare -b main "$tmp/remote.git"
mkdir -p "$tmp/github/o" "$tmp/github/p"
git init -q --bare -b main "$tmp/github/o/r-plug.git"
git clone -q "$tmp/remote.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
mkdir hack && cp "$root/hack/owner-approval" hack/ && git add hack && git commit -qm base
git push -q origin HEAD:main HEAD:refs/heads/claim/OIK-20
git clone -q "$tmp/github/o/r-plug.git" "$tmp/plug" 2>/dev/null
(cd "$tmp/plug" && echo plug >README.md && git add README.md && git commit -qm plug && git push -q origin HEAD:main)

# hack/plan with a stub linear-token: CI has no Keychain, and curl plays Linear.
plan="$tmp/plan/hack/plan"
mkdir -p "$tmp/plan/hack"
cp "$root/hack/plan" "$root/hack/agent-token" "$tmp/plan/hack/"
printf '#!/usr/bin/env bash\necho lin_test\n' >"$tmp/plan/hack/linear-token"
chmod +x "$tmp/plan/hack/"*
# OIK-21 merged here as PR #3.
here=$(git rev-parse HEAD)
git push -q origin "$here:refs/pull/3/head"
fx list-o_r-work_OIK-21.json <<<'[{"number":3,"state":"MERGED","baseRefName":"main","headRefOid":"'"$here"'"}]'
fx pr-o_r-3.json <<<'{"state":"MERGED","headRefName":"work/OIK-21","headRefOid":"'"$here"'","baseRefName":"main"}'
fx list-o_r-plug-work_OIK-22.json <<<'[]'

# step, from OIK-21's worktree, starts OIK-22 in its own, on o/r-plug's main.
git switch -q -c work/OIK-21
"$plan" step OIK-20 >"$tmp/out" 2>&1 || fail "step refused OIK-22: $(cat "$tmp/out")"
dir="$tmp/work/.claude/worktrees/work-OIK-22"
grep -q "On work/OIK-22 in .*/work/.claude/worktrees/work-OIK-22, on top of main of o/r-plug" "$tmp/out" || fail "step didn't say where OIK-22 is: $(cat "$tmp/out")"
[[ -f "$dir/README.md" && ! -d "$dir/hack" ]] || fail "OIK-22's worktree isn't o/r-plug's main"

# pr pushes to o/r-plug and opens the PR there, on main.
cd "$dir"
echo more >>README.md && git commit -qam more
: >"$tmp/fx/writes.log"
echo body >"$tmp/body.md"
"$plan" pr OIK-20 "feat: plug" "$tmp/body.md" >"$tmp/out" 2>&1 || fail "pr refused OIK-22: $(cat "$tmp/out")"
grep -q '^\[o/r-plug\] GET api repos/o/r-plug/pulls -f head=work/OIK-22 -f base=main' "$tmp/fx/writes.log" ||
	fail "pr didn't open the PR in o/r-plug on main: $(cat "$tmp/fx/writes.log")"
git -C "$tmp/plug" fetch -q origin work/OIK-22 || fail "pr didn't push work/OIK-22 to o/r-plug"
oid=$(git rev-parse HEAD)

# show lists it as o/r-plug's PR.
fx list-o_r-plug-work_OIK-22.json <<<'[{"number":5,"state":"OPEN","baseRefName":"main","headRefOid":"'"$oid"'"}]'
"$plan" show OIK-20 >"$tmp/out" 2>&1 || fail "show failed: $(cat "$tmp/out")"
grep -q 'OIK-22 There \[started\] PR #5 OPEN → main (in o/r-plug)' "$tmp/out" || fail "show didn't list o/r-plug's PR: $(cat "$tmp/out")"

# merge, named as o/r-plug#5, merges it in o/r-plug.
git push -q "$tmp/github/o/r-plug.git" "$oid:refs/pull/5/head"
fx pr-o_r-plug-5.json <<<'{"state":"OPEN","headRefName":"work/OIK-22","baseRefName":"main","isDraft":false,"mergeable":"MERGEABLE","headRefOid":"'"$oid"'","labels":[]}'
fx checks-o_r-plug-5.json <<<'[{"bucket":"pass","workflow":"Tests","name":"t"},{"bucket":"pass","workflow":"Lint","name":"l"}]'
cd "$tmp/work"
: >"$tmp/fx/writes.log"
"$plan" merge OIK-20 o/r-plug#5 >"$tmp/out" 2>&1 || fail "merge refused o/r-plug#5: $(cat "$tmp/out")"
grep -q "^\[o/r-plug\] PUT api repos/o/r-plug/pulls/5/merge -f merge_method=squash -f sha=$oid" "$tmp/fx/writes.log" ||
	fail "merge didn't merge in o/r-plug: $(cat "$tmp/fx/writes.log")"
! grep -q 'repos/o/r/pulls' "$tmp/fx/writes.log" || fail "merge wrote to this repo's PRs: $(cat "$tmp/fx/writes.log")"

# claims finds the step's PR and branch in o/r-plug.
fx list-o_r-.json <<<'[]'
"$plan" claims >"$tmp/out" 2>&1 || fail "claims failed: $(cat "$tmp/out")"
grep -q $'^OIK-20\t0h since last commit\to/r-plug#5$' "$tmp/out" || fail "claims didn't list o/r-plug#5 as fresh: $(cat "$tmp/out")"

# note on o/r-plug#5 comments there, and on o/r#7, named in full, here.
: >"$tmp/fx/writes.log"
"$plan" note o/r-plug#5 hello >"$tmp/out" 2>&1 || fail "note on o/r-plug#5 failed: $(cat "$tmp/out")"
"$plan" note o/r#7 hi >"$tmp/out" 2>&1 || fail "note on o/r#7 failed: $(cat "$tmp/out")"
grep -q '^\[o/r-plug\] GET api repos/o/r-plug/issues/5/comments' "$tmp/fx/writes.log" || fail "note didn't comment in o/r-plug: $(cat "$tmp/fx/writes.log")"
grep -q '^\[o/r\] GET api repos/o/r/issues/7/comments' "$tmp/fx/writes.log" || fail "note didn't comment here: $(cat "$tmp/fx/writes.log")"

# digest lists agent merges in o/r-plug too.
fx list-o_r-plug-.json <<<'[{"number":5,"title":"feat: plug","url":"https://x/o/r-plug/pull/5","mergedAt":"2026-11-03T00:00:00Z","additions":1,"deletions":0,"files":[{"path":"README.md"}]}]'
"$plan" digest >"$tmp/out" 2>&1 || fail "digest failed: $(cat "$tmp/out")"
grep -q '#5 feat: plug' "$tmp/out" || fail "digest didn't list o/r-plug#5: $(cat "$tmp/out")"

# The same PR by number alone is looked up here, and refused as another repo's.
fx pr-o_r-5.json <<<'{"state":"OPEN","headRefName":"work/OIK-22","baseRefName":"main","isDraft":false,"mergeable":"MERGEABLE","headRefOid":"'"$oid"'","labels":[]}'
if "$plan" merge OIK-20 5 >"$tmp/out" 2>&1; then fail "merge took PR 5 as this repo's"; fi
grep -q 'name it as o/r-plug#5' "$tmp/out" || fail "merge didn't say how to name it: $(cat "$tmp/out")"

# repo init starts an empty repo, and refuses one with a main.
: >"$tmp/fx/writes.log"
fx repo-p_new.json <<<'{"isEmpty":true}'
"$plan" repo init p/new >"$tmp/out" 2>&1 || fail "repo init refused an empty repo: $(cat "$tmp/out")"
grep -q 'PUT api repos/p/new/contents/README.md' "$tmp/fx/writes.log" || fail "repo init didn't commit a README: $(cat "$tmp/fx/writes.log")"
: >"$tmp/fx/writes.log"
fx repo-o_r-plug.json <<<'{"isEmpty":false}'
if "$plan" repo init o/r-plug >"$tmp/out" 2>&1; then fail "repo init took a repo that isn't empty"; fi
[[ ! -s "$tmp/fx/writes.log" ]] || fail "repo init wrote to a repo that isn't empty: $(cat "$tmp/fx/writes.log")"

# Without PLAN_GITHUB, a plugin repo's remote is reached the way origin is, and
# an existing one is moved to match.
git clone -q "$tmp/remote.git" "$tmp/ssh" 2>/dev/null
cd "$tmp/ssh"
git remote add plan-o-r-plug https://github.com/o/r-plug.git
for origin in git@github.com:o/r.git https://github.com/o/r.git; do
	want="${origin%r.git}r-plug.git"
	git remote set-url origin "$origin"
	PLAN_GITHUB='' "$plan" note o/r-plug#5 hi >"$tmp/out" 2>&1 || fail "note on o/r-plug#5 failed: $(cat "$tmp/out")"
	[[ "$(git remote get-url plan-o-r-plug)" == "$want" ]] ||
		fail "with origin $origin, o/r-plug's remote is $(git remote get-url plan-o-r-plug), want $want"
	PLAN_GITHUB='' "$plan" digest >"$tmp/out" 2>&1 || fail "digest with origin $origin failed: $(cat "$tmp/out")"
	grep -q '#5 feat: plug' "$tmp/out" || fail "digest with origin $origin didn't list o/r-plug#5: $(cat "$tmp/out")"
done

(( failed == 0 )) && echo "ok: plugin repo steps"
exit "$failed"
