# Planning

How Oikade's work is planned, decided and built, by the user and by agents working in parallel. It describes the workflow as it is now. It replaces the design docs 0008, 0009 and 0108, whose history is in git.

The base is the same as it always was: a tracker holds the plan, and the user decides anything that's expensive to reverse. The user reviews designs, not diffs.

## Where things live

| What | Where |
|---|---|
| Ideas, features, steps, designs and their discussion | Linear, workspace `oikade`, team **Oikade (`OIK`)** |
| Releases | Linear projects (`v0.1.0`, `v0.2.0`, …) |
| Manual checks on real devices | The Linear document "Manual testing" |
| PRs, code review and CI | GitHub |
| "Not doing" | The README |
| How Oikade works now | The README for users, `docs/architecture.md` for the internals, `docs/plugin-contract.md` for plugins |
| This workflow, and the rules agents work under | This file, `AGENTS.md`, `.claude/` and `hack/` |

Linear is the source of truth for the plan and for every design. The repo keeps no copy of a design: the feature's issue, with its comments, is the record of why. The docs in the repo say how things work now, and link the issue for why. `#N` references in older PRs, commits and docs point at GitHub issues that were deleted when the plan moved to Linear.

## Ideas, features and releases

- **An idea is an issue in Triage.** `hack/plan idea` adds one, and `hack/plan promote` moves it to Backlog (or Design Review) once it's going to be built. Bugs skip Triage: they go straight to Backlog with the Bug label.
- **A feature is a top-level issue,** and each PR-sized step is one of its sub-issues, in order. A step's description has its scope and acceptance. A feature with no sub-issues is a single step.
- **"Blocked by" is only for dependencies between features.** Steps are ordered by their position under the feature.
- **A release is a Linear project.** Its description gives the goal, the exit criteria and why it sits where it does. The version numbers give the order.

## Designs

A design is needed only when the work changes something expensive to reverse:

- **public API:** CRD fields, capability names and semantics, metric names and labels, the CEL environment, the plugin contract, Matter endpoint IDs
- **state that must survive:** the Matter bridge's setup code and fabrics, network keys
- **component boundaries:** manager, runtime and matter
- **a new kind, a new driver, or a new runtime dependency**
- **anything else** an agent can't cheaply undo

How it works:

- **The design is written into the feature's description,** under `## Design`: Problem, Options with their cost of reversing, Recommendation, Consequences and Open questions. The feature sits in **Design Review** while it's under discussion, and the user replies in comments.
- **The user decides by moving it out of Design Review to Todo.** That move is the approval, and it also means "agents may start". Moving it back to Backlog says "not now". `hack/plan` reads who made the move from the issue's history, and counts it only if it was the user, the workspace's one human admin. Agents never move an issue out of Design Review, and never move one into Todo once it has been there.
- **An approved design is fixed.** `hack/plan edit` changes only `## Steps` and below once a feature is approved. A change to the design itself is a question for the user, in a comment.
- **An approved design covers the features that build it.** A feature whose description has a `Design: OIK-M` line, where the user approved OIK-M, may be built without a review of its own, as long as it makes no decision that OIK-M's design doesn't.
- **Work with nothing expensive to reverse needs no review.** An agent files it in Backlog and may move it to Todo itself, saying why in one comment. Examples are a plugin built on the existing contract that ships as its own image, CI, and docs. The user vetoes by moving it to Design Review.
- **Nothing is committed for a design.** Code, docs and comments that need to cite a design link its Linear issue.
- **The docs follow the code instead.** A step that changes how something works updates the doc that describes it in the same PR: the README for what users write and see, `docs/architecture.md` for the internals, `docs/plugin-contract.md` for the contract. These docs describe the present, with no options or history, so they never drift from the code. A design goes stale once it's built; the docs don't.

Designs from before the move to Linear are in Done issues: room-targeted automations (OIK-60), plugin drivers (OIK-61), the Go plugin SDK (OIK-62), release automation (OIK-63) and the rule model (OIK-64). The schedules design is in its feature, OIK-5. Each says at the top who decided which parts of it. An agent-written design is the user's decision only where it says so.

