#!/usr/bin/env bash
# no-raw-gh-writes: check that no skill or agent guide tells an agent to run a
# gh write that .claude/settings.json denies. Agents write through hack/plan,
# so their writes show as oikade-agent[bot] (docs/design/0108-agent-identity.md).
# Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

# "Bash(gh pr create:*)" -> "gh pr create"
denied=$(jq -r '.permissions.deny[] | select(startswith("Bash(gh ")) | sub("^Bash\\("; "") | sub(":\\*\\)$"; "")' .claude/settings.json)
[[ -n "$denied" ]] || { echo "FAIL: .claude/settings.json denies no gh commands" >&2; exit 1; }

docs=(AGENTS.md)
[[ -f CLAUDE.md ]] && docs+=(CLAUDE.md)
hits=$(grep -rnF -f <(printf '%s\n' "$denied") .claude/skills "${docs[@]}" || true)
if [[ -n "$hits" ]]; then
	echo "FAIL: these tell agents to run a gh write that .claude/settings.json denies. Use hack/plan instead:" >&2
	echo "$hits" >&2
	exit 1
fi
echo "ok: no raw gh writes in skills"
