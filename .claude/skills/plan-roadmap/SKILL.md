---
name: plan-roadmap
description: Build or update Oikade's roadmap in Backlog.md by sequencing work into milestones with observable exit criteria, dependencies and an explicit "not doing" list, kept in the Roadmap doc (doc-0005) plus milestone membership, and reconciled against the board, design docs, the README's "Not yet" list and recent git history. Use this whenever the user asks "what should I work on next", "what's the plan", "prioritise", "sequence these", "roadmap", "milestones", "what's left for the next release", wants to slot a new idea or design into the plan, or reports that something shipped and the plan needs updating, even if they never say the word "roadmap".
---

# Planning Oikade's roadmap

This is the third of three planning skills (design-feature → write-spec → **plan-roadmap**). The roadmap answers three questions: what's next and why, what each milestone has to prove, and what we're deliberately not doing. What's done and what's in flight is already on the board, so the roadmap doesn't repeat it.

**Read `.claude/skills/write-spec/references/tracking.md` first.** In short, the roadmap is split two ways:

- **Backlog state** is membership and status. Each task or draft's milestone, its dependencies, and its status. Drafts with no milestone are the "later" list.
- **The Roadmap doc (doc-0005)** holds what Backlog can't: the direction, the milestones in order with each one's goal, exit criteria and reason for its place, and "Not doing". It has no item tables and no statuses.

This is one person's home platform, so the roadmap should help them pick the next weekend's work with confidence. A corporate quarterly plan is the wrong model: don't use dates unless the user gives them, and don't build a Gantt chart.

## 1. Gather the current truth

Read these before proposing anything. Roadmaps written from memory drift from reality.

- The Roadmap doc: `backlog doc view doc-0005 --plain`. You're updating it, not replacing it. Keep the user's wording unless it's now wrong.
- The board: `backlog milestone list --plain`, `backlog task list --exclude-status Done --plain`, and `backlog draft list --plain`.
- The design docs: `backlog doc list --plain`, and the Decision line of any you're placing.
- `README.md`, the **Not yet** section and "Requirements".
- `git log --oneline -30`, to see what actually shipped.
- `.claude/skills/write-spec/references/oikade-surfaces.md`, for the constraints that drive ordering.
- Anything the user just said about priorities, hardware or frustrations. That outranks everything else.

Reconcile these sources. A milestone whose tasks are all Done is finished. Flag tasks that look shipped in git but aren't Done, design docs with no work item, and ideas in the README that have no draft.

## 2. Decide the order

Order work by what it unlocks, not by how interesting it is. For Oikade, these principles usually decide it, in roughly this priority:

1. **Real use beats new features.** Until Oikade runs the user's actual home on the Pi cluster (HomeKit paired, real Zigbee devices, backups working), problems found there come first. A feature nobody runs teaches nothing.
2. **Protect state that must survive, and the public API.** Work that would force a re-pair, or a rename of CRD fields, capabilities or metrics, gets cheaper the earlier it's done. Ship breaking API cleanups before people commit lots of YAML against them.
3. **Foundations before the things that use them.** For example: MQTT auth before exposing the broker more widely, a second driver before abstractions that assume many drivers, and room-targeted actions before scenes that would use them.
4. **Retire risk early.** Put the item with the biggest unknown (HAP behaviour, a new protocol, Pi resource limits) early, as a small spike if necessary.
5. **Keep milestones small.** Each milestone should be finishable in a few weeks of hobby time and leave the home working. Three to six items is about right.

When two orders are both reasonable, pick one and say why in a sentence. The user can swap them.

## 3. Update Backlog

Read `backlog instructions task-execution` before editing tasks.

- **Milestones.** Create one per planned milestone with the fixed description from `tracking.md`. Rename with `backlog milestone rename`. Archive a finished one with `backlog milestone archive`.
- **Membership.** `backlog task edit <id> -m "<name>"` or `backlog draft edit <id> -m "<name>"`. Put the feature's parent task in, not each subtask. Use `--clear-milestone` when something moves out to "later".
- **Order between items** is `--dep`, not a table. Add a dependency where one item really needs another, including across features.
- **New items.** Something that needs a design is a draft (`backlog draft create`, then `draft edit -m`). Recommend design-feature for it. Something that's ready to build, such as ops work, is a task. Read `backlog instructions task-creation` first.
- **Later ideas** are drafts with no milestone. Put the one-line reason it's not sooner in the draft's description.

## 4. Update the Roadmap doc

Edit it as `tracking.md` describes. Keep this shape:

```markdown
<One paragraph: what this doc holds and the backlog commands for everything else.>

## Direction
Two or three sentences on where Oikade is heading and what it is not.

## Milestones, in order

### 1. <milestone name, exactly as in Backlog>
**Goal:** one sentence.
**Done when:** observable exit criteria, things you could check on the Pi or in Grafana.
**Why here:** one or two sentences on what it unlocks or depends on.

## Not doing
Things we've decided against, and why: for example Home Assistant integration, or HA runtime replicas. This stops them being re-litigated.
```

Guidance:

- **Exit criteria must be observable.** Write "the hallway motion light runs from the Pi for a week with no manual restarts", not "automations stable". Write "`kubectl delete ns oikade-system` followed by a GitOps restore re-pairs nothing", not "backups done".
- **No item lists, sizes or statuses in the doc.** They're on the board. Name a task or draft only when the reason for the order depends on it.
- **Drop finished milestones from the doc** once they're archived in Backlog. Git history and the archived tasks keep the record.
- **Use the user's framing.** If they call it "v0.2" or "Pi bring-up", use those names, the same in Backlog and the doc.

If the README's **Not yet** section disagrees with the plan, mention it to the user rather than rewriting it.

## What to hand back

Give:

- the milestones in order, each with its goal and its open tasks and drafts
- what changed: milestones or items added, moved, finished or dropped
- drafts that need a design before they can start
- one recommended next action, for example "run design-feature on DRAFT-0002", or "start TASK-0001.02"

Don't commit unless asked.
