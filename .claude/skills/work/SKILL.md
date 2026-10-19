---
name: work
description: Pick up Oikade work and build it. Take the next ready feature from GitHub (or the one the user names), claim it so no other agent takes it, build exactly one step in its own worktree, test it, run code review and open the PR. Use this whenever the user says "work on #N", "pick something up", "do the next thing", "implement", "build the next step", "/work", or runs several sessions or a /loop to work in parallel, even if they never say "work". To decide what should be built, use design; to change the plan, use plan.
---

# Working on an Oikade feature

Several agents may run this skill at once, each in its own session. The claim keeps them apart: a feature belongs to whoever holds the branch `work/N`, and `hack/plan` is the only safe way to take it. How the workflow fits together is in `docs/design/0009-planning-workflow.md`.

You build **one step** of **one feature**, then stop. The user reviews and merges. Steps in a feature build on each other, so the feature stays claimed until the user merges your PR. Then the next session can take the next step.

## 1. Choose and claim

- **If the user named an issue,** use it. Otherwise run `hack/plan ready` and take the first line.
- **If nothing is ready,** don't invent work. Report what's waiting on the user: `gh issue list --label needs-design`, features without the `approved` label, and stale claims from `hack/plan claims`. Then stop.
- **Claim it:** `hack/plan claim N`. It prints a worktree path. **Do everything after this in that worktree**, not in the main checkout, which other sessions share.
- **If the claim fails** because the feature is already claimed, take the next line from `hack/plan ready`. Never work on a feature you didn't claim, and never delete someone else's `work/N` branch unless the user tells you to.

## 2. Understand the step

Run `hack/plan show N`. It prints the issue, its comments and the next step. Read:

- the **Why**, and the step's `### <step>` section: its scope and acceptance
- the design (in the issue, or the `docs/design/` file it links), and the comments, which may hold notes from earlier steps
- the code the step touches, plus `.claude/skills/design/references/surfaces.md` if the step touches public API or state that must survive

**If building the step needs a decision the issue doesn't make,** and that decision is expensive to reverse (public API, state that must survive, component boundaries, a new dependency), don't make it. Ask with `hack/plan note N "<the question and the options>"`, then `hack/plan release N`, and stop. A cheap, local choice is yours to make. Say what you chose in the PR.

## 3. Build it

- **Only this step.** If you find work for later, mention it in the PR, or add it with `hack/plan idea` if it's new.
- **Follow AGENTS.md:** scaffold with `kubebuilder`, run `make manifests generate` after editing types, and `make lint-fix test` after Go changes. For the TypeScript workspace, run `corepack yarn fix` and `make test-plugins`.
- **e2e gets its own cluster,** so parallel agents don't collide and nothing touches the user's current context:

  ```sh
  export KIND_CLUSTER=oikade-e2e-N KUBECONFIG="$TMPDIR/oikade-e2e-N.kubeconfig"
  make test-e2e && make cleanup-test-e2e
  ```

- **Pi-only acceptance** (the `pi` label, or real HomeKit pairing and devices) can't be checked here. Leave it for the user, and say so in the PR.
- **Commit as you go,** with conventional commits like the git log (`feat(schedule): …`). Stage explicit paths only.

## 4. Review and open the PR

1. Run the `code-review` skill on the branch. Fix each finding, or explain in the PR why you didn't.
2. Push: `git push origin work/N`. The branch already exists from the claim, so this is a plain push. Don't force-push over another session's commits.
3. Open the PR from `work/N`:

   ```sh
   gh pr create --head work/N --title "<conventional title>" --body "<body>"
   ```

   The body has `Part of #N`, or `Closes #N` if this is the feature's last unticked step. Then the step, what you tested and how, anything left for the Pi, and any choices you made.
4. Record it: `hack/plan link N <PR>`.
5. If the next step needs to know something that isn't in the code or the PR, leave it with `hack/plan note N "…"`.

## 5. Stop

Reply with the PR link, what's verified and what isn't, and anything that needs the user. Don't claim another feature unless the user asked you to keep going, as with a `/loop`.

- **If the user comments on your PR,** address it in the same worktree, on `work/N`.
- **If you can't finish the step,** push what you have to `work/N` and use the `handoff` skill, which also posts a note on the issue. Keep the claim, so the next session resumes from the branch rather than starting over.
- **After the merge,** the branch is deleted for you. `hack/plan sync N` ticks the step, and `git worktree remove <path>` cleans up locally.
