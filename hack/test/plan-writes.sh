#!/usr/bin/env bash
# plan-writes: check that hack/plan's writes to plan/ each commit what they
# changed under the plan's lock: new issues get IDs nobody else gets, even
# when created at once, and never one used before; changes made by hand are
# committed apart; and titles, labels, blockers, releases and ideas land where
# OIK-350 says. The plan is a scratch plan/, and gh a stub. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
fail() { echo "FAIL: $*" >&2; failed=1; }
# gone_pid: the pid of a process that has gone.
gone_pid() { sleep 0 & local p=$!; wait "$p"; echo "$p"; }

mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
[[ "$1 $2" == "repo view" ]] && echo o/r
exit 0
STUB
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" CLAUDECODE='' PLAN_DIR="$tmp/plan" \
	GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
# shellcheck source=hack/test/plan-fixture.sh
. "$root/hack/test/plan-fixture.sh"
plan_init
plan="$root/hack/plan"
# A checkout with a remote, for the claims show looks up.
git init -q --bare -b main "$tmp/remote.git"
git clone -q "$tmp/remote.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
echo "Why." >"$tmp/body.md"

# Five issues created at once get five IDs, after the highest there is.
plan_issue "done" OIK-7 Seven ""
plan_mark
for i in 1 2 3 4 5; do "$plan" issue "Idea $i" "$tmp/body.md" --state Triage >"$tmp/id-$i" 2>"$tmp/err-$i" & done
wait
ids=$(cat "$tmp"/id-* | sort)
[[ "$ids" == $'OIK-10\nOIK-11\nOIK-8\nOIK-9\nOIK-12' || "$(sort -u <<<"$ids" | wc -l | tr -d ' ')" == 5 ]] ||
	fail "five issues created at once didn't get five IDs: $ids $(cat "$tmp"/err-*)"
[[ "$(sort -t- -k2 -n <<<"$ids" | tr '\n' ' ')" == "OIK-8 OIK-9 OIK-10 OIK-11 OIK-12 " ]] || fail "the IDs aren't the five after OIK-7: $ids"
[[ "$(plan_commits | grep -c '^New issue: Idea')" == 5 ]] || fail "each new issue isn't its own commit: $(plan_commits)"
[[ -f "$PLAN_DIR/triage/OIK-8-idea-$(grep -l OIK-8 "$tmp"/id-* | sed 's/.*id-//')/issue.md" ]] || fail "OIK-8 isn't a folder in triage/: $(ls "$PLAN_DIR/triage")"
f=$(plan_file OIK-12)
grep -q '^status-history: \["[^"]* none -> Triage by Oikade Agent"\]$' "$f" || fail "a new issue's history doesn't say who made it: $(cat "$f")"
[[ "$(plan_body OIK-12)" == "Why." ]] || fail "a new issue's description isn't its file: $(plan_body OIK-12)"

# An ID is never used again, even once its issue's folder has gone.
rm -rf "$(dirname "$(plan_file OIK-12)")"
git -C "$PLAN_DIR" commit -qam "Delete OIK-12"
[[ "$("$plan" issue "After" "$tmp/body.md")" == OIK-13 ]] || fail "the ID of a deleted issue was used again"
[[ -n "$(plan_file OIK-13)" && "$(plan_status OIK-13)" == backlog ]] || fail "a new issue isn't in Backlog by default"

# Changes made by hand are committed on their own before the next write.
echo "note" >>"$(plan_file OIK-7)"
plan_mark
"$plan" title OIK-7 "Seven, renamed" >/dev/null
[[ "$(plan_commits | tr '\n' '|')" == "OIK-7: retitle|Changes made by hand|" ]] || fail "changes by hand weren't committed apart: $(plan_commits)"
# The folder's name follows the title.
[[ -f "$PLAN_DIR/done/OIK-7-seven-renamed/issue.md" ]] || fail "retitling didn't rename the folder: $(ls "$PLAN_DIR/done")"
grep -qx 'title: "Seven, renamed"' "$(plan_file OIK-7)" || fail "retitling didn't change the title"

