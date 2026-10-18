---
name: plan-roadmap
description: Build or update Oikade's roadmap in docs/ROADMAP.md by sequencing features into milestones with observable exit criteria, dependencies, sizes and an explicit "not doing" list, reconciled against docs/specs, the README's "Not yet" list and recent git history. Use this whenever the user asks "what should I work on next", "what's the plan", "prioritise", "sequence these", "roadmap", "milestones", "what's left for the next release", wants to slot a new idea or spec into the plan, or reports that something shipped and the plan needs updating, even if they never say the word "roadmap".
---

# Planning Oikade's roadmap

This is the third of three planning skills (design-feature → write-spec → **plan-roadmap**). The roadmap answers three questions: what's done, what's next and why, and what we're deliberately not doing. It lives at `docs/ROADMAP.md` so it's reviewed in Git like everything else.

This is one person's home platform, so the roadmap should help them pick the next weekend's work with confidence. A corporate quarterly plan is the wrong model: don't use dates unless the user gives them, and don't build a Gantt chart.

## 1. Gather the current truth

Read these before proposing anything. Roadmaps written from memory drift from reality.

- `docs/ROADMAP.md`, if it exists. You're updating it, not replacing it. Keep the user's own edits and wording unless they're now wrong.
- `docs/specs/README.md` and the spec files: each spec's status and its `Roadmap:` field.
- `README.md`, the **Not yet** list and "Requirements".
- `git log --oneline -30`, to see what actually shipped since the roadmap was last updated.
- `.claude/skills/write-spec/references/oikade-surfaces.md`, for the constraints that drive ordering.
- Anything the user just said about priorities, hardware or frustrations. That outranks everything else.

Reconcile these sources. Mark things that shipped as done. Flag specs whose status disagrees with the roadmap. Notice ideas that appear in the README but in no spec.

## 2. Decide the order

Order work by what it unlocks, not by how interesting it is. For Oikade, these principles usually decide it, in roughly this priority:

1. **Real use beats new features.** Until Oikade runs the user's actual home on the Pi cluster (HomeKit paired, real Zigbee devices, backups working), problems found there come first. A feature nobody runs teaches nothing.
2. **Protect state that must survive, and the public API.** Work that would force a re-pair, or a rename of CRD fields, capabilities or metrics, gets cheaper the earlier it's done. Ship breaking API cleanups before people commit lots of YAML against them.
3. **Foundations before the things that use them.** For example: MQTT auth before exposing the broker more widely, a second driver before abstractions that assume many drivers, and room-targeted actions before scenes that would use them.
4. **Retire risk early.** Put the item with the biggest unknown (HAP behaviour, a new protocol, Pi resource limits) early, as a small spike if necessary.
5. **Keep milestones small.** Each milestone should be finishable in a few weeks of hobby time and leave the home working.

When two orders are both reasonable, pick one and say why in a sentence. The user can swap them.

## 3. Write the roadmap

Use this structure. Adapt the headings if the existing file already has a style.

```markdown
# Oikade roadmap

_Updated YYYY-MM-DD._ How to change this: edit it in a PR. Specs live in [docs/specs](specs/).

## Direction
Two or three sentences on where Oikade is heading and what it is not.

## Now: <milestone name>
**Goal:** one sentence.
**Done when:** observable exit criteria, things you could check on the Pi or in Grafana.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|

## Next: <milestone name>
(same shape)

## Later
Short bullets, roughly ordered, each with a one-line reason it's not sooner.

## Not doing
Things we've decided against, and why: for example Home Assistant integration, or HA runtime replicas. This stops them being re-litigated.

## Done
Milestones or items shipped, newest first, each with a commit or spec reference.
```

Guidance:

- **Exit criteria must be observable.** Write "the hallway motion light runs from the Pi for a week with no manual restarts", not "automations stable". Write "`kubectl delete ns oikade-system` followed by a GitOps restore re-pairs nothing", not "backups done".
- **Sizes are S, M or L.** S is an evening, M a weekend, and L more than that. An L item probably needs a spec, and maybe splitting.
- **Spec column.** Link `specs/NNNN-slug.md` if a spec exists. If an item in Now or Next has no spec and isn't trivial, write "needs design" and recommend running design-feature on it. Items in Later don't need specs.
- **Keep Now to three to six items.** A long Now list is a backlog, not a milestone.
- **Use the user's framing.** If they call it "v0.2" or "Pi bring-up", use those names.

## 4. Keep it consistent

- Update each touched spec's `Roadmap:` field and its row in `docs/specs/README.md`, so that specs and roadmap agree. Create `docs/specs/README.md` if any spec exists without an index.
- If the README's **Not yet** list now disagrees with the roadmap, mention it to the user. Either suggest pointing that section at `docs/ROADMAP.md`, or suggest specific edits. Don't rewrite the README unless asked.

## What to hand back

Give:

- the path, and a short summary of **Now** (the goal and its items) and **Next**
- what changed since the last version: items added, moved, finished or dropped
- items that need a design or spec before they can start
- one recommended next action, for example "run design-feature on scenes", or "start item 1 of spec 0002"

Don't commit unless asked.
