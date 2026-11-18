# shellcheck shell=bash
# plan-fixture: a scratch plan/ for hack/plan's tests, sourced by them. Set
# PLAN_DIR first: hack/plan reads the plan there.

# plan_init: an empty plan in PLAN_DIR, its own git repository.
plan_init() {
	mkdir -p "$PLAN_DIR"
	git -C "$PLAN_DIR" init -q
	git -C "$PLAN_DIR" commit -q --allow-empty -m "Start the plan"
}

# plan_issue FOLDER ID TITLE BODY [FRONT_MATTER_JSON]: write the issue ID into
# the status FOLDER (todo, design-review, …), replacing any issue with that ID,
# and commit it. FRONT_MATTER_JSON adds fields, such as status-history.
plan_issue() {
	local d extra="${5:-}"
	[[ -n "$extra" ]] || extra='{}'
	for d in "$PLAN_DIR"/*/"$2" "$PLAN_DIR"/*/"$2"-*; do
		if [[ -d "$d" ]]; then rm -rf "$d"; fi
	done
	d="$PLAN_DIR/$1/$2"
	mkdir -p "$d"
	{
		echo ---
		jq -rn --arg id "$2" --arg t "$3" --argjson e "$extra" \
			'{id: $id, title: $t} + $e | to_entries[] | select(.value != null) | "\(.key): \(.value | tojson)"'
		echo ---
		echo
		printf '%s\n' "$4"
	} >"$d/issue.md"
	git -C "$PLAN_DIR" add -A
	git -C "$PLAN_DIR" commit -q -m "Fixture: $2" >/dev/null || true
}

# plan_release NAME BODY: write a release.
plan_release() {
	mkdir -p "$PLAN_DIR/releases"
	printf -- '---\nname: "%s"\nstate: "started"\n---\n\n%s\n' "$1" "$2" >"$PLAN_DIR/releases/$1.md"
	git -C "$PLAN_DIR" add -A
	git -C "$PLAN_DIR" commit -q -m "Fixture: release $1"
}

# plan_status ID: the folder of the status the issue is in.
plan_status() {
	local d
	for d in "$PLAN_DIR"/*/"$1" "$PLAN_DIR"/*/"$1"-*; do
		if [[ -f "$d/issue.md" ]]; then basename "$(dirname "$d")"; return; fi
	done
}

# plan_file ID: the issue's issue.md.
plan_file() {
	local d
	for d in "$PLAN_DIR"/*/"$1" "$PLAN_DIR"/*/"$1"-*; do
		if [[ -f "$d/issue.md" ]]; then echo "$d/issue.md"; return; fi
	done
}

# plan_body ID: the issue's description.
plan_body() {
	awk 'NR == 1 && $0 == "---" {f = 1; next} f == 1 && $0 == "---" {f = 2; next}
		f == 2 && !s {s = 1; if ($0 == "") next} f == 2 {print}' "$(plan_file "$1")"
}

# plan_mark and plan_commits: the subjects of the plan's commits since the mark,
# so a test sees what a command wrote.
plan_mark() { PLAN_MARK=$(git -C "$PLAN_DIR" rev-parse HEAD); }
plan_commits() { git -C "$PLAN_DIR" log --format=%s "$PLAN_MARK..HEAD"; }
# plan_moves: the issues a command moved since the mark, one a line.
plan_moves() {
	plan_commits | sed -nE 's/^([A-Z]+-[0-9]+): (Triage|Backlog|Design Review|Todo|In Progress|In Review|Done|Canceled|Duplicate)$/\1/p'
}