# Every issue has its ID as an alias, for Obsidian: a new one from the start,
# and one without it from the next write, in a commit of its own that leaves
# its updated as it was.
grep -qx 'aliases: \["OIK-13"\]' "$(plan_file OIK-13)" || fail "a new issue has no alias: $(cat "$(plan_file OIK-13)")"
mkdir -p "$PLAN_DIR/backlog/OIK-30-old"
printf -- '---\nid: "OIK-30"\ntitle: "Old"\nupdated: "2026-01-01T00:00:00Z"\n---\n\naliases: not front matter\n' >"$PLAN_DIR/backlog/OIK-30-old/issue.md"
git -C "$PLAN_DIR" add -A && git -C "$PLAN_DIR" commit -qm "Fixture: OIK-30 with no alias"
plan_mark
"$plan" note OIK-11 aliased >/dev/null
[[ "$(plan_commits | tr '\n' '|')" == "OIK-11: comment|Aliases for Obsidian|" ]] || fail "the aliases weren't a commit of their own: $(plan_commits)"
grep -qx 'aliases: \["OIK-30"\]' "$(plan_file OIK-30)" || fail "an issue without an alias didn't get one: $(cat "$(plan_file OIK-30)")"
grep -qx 'updated: "2026-01-01T00:00:00Z"' "$(plan_file OIK-30)" || fail "adding an alias changed updated"
[[ "$(plan_body OIK-30)" == "aliases: not front matter" ]] || fail "adding an alias changed the description: $(plan_body OIK-30)"
[[ "$(git -C "$PLAN_DIR" show --stat --format= HEAD~1 | grep -c issue.md)" == 1 ]] || fail "the aliases commit touched issues that had one"
# Adding one changes nothing else, even in front matter hack/plan can't read,
# and an aliases list in YAML's block style, as Obsidian writes it, counts.
mkdir -p "$PLAN_DIR/backlog/OIK-31-yaml" "$PLAN_DIR/backlog/OIK-32-block"
printf -- '---\ntitle: Unquoted\ntags:\n  - a\nlabels: []\n---\n\nBody.\n' >"$PLAN_DIR/backlog/OIK-31-yaml/issue.md"
printf -- '---\nid: "OIK-32"\naliases:\n  - OIK-32\n  - pump\n---\n\nBody.\n' >"$PLAN_DIR/backlog/OIK-32-block/issue.md"
git -C "$PLAN_DIR" add -A && git -C "$PLAN_DIR" commit -qm "Fixture: OIK-31 and OIK-32"
cp "$PLAN_DIR/backlog/OIK-32-block/issue.md" "$tmp/block.md"
"$plan" note OIK-11 again >/dev/null
[[ "$(cat "$PLAN_DIR/backlog/OIK-31-yaml/issue.md")" == $'---\ntitle: Unquoted\ntags:\n  - a\nlabels: []\naliases: ["OIK-31"]\n---\n\nBody.' ]] ||
	fail "adding an alias changed more than its line: $(cat "$PLAN_DIR/backlog/OIK-31-yaml/issue.md")"
cmp -s "$tmp/block.md" "$PLAN_DIR/backlog/OIK-32-block/issue.md" || fail "an aliases list in block style didn't count: $(cat "$PLAN_DIR/backlog/OIK-32-block/issue.md")"
# hack/plan reads only JSON front matter, so the rest of this file can't share a plan with OIK-31.
rm -rf "$PLAN_DIR/backlog/OIK-31-yaml" "$PLAN_DIR/backlog/OIK-32-block"
git -C "$PLAN_DIR" add -A && git -C "$PLAN_DIR" commit -qm "Fixture: remove OIK-31 and OIK-32"

