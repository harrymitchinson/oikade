# 0009: Planning workflow, revised

Replaces [0008](0008-planning-workflow.md). It keeps 0008's base: a tracker holds the plan, and you decide anything that's expensive to reverse.

*Revised on 2026-10-19, because you had become the bottleneck: agents stopped at every approval and every merge. An approved design now covers the features that build it, work with nothing expensive to reverse needs no approval, a feature's steps stack while they wait for review, and agents merge steps that touch nothing you review first. See [Revision: fewer waits](#revision-fewer-waits).*

*Revised on 2026-10-20, at your request: `docs/roadmap.md` is gone, because the milestones already held most of it. Each release's description now says why it sits where it does, the version numbers give the order, and "Not doing" is in the README.*

*Revised on 2026-10-21 by [0108](0108-agent-identity.md): agents act on GitHub as `oikade-agent[bot]`, and you review a `needs-review` PR by approving it. See [Revision: agent identity](#revision-agent-identity).*

*Revised on 2026-10-24, at your request: you review designs, not diffs. Agents merge every step once its checks pass, except a small floor of changes to their own guardrails and to state whose loss means re-pairing. See [Revision: review designs, not diffs](#revision-review-designs-not-diffs).*

*Revised on 2026-10-24, at your request: the plan moved from GitHub issues and Projects to Linear, and you approve a design by moving it out of Design Review instead of adding a label. Every GitHub issue and the board were deleted, so `#N` references in older PRs, commits and docs no longer resolve. See [Revision: Linear](#revision-linear).*

## Problem

- **Too many issues.** One issue per PR-sized step made 26 open issues out of about a dozen pieces of work. Ten of them were ideas nobody had committed to.
- **The tracker is awkward for agents.** Sub-issues, "blocked by" links and board fields needed GraphQL, which agents got wrong or retried.
- **Reviewing a design as a PR feels odd.** The PR has no code, merging it feels like shipping, a rejected design leaves a closed PR, and you're reviewing in a diff.
- **Agents can't pick up work in parallel.** Nothing said which work was ready for an agent, or stopped two agents taking the same work.

## Decision

### Ideas are in Triage, features are issues

All of this lives in the Linear team **Oikade (`OIK`)**.

- **An idea is an issue in Triage.** `hack/plan idea` adds one, and `hack/plan promote` moves it to Backlog (or Design Review) once it's going to be built. Bugs skip Triage: they go straight to Backlog with the Bug label.
- **A feature is a top-level issue,** and each PR-sized step is one of its sub-issues, in order. A step's description has its scope and acceptance. A feature with no sub-issues is a single step. Linear shows the sub-issues' progress on the feature.
- **"Blocked by" is only for dependencies between features.** Steps are ordered by their position under the feature.
- **A release is a Linear project** (`v0.1.0`, `v0.2.0`, …). Its description gives the goal, the exit criteria and why it sits where it does. The version numbers give the order.

### Designs are reviewed in the issue

- **The design is written into the feature's description,** with the same content as before: Problem, Options with their cost of reversing, Recommendation, Consequences and Open questions. The feature sits in **Design Review** while it's under discussion, and you reply in comments.
- **You decide by moving it out of Design Review to Todo.** That move is the approval, and it also means "agents may start". Moving it back to Backlog says "not now". `hack/plan` reads who made the move from the issue's history, and counts it only if it was you, the workspace's one human admin. Agents never move an issue out of Design Review, and never move one into Todo once it has been there.
- **An approved design covers the features that build it.** A feature whose description has a `Design: OIK-M` line, where you approved OIK-M, may be built without a review of its own, as long as it makes no decision that OIK-M's design doesn't.
- **Work with nothing expensive to reverse needs no review.** An agent files it in Backlog and may move it to Todo itself, saying why in one comment. Examples are a plugin built on the existing contract that ships as its own image, CI, and docs. You veto by moving it to Design Review.
- **The file in `docs/design/` is the record, not the review.** When the decision is expensive to reverse (the same list as 0008), the first step of the feature commits a one-page `docs/design/NNNN-slug.md` condensed from the approved issue. NNNN is the feature's issue number, so agents working in parallel can't pick the same one. It goes in with the code that needs it, not in a PR of its own.

### Agents pick up work through `hack/plan`

`hack/plan` is the only way skills and agents change planning state. `hack/plan help` lists its commands.

- **Ready** means a top-level issue in Todo, which may be built (approved, never needed a design, or a `Design: OIK-M` line naming an approved feature), with no open blocker, unclaimed, a step left to build and fewer than three steps waiting for review. The earliest release comes first.
- **A claim is the branch `claim/OIK-N`.** `hack/plan claim OIK-N` pushes a unique empty commit to it, but only if that branch doesn't exist yet. The server checks this atomically, so exactly one of two racing agents wins. It then creates a worktree for the next step, moves the feature and the step to In Progress, and comments on the feature. The claim means "an agent is building this now": the agent releases it when its session stops, even with PRs still open.
- **Each step has its own branch, `work/OIK-S`,** named for the step's own sub-issue. Linear's GitHub integration links the PR to the step by that name, and moves the step to In Review and then Done. `hack/plan pr` pushes the step and opens its PR, ending its description with `Linear: OIK-S, part of OIK-N`. `hack/plan sync` sets any status the integration missed, and the feature to Done once every step has merged.
- **Steps stack while they wait for review.** An agent doesn't stop at a PR: `hack/plan step` starts the next step on top of it, and its PR's base is the step below. You can review a stack in one sitting, bottom first. When a step merges, `hack/plan restack` rebases the steps above it onto main, because a squash merge gives main a new commit. At most three of a feature's steps wait at once, so a change you ask for low in the stack doesn't ripple far.
- **Parallelism comes from features and stacks.** One feature's steps are built in order, because each builds on the last, but they don't wait for your merges. Different features run at the same time.
- **Stale claims are visible.** `hack/plan claims` flags any claim with no commit on it or its step branches for 24 hours. `hack/plan release` gives one up, and refuses to delete a claim branch that has commits unless it's forced.
- **Kind is per feature.** Each agent runs e2e with `KIND_CLUSTER=oikade-e2e-<N>` and its own `KUBECONFIG`, so parallel e2e runs don't collide or touch your current context.

### Skills

| Skill | Job |
|---|---|
| `design` | Decide whether a design is needed, write it into the issue, and once you approve it, write the steps |
| `plan` | What's next, ideas, releases and the README's "Not doing" |
| `work` | Take the next ready feature, build one step, review it and open the PR |
| `handoff` | Also posts the status and next steps on the feature, so another agent can see them |

## Consequences

- **What 0008 rules this replaces:** design-only PRs and merging as acceptance. Its other rules stand: explicit staging, worktrees, and never treating an agent's choice as yours.
- **Branches named `claim/*` and `work/*` are reserved** for claims and their steps. Merge their PRs with squash, as the repo already does, and merge a stack bottom first. A step merged into the step below it, rather than into main, isn't done until that one merges.
- **Agents in parallel are bounded by your reviews,** but less than before (see the revision below).

## Revision: fewer waits

*2026-10-19. You chose these in conversation, out of options an agent proposed. The details, such as the stack of three, are the agent's, and you reviewed them in the PR.*

Before this revision a feature waited on you twice: once to approve it, and once per step to merge, with nothing built in between. Now:

- **Approval carries over** from a design to the features that build it (`Design: OIK-M`).
- **Work with nothing expensive to reverse** may be built without your approval.
- **Steps stack:** an agent builds a feature's steps one after another, each on its own branch, and leaves up to three for you to review together.

- **Agents merge low-risk steps.** `hack/plan merge OIK-N PR` merges a step's PR only if all of these hold:
  - the session merging it holds the claim;
  - it's the bottom of its stack, and up to date with main;
  - its Tests and Lint checks, and any others, all passed;
  - neither it nor the feature has the `needs-review` label;
  - its diff touches no protected path, counting both sides of a rename.

  The protected paths are:
  - public API, state and boundaries: `api/`, `cmd/`, `config/`, capability, HAP, hub, identity, metrics, status, the CEL environment, the Driver interface, the homekit and zigbee controllers, and the plugin contract, decoder, SDK and conformance suite;
  - dependency manifests;
  - what the checks run: `Makefile`, Dockerfiles, `.golangci.yml` and `.github/`;
  - the rules agents work under: `.claude/`, `.mcp.json`, `hack/`, `AGENTS.md` and the design docs.

  `.claude/settings.json` denies agents `gh pr merge`, every other raw `gh` write, and the Linear MCP tools that merge or change issues, so they can't merge any other way. A PR that touches one gets `needs-review` and waits for you. A merged one gets `agent-merged`, so you can review it afterwards and remove the label. Agents can't merge changes to their own rules, and a feature's first step, which commits its design doc, always waits for you. Add the `needs-review` label to a feature in Linear to review all of its steps yourself: `hack/plan pr` then labels each of its PRs.

## Revision: agent identity

*2026-10-21, from [0108](0108-agent-identity.md), approved on GitHub.*

Until this revision every agent action showed up as you, so the two gates above held only because agents behaved. Now:

- **Agents write as themselves.** Inside Claude Code, `hack/plan` writes to PRs through the `oikade-agent` GitHub App, and agent commits name the bot as committer, with you as author. Pushes still act as you. In Linear, agents act as the Oikade Agent app (see [Revision: Linear](#revision-linear)). Run `hack/plan` yourself from a terminal, not with `!` inside Claude Code, which acts as the bot.
- **Only your decisions count as yours.** `hack/plan` counts a design as approved only if you moved it out of Design Review, never removes `needs-review` or `agent-merged`, and edits only `## Steps` and below on an approved design.
- **You review a `needs-review` PR by approving it.** Agents open PRs as the bot, so you can approve them. The required check "Approved by the owner" fails on a PR labelled `needs-review` until your latest review is an Approve of the code at its head. A restack doesn't reset it, since the step's own diff is unchanged, but new commits do. A PR you opened yourself passes, since GitHub won't let you approve it. Low-risk steps without the label still merge with `hack/plan merge`.

## Revision: Linear

*2026-10-24. You asked to move off GitHub for planning, chose Linear, and told the agent to go ahead without a design review. You chose a project per release, steps as sub-issues, ideas in Triage, the Design Review status in place of the `approved` label, and deleting every GitHub issue and the board. The rest is the agent's.*

- **The plan is in Linear,** in the workspace `oikade`, team `OIK`. PRs, code review and CI stay on GitHub.
- **Agents act in Linear as the Oikade Agent,** a Linear OAuth app with client credentials. `hack/linear-token` gets its token from the app's credentials in the Keychain (service `oikade-agent`) and caches it there for its 30 days. `hack/plan` uses it for every Linear call, inside Claude Code or not. `.mcp.json` connects Linear's MCP server with the same token, so agents read Linear through MCP as the app. Your own Linear MCP connection for this repo is read-only.
- **The app can't change the team's settings.** It can't add statuses or labels, which keeps the workflow yours.
- **Raw MCP writes are denied** where they would skip `hack/plan`'s rules: changing issues, projects and labels, and merging or reviewing PRs from Linear. Agents may still comment and write documents through MCP.
- **Approval is a status move, read from history.** A Linear workspace can't stop an app from moving an issue, so `hack/plan` checks who made the move instead. A move it didn't make, such as one through MCP, still doesn't count unless it was yours.
- **Imported, not linked.** The open issues and drafts were copied to OIK-5 to OIK-58, with their comments. The Manual testing checklist became a Linear document in the `v0.2.0` project.

## Revision: review designs, not diffs

*2026-10-24. You asked whether you'd realistically review agents' PRs. Of the 21 PRs labelled `needs-review` until then, 20 merged, a median of two hours after opening, and 4 had a review on GitHub. You chose to keep Design Review and drop `needs-review` as the default. The paths in the floor are the agent's.*

- **Your review is the design,** in Design Review, before any code. Code is vouched for by CI, the code-review skill and e2e.
- **Agents merge every step** once its checks pass, with `hack/plan merge`, which still requires the claim, the bottom of the stack, up to date with main, and every check green.
- **The floor that waits for you** (`PROTECTED` in `hack/plan`):
  - the agents' own guardrails, so an agent can't loosen its rules or checks and then merge: `.github/`, `.claude/settings.json`, `.mcp.json`, `hack/plan`, its token helpers and tests, the `Makefile` and `.golangci.yml`;
  - the code that keeps state whose loss means re-pairing every HomeKit accessory or Zigbee device: `internal/identity/`, `internal/hap/`, the HAP and Zigbee2MQTT renderers, the homekit and zigbee controllers, and `cmd/hap.go`.

  Public API, dependencies, skills and design docs are no longer in it: a design you approved covers them.
- **`agent-merged` still marks every agent merge,** and `hack/plan digest` lists the last week's, so you can skim what changed when you want to. You don't need to remove the label.
- **`needs-review` on a feature in Linear** still holds all of its PRs for you, if you want to read one feature's code.

This replaces the protected list in [Revision: fewer waits](#revision-fewer-waits).

## Not doing

- **Agents running unattended,** in GitHub Actions or as Linear agent sessions. It's the way to keep agents working while your laptop is closed, but it needs credentials outside the Keychain, the Go 1.27 toolchain on runners, and trust in unattended agents. Revisit once the local flow has proven itself.
- **Agents merging what they can't prove is low risk.** A PR that touches a protected path waits for you, whatever its checks say. The list lives in `hack/plan` (`PROTECTED`), and only you change it.
