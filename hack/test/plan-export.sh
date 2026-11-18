#!/usr/bin/env bash
# plan-export: check that hack/plan-export lays the plan out in plan/ as
# OIK-350 says, from recorded Linear answers: a folder per status and per
# issue, front matter from its fields, a file per comment, uploads downloaded
# and links into Linear rewritten, releases and documents, one commit per
# export that changed something, and the CSV cross-check. curl plays Linear.
# Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
fail() { echo "FAIL: $*" >&2; failed=1; }

mkdir -p "$tmp/bin" "$tmp/fx" "$tmp/hack"
cp "$root/hack/plan-export" "$tmp/hack/"
printf '#!/usr/bin/env bash\necho lin_test\n' >"$tmp/hack/linear-token"
chmod +x "$tmp/hack/linear-token"

# curl: a download (-o FILE) writes "bytes of NAME". A GraphQL call answers
# from CONNECTION.json, issues come in two pages, to check the paging, and
# issue(id:) answers with the whole history from issue.json.
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
out="" url=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-o) out="$2"; shift 2 ;;
	-H | --data-binary) shift 2 ;;
	-*) shift ;;
	*) url="$1"; shift ;;
	esac
done
if [[ -n "$out" ]]; then printf 'bytes of %s\n' "${url##*/}" >"$out"; exit 0; fi
req=$(cat)
q=$(jq -r .query <<<"$req")
conn=$(sed -E 's/^[^{]*\{ *([a-zA-Z]+)\(.*/\1/' <<<"$(tr -s ' \n\t' ' ' <<<"$q")")
f="$STUB_FX/$conn.json"
[[ -f "$f" ]] || { echo "curl stub: no fixture for $conn" >&2; exit 1; }
if [[ "$conn" == issue ]]; then
	jq -c '{data: {issue: {history: {nodes: ., pageInfo: {hasNextPage: false, endCursor: null}}}}}' "$f"
elif [[ "$conn" == issues ]]; then
	if [[ "$(jq -r .variables.a <<<"$req")" == null ]]; then
		jq -c '{data: {issues: {nodes: .[:2], pageInfo: {hasNextPage: true, endCursor: "c1"}}}}' "$f"
	else
		jq -c '{data: {issues: {nodes: .[2:], pageInfo: {hasNextPage: false, endCursor: null}}}}' "$f"
	fi
else
	jq -c --arg c "$conn" '{data: {($c): {nodes: ., pageInfo: {hasNextPage: false, endCursor: null}}}}' "$f"
fi
STUB
chmod +x "$tmp/bin/curl"