# Labels keep the plan's spelling, and come off in any case.
"$plan" label OIK-7 +Bug >/dev/null
"$plan" label OIK-8 +bug +later >/dev/null
grep -qx 'labels: \["Bug","later"\]' "$(plan_file OIK-8)" || fail "a label didn't take the plan's spelling: $(grep labels "$(plan_file OIK-8)")"
"$plan" label OIK-8 -LATER >/dev/null
grep -qx 'labels: \["Bug"\]' "$(plan_file OIK-8)" || fail "a label didn't come off: $(grep labels "$(plan_file OIK-8)")"

# A blocker is recorded on the issue it blocks, and holds it back until done.
plan_issue todo OIK-20 Feature $'## Steps\n\n- [ ] One: it. Acceptance: it works (unit)'
"$plan" block OIK-20 OIK-9 >/dev/null
grep -qx 'blocked-by: \["OIK-9"\]' "$(plan_file OIK-20)" || fail "block didn't record OIK-9 on OIK-20"
grep -qx 'Blocked by: OIK-9' <<<"$("$plan" show OIK-20)" || fail "show doesn't say OIK-20 is blocked"
mkdir -p "$PLAN_DIR/done" && git -C "$PLAN_DIR" mv "$(dirname "$(plan_file OIK-9)")" "$PLAN_DIR/done/"
grep -qx 'Blocked by: nothing open' <<<"$("$plan" show OIK-20)" || fail "a blocker that's done still blocks"

# Releases: new, set, listed with their progress, closed and renamed.
printf 'Goal: it runs.\n\nDone when: it does.\n' >"$tmp/release.md"
"$plan" project new v0.9.0 "$tmp/release.md" >/dev/null
grep -qx 'summary: "it runs."' "$PLAN_DIR/releases/v0.9.0.md" || fail "a new release has no summary: $(cat "$PLAN_DIR/releases/v0.9.0.md")"
"$plan" project set OIK-7 v0.9.0 >/dev/null
"$plan" project set OIK-20 v0.9.0 >/dev/null
grep -qx '## v0.9.0 (backlog, 50% done)' <<<"$("$plan" projects)" || fail "projects doesn't show the release's progress: $("$plan" projects)"
if "$plan" project set OIK-7 v9 2>/dev/null; then fail "project set took a release that doesn't exist"; fi
"$plan" project close v0.9.0 >/dev/null
grep -qx 'state: "completed"' "$PLAN_DIR/releases/v0.9.0.md" || fail "close didn't complete the release"
"$plan" project rename v0.9.0 v1.0.0-pre >/dev/null
[[ -f "$PLAN_DIR/releases/v1.0.0-pre.md" && ! -e "$PLAN_DIR/releases/v0.9.0.md" ]] || fail "rename didn't move the release's file"
grep -qx 'release: "v1.0.0-pre"' "$(plan_file OIK-20)" || fail "rename didn't follow on the release's issues"
"$plan" issue "In it" "$tmp/body.md" --project v1.0.0-pre >/dev/null
grep -q $'^OIK-20\tv1.0.0-pre\tFeature$' <<<"$("$plan" ready)" || fail "ready doesn't list OIK-20 in its release: $("$plan" ready)"

# Ideas are what's in Triage, and promote moves one out.
grep -q $'^OIK-10\t-\tIdea' <<<"$("$plan" ideas)" || fail "ideas doesn't list OIK-10: $("$plan" ideas)"
"$plan" promote OIK-10 >/dev/null
[[ "$(plan_status OIK-10)" == backlog ]] || fail "promote didn't move OIK-10 to Backlog"
if "$plan" promote OIK-10 2>/dev/null; then fail "promote moved an issue that isn't in Triage"; fi

# Two comments in one second by one author both stay.
"$plan" note OIK-8 one >/dev/null
"$plan" note OIK-8 two >/dev/null
[[ "$(find "$(dirname "$(plan_file OIK-8)")/comments" -name '*.md' | wc -l | tr -d ' ')" == 2 ]] || fail "two comments didn't make two files"

