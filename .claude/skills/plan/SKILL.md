---
name: plan
description: Keep Oikade's plan on GitHub honest and answer what to do next. Manage issues, sub-issues and blocked-by links, milestones with observable exit criteria, the Oikade Project board, and docs/roadmap.md (direction, milestone order with reasons, and the "Not doing" list), reconciled against open design PRs, the README's "Not yet" list and recent git history. Use this whenever the user asks "what should I work on next", "what's the plan", "prioritise", "sequence these", "roadmap", "milestones", "what's left for the next release", "create an issue for…", "track this", wants to slot a new idea in, or reports that something shipped, even if they never say "roadmap". To decide how to build something, use the design skill instead.
---

# Planning Oikade

The plan lives in four places, and each fact has one owner. Don't copy facts between them:

| Fact | Owner |
|---|---|
| A piece of work, its scope and acceptance | The GitHub issue (the parent is a feature, sub-issues are steps) |
| Order between steps | Its "blocked by" relationships |
| Status | The Project board's Status field, [Oikade, project 5](https://github.com/users/harrymitchinson/projects/5). Closing an issue moves it to Done. |
| What's in a release | The GitHub milestone. Its description holds the goal and exit criteria. |
| Direction, milestone order and why, "Not doing" | `docs/roadmap.md` |
| How something is built, and why | `docs/design/NNNN-*.md`, via the design skill |

Background is in `docs/design/0008-planning-workflow.md`. Do bookkeeping (creating, editing, labelling and closing issues, and milestones) directly with `gh`. It never needs a PR. Only a change to `docs/roadmap.md` is a file change, and it goes in with the next PR that has a reason to touch it. Open a PR just for it only when the user asks.

## Answering "what's next"

1. **Read the current state:**
   - `gh issue list --state open --json number,title,labels,milestone --limit 200`
   - `gh pr list` (open design PRs are decisions waiting on the user)
   - `git log --oneline -20`
   - `docs/roadmap.md`
2. **Reconcile it:**
   - Close issues that merged PRs finished but didn't close.
   - Flag issues that are in a milestone but carry `needs-design` with no design PR.
   - Flag any milestone whose exit criteria nothing covers.
3. **Recommend** the first unblocked issue in the earliest milestone, with a one-line reason. List any design PRs waiting on the user, because they unblock the most work.

## Changing the plan

- **New idea:** an issue with no milestone, which puts it on the "later" list. Add `needs-design` if the design skill's step 2 says it needs one.
- **Milestones:** each has a goal and exit criteria that the user could observe, such as "in Kind, X reaches `Ready=True`". Avoid criteria like "code written". To create one:

  ```sh
  gh api -X POST repos/{owner}/{repo}/milestones -f title=… -f description=…
  ```

  Put an issue in one with `gh issue edit N -m "<title>"`. Assign milestones only when the user asks, or after agreeing an order with them.
- **Order:** `docs/roadmap.md` holds the milestone order, with one line on why each sits where it does. Changes to the order come from the user. Propose one; don't just make it.
- **Shipped:** check the milestone's exit criteria against what's actually observable, close it on GitHub, and mark it done in `docs/roadmap.md`.
- **"Not doing":** add an entry only when the user rules something out, and say who ruled it out and why.

## Rules

- Stage explicit paths. Never run `git add` on a directory: other sessions share the checkout.
- Never present an agent's choice as the user's decision. Only a design that was merged after review was decided by the user.
- Keep the plan vendor-neutral. A specific device family is an issue, not a direction.