none='{"nodes": []}'
cat >"$tmp/fx/issues.json" <<EOF
[
{"identifier": "OIK-1", "title": "Schedules", "priority": 2, "estimate": null, "dueDate": null,
 "description": "Shot: ![shot.png](https://uploads.linear.app/org/a/u1)\nAfter [OIK-2](https://linear.app/oikade/issue/OIK-2/run-a-step), see <https://linear.app/oikade/issue/OIK-4/pause>, [the step](https://linear.app/oikade/issue/OIK-2) and [Manual testing](https://linear.app/oikade/document/manual-testing-abc123).",
 "createdAt": "2026-10-01T10:00:00.123Z", "updatedAt": "2026-10-02T10:00:00.000Z", "startedAt": null,
 "completedAt": "2026-10-02T10:00:00.000Z", "canceledAt": null, "archivedAt": null, "triagedAt": null,
 "state": {"name": "Done"}, "project": {"name": "v0.1.0"}, "parent": null,
 "creator": {"name": "Oikade Agent"}, "assignee": null, "labels": {"nodes": [{"name": "Feature"}]},
 "relations": {"nodes": [{"type": "related", "relatedIssue": {"identifier": "OIK-4"}}]}, "inverseRelations": $none,
 "attachments": {"nodes": [{"title": "feat: schedules (#5)", "url": "https://github.com/o/r/pull/5"}]},
 "history": {"nodes": [
   {"createdAt": "2026-10-01T12:00:00.000Z", "fromState": {"name": "Design Review"}, "toState": {"name": "Todo"}, "actor": {"name": "Owner"}, "botActor": null},
   {"createdAt": "2026-10-01T11:00:00.000Z", "fromState": null, "toState": {"name": "Design Review"}, "actor": null, "botActor": {"name": "Oikade Agent"}},
   {"createdAt": "2026-10-01T11:30:00.000Z", "fromState": null, "toState": null, "actor": {"name": "Owner"}, "botActor": null}
 ], "pageInfo": {"hasNextPage": false}}},
{"identifier": "OIK-2", "title": "Run: a step", "priority": 0, "estimate": null, "dueDate": null, "description": null,
 "createdAt": "2026-10-01T10:05:00.000Z", "updatedAt": "2026-10-01T10:05:00.000Z", "startedAt": null,
 "completedAt": null, "canceledAt": null, "archivedAt": "2026-11-12T09:00:00.000Z", "triagedAt": null,
 "state": {"name": "Todo"}, "project": null, "parent": {"identifier": "OIK-1"},
 "creator": {"name": "Oikade Agent"}, "assignee": null, "labels": $none,
 "relations": $none, "inverseRelations": {"nodes": [{"type": "blocks", "issue": {"identifier": "OIK-1"}}, {"type": "related", "issue": {"identifier": "OIK-4"}}]},
 "attachments": $none, "history": {"nodes": [], "pageInfo": {"hasNextPage": false}}},
{"identifier": "OIK-4", "title": "Pause, then resume!", "priority": 0, "estimate": null, "dueDate": null, "description": "Why.\nhttps://uploads.linear.app/org/d/u4\n- next",
 "createdAt": "2026-10-03T10:00:00.000Z", "updatedAt": "2026-10-03T10:00:00.000Z", "startedAt": null,
 "completedAt": null, "canceledAt": null, "archivedAt": null, "triagedAt": null,
 "state": {"name": "Design Review"}, "project": null, "parent": null,
 "creator": {"name": "Owner"}, "assignee": null, "labels": $none, "relations": $none, "inverseRelations": $none,
 "attachments": $none, "history": {"nodes": [{"createdAt": "2026-10-03T10:00:00.000Z", "fromState": null, "toState": {"name": "Design Review"}, "actor": {"name": "Owner"}, "botActor": null}],
   "pageInfo": {"hasNextPage": true}}}
]
EOF
cat >"$tmp/fx/comments.json" <<'EOF'
[
{"id": "c1", "body": "First.", "createdAt": "2026-10-01T10:10:00.100Z", "updatedAt": "2026-10-01T10:10:00.100Z", "editedAt": null,
 "quotedText": null, "resolvedAt": null, "issue": {"identifier": "OIK-1"}, "parent": null, "user": null, "botActor": {"name": "Oikade Agent"}, "externalUser": null},
{"id": "c2", "body": "Second, same second: ![log](https://uploads.linear.app/org/b/u2)", "createdAt": "2026-10-01T10:10:00.900Z", "updatedAt": "2026-10-01T10:10:00.900Z", "editedAt": null,
 "quotedText": null, "resolvedAt": null, "issue": {"identifier": "OIK-1"}, "parent": null, "user": null, "botActor": {"name": "Oikade Agent"}, "externalUser": null},
{"id": "c3", "body": "Why not [OIK-4](https://linear.app/oikade/issue/OIK-4)?", "createdAt": "2026-10-01T11:00:00.000Z", "updatedAt": "2026-10-01T11:05:00.000Z", "editedAt": "2026-10-01T11:05:00.000Z",
 "quotedText": "the design", "resolvedAt": null, "issue": {"identifier": "OIK-1"}, "parent": {"id": "c1"}, "user": {"name": "Owner"}, "botActor": null, "externalUser": null}
]
EOF
cat >"$tmp/fx/projects.json" <<'EOF'
[
{"name": "v0.1.0", "state": "completed", "description": "The first release.", "content": "Goal: it runs.\n\nSee [Manual testing](https://linear.app/oikade/document/manual-testing-abc123). ![chart](https://uploads.linear.app/org/c/u3)",
 "startedAt": "2026-10-01T00:00:00.000Z", "completedAt": "2026-11-01T00:00:00.000Z", "canceledAt": null, "targetDate": null, "archivedAt": null, "teams": {"nodes": [{"key": "OIK"}]}},
{"name": "Elsewhere", "state": "started", "description": "Another team's.", "content": "", "startedAt": null, "completedAt": null, "canceledAt": null,
 "targetDate": null, "archivedAt": null, "teams": {"nodes": [{"key": "OTH"}]}}
]
EOF
# OIK-4 has more history than one page holds: the rest comes on its own.
cat >"$tmp/fx/issue.json" <<'EOF'
[{"createdAt": "2026-10-03T10:00:00.000Z", "fromState": null, "toState": {"name": "Design Review"}, "actor": {"name": "Owner"}, "botActor": null},
 {"createdAt": "2026-10-04T10:00:00.000Z", "fromState": null, "toState": null, "actor": {"name": "Owner"}, "botActor": null},
 {"createdAt": "2026-10-05T10:00:00.000Z", "fromState": {"name": "Design Review"}, "toState": {"name": "Backlog"}, "actor": {"name": "Owner"}, "botActor": null}]
