---
name: design
description: Decide how to build an Oikade feature before anyone writes code, and get the user's decision on anything expensive to reverse. First decide whether a design doc is needed at all (public API, state that must survive, component boundaries, a new kind, driver or dependency). If it is, compare two or three real options with their cost of reversing, write a one-page doc in docs/design/, and open a PR containing only that doc for the user to review; merging it is acceptance. Once it's merged, turn it into a GitHub parent issue with PR-sized sub-issues. Use this whenever the user floats an idea or asks "how should we…", "what's the best way to…", "I want Oikade to…", "thinking about adding…", "spec out", "flesh out", "plan the implementation of", or wants to talk through scenes, presence, a new driver, MQTT auth, backups or any other change that needs a decision, even if they never say "design". If the request is only a new capability on an existing driver, use add-capability instead.
---

# Designing an Oikade feature

The user is an SRE building their own home on Oikade. A design that reads well but quietly breaks an invariant can cost them a re-pair of every HomeKit accessory or Zigbee device later. This skill exists to make that kind of consequence visible, and to make sure **the user** makes the call, not you.

How the workflow fits together is in `docs/design/0008-planning-workflow.md`. Read `references/surfaces.md` for the planes, the public API surfaces, the state that must survive, the component boundaries, the hardware and the test layers. Then read just enough of the code the feature touches to cite real files and types.

## 1. Frame the problem

Restate the request as a problem with a concrete scenario from the user's home ("when the last person leaves, everything downstairs turns off"), not as a solution ("add a Presence CRD"). Work out who notices the problem and when, and what "done" looks like in terms the user could observe.

If anything important is unclear, ask, but no more than three sharp questions at a time. Keep platform designs vendor-neutral: use "a device whose best library is in Python", not a specific product.

## 2. Does it need a design doc?

It does if the work changes anything expensive to reverse:

- **public API:** CRD fields and enums, capability names and semantics, `oikade_*` metric names and labels, the CEL environment, the plugin contract, HomeKit IIDs
- **state that must survive:** identity, pairings, network keys, AIDs
- **component boundaries:** manager, runtime and hap
- **a new kind, a new driver, or a new runtime dependency**
- **anything else** you can't cheaply undo

**If it doesn't need one,** skip to step 5 and just create the issues. Say in one line why no design was needed.

**If you're unsure,** ask the user, or create the issue with the `needs-design` label and ask there.

## 3. Options and recommendation

Give two or three options that differ in a real way: where the feature lives, how much Oikade owns, or how much scope it covers. Include "do less" when that's credible.

For each option, give its pros and cons in Oikade terms, plus its **cost of reversing**: what breaks for the user if we change our minds in six months. When options are otherwise close, the reversal cost usually decides.

Then recommend one option and say what it gives up. Sketch the public names it introduces, and show the YAML the user would commit. Seeing the YAML is often where a design falls apart.

Constraints:

- Never propose Home Assistant as a driver.
- Plugins are Go first, in this repo (see 0006).
- When an earlier design constrains this one, check who decided it. An agent-written doc isn't the user's decision unless it was merged after their review. If it wasn't, say so.

## 4. Write the doc and open the review PR

- **The file:** `docs/design/NNNN-slug.md`. NNNN is one more than the highest number in `docs/design/` and in any open design PR (`gh pr list --search "docs(design)"`).
- **The structure:** a `# NNNN: Title` heading, then Problem, Options, Decision (what you recommend), Consequences (the public names it adds, state, what breaks, the upgrade path) and Open questions.
- **The length:** about one page, so a reviewer can disagree with it in five minutes. Add an API sketch only when the names are the decision.
- **No status line.** A doc on `main` is accepted and an open PR is proposed. Never write "accepted".

Create a branch from `origin/main` in **your own git worktree**, because other sessions share the main checkout. Commit **only that file**, staged by its explicit path. Then open the PR:

```sh
gh pr create --title "docs(design): <title> (NNNN)" --body "<the problem in two lines, the recommendation, and the open questions>"
```

If an issue already exists for this work, link it in the PR body and add the `needs-design` label to the issue.

Then stop. The user reviews the doc in the PR, and you update it from their comments. **Merging the PR is their acceptance.** Don't create the build issues until it's merged.

## 5. Break the work into issues

Once the design is merged, or wasn't needed:

- **The parent issue** is the feature. Reuse an existing issue if there is one (`gh issue list --search "<topic>"`), and remove its `needs-design` label.
- **Sub-issues** are one per PR-sized step. Each step must leave `CGO_ENABLED=0 make test` green and deliver something reviewable: the API and generated code, then the controller, then the runtime, then HAP, then the docs. Put the risky or uncertain step first.
- **An issue body** has:
  - **Why:** one or two lines.
  - **Scope:** the files, commands (`kubebuilder create …`, `make manifests generate`) and risks.
  - **Acceptance:** a checklist of observable outcomes, each naming the test layer that proves it (unit, integration, envtest, Kind e2e or Pi). Use the `pi` label when a step can only be verified on the Pi.
  - **The design link:** `docs/design/NNNN-slug.md`.

Detail like exact types, failure modes and test names belongs in the sub-issue it affects, not in the design doc.

To create and link them:

```sh
gh issue create -t "<title>" -F body.md                 # prints the URL
id=$(gh api repos/{owner}/{repo}/issues/<child> --jq .id)
gh api -X POST repos/{owner}/{repo}/issues/<parent>/sub_issues -F sub_issue_id=$id
gh api -X POST repos/{owner}/{repo}/issues/<child>/dependencies/blocked_by -F issue_id=<blocker id>
gh project item-add 5 --owner harrymitchinson --url <issue url>
```

Don't assign a milestone unless the user asks: that's the `plan` skill's job.

## What to hand back

- the problem in one line
- the recommended option and its main trade-off
- the public names it introduces
- the open questions
- the design PR, or the issue numbers if no design was needed
