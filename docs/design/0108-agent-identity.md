# 0108: Agent identity on GitHub

> Approved in [#108](https://github.com/harrymitchinson/oikade/issues/108), which has the discussion. It changes how [0009](0009-planning-workflow.md)'s two gates are enforced, not what they are.

## Problem

Every agent action on GitHub showed up as the user. Commits were authored by the user with a `Co-Authored-By: Claude` trailer, and every comment, claim, label, PR and merge that `hack/plan` made went through `gh` as `harrymitchinson`. That broke 0009 in two places:

- **The `approved` label proved nothing.** In an issue's timeline, an agent adding it looked exactly like the user adding it. The rule held only because agents behaved.
- **The user couldn't approve agent PRs.** GitHub won't let a PR's author approve it, and every PR was opened as the user, so `needs-review` couldn't be an enforced gate.

A second GitHub user account was ruled out.

## Decision

**Agents act through our own GitHub App, `oikade-agent`,** registered under the user's account and installed on this repo only, with Issues, Pull requests and Contents read/write. Its App ID and private key live in the macOS Keychain (service `oikade-agent`). `hack/agent-token` signs a JWT with the key and exchanges it for a one-hour installation token. `hack/plan` uses that token for its issue and PR writes when it runs inside Claude Code (`CLAUDECODE=1`).

| Action | Shows as |
|---|---|
| Issue/PR comments, claims, opening PRs, labels, merges | `oikade-agent[bot]` |
| Board item moves (`gh project ...`) | the user: Apps can't access a personal account's Projects |
| Commits | author the user, committer `oikade-agent[bot]`, with the Claude trailer |
| Pushes | the user's git credentials |

- **A missing key fails loudly.** An agent's write stops before it writes anything, with an error naming the fix. It never falls back to the user's login.
- **The `approved` guard.** A workflow on `issues: labeled` removes `approved`, and comments, when anyone other than the user added it. `hack/plan ready` also checks who added it, so a missed workflow run can't make a feature ready.
- **The `needs-review` gate.** A required check fails while a PR has `needs-review` and no approving review from the user. Low-risk steps still merge with `hack/plan merge`.
- **No raw `gh` writes.** Skills write through `hack/plan`, and `.claude/settings.json` denies the raw write forms, as it already does `gh pr merge`.

### Options not taken

- **The official Claude GitHub App via `claude-code-action`.** Only Actions runs get its token, so agent work would move into Actions, with billed runs, e2e on runner Kind clusters and no shared checkout. That's a different workflow, not an identity change.
- **A marker plus honour-system rules.** Neither gate becomes enforceable, and a stray raw `gh` call skips the marker.

## Consequences

- **New names:** the App `oikade-agent`, `hack/agent-token`, a workflow for the label guard and one for the review gate, and `GIT_COMMITTER_NAME`/`GIT_COMMITTER_EMAIL` in `.claude/settings.json`. The email is `<bot-user-id>+oikade-agent[bot]@users.noreply.github.com`, with the bot user's id from `gh api users/oikade-agent%5Bbot%5D`, not the App id.
- **No product impact:** no public API, CRDs, metrics or stored device state change.
- **Setup by the user:** register and install the App, and put its ID and key in the Keychain, as `hack/agent-token`'s header says.
- **Reversing it** is cheap: uninstall the App and drop the token path in `hack/plan`. Past comments keep the bot's name, which is the point.
