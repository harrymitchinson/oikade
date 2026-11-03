---
name: design
description: Decide how to build an Oikade feature before anyone writes code, and get the user's decision on anything expensive to reverse. First decide whether a design is needed at all (public API, state that must survive, component boundaries, a new kind, driver or dependency). If it is, compare two or three real options with their cost of reversing and write the design into the feature's Linear issue, in Design Review, for the user to review in comments; the user decides by moving it out of Design Review to Todo. Then write the feature's steps as sub-issues, one PR each, with scope and acceptance, so agents can build it with the work skill. Use this whenever the user floats an idea or asks "how should we…", "what's the best way to…", "I want Oikade to…", "thinking about adding…", "spec out", "flesh out", "plan the implementation of", or wants to talk through scenes, presence, a new driver, MQTT auth, backups or any other change that needs a decision, even if they never say "design". If the request is only a new capability on an existing driver, use add-capability instead.
---

# Designing an Oikade feature

The user is an SRE building their own home on Oikade. A design that reads well but quietly breaks an invariant can cost them re-pairing the Matter bridge or every Zigbee device later. This skill exists to make that kind of consequence visible, and to make sure **the user** makes the call, not you.

How the workflow fits together is in `docs/planning.md`. Read `references/surfaces.md` for the planes, the public API surfaces, the state that must survive, the component boundaries, the hardware and the test layers. Then read just enough of the code the feature touches to cite real files and types.

## 1. Frame the problem

Restate the request as a problem with a concrete scenario from the user's home ("when the last person leaves, everything downstairs turns off"), not as a solution ("add a Presence CRD"). Work out who notices the problem and when, and what "done" looks like in terms the user could observe.

If anything important is unclear, ask, but no more than three sharp questions at a time. Keep platform designs vendor-neutral: use "a device whose best library is in Python", not a specific product.

## 2. Does it need a design?

It does if the work changes anything expensive to reverse:

- **public API:** CRD fields and enums, capability names and semantics, `oikade_*` metric names and labels, the CEL environment, the plugin contract, Matter endpoint IDs
- **state that must survive:** the Matter bridge's setup code and fabrics, endpoint IDs, network keys
- **component boundaries:** manager, runtime and matter
- **a new kind, a new driver, or a new runtime dependency**
- **anything else** you can't cheaply undo

It doesn't need its own if it only builds part of a design the user has already approved in another feature, and makes no decision that design doesn't. Then put a `Design: OIK-M` line in the description, naming that feature, and skip to step 5. Agents may build it on that approval. Don't use this to cover a new decision: if anything in the list above isn't settled by OIK-M's design, this feature needs its own review.

**If it doesn't need one,** comment on the issue with one line on why (`hack/plan note OIK-N "…"`), and skip to step 5. Once its steps are written, move it to Todo (`hack/plan state OIK-N Todo`): agents may then build it without the user's approval, and the user vetoes by moving it to Design Review. Typical cases:

- a plugin built on the existing plugin contract, which ships as its own image and touches nothing in core
- CI, release tooling and docs that change no public name
- a bug fix that keeps the public API and stored state as they are

A plugin that needs a contract change, or a new capability in the registry, does need a design.

**If you're unsure,** it needs one: move it to Design Review (`hack/plan state OIK-N "Design Review"`) and ask the user in the issue.

Every change to Linear goes through `hack/plan`, which acts as the Oikade Agent rather than as the user (`docs/planning.md`). Read with the Linear MCP tools; the ones that would change issues are denied.

## 3. Options and recommendation

Give two or three options that differ in a real way: where the feature lives, how much Oikade owns, or how much scope it covers. Include "do less" when that's credible.

For each option, give its pros and cons in Oikade terms, plus its **cost of reversing**: what breaks for the user if we change our minds in six months. When options are otherwise close, the reversal cost usually decides.