## Building: `hack/plan`

`hack/plan` is the only way skills and agents change planning state. `hack/plan help` lists its commands.

- **Ready** means a top-level issue in Todo, which may be built (approved, never needed a design, or a `Design: OIK-M` line naming an approved feature), with no open blocker, unclaimed, a step left to build and fewer than three steps waiting for review. The earliest release comes first.
- **A claim is the branch `claim/OIK-N`.** `hack/plan claim OIK-N` pushes a unique empty commit to it, but only if that branch doesn't exist yet. The server checks this atomically, so exactly one of two racing agents wins. It then creates a worktree for the next step, moves the feature and the step to In Progress, and comments on the feature. The claim means "an agent is building this now": the agent releases it when its session stops, even with PRs still open.
- **Each step has its own branch, `work/OIK-S`,** named for the step's own sub-issue. Linear's GitHub integration links the PR to the step by that name, and moves the step to In Review and then Done. `hack/plan pr` pushes the step and opens its PR, ending its description with `Linear: OIK-S, part of OIK-N`. `hack/plan sync` sets any status the integration missed, and the feature to Done once every step has merged.
- **Steps stack while they wait.** An agent doesn't stop at a PR: `hack/plan step` starts the next step on top of it, and its PR's base is the step below. When a step merges, `hack/plan restack` rebases the steps above it onto main, because a squash merge gives main a new commit. At most three of a feature's steps wait at once, so a change asked for low in the stack doesn't ripple far.
- **Parallelism comes from features and stacks.** One feature's steps are built in order, because each builds on the last. Different features run at the same time.
- **Stale claims are visible.** `hack/plan claims` flags any claim with no commit on it or its step branches for 24 hours. `hack/plan release` gives one up, and refuses to delete a claim branch that has commits unless it's forced.
- **Shared checkout.** Sessions share one checkout and its git index, so each works in its own worktree and stages explicit paths only.
- **Branches named `claim/*` and `work/*` are reserved** for claims and their steps. PRs merge with squash, bottom of a stack first.

## Merging

The user's review is the design, before any code. Code is vouched for by CI, the code-review skill and unit tests and envtest.

- **Agents never run Kind e2e within a step.** The bar for every PR is unit tests and envtest. CI doesn't run e2e on PRs: main runs it after each merge, nightly and before a release. If main's e2e breaks, fix it forward in a new PR.
- **Test at the lowest level that shows the behaviour.** Mappings, coalescing and endpoint IDs in unit tests, controllers in envtest, Matter commissioning in an in-process node test (a Go controller talking to the node, no Kind). Don't add e2e specs per feature: e2e keeps a few journeys across components.