EOF
cat >"$tmp/fx/projectUpdates.json" <<'EOF'
[{"body": "Shipped.", "createdAt": "2026-11-01T00:00:00.000Z", "health": "onTrack", "project": {"name": "v0.1.0"}, "user": {"name": "Owner"}}]
EOF
cat >"$tmp/fx/documents.json" <<'EOF'
[{"id": "d1234567-x", "slugId": "abc123", "title": "Manual testing", "content": "## Schedules\n\n- [ ] It fires.", "createdAt": "2026-10-01T00:00:00.000Z",
  "updatedAt": "2026-10-02T00:00:00.000Z", "archivedAt": null, "project": {"name": "v0.1.0"}, "creator": {"name": "Oikade Agent"}}]
EOF

export PATH="$tmp/bin:$PATH" STUB_FX="$tmp/fx" PLAN_DIR="$tmp/out/plan"
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
plan="$tmp/out/plan"
# The script finds the main checkout from git, so run it inside a repository.
git init -q "$tmp/out"
cd "$tmp/out"

has() { grep -qF -- "$2" "$plan/$1" || fail "$1 has no line $2"; }
lacks() { if grep -qF -- "$2" "$plan/$1"; then fail "$1 still has $2"; fi; }
exists() { [[ -e "$plan/$1" ]] || fail "no $1 in the export"; }

out=$("$tmp/hack/plan-export" 2>&1) || fail "the export failed: $out"

# A folder per status, and one per issue in it, named for its ID and title.
one="done/OIK-1-schedules" two="todo/OIK-2-run-a-step" four="design-review/OIK-4-pause-then-resume"
exists "$one/issue.md"
exists "$two/issue.md"
exists "$four/issue.md"
exists README.md

has "$one/issue.md" 'id: "OIK-1"'
has "$one/issue.md" 'title: "Schedules"'
has "$one/issue.md" 'release: "v0.1.0"'
has "$one/issue.md" 'labels: ["Feature"]'
has "$one/issue.md" 'related: ["OIK-4"]'
has "$one/issue.md" 'priority: 2'
has "$one/issue.md" 'created: "2026-10-01T10:00:00Z"'
has "$one/issue.md" 'links: ["feat: schedules (#5) https://github.com/o/r/pull/5"]'
# Status changes oldest first, by whoever made them, and nothing else from the history.
has "$one/issue.md" 'status-history: ["2026-10-01T11:00:00Z none -> Design Review by Oikade Agent","2026-10-01T12:00:00Z Design Review -> Todo by Owner"]'
lacks "$one/issue.md" 'blocked-by'
has "$two/issue.md" 'parent: "OIK-1"'
has "$two/issue.md" 'blocked-by: ["OIK-1"]'
has "$two/issue.md" 'archived: "2026-11-12T09:00:00Z"'
lacks "$two/issue.md" 'related'
lacks "$two/issue.md" 'priority'

