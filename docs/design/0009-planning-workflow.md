# 0009: Planning workflow, revised

Replaces [0008](0008-planning-workflow.md). It keeps 0008's base: GitHub holds the plan, `docs/roadmap.md` holds the direction, and you decide anything that's expensive to reverse. It changes the three things that didn't work in practice.

*Revised on 2026-10-19, because you had become the bottleneck: agents stopped at every approval and every merge. An approved design now covers the features that build it, work with nothing expensive to reverse needs no approval, a feature's steps stack while they wait for review, and agents merge steps that touch nothing you review first. See [Revision: fewer waits](#revision-fewer-waits).*

*Revised on 2026-10-21 by [0108](0108-agent-identity.md): agents act on GitHub as `oikade-agent[bot]`, `approved` counts only when you add it, and you review a `needs-review` PR by approving it. See [Revision: agent identity](#revision-agent-identity).*

*Revised on 2026-10-20, at your request: `docs/roadmap.md` is gone, because the milestones already held most of it. Each milestone's description now says why it sits where it does, the version numbers give the order, and "Not doing" is in the README.*

## Problem

- **Too many issues.** One sub-issue per PR-sized step made 26 open issues out of about a dozen pieces of work. Ten of them are ideas nobody has committed to.
- **`gh` is awkward for agents.** Sub-issues, "blocked by" links and board fields need GraphQL through `gh api`, which agents get wrong or retry.
- **Reviewing a design as a PR feels odd.** The PR has no code, merging it feels like shipping, a rejected design leaves a closed PR, and you're reviewing in a diff.
- **Agents can't pick up work in parallel.** Nothing says which work is ready for an agent, or stops two agents taking the same work. Every agent uses your GitHub account, so assignees don't tell agents apart.

## Decision

*Proposed. This is the last design decided 0008's way: merging this PR is your acceptance. Designs after it are decided with the `approved` label.*

### Ideas are board drafts, features are issues

- **An idea is a draft item on the Oikade board.** It isn't an issue, so it notifies nobody and stays out of the Issues tab. It sits in the board's No Status column. `hack/plan idea` adds one, and `hack/plan promote` turns it into an issue once it's going to be built.
- **A feature is one issue.** It has a `## Steps` checklist with one line per PR-sized step, in order, and a `### <step>` section for each step with its scope and acceptance. GitHub shows "2 of 7 tasks" in issue lists, which is the progress.
- **"Blocked by" is only for dependencies between features.** Steps are ordered by the checklist.

### Designs are reviewed in the issue

- **The design is written into the feature issue,** with the same content as before: Problem, Options with their cost of reversing, Recommendation, Consequences and Open questions. The issue carries `needs-design` while it's under discussion, and you reply in comments.
- **You decide by adding the `approved` label.** Agents never add it. It also means "agents may start".
- **An approved design covers the features that build it.** A feature whose body has a `Design: #M` line, where #M is approved, may be built without a label of its own, as long as it makes no decision that #M's design doesn't. A feature that needs a new decision of that kind gets its own review.
- **Work with nothing expensive to reverse needs no approval.** The `design` skill labels it `no-design` and says why in one comment. Examples are a plugin built on the existing contract that ships as its own image, CI, and docs. You veto by removing the label.
- **The file in `docs/design/` is the record, not the review.** When the decision is expensive to reverse (the same list as 0008), the first step of the feature commits a one-page `docs/design/NNNN-slug.md` condensed from the approved issue. NNNN is the issue number, so agents working in parallel can't pick the same one. It goes in with the code that needs it, not in a PR of its own.
- **A long design can be reviewed in a Claude Doc instead,** if you ask for one. The decision is still the `approved` label.

### Agents pick up work through `hack/plan`

`hack/plan` is the only way skills and agents touch planning state. Its commands are `ready`, `show`, `claim`, `step`, `pr`, `restack`, `link`, `release`, `claims`, `sync`, `note`, `ideas`, `idea` and `promote`.

- **Ready** means open, may be built (`approved`, `no-design`, or a `Design: #M` line naming an approved feature), no `needs-design` or `manual` label, no open blocker, unclaimed, and fewer than three steps waiting for review. The earliest milestone comes first.
- **A claim is the branch `work/N`.** `hack/plan claim N` pushes a unique empty commit to `work/N`, but only if that branch doesn't exist yet. The server checks this atomically, so exactly one of two racing agents wins. It then creates a worktree for the next step, moves the card to In Progress, and comments on the issue. The claim means "an agent is building this now": the agent releases it when its session stops, even with PRs still open.
- **Each step has its own branch, `work/N-S`,** where S is the step's line in the checklist. Nothing builds on `work/N` itself. `hack/plan pr` pushes the step, opens its PR and records it against the step as `(PR #M)`. The PR says `Part of #N`, or `Closes #N` for the last step. When you merge it, GitHub deletes its branch and `sync` ticks the step.
- **Steps stack while they wait for review.** An agent doesn't stop at a PR: `hack/plan step` starts the next step on top of it, and its PR's base is the step below. You can review a stack in one sitting, bottom first. When you merge a step, `hack/plan restack` rebases the steps above it onto main, because a squash merge gives main a new commit. At most three of a feature's steps wait at once, so a change you ask for low in the stack doesn't ripple far.
- **Parallelism comes from features and stacks.** One feature's steps are built in order, because each builds on the last, but they no longer wait for your merges. Different features run at the same time.
- **Stale claims are visible.** `hack/plan claims` flags any claim with no commit on it or its step branches for 24 hours. `hack/plan release` gives one up, and refuses to delete a branch that has an open PR or commits unless it's forced.
- **Kind is per feature.** Each agent runs e2e with `KIND_CLUSTER=oikade-e2e-<N>` and its own `KUBECONFIG`, so parallel e2e runs don't collide or touch your current context.

### Skills

| Skill | Job |
|---|---|
| `design` | Decide whether a design is needed, write it into the issue, and once you approve it, write the Steps |
| `plan` | What's next, ideas, milestones, the board and the README's "Not doing" |
| `work` (new) | Take the next ready feature, build one step, review it and open the PR |
| `handoff` | Also posts the status and next steps on the issue, so another agent can see them |

## Consequences

- **What 0008 rules this replaces:** design-only PRs, merging as acceptance, and sub-issues as steps. Its other rules stand: explicit staging, worktrees, and never treating an agent's choice as yours.
- **There's a new label, `approved`.** It already exists on GitHub.
- **Branches named `work/*` are reserved for claims and their steps.** Merge their PRs with squash, as the repo already does, and merge a stack bottom first. A step merged into the step below it, rather than into main, isn't ticked until that one merges.
- **Agents in parallel are bounded by your reviews,** but less than before (see the revision below).

## Revision: fewer waits

*2026-10-19. You chose these in conversation, out of options an agent proposed. The details, such as the label names and the stack of three, are the agent's, and you review them in this PR.*

Before this revision a feature waited on you twice: once to approve it, and once per step to merge, with nothing built in between. Now:

- **Approval carries over** from a design to the features that build it (`Design: #M`).
- **`no-design`** marks work with nothing expensive to reverse, which agents may build without your approval.
- **Steps stack:** an agent builds a feature's steps one after another, each on its own branch, and leaves up to three for you to review together.

- **Agents merge low-risk steps.** `hack/plan merge N PR` merges a step's PR only if all of these hold:
  - the session merging it holds the claim;
  - it's the bottom of its stack, and up to date with main;
  - its Tests and Lint checks, and any others, all passed;
  - neither it nor the feature has the `needs-review` label;
  - its diff touches no protected path, counting both sides of a rename.

  The protected paths are:
  - public API, state and boundaries: `api/`, `cmd/`, `config/`, capability, HAP, hub, identity, metrics, status, the CEL environment, the Driver interface, the homekit and zigbee controllers, and the plugin contract, decoder, SDK and conformance suite;
  - dependency manifests;
  - what the checks run: `Makefile`, Dockerfiles, `.golangci.yml` and `.github/`;
  - the rules agents work under: `.claude/`, `hack/`, `AGENTS.md` and the design docs.

  `.claude/settings.json` denies agents `gh pr merge` and every other raw `gh` write, so they can't merge any other way. A PR that touches one gets `needs-review` and waits for you. A merged one gets `agent-merged`, so you can review it afterwards and remove the label. Agents can't merge changes to their own rules, and a feature's first step, which commits its design doc, always waits for you. Add `needs-review` to a feature to review all of its steps yourself.

That also changes claims. A claim lasts while an agent is building, not until you merge. Claims taken before this revision, with their PR on `work/N` itself, still work: merging that PR deletes `work/N` and frees the claim as before. `hack/plan merge` refuses those, since merging one would delete a claim.

## Revision: agent identity

*2026-10-21, from [0108](0108-agent-identity.md), approved in #108.*

Until this revision every agent action showed up as you, so the two gates above held only because agents behaved. Now:

- **Agents write as `oikade-agent[bot]`.** Inside Claude Code, `hack/plan` writes to issues and PRs through the `oikade-agent` GitHub App, and agent commits name the bot as committer, with you as author. Board moves and pushes still act as you. Agents make every GitHub write through `hack/plan`, and `.claude/settings.json` denies them raw `gh` writes and `gh api`. Run `hack/plan` yourself from a terminal, not with `!` inside Claude Code, which acts as the bot.
- **`approved` counts only when you add it.** The Approved guard workflow removes it, with a comment, when anyone else adds it. `hack/plan`'s gate also checks who added it last, so a missed run can't make a feature ready. `hack/plan` never adds or removes `approved`, never removes `needs-review` or `agent-merged`, and edits only `## Steps` and below on an approved feature.
- **You review a `needs-review` PR by approving it.** Agents open PRs as the bot, so you can approve them. The required check "Approved by the owner" fails on a `needs-review` PR until your latest review is an Approve. A restack doesn't reset it. Low-risk steps without the label still merge with `hack/plan merge`.

## Migration

This is GitHub bookkeeping, done with `gh` after this PR merges:

- **Schedules (#19) and Go plugin SDK (#30):** fold their sub-issues into a Steps checklist on the parent, keeping each step's scope and acceptance. Then close the sub-issues as "not planned", with a link to the parent. A sub-issue that doesn't depend on its siblings, such as #31 if it really stands alone, becomes its own feature instead.
- **The nine `needs-design` issues that are only ideas:** turn them into board drafts and close the issues. Time triggers (#39, design PR #47) and Release automation (#36, design PR #46) stay as issues. Their design PRs are reviewed as they stand, since they were already open.
- **`approved`:** you add it to whatever you've already decided. Schedules (#19) qualifies, because #45 merged. An agent doesn't add it for you.

## Not doing

- **GitHub Actions running agents on labelled issues.** It's the way to keep agents working while your laptop is closed, but it needs API credentials in the repo, the Go 1.27 toolchain on runners, and trust in unattended agents. Revisit once the local flow has proven itself.
- **A tracker outside GitHub,** such as Linear or Trello. They're nicer to look at, but they lose the PR link, and Trello also loses dependencies.
- **Agents merging what they can't prove is low risk.** A PR that touches a protected path waits for you, whatever its checks say. The list lives in `hack/plan` (`PROTECTED`), and only you change it.