- **Agents merge every step** once its checks pass, with `hack/plan merge OIK-N PR`. It runs under the feature's claim: the session building the feature merges under its own, and if nobody holds one, as once that session has released it, the merge takes the claim while it runs, so an approved step still merges. Only the session holding a claim merges under it. It merges only if the PR is the bottom of its stack and up to date with main, and every check passed. If neither it nor the feature has `needs-review`, that's all, whatever the diff touches, CI and the agents' own tooling included. Otherwise it's held for the user, and merges only once their Approve covers its head.
- **Nothing waits for the user by default** ([OIK-115](https://linear.app/oikade/issue/OIK-115)). `needs-review` is theirs to add, to a feature in Linear or to one PR, when they want to read the code first. Agents don't add it: a step that needs the user's decision asks in Linear (`hack/plan note`) instead. One change still needs the user's hand: `.claude/settings.json`, which Claude Code's auto mode won't let an agent loosen, so the user applies a change an agent proposes there.
- **Approving is the whole review.** Once the user approves a held PR, the agent merges it with `hack/plan merge` like any other step, so the user never has to merge. `hack/owner-approval` decides whether the Approve covers the PR as it is now, and `hack/plan merge` runs main's copy, never the PR's. A merge on the user's Approve isn't labelled `agent-merged`, since they've reviewed it.
- **`needs-review` on a feature in Linear** holds all of its PRs for the user: `hack/plan pr` copies the label to each.
- **`agent-merged` marks every agent merge,** and `hack/plan digest` lists the last week's, so the user can skim what changed. They don't need to remove the label.

## Agent identity

Agents act as themselves, so only the user's decisions count as the user's.

- **On GitHub, agents act through the `oikade-agent` GitHub App,** installed on this repo only. Its App ID and private key live in the macOS Keychain (service `oikade-agent`). `hack/agent-token` signs a JWT with the key and exchanges it for a one-hour installation token, which `hack/plan` uses for PR comments, labels, opening PRs and merges when it runs inside Claude Code (`CLAUDECODE=1`). Agent commits have the user as author and `oikade-agent[bot]` as committer, with the Claude trailer. Pushes use the user's git credentials.
- **In Linear, agents act as the Oikade Agent,** a Linear OAuth app with client credentials. `hack/linear-token` gets its token from the app's credentials in the Keychain (service `oikade-agent`) and caches it there for its 30 days. `hack/plan` uses it for every Linear call, and `.mcp.json` connects Linear's MCP server with the same token, so agents read Linear as the app. The app can't change the team's settings, so it can't add statuses or labels. The user's own Linear MCP connection for this repo is read-only.
- **The switch is `CLAUDECODE=1`.** Commands the user runs with `!` inside Claude Code inherit it and act as the bot, so the user runs `hack/plan` from their own terminal. There's deliberately no override, since an agent could set it too.
- **A missing credential fails loudly.** An agent's write stops before it writes anything, with an error naming the fix. It never falls back to the user's login.
- **The review gate.** "Approved by the owner" is a required check on main. It fails on a PR labelled `needs-review` until the user's latest review is an Approve of the code at its head, as `hack/owner-approval` on main decides. A restack keeps the Approve, since the step's own diff is unchanged byte for byte, even if the user approved it while it was stacked: the earlier bases GitHub recorded for the PR give the step below. New commits don't. A PR the user opened passes, since GitHub won't let them approve their own. GitHub runs the check from the PR's branch, so a PR could change it, but that doesn't open the gate: `hack/plan merge` reads the Approve itself, with main's `hack/owner-approval`, before it merges a held PR. The check runs main's `hack/owner-approval`, not the PR's.
- **Writes that act as the user, or can't be undone, are denied.** `.claude/settings.json` denies agents the `gh pr` writes `hack/plan` makes as the bot (`create`, `comment`, `edit`, `merge`, `review`), `gh api` (it can write anything without saying so up front), the deletes that can't be undone (repos, releases, secrets, labels, runs, deploy keys), repo renames and archiving, and the Linear MCP tools that change issues, projects or labels, or merge or review PRs. Other `gh` writes are allowed and act as the user, such as rerunning CI, `gh repo create` or `gh workflow run`. Agents may still comment and write documents through MCP. `make test-hack` checks that no skill or `AGENTS.md` names a denied form.
- **Labels that record the user's decisions.** `hack/plan` never removes `needs-review` or `agent-merged`.

## Skills

| Skill | Job |
|---|---|
| `design` | Decide whether a design is needed, write it into the issue, and once the user approves it, write the steps |
| `plan` | What's next, ideas, releases and the README's "Not doing" |
| `work` | Take the next ready feature, build its steps, review them and open the PRs |
| `add-capability` | A new capability end to end |
| `handoff` | A handoff note, and the status and next steps on the feature, so another agent can see them |

## Not doing

- **Agents running unattended,** in GitHub Actions or as Linear agent sessions. It's the way to keep agents working while the user's laptop is closed, but it needs credentials outside the Keychain, the Go 1.27 toolchain on runners, and trust in unattended agents. Revisit once the local flow has proven itself.
- **A second GitHub user account for agents,** or the official Claude GitHub App through `claude-code-action`. The first was ruled out; the second would move agent work into billed Actions runs.
