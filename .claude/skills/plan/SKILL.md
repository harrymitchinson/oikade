---
name: plan
description: Keep Oikade's plan in Linear honest and answer what to do next. Manage ideas (issues in Triage), features (issues whose steps are sub-issues), blocked-by links between features, releases (Linear projects) with observable exit criteria, claims by parallel agents, and the README's "Not doing" list, reconciled against recent git history and the README's "Not yet" list. Use this whenever the user asks "what should I work on next", "what's the plan", "what's waiting on me", "prioritise", "sequence these", "roadmap", "milestones", "releases", "what's left for the next release", "create an issue for…", "track this", "note this idea", wants to slot a new idea in, or reports that something shipped, even if they never say "roadmap". To decide how to build something, use design; to build it, use work.
---

# Planning Oikade

How the workflow fits together is in `docs/planning.md`. The plan is in Linear, team `OIK`. Each fact has one owner, so don't copy facts between them:

| Fact | Owner |
|---|---|
| An idea nobody has committed to | An issue in Triage |
| A feature: why and its design | A top-level issue's description |
| Its steps and their acceptance | Its sub-issues, in order. A feature with none is a single step. |
| That a design waits for the user | The status **Design Review** |
| That the user decided it, and agents may start | The user moving it out of Design Review to Todo. **Only the user does that.** |
| That agents may start without the user's approval | Never having been in Design Review (nothing expensive to reverse), or a `Design: OIK-M` line naming an approved feature whose design it builds. The design skill decides which. |
| Order between features | "Blocked by" relationships |
| Who's working on it | The branch `claim/OIK-N` (`hack/plan claims`). Its steps are on `work/OIK-S`, stacked while they wait for review. |
| Status | The issue's status. Claiming moves it to In Progress, and Linear's GitHub integration moves each step as its PR opens and merges. |
| What's in a release | The Linear project (`v0.1.0`, …). Its description holds the goal, the exit criteria and a "Why here" line. |
| Order between releases | The version numbers, with each project's "Why here" line |
| What the user ruled out, and why | The README's "Not doing" |

Read Linear through its MCP tools (`mcp__linear__list_issues`, `get_issue` and the like), which act as the Oikade Agent. Use `hack/plan` for everything it covers (`hack/plan help`), and for every change to issues, statuses, labels, projects and blocked-by links: the MCP tools that would change them are denied, so `hack/plan`'s rules always apply. Comments can go through either. Bookkeeping never needs a PR. Only "Not doing" is a file, in the README. A change to it goes in with the next PR that has a reason to touch it, or its own PR if the user asks.

## Answering "what's next"

1. **Read the current state:**
   - `hack/plan sync` sets steps from their PRs, and sends features nobody holds back to Todo
   - `hack/plan ready` lists what an agent can start now
   - `hack/plan claims` lists what's in flight, and what's stale
   - issues in Design Review, and `gh pr list`, show what's waiting on the user
   - `git log --oneline -20`, and the releases with `hack/plan projects`
2. **Reconcile it:**
   - Flag stale claims. Release one only if the user agrees.
   - Flag any release whose exit criteria no feature covers.
3. **Answer in this order:**
   - what's waiting on the user (designs in Design Review, `needs-review` PR stacks to merge bottom first, questions in comments), since that unblocks the most
   - what agents merged that the user hasn't looked at yet: `gh pr list --state merged --label agent-merged`
   - what's ready, with the first item and a one-line reason
   - what's in flight

## Changing the plan

- **A new idea:** `hack/plan idea "<title>" "<a few lines: the problem, and where it came from>"`. It lands in Triage.
- **Committing to an idea:** when the user wants it built, `hack/plan promote OIK-N` moves it to Backlog. Then use the design skill: it decides whether a design is needed, and writes the steps.
- **A bug:** `hack/plan issue "<title>" <body file> --label Bug`, written as a feature with one step unless it needs more.
- **A feature's description:**

  ```markdown
  ## Why
  One or two lines.

  ## Design
  (only if it needs one: see the design skill)
  ```

  Each step is a sub-issue, created in order:

  ```sh
  hack/plan issue "<step one>" <file> --parent OIK-N
  ```

  A step's description holds its scope and acceptance:

  ```markdown
  Scope: the files, commands and risks.
  Acceptance:
  - <observable outcome> (unit / integration / journey / manual)
  ```

  Acceptance that needs real devices or the Home app, which CI can't provide, reads `(manual, in the Manual testing doc)`. Its check goes in the Linear document "Manual testing" (project `v0.2.0`), under a heading for the feature. The feature is done when its last step merges, and the check stays open in the document until the user has done it.
- **Order between features:**

  ```sh
  hack/plan block <blocked> <blocker>
  ```

- **Releases:** each has a goal, exit criteria that the user could observe, and a closing "Why here:" line on why it sits where it does. The exit criteria are things such as "`oikadectl status` shows X". Avoid criteria like "code written". To create one:

  ```sh
  hack/plan project new "<version>" <description file>
  ```

  Change its description with `hack/plan project edit "<version>" <file>`, and its name with `hack/plan project rename "<version>" "<new>"`. Put a feature in one with `hack/plan project set OIK-N "<version>"`. Assign releases only when the user asks, or after agreeing an order with them.
- **Order of releases:** the version numbers give it, and each project's description ends with a "Why here" line. Changes come from the user. Propose one; don't just make it.
- **Shipped:** check the release's exit criteria against what's actually observable, and close it with `hack/plan project close "<version>"`.
- **"Not doing":** add an entry only when the user rules something out, and say who ruled it out and why.

## Rules

- Never move an issue out of Design Review, and never present an agent's choice as the user's decision.
- Stage explicit paths. Never run `git add` on a directory: other sessions share the checkout.
- Keep the plan vendor-neutral. A specific device family is a feature or an idea, not a direction.
