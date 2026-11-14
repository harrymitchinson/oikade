---
name: work
description: Pick up Oikade work and build it. Take the next ready feature from Linear (or the one the user names), claim it so no other agent takes it, then build its steps one PR at a time in its own worktree, stacking each on the last while they wait for review. Test each step, run code review and open its PR. Use this whenever the user says "work on OIK-N", "pick something up", "do the next thing", "implement", "build the next step", "/work", or runs several sessions or a /loop to work in parallel, even if they never say "work". To decide what should be built, use design; to change the plan, use plan.
---

# Working on an Oikade feature

Several agents may run this skill at once, each in its own session. The claim keeps them apart: a feature belongs to whoever holds the branch `claim/OIK-N`, and `hack/plan` is the only safe way to take it. How the workflow fits together is in `docs/planning.md`.

You work on **one feature**. Its steps are the checklist lines under its `## Steps` heading, or its sub-issues, in order (a feature with neither is a single step). You build them one at a time, one PR each, on a branch per step: `work/OIKN-stepk` for line k, `work/OIK-S` for a sub-issue. You don't wait for the user to merge. You merge each step yourself once its checks pass. Only a step the user labelled `needs-review` waits for their review, and the next one stacks on it, so they review the stack bottom first. You stop when the feature has no steps left, when three of its steps are waiting for review, or when a step needs the user's decision. Then you release the claim.

## 1. Choose and claim

- **If the user named a feature,** use it. Otherwise run `hack/plan ready` and take the first line.
- **If nothing is ready,** don't invent work. Report what's waiting on the user: issues in Design Review, features nobody may build yet (`hack/plan show OIK-N` says why), PRs waiting for review, and stale claims from `hack/plan claims`. Then stop.
- **Claim it:** `hack/plan claim OIK-N`. It prints a worktree path, already on the next step's branch. **Do everything after this in that worktree**, not in the main checkout, which other sessions share.
- **If the claim fails** because the feature is already claimed, take the next line from `hack/plan ready`. Never work on a feature you didn't claim, and never delete someone else's `claim/OIK-N` branch unless the user tells you to.

## 2. Understand the step

Run `hack/plan show OIK-N`. It prints the feature, its steps and their PRs, the next step and why it may be built. Then read, through the Linear MCP tools:

- the feature's **Why**, and the step's scope and acceptance: its checklist line, or its sub-issue
- the design (in the feature, the feature its `Design: OIK-M` line names), and the comments on both, which may hold notes from earlier steps
- the code the step touches, plus `.claude/skills/design/references/surfaces.md` if the step touches public API or state that must survive

**If building the step needs a decision the design doesn't make,** and that decision is expensive to reverse (public API, state that must survive, component boundaries, a new dependency), don't make it. Ask with `hack/plan note OIK-N "<the question and the options>"`, then go to step 5. A feature that never went through Design Review, or builds under a `Design: OIK-M` line, has no review of its own, so be strict about this there. A cheap, local choice is yours to make. Say what you chose in the PR.

## 3. Build it

- **Only this step.** If you find work for later, mention it in the PR, or add it with `hack/plan idea` if it's new.
- **Follow AGENTS.md:** run `make lint-fix test` after Go changes. After changing the plugin contract, the SDK or a plugin, also run `make test-conformance`.
- **Keep the docs true.** The README stays lean: what Oikade is, getting started and a list of guides, and detail goes in a guide in `docs/`. If the step changes how something works, update the doc that says so in the same PR: the README and the guides in `docs/` for what users write and see, a plugin's own README (`plugins/<name>/README.md`) for that plugin, `docs/architecture.md` for the internals, `docs/plugin-contract.md` for the contract. Describe the present and link the feature for why. Don't copy the design.
- **Never run the journeys within a step.** The bar for a PR is `make test`. CI runs them only on main after a merge and nightly. If they break on main, fix it forward in a new PR. Say in the PR that they weren't run.
- **Test at the lowest level that shows the behaviour:** mappings, coalescing and IDs in unit tests, the host in `test/host`, Matter commissioning in an in-process node test (a Go controller talking to the node). Don't add a journey per feature: `test/journeys` keeps a few, across components.

- **Manual acceptance** (`(manual, in the Manual testing doc)`, such as pairing with the Home app and real devices) can't be checked here. Leave it for the user, check that the Linear document "Manual testing" lists it, and say so in the PR.
- **Commit as you go,** with conventional commits like the git log (`feat(schedule): …`). Stage explicit paths only.

## 4. Review and open the PR

