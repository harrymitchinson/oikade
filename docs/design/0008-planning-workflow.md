# 0008: Planning workflow

## Problem

Planning Oikade costs more than it should, and it can say things that aren't true.

- **State lives in six places:** Backlog docs, tasks and drafts, the Roadmap doc (doc-0005), `.claude/handoffs/`, Groma, and the README's "Not yet" list. Three skills (design-feature, write-spec and plan-roadmap) pass work between them.
- **Editing is indirect.** Backlog has no partial doc edit, and hand-editing `backlog/` is forbidden. So every change to a doc means exporting the whole body, editing it, and pushing it back with `--content`. One design took about ten round trips.
- **The same fact is kept in several places by hand.** A doc's `**Decision:**` and `**Work:**` lines duplicate the task's status, and promoting a draft changes its ID, so the doc has to be patched.
- **An agent can accept its own design.** doc-0002, doc-0003 and doc-0004 say `accepted`, and doc-0002 even has a "Settled in review" section, but you never reviewed them.
- **Parallel sessions collide.** Uncommitted `backlog/` changes are shared, so one session's `git add backlog/` swept up another's doc (PR #17).
- **Pure bookkeeping gets PRs of its own.**

This is what the workflow needs to do:

1. **You decide** anything that's expensive to reverse, by reviewing a doc.
2. **The next session knows** what to build and why, without asking you again.
3. **You can see what's next** from anywhere, including your phone.

## Decision

*Proposed. Merging this PR is your acceptance.*

### Tasks and the board live in GitHub

- **One issue per PR-sized piece of work.** Its body has **Why**, **Scope** (files, commands, risks) and **Acceptance** (a checklist of observable outcomes).
- **A feature is a parent issue with sub-issues.** Order is set with GitHub's "blocked by" relationships.
- **GitHub milestones** hold which issues are in each release. Each milestone's description holds its goal and exit criteria, and it can be edited.
- **One Project board ("Oikade")** has a Status field (Todo, In progress, Done) and is grouped by milestone. It's what you check from your phone.
- **Labels:** `needs-design`, `bug`, and `pi` for work that can only be verified on the Pi. Nothing else until a label is actually needed.
- **PRs say `Closes #N`.** Merging closes the issue and moves the card. Nobody updates status by hand.
- **Bookkeeping is done with `gh`, never in a PR:** creating, editing, labelling and closing issues.

### A design doc only when it's needed

A design doc is required when work changes something expensive to reverse:

- **public API:** CRD fields, capability names and semantics, metric names and labels, the CEL environment, the plugin contract, HomeKit IIDs
- **state that must survive:** identity, pairings, network keys, AIDs
- **component boundaries:** manager, runtime and hap
- **a new kind, a new driver, or a new runtime dependency**
- **anything else** an agent can't cheaply undo

Everything else is an issue with no doc. When in doubt, the agent adds `needs-design` to the issue and asks you.

- **Where designs live:** `docs/design/NNNN-slug.md`, as an ordinary file edited directly. That's about one page: Problem, Options (each with its cost of reversing), Decision, and Consequences (the public names it adds, state, what breaks). An API sketch goes in only when the names are the decision.
- **How review works:** the design arrives as a PR containing just that doc. You review it with inline comments. **Merging is acceptance.** That's the one kind of planning PR left, and it exists because you asked to decide by reviewing a doc.
- **Status:** there's no status line. A design on `main` is accepted, and an open PR is proposed. A replaced design gets one line at the top, `Superseded by 0012`.
- **Agents never mark anything accepted.** An agent-written design that hasn't been merged is a proposal, however confident it reads.
- **Implementation detail lives in the issues.** That includes exact types, test plans and file lists. There's no separate spec document: the issue is what the implementer reads, and the PR is what proves it.

### Direction lives in one file

`docs/roadmap.md` holds the direction in a paragraph, the milestone order with a line on why each is in its place, and the "Not doing" list. It changes rarely, so it can be a plain file. Milestone membership stays on GitHub only.

### Skills

| Now | Becomes |
|---|---|
| design-feature + write-spec | **`design`:** decides whether a doc is needed. If it is, writes it and opens the review PR. Once that PR is merged, creates the parent issue and sub-issues. |
| plan-roadmap and the Backlog CLI guides | **`plan`:** issues, sub-issues, milestones and the board. It answers "what's next" and keeps `docs/roadmap.md` up to date. |
| add-capability, handoff | Unchanged, except that handoff links the issue it's working on. |
| `write-spec/references/oikade-surfaces.md` | Moves to `.claude/skills/design/references/surfaces.md`. Both skills use it. |

### Rules for agents (in AGENTS.md)

- Stage explicit paths. Never `git add <dir>` for planning files.
- Planning goes in with the PR it belongs to. The only exception is a design review PR.
- Never treat an agent-written doc as your decision. Say who decided.

## Migration

Done in one PR after this one merges. Issues are created with `gh` before it.

- **Open work becomes issues.**
  - The 17 open tasks and 10 drafts become issues. Subtasks become sub-issues, with titles and acceptance criteria kept. Drafts get `needs-design` where they say a design is needed.
  - m-4 (Schedules) and m-5 (Retire Homebridge) become GitHub milestones.
  - Done tasks aren't migrated: they stay in git history.
- **Docs move to `docs/design/` and keep their numbers**, so existing references still read correctly:
  - doc-0001: room-targeted automations, shipped
  - doc-0002: plugin drivers, with your 2026-10-19 review note
  - doc-0006: Go plugin SDK, with TASK-0008 becoming issues
- **Designs nobody has reviewed go to design PRs instead,** one per doc, for you to review: doc-0003 (schedules) first, then doc-0004 (release automation) and doc-0007 (time triggers, from another session). Until they merge, their issues carry `needs-design`.
- **doc-0005 (Roadmap) becomes `docs/roadmap.md`.**
- **What gets deleted:** `backlog/`, the Backlog.md block in AGENTS.md, and the three old skills.
- **What gets updated:** two references (README line 254 and `docs/plugin-contract.md` line 3).
- **PR #17 is closed without merging.** Its content migrates.
- **Before migrating,** you run `! gh auth refresh -s project`. The current token lacks the Project scope.

## Not doing

- Changing Groma. It records architecture, not plans.
- Issue forms or templates beyond a single feature template. Add them when they'd stop something going wrong.
- Automation beyond `Closes #N`, such as bots or auto-labelling.

## Open questions

None.

Settled in review:

- **doc-0003 (Schedules) and doc-0004 (Release automation) aren't accepted.** Each one moves to its own design PR for you to review, Schedules first. doc-0007 does the same.
- **Design review happens in a PR.**
