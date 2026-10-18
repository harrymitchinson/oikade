---
name: design-feature
description: Design a new Oikade feature before anyone writes code. Frame the problem, check it against Oikade's control plane, data plane and history split, its public API and the state that must survive, compare two or three real options, recommend one, and record the decision as a Backlog design doc. Use this whenever the user floats an idea or asks "how should we…", "what's the best way to…", "I want Oikade to…", "thinking about adding…", or wants to talk through scenes, presence, a new driver (ESPHome, Matter, Homebridge, Shelly), room-targeted actions, MQTT auth, HA runtime, backups or any other change that needs a decision before implementation, even if they never say "design". If the request is only a new capability on an existing driver, use add-capability instead.
---

# Designing an Oikade feature

This is the first of three planning skills. Each one hands off to the next:

1. **design-feature** (this one) decides *what* to build and *why this way*.
2. **write-spec** turns that decision into something you can implement.
3. **plan-roadmap** decides *when*, relative to everything else.

The user is an SRE building their own home on this, so a design that reads well but quietly breaks one of the invariants below costs them a re-pair of every HomeKit accessory, or every Zigbee device, later. The job here is to make that kind of consequence visible *before* the decision is made.

**Read `.claude/skills/write-spec/references/tracking.md` and `.claude/skills/write-spec/references/oikade-surfaces.md` first.** The first says where everything is recorded. The second lists the planes, the public API surfaces, the state that must survive, the component boundaries and the hardware constraints. Then read the code the feature would touch, just enough to be accurate. Designs that cite real files and types are much more useful than ones written in the abstract.

## 1. Frame the problem

Restate the idea as a problem with a concrete scenario from the user's home: "when the last person leaves, everything downstairs should turn off". Don't restate it as a solution ("add a Presence CRD"). A solution-shaped request often hides a simpler need. Work out:

- Who notices the problem, and when? Is that the user editing Git, someone in the Home app, or someone reading Grafana?
- What does "done" look like, in terms the user could observe?
- Is it small enough to skip design? If it's only a new capability, hand off to `add-capability`. If it's a one-line fix, say so and don't invent process.

If the user is around and something important is unclear, ask, but no more than three sharp questions at once. If they aren't (they said "just go", or you're running unattended), make a reasonable assumption, label it, and keep going. Unanswered questions go in **Open questions**; they shouldn't block you.

## 2. Map it onto Oikade

Go through the surfaces file and note each thing this feature touches. Most features touch three or four. The ones that matter most:

- **Which plane?** Is each new piece of data config (a CRD, reviewed in Git), state (MQTT), or history (metrics and logs)? Anything that changes faster than once every few minutes isn't config, and doesn't belong in `status` either.
- **Which public surfaces change?** That means CRD fields, capability names, metric names and labels, CEL variables, and HomeKit IIDs. New ones are cheap to add now and expensive to rename later, so spend your naming effort here.
- **Does it create or touch state that must survive?** If so, it needs no owner reference, a predictable name, and a way to restore it.
- **Which component owns it?** That means manager, runtime or hap. Respect the boundaries: the manager doesn't talk to MQTT, the runtime is a single replica, and hap is read-only. Also check that HomeKit still works when the runtime is down.
- **Does it fit on a Pi?** Think about arm64 images, memory, and whether it needs L2 or mDNS.

## 3. Generate real options

Give two or three options that are really different. Three variations on one idea don't count. Good axes to vary:

- **Where it lives**: a new CRD, a field on an existing CRD, an engine feature, a HomeKit-side feature, or a driver.
- **How much Oikade owns**: render config for an upstream tool, or reimplement the behaviour.
- **Scope**: the smallest useful version against the general one.

Include "do less" or "don't build it" when it's credible. For example, a HomeKit scene might already solve the problem without Oikade. For each option, give its pros and cons in Oikade terms, and the **cost of reversing** it: how much breaks for the user if we change our minds in six months. When options are otherwise close, that reversal cost is usually what decides between them.

Never propose Home Assistant or HA integrations. The user has ruled it out. Prefer MQTT-native projects, Matter, Homebridge as a driver, or native Go.

## 4. Recommend and decide

Recommend one option and explain why in a few sentences, including what it gives up. Have an opinion: the user can overrule it, but "it depends" is not a design. If the user is present, confirm the decision before you mark it `accepted`. Otherwise leave it `proposed`.

Sketch the main public names now (field names, capability names, metric names) and show a short YAML example of what the user would commit. Seeing the YAML is often where a design falls apart, and it's better to find that here.

## 5. Write it down

The design is a Backlog doc, reviewed in the same PR as everything else under `backlog/`. Follow `tracking.md` for how to create and edit it.

- Search first so you don't duplicate an existing design or work item: `backlog search "<topic>" --plain` and `backlog doc list --plain`. If a draft or task for this already exists (the roadmap often has one waiting for a design), link to it rather than creating another.
- Create the doc: `backlog doc create "<Feature name>" -p specs -t specification --plain`.
- Fill its body from `.claude/skills/write-spec/assets/spec-template.md`. Write everything **above the `---` line**: Problem, Constraints, Options, Decision and Non-goals. Leave the sections below it as headings with a one-line `TODO (write-spec)`, so the handoff is obvious and nobody mistakes it for a finished spec.
- Set the first line to `**Decision:** accepted` or `proposed`, and `**Work:**` to the draft or task ID from step 6.
- Keep it tight. A design is good when a reviewer can disagree with it in five minutes. Aim for one to two screens, not an essay.

If the user explicitly wants to talk it through without writing anything, do the same thinking in chat and offer to write it down at the end.

## 6. Make sure there's a work item

Read `backlog instructions task-creation` first. Every design doc belongs to one draft or task, linked with `--doc`.

- **There's already one:** run `backlog draft edit <id> --doc doc-NNNN` (or `task edit`).
- **There isn't:** create a draft, because the work isn't ready to build until write-spec has broken it into steps:
  `backlog draft create "<Feature name>" -d "<the problem in one or two lines, and why it matters>"`, then `backlog draft edit <id> --doc doc-NNNN`.

Don't promote the draft or create subtasks here. That's write-spec's job, once the design is accepted. Don't assign a milestone either, unless the user asked: that's plan-roadmap's job.

## What to hand back

Give a short summary:

- the problem in one line
- the recommended option and the main trade-off
- the public names it introduces
- the open questions
- the doc ID and the draft or task ID

Then suggest the next step: `write-spec` if the decision is Accepted, or which question to settle first if it isn't. Don't commit unless asked.
