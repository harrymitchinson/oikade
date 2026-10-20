---
name: plan
description: Keep Oikade's plan on GitHub honest and answer what to do next. Manage ideas (board drafts), features (issues with a Steps checklist), blocked-by links between features, milestones with observable exit criteria, the Oikade Project board, claims by parallel agents, and docs/roadmap.md (direction, milestone order with reasons, and the "Not doing" list), reconciled against recent git history and the README's "Not yet" list. Use this whenever the user asks "what should I work on next", "what's the plan", "what's waiting on me", "prioritise", "sequence these", "roadmap", "milestones", "what's left for the next release", "create an issue for…", "track this", "note this idea", wants to slot a new idea in, or reports that something shipped, even if they never say "roadmap". To decide how to build something, use design; to build it, use work.
---

# Planning Oikade

How the workflow fits together is in `docs/design/0009-planning-workflow.md`. Each fact has one owner, so don't copy facts between them:

| Fact | Owner |
|---|---|
| An idea nobody has committed to | A draft item on the [Oikade board](https://github.com/users/harrymitchinson/projects/5) |
| A feature: why, its design, its steps and their acceptance | The GitHub issue. Its `## Steps` checklist is the order. |
| That the user decided it, and agents may start | The `approved` label. **Only the user adds it.** |
| That agents may start without the user's approval | The `no-design` label (nothing expensive to reverse), or a `Design: #M` line naming an approved feature whose design it builds. The design skill sets these. |
| Order between features | "Blocked by" relationships |
| Who's working on it | The branch `work/N` (`hack/plan claims`). Its steps are on `work/N-S`, stacked while they wait for review. |
| Status | The board. Claiming moves it to In Progress, and closing moves it to Done. |
| What's in a release | The GitHub milestone. Its description holds the goal and exit criteria. |
| Direction, milestone order and why, "Not doing" | `docs/roadmap.md` |

Use `hack/plan` for everything it covers (`hack/plan help`), not raw `gh api`. Bookkeeping (ideas, issues, labels, milestones) is done directly and never needs a PR. Only `docs/roadmap.md` is a file. A change to it goes in with the next PR that has a reason to touch it, or its own PR if the user asks.

## Answering "what's next"

1. **Read the current state:**
   - `hack/plan sync` ticks steps whose PRs merged, and resets cards nobody holds
   - `hack/plan ready` lists what an agent can start now
   - `hack/plan claims` lists what's in flight, and what's stale
   - `gh issue list --label needs-design` and `gh pr list` show what's waiting on the user
   - `git log --oneline -20` and `docs/roadmap.md`
2. **Reconcile it:**
   - Close features whose last step merged without closing them.
   - Flag stale claims. Release one only if the user agrees.
   - Flag any milestone whose exit criteria no feature covers.
3. **Answer in this order:**
   - what's waiting on the user (designs to decide, `needs-review` PR stacks to merge bottom first, questions in issue comments), since that unblocks the most
   - what agents merged that the user hasn't looked at yet: `gh pr list --state merged --label agent-merged`
   - what's ready, with the first item and a one-line reason
   - what's in flight

## Changing the plan

- **A new idea:** `hack/plan idea "<title>" "<a few lines: the problem, and where it came from>"`. Don't open an issue for it.
- **Committing to an idea:** when the user wants it built, `hack/plan promote <item id>` turns it into an issue. Then use the design skill: it decides whether a design is needed, and writes the Steps.
- **A bug:** an issue with the `bug` label, written as a feature with one step unless it needs more.
- **A feature issue's body:**

  ```markdown
  ## Why
  One or two lines.

  ## Steps
  - [ ] <step one>
  - [ ] <step two>

  ### <step one>
  Scope: the files, commands (`kubebuilder create …`, `make manifests generate`) and risks.
  Acceptance:
  - <observable outcome> (unit / integration / envtest / Kind e2e / manual)

  ### <step two>
  …
  ```

  Only steps are checkboxes. Acceptance lines are plain bullets, so GitHub's "N of M tasks" counts steps. Acceptance that needs real devices or the Home app, which CI and Kind can't provide, reads `(manual, in #27)`. Its check goes in #27, Manual testing, under a heading for the feature. The feature closes when its last step merges, and the check stays open in #27 until the user has done it. Only #27 carries the `manual` label.
- **Order between features:**

  ```sh
  gh api -X POST repos/{owner}/{repo}/issues/<blocked>/dependencies/blocked_by -F issue_id=$(gh api repos/{owner}/{repo}/issues/<blocker> -q .id)
  ```

- **Milestones:** each has a goal and exit criteria that the user could observe, such as "in Kind, X reaches `Ready=True`". Avoid criteria like "code written". To create one:

  ```sh
  gh api -X POST repos/{owner}/{repo}/milestones -f title=… -f description=…
  ```

  Put a feature in one with `gh issue edit N -m "<title>"`. Assign milestones only when the user asks, or after agreeing an order with them.
- **Order of milestones:** `docs/roadmap.md` holds it, with one line on why each sits where it does. Changes come from the user. Propose one; don't just make it.
- **Shipped:** check the milestone's exit criteria against what's actually observable, close it on GitHub, and mark it done in `docs/roadmap.md`.
- **"Not doing":** add an entry only when the user rules something out, and say who ruled it out and why.

## Rules

- Never add `approved`, and never present an agent's choice as the user's decision.
- Stage explicit paths. Never run `git add` on a directory: other sessions share the checkout.
- Keep the plan vendor-neutral. A specific device family is a feature or an idea, not a direction.