# A lock left by a process that's gone is taken over; one held stops a write.
mkdir "$PLAN_DIR/.git/plan.lock" && gone_pid >"$PLAN_DIR/.git/plan.lock/pid"
"$plan" note OIK-8 three >/dev/null || fail "a lock left by a process that's gone stopped a write"
[[ ! -e "$PLAN_DIR/.git/plan.lock" ]] || fail "the write didn't give the lock back"
sleep 60 &
holder=$!
mkdir "$PLAN_DIR/.git/plan.lock" && echo "$holder" >"$PLAN_DIR/.git/plan.lock/pid"
plan_mark
if PLAN_LOCK_WAIT=1 "$plan" note OIK-8 four 2>"$tmp/err"; then fail "a write went ahead while another held the lock"; fi
grep -q 'locked for 1 s' "$tmp/err" || fail "the write didn't say the plan was locked: $(cat "$tmp/err")"
[[ -z "$(plan_commits)" ]] || fail "a write committed while another held the lock"
kill "$holder" && { wait "$holder" 2>/dev/null || true; }
rm -rf "$PLAN_DIR/.git/plan.lock"

# A writer slow to act on who holds the lock never takes it from the next
# holder (OIK-357). A writer that finds the lock held reads who holds it, and
# takes the lock over if that process has gone. Here the writer is held up just
# after reading, as a busy machine holds a process up, until the holder it read
# has gone and another process has the lock: it must not take that one's lock
# for the dead one's.
lock="$PLAN_DIR/.git/plan.lock"
real_cat=$(command -v cat)
cat >"$tmp/bin/cat" <<STUB
#!/usr/bin/env bash
if [[ "\$#" == 1 && "\$1" == */plan.lock/pid && ! -e "$tmp/go" ]]; then
	pid=\$("$real_cat" "\$1") || exit 1
	: >"$tmp/read"
	i=0
	while [[ ! -e "$tmp/go" ]] && ((i++ < 200)); do sleep 0.05; done
	echo "\$pid"
	exit 0
fi
exec "$real_cat" "\$@"
STUB
chmod +x "$tmp/bin/cat"
sleep 10 &
gone=$!
mkdir "$lock" && echo "$gone" >"$lock/pid"
plan_mark
PLAN_LOCK_WAIT=1 "$plan" note OIK-8 slow >/dev/null 2>"$tmp/err" &
slow=$!
i=0
while [[ ! -e "$tmp/read" ]] && ((i++ < 100)); do sleep 0.05; done
[[ -e "$tmp/read" ]] || fail "the writer never read who holds the lock"
kill "$gone" && { wait "$gone" 2>/dev/null || true; }
sleep 10 &
next=$!
rm -rf "$lock" && mkdir "$lock" && echo "$next" >"$lock/pid"
: >"$tmp/go"
if wait "$slow"; then fail "a writer took the lock from the next holder, for the one it read before"; fi
[[ "$(cat "$lock/pid" 2>/dev/null)" == "$next" ]] || fail "the next holder lost the lock"
[[ -z "$(plan_commits)" ]] || fail "a writer committed while the next holder had the lock"
kill "$next" && { wait "$next" 2>/dev/null || true; }
rm -rf "$lock" "$tmp/bin/cat"

# A takeover left half done, by a process killed while it looked, isn't taken
# over itself, which would race the same way: it stops writes, and the error
# names it.
mkdir "$lock" && gone_pid >"$lock/pid" && ln -s "$(gone_pid)" "$lock.stale"
if PLAN_LOCK_WAIT=1 "$plan" note OIK-8 five 2>"$tmp/err"; then fail "a write went ahead past a takeover left half done"; fi
grep -q "was taking it over. If nothing holds it: rm -r $lock $lock.stale" "$tmp/err" ||
	fail "the write didn't name the takeover's lock: $(cat "$tmp/err")"
rm -rf "$lock" "$lock.stale"

(( failed == 0 )) && echo "ok: plan writes"
exit "$failed"