# Uploads are copied beside the issue, and links into Linear are rewritten.
has "$one/issue.md" '![shot.png](files/u1)'
has "$one/files/u1" 'bytes of u1'
has "$one/issue.md" 'After OIK-2, see OIK-4, the step (OIK-2) and [Manual testing](../../docs/manual-testing.md).'
lacks "$one/issue.md" 'linear.app'
# An upload at the end of a line stops there.
has "$four/issue.md" 'files/u4'
has "$four/issue.md" '- next'
has "$four/files/u4" 'bytes of u4'
# A history longer than a page is fetched in full.
has "$four/issue.md" 'status-history: ["2026-10-03T10:00:00Z none -> Design Review by Owner","2026-10-05T10:00:00Z Design Review -> Backlog by Owner"]'

# One file per comment, named for its time and author; two in one second get -2.
c="$one/comments/2026-10-01T10-10-00Z-oikade-agent"
exists "$c.md"
exists "$c-2.md"
has "$c.md" 'author: "Oikade Agent"'
has "$c.md" 'First.'
has "$c-2.md" '![log](../files/u2)'
has "$one/files/u2" 'bytes of u2'
r="$one/comments/2026-10-01T11-00-00Z-owner.md"
has "$r" 'reply-to: "2026-10-01T10-10-00Z-oikade-agent"'
has "$r" 'quote: "the design"'
has "$r" 'edited: "2026-10-01T11:05:00Z"'
has "$r" 'Why not OIK-4?'

# Only this team's releases, with their updates, and the documents.
has releases/v0.1.0.md 'state: "completed"'
has releases/v0.1.0.md 'summary: "The first release."'
has releases/v0.1.0.md 'See [Manual testing](../docs/manual-testing.md).'
has releases/v0.1.0.md '## Update, 2026-11-01T00:00:00Z by Owner (onTrack)'
has releases/v0.1.0.md '![chart](files/u3)'
has releases/files/u3 'bytes of u3'
[[ ! -e "$plan/releases/Elsewhere.md" ]] || fail "exported another team's project"
has docs/manual-testing.md 'title: "Manual testing"'
has docs/manual-testing.md '- [ ] It fires.'

grep -qF 'Exported 3 issues (1 archived) up to OIK-4, 3 comments, 1 releases, 1 documents and 4 uploads' <<<"$out" ||
	fail "the summary is wrong: $out"
grep -qF 'Linear has no issue numbered OIK-3' <<<"$out" || fail "the gap at OIK-3 isn't reported: $out"

# plan/ is its own repository, and an export that changes nothing commits nothing.
[[ "$(git -C "$plan" rev-list --count HEAD)" == 1 ]] || fail "the first export isn't one commit"
out=$("$tmp/hack/plan-export" 2>&1) || fail "the second export failed: $out"
[[ "$(git -C "$plan" rev-list --count HEAD)" == 1 ]] || fail "an export that changed nothing made a commit"
grep -qF "hasn't changed" <<<"$out" || fail "the second export doesn't say nothing changed: $out"

