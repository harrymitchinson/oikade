#!/usr/bin/env bash
# no-linear: check that nothing in the repo links into Linear or calls it. The
# plan is in plan/ (docs/planning.md), and Linear's workspace is gone, so a
# link there leads nowhere and a call fails. Run by make test-hack.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

# linear.app covers links, uploads, its API and its MCP server. The rest are
# the ways this repo used to reach it. .claude/settings.json is the user's to
# change (AGENTS.md), so its deny rules for Linear's MCP tools aren't counted.
hits=$(git grep -n -I -E 'linear\.app|linear-token|mcp__linear|LINEAR_API' -- . \
	':!hack/test/no-linear.sh' ':!.claude/settings.json' || true)
if [[ -n "$hits" ]]; then
	echo "FAIL: these link into Linear or call it, and the plan is in plan/ now. Name an issue as OIK-N:" >&2
	echo "$hits" >&2
	exit 1
fi
echo "ok: no Linear"
