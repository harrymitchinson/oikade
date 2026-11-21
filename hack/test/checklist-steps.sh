#!/usr/bin/env bash
# checklist-steps: check that a feature whose steps are the checklist lines
# under its "## Steps" heading is claimed, stacked, opened and synced one line
# at a time on work/OIKN-stepK, with each line ticked as its PR reaches main
# and the feature Done after the last. git runs for real against a scratch
# remote, and the plan is a scratch plan/. gh is a stub that answers from JSON
# fixtures. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
fail() { echo "FAIL: $*" >&2; failed=1; }

# gh: "pr view N" answers from pr-N.json, "pr list --head B" from list-B.json
# (with / as _), or [] if there's none, and "repo view" from repo.json. Writes
# (-X other than GET, or fields on api) answer {"html_url": ".../pull/21"} and
# are logged to writes.log.
mkdir "$tmp/bin" "$tmp/fx"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
fx="$STUB_FX" q="" method=GET fields=0 args=() name="" head=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-q) q="$2"; shift 2 ;;
	-X) method="$2"; shift 2 ;;
	-f | -F) fields=1; args+=("$1" "$2"); shift 2 ;;
	--head) head="$2"; shift 2 ;;
	--json | --state | --limit) shift 2 ;;
	*) args+=("$1"); shift ;;
	esac
done
set -- "${args[@]}"
if [[ "$method" != GET || ( "$1" == api && $fields == 1 ) ]]; then
	printf '%s\n' "$method $*" >>"$fx/writes.log"
	out='{"html_url":"https://github.com/o/r/pull/21"}'
	if [[ -n "$q" ]]; then jq -r "$q" <<<"$out"; else echo "$out"; fi
	exit 0
fi
case "$1 ${2:-}" in
"pr view") name="pr-$3.json" ;;
"pr list") name="list-${head//\//_}.json"; [[ -f "$fx/$name" ]] || echo '[]' >"$fx/$name" ;;
"repo view") name="repo.json" ;;
esac
[[ -f "$fx/$name" ]] || { echo "gh stub: no fixture $name for: $*" >&2; exit 1; }
if [[ -n "$q" ]]; then jq -r "$q" "$fx/$name"; else cat "$fx/$name"; fi
STUB
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH" STUB_FX="$tmp/fx" CLAUDECODE='' PLAN_DIR="$tmp/plan-dir" \
	GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
fx() { cat >"$tmp/fx/$1"; }
# shellcheck source=hack/test/plan-fixture.sh
. "$root/hack/test/plan-fixture.sh"
plan_init
fx repo.json <<<'{"nameWithOwner":"o/r"}'

# feature STATE TICKS: OIK-9 in STATE, with three checklist steps, ticked
# where TICKS has an x: "x x -" ticks the first two.
feature() {
	local t d="## Why"$'\n\n'"Fewer issues."$'\n\n'"## Steps"$'\n\n' i=0 name
	read -ra t <<<"$2"
	for name in One Two Three; do
		d+="- [$([[ "${t[$i]}" == x ]] && echo x || echo ' ')] $name: scope. Acceptance: it works (unit)"$'\n'
		# A nested item belongs to the step above it.
		[[ "$name" != One ]] || d+="  - [ ] a sub-item"$'\n'
		i=$((i + 1))
	done
	d+=$'\n'"## Notes"$'\n\n'"- [ ] not a step"
	plan_issue "$([[ "$1" == Todo ]] && echo todo || echo in-progress)" OIK-9 F "$d"
}
feature Todo "- - -"

git init -q --bare -b main "$tmp/remote.git"
git clone -q "$tmp/remote.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
echo base >base && git add base && git commit -qm base && git push -q origin HEAD:main

plan="$tmp/plan/hack/plan"
mkdir -p "$tmp/plan/hack"
cp "$root/hack/plan" "$root/hack/agent-token" "$tmp/plan/hack/"
chmod +x "$tmp/plan/hack/"*
logs() { : >"$tmp/fx/writes.log"; plan_mark; }
set_states() { plan_moves; }

# show lists the three lines, and not the one under ## Notes.
"$plan" show OIK-9 >"$tmp/out" 2>&1 || fail "show failed: $(cat "$tmp/out")"
grep -q '1. OIK9-step1 One: scope. Acceptance: it works (unit) \[unstarted\]' "$tmp/out" || fail "show didn't list step 1: $(cat "$tmp/out")"
grep -q '3. OIK9-step3 Three' "$tmp/out" || fail "show didn't list step 3: $(cat "$tmp/out")"
! grep -q '^  4\.' "$tmp/out" || fail "show took a line under another heading as a step: $(cat "$tmp/out")"
grep -q '^Next step:  OIK9-step1 One' "$tmp/out" || fail "show's next step isn't line 1: $(cat "$tmp/out")"

# claim puts line 1 on work/OIK9-step1, and moves only the feature.
logs
dir="$tmp/work/.claude/worktrees/work-OIK-9"
"$plan" claim OIK-9 >"$tmp/out" 2>&1 || fail "claim refused OIK-9: $(cat "$tmp/out")"
[[ "$(git -C "$dir" branch --show-current)" == work/OIK9-step1 ]] || fail "claim didn't start work/OIK9-step1: $(cat "$tmp/out")"
[[ "$(set_states)" == OIK-9 ]] || fail "claim moved something besides the feature: $(set_states)"
[[ "$(plan_status OIK-9)" == in-progress ]] || fail "claim didn't move the feature to In Progress: $(plan_status OIK-9)"
feature "In Progress" "- - -"

