---
name: work
description: Pick up Oikade work and build it. Take the next ready feature from GitHub (or the one the user names), claim it so no other agent takes it, then build its steps one PR at a time in its own worktree, stacking each on the last while they wait for review. Test each step, run code review and open its PR. Use this whenever the user says "work on #N", "pick something up", "do the next thing", "implement", "build the next step", "/work", or runs several sessions or a /loop to work in parallel, even if they never say "work". To decide what should be built, use design; to change the plan, use plan.
---

# Working on an Oikade feature

Several agents may run this skill at once, each in its own session. The claim keeps them apart: a feature belongs to whoever holds the branch `work/N`, and `hack/plan` is the only safe way to take it. How the workflow fits together is in `docs/design/0009-planning-workflow.md`.

You work on **one feature**. You build its steps one at a time, one PR each, on branches named `work/N-S`. You don't wait for the user to merge. A low-risk step you merge yourself once its checks pass. Any other step waits for the user's Approve, and the next one stacks on it, so the user reviews the stack bottom first. Once they've approved a step, you merge it too. You stop when the feature has no steps left, when three of its steps are waiting for review, or when a step needs the user's decision. Then you release the claim.

## 1. Choose and claim

- **If the user named an issue,** use it. Otherwise run `hack/plan ready` and take the first line.
- **If nothing is ready,** don't invent work. Report what's waiting on the user: `gh issue list --label needs-design`, features nobody may build yet (`hack/plan show N` says why), PRs waiting for review, and stale claims from `hack/plan claims`. Then stop.
- **Claim it:** `hack/plan claim N`. It prints a worktree path, already on the next step's branch. **Do everything after this in that worktree**, not in the main checkout, which other sessions share.
- **If the claim fails** because the feature is already claimed, take the next line from `hack/plan ready`. Never work on a feature you didn't claim, and never delete someone else's `work/N` branch unless the user tells you to.

## 2. Understand the step

Run `hack/plan show N`. It prints the issue, its comments, the next step and why it may be built. Read:

- the **Why**, and the step's `### <step>` section: its scope and acceptance
- the design (in the issue, the feature its `Design: #M` line names, or the `docs/design/` file it links), and the comments, which may hold notes from earlier steps
- the code the step touches, plus `.claude/skills/design/references/surfaces.md` if the step touches public API or state that must survive

**If building the step needs a decision the design doesn't make,** and that decision is expensive to reverse (public API, state that must survive, component boundaries, a new dependency), don't make it. Ask with `hack/plan note N "<the question and the options>"`, then go to step 5. A feature built under `no-design` or a `Design: #M` line has no review of its own, so be strict about this there. A cheap, local choice is yours to make. Say what you chose in the PR.

## 3. Build it

- **Only this step.** If you find work for later, mention it in the PR, or add it with `hack/plan idea` if it's new.
- **Follow AGENTS.md:** scaffold with `kubebuilder`, run `make manifests generate` after editing types, and `make lint-fix test` after Go changes. After changing the plugin contract, the SDK or a plugin, also run `make test-conformance`.
- **e2e gets its own cluster,** so parallel agents don't collide and nothing touches the user's current context:

  ```sh
  export KIND_CLUSTER=oikade-e2e-N KUBECONFIG="$TMPDIR/oikade-e2e-N.kubeconfig"
  make test-e2e && make cleanup-test-e2e
  ```

- **Manual acceptance** (`(manual, in #27)`, such as real HomeKit pairing and devices) can't be checked here. Leave it for the user, check that #27 lists it, and say so in the PR.
- **Commit as you go,** with conventional commits like the git log (`feat(schedule): …`). Stage explicit paths only.

## 4. Review and open the PR

1. Run the `code-review` skill on the step. When it's stacked, review only the step's own commits: `git diff <base>...HEAD`. Fix each finding, or explain in the PR why you didn't.
2. Write the PR body to a file in your scratchpad. It has `Part of #N`, or `Closes #N` if this is the feature's last unticked step. Then the step, what you tested and how, anything left for manual testing, and any choices you made. If the step is stacked, say which PR it sits on, and that the user should merge that one first.
3. Open it with `hack/plan pr N "<conventional title>" <body file>`. It pushes `work/N-S`, opens the PR against the step below it (or main) and records it against the step. Don't open it any other way.
4. **Merge it yourself if it's low risk.** It is when the step's acceptance needs no manual testing, and the step changes nothing in `.claude/skills/design/references/surfaces.md` (public API, state that must survive, component boundaries, dependencies). It also has to be the bottom of its stack. Wait for CI with `gh pr checks <PR> --watch`, then run `hack/plan merge N <PR>`. That checks again: it refuses a PR that touches a protected path, labels it `needs-review` and leaves it for the user's Approve. Once they've approved it, `hack/plan merge` merges it: it reads their review itself. If you judge a step risky though it touches no protected path, label it yourself: `hack/plan label <PR> +needs-review`. **Never work around a refusal,** and never merge any other way.
   - If a check fails, fix it on the branch and push, rather than leaving it red.
5. If a later step needs to know something that isn't in the code or the PR, leave it with `hack/plan note N "…"`.

**Then the next step:** if the feature has one, run `hack/plan step N` in the same worktree and go back to section 2. It refuses when three steps are waiting for review. That's the cue to stop.

## 5. Stop

1. **Merge what the user approved:** for each step waiting for review, bottom first, run `hack/plan merge N <PR>`. It merges a step whose head the user's Approve covers, restacks the rest, and refuses the others. Stop at the first refusal.
2. **Release the claim:** `hack/plan release N`. Your PRs and step branches stay, and the next session continues from them. Don't hold a claim you aren't building under.
3. **Reply** with the PR links in stack order, what's verified and what isn't, and anything that needs the user. Don't claim another feature unless the user asked you to keep going, as with a `/loop`.

- **If the user asks you to address comments on a step's PR,** you don't need a claim, because the PR's branch belongs to that step. If `hack/plan claims` shows another session holding the feature, ask the user first, so you don't rewrite a branch it's building on. Check out the branch in your own worktree (`git worktree add <path> origin/work/N-S`, then `git switch -c work/N-S`), fix it, and `git push`. If it's low in a stack, rebase each step above it onto the one below (`git rebase --onto <new tip> <old tip>`) and push each with `--force-with-lease`.
- **Before `hack/plan step`,** push any commits you've added to the current step since its PR opened. The next step builds on the pushed branch, and `step` refuses while it's behind.
- **If you can't finish a step,** commit and push what you have to its `work/N-S` branch, use the `handoff` skill, then release the claim. The next claim finds the pushed branch and resumes it rather than starting over.
- **After a merge,** whether yours or the user's, GitHub deletes the step's branch. The next `hack/plan claim` ticks the step and restacks the steps above it onto main. `git worktree remove <path>` cleans up locally.