# One that changes something replaces what was there, in a new commit.
jq '.[2].state.name = "Todo"' "$tmp/fx/issues.json" >"$tmp/fx/i" && mv "$tmp/fx/i" "$tmp/fx/issues.json"
"$tmp/hack/plan-export" >/dev/null 2>&1 || fail "the third export failed"
[[ -e "$plan/todo/OIK-4-pause-then-resume/issue.md" && ! -e "$plan/$four" ]] || fail "a moved issue isn't only in its new status"
[[ "$(git -C "$plan" rev-list --count HEAD)" == 2 ]] || fail "an export that changed something didn't commit"

# Changes in plan/ that aren't committed stop it before it writes.
echo note >"$plan/scratch.md"
if out=$("$tmp/hack/plan-export" 2>&1); then fail "exported over changes that weren't committed"; fi
grep -qF "aren't committed" <<<"$out" || fail "the refusal doesn't say why: $out"
[[ -e "$plan/scratch.md" ]] || fail "the refusal removed the change"
rm "$plan/scratch.md"

# The CSV check passes when every row matches, and names each row that doesn't.
printf '%s\n' 'ID,Title,Status,Project,Description' 'OIK-1,Schedules,Done,v0.1.0,"two' 'lines"' 'OIK-4,"Pause, then resume!",Todo,,' >"$tmp/good.csv"
out=$("$tmp/hack/plan-export" --csv "$tmp/good.csv" 2>&1) || fail "a matching CSV failed: $out"
grep -qF "Every one of the CSV's 2 rows matches" <<<"$out" || fail "a matching CSV isn't confirmed: $out"
printf '%s\n' 'ID,Title,Status,Project' 'OIK-1,Schedules,Todo,v0.1.0' 'OIK-9,Gone,Done,' >"$tmp/bad.csv"
if out=$("$tmp/hack/plan-export" --csv "$tmp/bad.csv" 2>&1); then fail "a CSV that disagrees passed"; fi
grep -qF 'OIK-1: Done, not Todo' <<<"$out" || fail "the CSV check doesn't name the status: $out"
grep -qF 'OIK-9: in the CSV but not exported' <<<"$out" || fail "the CSV check doesn't name the missing issue: $out"

# Two documents with one name both come over, each with its slugId.
cp "$tmp/fx/documents.json" "$tmp/documents.json"
jq '. + [.[0] | .id = "e7654321-y" | .slugId = "def456" | .title = "Manual Testing!" | .content = "Other."]' \
	"$tmp/documents.json" >"$tmp/fx/documents.json"
"$tmp/hack/plan-export" >/dev/null 2>&1 || fail "the export with two documents of one name failed"
has docs/manual-testing-abc123.md '- [ ] It fires.'
has docs/manual-testing-def456.md 'Other.'
has "$one/issue.md" '[Manual testing](../../docs/manual-testing-abc123.md)'
cp "$tmp/documents.json" "$tmp/fx/documents.json"

# What it can't copy yet stops it before it writes: a comment on no issue, or
# more labels, relations or links than one page holds.
cp "$tmp/fx/comments.json" "$tmp/comments.json"
jq '.[0].issue = null' "$tmp/comments.json" >"$tmp/fx/comments.json"
if out=$("$tmp/hack/plan-export" 2>&1); then fail "exported with a comment on no issue"; fi
grep -qF "1 comments aren't on an issue" <<<"$out" || fail "the refusal doesn't say why: $out"
cp "$tmp/comments.json" "$tmp/fx/comments.json"
cp "$tmp/fx/issues.json" "$tmp/issues.json"
jq '.[0].attachments.pageInfo = {hasNextPage: true}' "$tmp/issues.json" >"$tmp/fx/issues.json"
if out=$("$tmp/hack/plan-export" 2>&1); then fail "exported an issue with more links than a page"; fi
grep -qF "OIK-1 has more than 50" <<<"$out" || fail "the refusal doesn't name the issue: $out"
cp "$tmp/issues.json" "$tmp/fx/issues.json"
[[ -e "$plan/$one/issue.md" ]] || fail "a refused export changed plan/"

[[ $failed == 0 ]] && echo "plan-export: ok"
exit "$failed"