1. Run the `code-review` skill on the step, or, when other agents run alongside you, a reviewer of your own (see [Running in parallel](#running-in-parallel)). When it's stacked, review only the step's own commits: `git diff <base>...HEAD`. Fix each finding, or explain in the PR why you didn't.
2. Write the PR body to a file in your scratchpad: the step, what you tested and how, anything left for manual testing, and any choices you made. If the step is stacked, say which PR it sits on, and that the user should merge that one first. `hack/plan pr` adds the Linear line.
3. Open it with `hack/plan pr OIK-N "<conventional title>" <body file>`. It pushes the step's branch and opens the PR against the step below it (or main), with a Linear line naming the step. Once it merges, `hack/plan merge` ticks the step's line. Don't open it any other way.
4. **Merge it yourself** once it's the bottom of its stack and CI passes: wait with `gh pr checks <PR> --watch`, then run `hack/plan merge OIK-N <PR>`. The user reviews designs, not diffs, so the checks and your code review are what vouch for it. Nothing waits for the user by default, CI and `hack/plan` changes included. `hack/plan merge` holds a step only if the user labelled it or its feature `needs-review`, and merges it once their Approve covers its head: it reads their review itself. Don't add `needs-review` yourself. A step that needs the user's decision asks with `hack/plan note` instead. Changes to `.claude/settings.json` are the one thing you can't make: write the proposed file to your scratchpad and ask the user to apply it. **Never work around a refusal,** and never merge any other way.
   - If a check fails, fix it on the branch and push, rather than leaving it red.
5. If a later step needs to know something that isn't in the code or the PR, leave it with `hack/plan note OIK-N "…"`.

**Then the next step:** if the feature has one, run `hack/plan step OIK-N` in the same worktree and go back to section 2. It refuses when three steps are waiting for review. That's the cue to stop.

## 5. Stop

1. **Merge what the user approved:** for each step waiting for review, bottom first, run `hack/plan merge OIK-N <PR>`. It merges a step whose head the user's Approve covers, restacks the rest, and refuses the others. Stop at the first refusal.
2. **Release the claim:** `hack/plan release OIK-N`. Your PRs and step branches stay, and the next session continues from them. Don't hold a claim you aren't building under.
3. **Reply** with the PR links in stack order, what's verified and what isn't, and anything that needs the user. Don't claim another feature unless the user asked you to keep going, as with a `/loop`.

- **If the user asks you to address comments on a step's PR,** you don't need a claim, because the PR's branch belongs to that step. If `hack/plan claims` shows another session holding the feature, ask the user first, so you don't rewrite a branch it's building on. Check out the branch in your own worktree (`git worktree add <path> origin/work/OIK-S`, then `git switch -c work/OIK-S`), fix it, and `git push`. If it's low in a stack, rebase each step above it onto the one below (`git rebase --onto <new tip> <old tip>`) and push each with `--force-with-lease`.
- **A step in another repo** (its sub-issue has `Repo: OWNER/NAME`) gets its own worktree from that repo's `main`: `hack/plan claim` and `step` print its path. Run `hack/plan` there by its path in this checkout (`<oikade>/hack/plan pr …`), and merge with `hack/plan merge OIK-N OWNER/NAME#PR`. If the repo doesn't exist yet, create it (`gh repo create OWNER/NAME --public`). If it's empty, run `hack/plan repo init OWNER/NAME`. If `hack/plan` can't write to it, ask the user to give the oikade-agent App access to it. Its CI needs workflows named `Tests` and `Lint`.
- **Before `hack/plan step`,** push any commits you've added to the current step since its PR opened. The next step builds on the pushed branch, and `step` refuses while it's behind.
- **If you can't finish a step,** commit and push what you have to its `work/OIK-S` branch, use the `handoff` skill, then release the claim. The next claim finds the pushed branch and resumes it rather than starting over.
- **An approved step left after its feature's claim was released** merges without claiming: run `hack/plan merge OIK-N <PR>`. It holds the claim while it runs if nobody else does, and gives it back.
- **After a merge,** whether yours or the user's, GitHub deletes the step's branch and Linear marks the step Done. The next `hack/plan claim` restacks the steps above it onto main. `git worktree remove <path>` cleans up locally.

## Running in parallel

When one session runs several agents on this skill at once, each on its own feature, these keep them from getting in each other's way (learnt on 2026-11-08, OIK-236):

- **About four at once.** Each runs `make test`, and more than that on one machine makes tests fail only under load (OIK-235). While iterating, test only the packages you touch, run the full `make test` once before the PR, and let CI be the bar after a rebase.
- **Review with a reviewer of your own.** The `code-review` skill's background reviewer is always named `code-review`, so with several agents running, a message to it can reach another agent's reviewer and a review can be lost. Start one with the Agent tool under a name of your own (`review-OIK-N`) and give it the diff, or review the diff yourself and say so in the PR. A review that hasn't come back within ten minutes is lost: don't wait for it.
- **Ask, don't decide, on what's expensive to reverse.** New saved state, a new message in a protocol or a new field in the network API is a question for the user (`hack/plan note`), even in a feature that never needed a design, unless the step's scope names it.
- **The session that started the agents watches the PRs, not the agents.** Every ten minutes or so, check `hack/plan claims` and each open PR's merge state (`gh pr view N --json mergeStateStatus`) and last commit. A PR that conflicts with main, or one with no commit for half an hour while its agent says it's waiting, needs a nudge. An agent's "waiting" doesn't say for what.
- **A brief says which features overlap.** Each agent should know which other features touch the same files, so it rebases onto them and keeps both sides, and a feature with several steps starts with the step that overlaps nobody.