# pr opens it on main, naming the feature with "part of" and the line, and
# moves nothing in the plan.
cd "$dir"
echo one >one && git add one && git commit -qm one
echo body >"$tmp/body.md"
logs
"$plan" pr OIK-9 "feat: one" "$tmp/body.md" >"$tmp/out" 2>&1 || fail "pr refused line 1: $(cat "$tmp/out")"
grep -q 'api repos/o/r/pulls -f head=work/OIK9-step1 -f base=main' "$tmp/fx/writes.log" || fail "pr didn't open line 1 on main: $(cat "$tmp/fx/writes.log")"
grep -q 'Plan: part of OIK-9, step 1' "$tmp/fx/writes.log" || fail "pr's body doesn't name the feature and line: $(cat "$tmp/fx/writes.log")"
[[ -z "$(set_states)" ]] || fail "pr moved an issue: $(set_states)"
s1=$(git rev-parse HEAD)
git push -q origin "$s1:refs/pull/21/head"
fx list-work_OIK9-step1.json <<<'[{"number":21,"state":"OPEN","baseRefName":"main","headRefOid":"'"$s1"'"}]'
fx pr-21.json <<<'{"state":"OPEN","headRefName":"work/OIK9-step1","headRefOid":"'"$s1"'","baseRefName":"main"}'

# step stacks line 2 on line 1.
"$plan" step OIK-9 >"$tmp/out" 2>&1 || fail "step refused line 2: $(cat "$tmp/out")"
grep -q 'On work/OIK9-step2, on top of work/OIK9-step1' "$tmp/out" || fail "step didn't stack line 2 on line 1: $(cat "$tmp/out")"

# sync leaves an open line alone, and ticks one whose PR merged into main.
logs
"$plan" sync OIK-9 >"$tmp/out" 2>&1 || fail "sync failed: $(cat "$tmp/out")"
[[ -z "$(plan_commits)" ]] || fail "sync wrote with no PR merged: $(plan_commits)"
fx list-work_OIK9-step1.json <<<'[{"number":21,"state":"MERGED","baseRefName":"main","headRefOid":"'"$s1"'"}]'
fx pr-21.json <<<'{"state":"MERGED","headRefName":"work/OIK9-step1","headRefOid":"'"$s1"'","baseRefName":"main"}'
logs
"$plan" sync OIK-9 >"$tmp/out" 2>&1 || fail "sync failed: $(cat "$tmp/out")"
grep -q 'OIK-9: ticked step 1, PR #21 merged' "$tmp/out" || fail "sync didn't tick line 1: $(cat "$tmp/out")"
desc=$(cat "$(plan_file OIK-9)")
grep -q '^- \[x\] One' <<<"$desc" || fail "line 1 isn't ticked: $desc"
grep -q '^- \[ \] Two' <<<"$desc" || fail "line 2 was ticked too: $desc"
grep -q '^- \[ \] not a step' <<<"$desc" || fail "a line under ## Notes changed: $desc"
grep -q '^  - \[ \] a sub-item' <<<"$desc" || fail "a nested item was ticked: $desc"
grep -q '^## Why' <<<"$desc" || fail "the rest of the description is gone: $desc"
[[ -z "$(set_states)" ]] || fail "sync moved an issue with lines left: $(set_states)"

# Once the last line's PR merges, sync ticks it and the feature is Done.
feature "In Progress" "x x -"
fx list-work_OIK9-step3.json <<<'[{"number":23,"state":"MERGED","baseRefName":"main","headRefOid":"'"$s1"'"}]'
logs
"$plan" sync OIK-9 >"$tmp/out" 2>&1 || fail "sync failed: $(cat "$tmp/out")"
grep -q 'OIK-9: ticked step 3, PR #23 merged' "$tmp/out" || fail "sync didn't tick line 3: $(cat "$tmp/out")"
grep -q 'OIK-9: done, every step merged' "$tmp/out" || fail "sync didn't finish the feature: $(cat "$tmp/out")"
[[ "$(plan_status OIK-9)" == "done" ]] || fail "the feature isn't in done/: $(plan_status OIK-9)"
cd "$tmp/work"

# edit keeps the lines in place: it adds one at the end, and refuses a
# reorder, since a line's branch and PR follow its position.
feature "In Progress" "x - -"
plan_body OIK-9 | awk '{print} /^- \[ \] Three/ {print "- [ ] Four: more. Acceptance: it works (unit)"}' >"$tmp/added.md"
logs
"$plan" edit OIK-9 "$tmp/added.md" >"$tmp/out" 2>&1 || fail "edit refused a line added at the end: $(cat "$tmp/out")"
awk '/^- \[ \] Two/ {two = $0; next} /^- \[ \] Three/ {print; print two; next} {print}' "$tmp/added.md" >"$tmp/swapped.md"
logs
if "$plan" edit OIK-9 "$tmp/swapped.md" >"$tmp/out" 2>&1; then fail "edit took a reordered checklist"; fi
grep -q 'numbered by their place' "$tmp/out" || fail "edit didn't say why it refused: $(cat "$tmp/out")"
[[ -z "$(plan_commits)" ]] || fail "edit wrote a reordered checklist: $(plan_commits)"

# A sub-issue would hide a checklist feature's lines, so issue --parent refuses.
logs
if "$plan" issue "Five" "$tmp/body.md" --parent OIK-9 >"$tmp/out" 2>&1; then fail "issue made a sub-issue of a checklist feature"; fi
grep -q 'which a sub-issue would hide' "$tmp/out" || fail "issue didn't say why it refused: $(cat "$tmp/out")"
[[ -z "$(plan_commits)" ]] || fail "issue created a sub-issue of a checklist feature: $(plan_commits)"

(( failed == 0 )) && echo "ok: checklist steps"
exit "$failed"