Then recommend one option and say what it gives up. Sketch the public names it introduces, and show the YAML the user would commit. Seeing the YAML is often where a design falls apart.

Constraints:

- Never propose Home Assistant as a driver.
- Plugins are Go first, each in its own repo, `oikade-plugin-<name>`, started from `oikade-plugin-template` (OIK-108). Of plugin code, this repo keeps only the contract, the Go SDK, the conformance suite and a test fixture. A design that adds a plugin plans its repo, not a directory here. Drivers, CRDs and the operator stay here.
- When an earlier design constrains this one, check who decided it. An agent-written doc is the user's decision only if the user moved its issue out of Design Review (`hack/plan show` says so), was approved on GitHub before the move to Linear, or was merged from a design PR they reviewed. Otherwise, say so.

## 4. Write the design into the issue

The design lives in the feature's issue, where the user reviews it in comments. The issue is its only record of why: nothing is committed for it, and code or docs that cite it link the issue. How things work now is in the repo's docs, which the steps keep up to date. Designs from before the move to Linear are in OIK-5 and the Done issues OIK-60 to OIK-64.

- **The issue:** reuse one if it exists (search with the Linear MCP tools). If the work is an idea in Triage, promote it with `hack/plan promote OIK-N "Design Review"`. Otherwise write the description to a file in your scratchpad and create it with `hack/plan issue "<title>" <file> --state "Design Review"`.
- **The description:** `## Why`, then `## Design` with Problem, Options (each with its cost of reversing), Recommendation, Consequences (the public names it adds, state, what breaks, the upgrade path) and Open questions. To change it later, edit your file and run `hack/plan edit OIK-N <file>`.
- **The length:** about one page, so the user can disagree with it in five minutes. Add an API sketch only when the names are the decision.
- **If the user asks to review it in a Claude Doc or a Linear document,** write it there instead, and put the link in the issue.

Then stop, and tell the user it's ready for review. Update the design from their comments: edit the description (`hack/plan edit`), and reply to say what changed (`hack/plan note`). **Only the user decides.** They do it by moving the issue out of Design Review to Todo. Never move it yourself, and never write "accepted", however clear the discussion looks. Moving it back to Backlog means "not now".

## 5. Write the steps

Once the user has approved the design, or if it didn't need one of its own:

- **Steps are one PR each,** and an agent running the work skill builds them in order, stacking each on the last until the user reviews them. Keep each one reviewable on its own. Each must leave `CGO_ENABLED=0 make test` green and deliver something reviewable: the API and generated code, then the controller, then the runtime, then the Matter mapping, then the docs. Put the risky or uncertain step first.
- **Each step updates the docs it changes,** in the same PR: the README for what users write and see, `docs/architecture.md` for the internals, `docs/plugin-contract.md` for the contract. Say which in the step's scope. The docs describe the present and link the feature for why; they never copy the design.
- **Each step is a sub-issue,** created in order with `hack/plan issue "<step>" <file> --parent OIK-N`, using the layout in the plan skill: its Scope and Acceptance. Acceptance is plain bullets of observable outcomes, each naming the test layer that proves it (unit, integration, envtest, Kind e2e or manual). A manual line reads `(manual, in the Manual testing doc)`: add its check, with a pass condition, to the Linear document "Manual testing" under a heading for the feature, so every manual check is in one place. Detail like exact types, failure modes and test names goes in the step it affects.
- **Separate features** if parts of the work don't depend on each other, so agents can build them in parallel. Link them with "blocked by" only where one really needs the other (the plan skill shows how).
- **If the user hasn't approved it yet,** you can draft the steps, but the work skill won't touch the feature until the user moves it out of Design Review.

Don't assign a release unless the user asks: that's the plan skill's job.

## What to hand back

- the problem in one line
- the recommended option and its main trade-off
- the public names it introduces
- the open questions
- the issue ID, and whether it's waiting on the user's review or ready for the work skill (approved, no design needed, or `Design: OIK-M`)
