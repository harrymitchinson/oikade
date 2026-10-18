---
name: write-spec
description: Write an implementable spec for an Oikade feature in docs/specs/NNNN-slug.md, covering the exact CRD changes with kubebuilder markers, behaviour and failure modes, state and upgrade, metrics and conditions, security, a test plan mapped to unit, integration, envtest, Kind e2e and Pi testing, the docs to update, and an ordered PR-sized implementation plan. Use this whenever the user asks to "spec out", "write up", "flesh out", "detail" or "plan the implementation of" a feature, wants an RFC or design doc turned into something buildable, or hands over a decided design, a GitHub issue or a README "Not yet" item and asks what exactly to build. If the approach hasn't been decided yet, run design-feature first.
---

# Writing an Oikade spec

This is the second of three planning skills (design-feature → **write-spec** → plan-roadmap). A spec turns a decision into something another session, or the user next month, can implement without re-deciding anything. Its readers are the implementer and the reviewer, and they want different things:

- the implementer needs exact names, types, files and an order of work
- the reviewer needs to see what breaks, what persists and how we'll know it works

**Read these first:**

- `references/oikade-surfaces.md` covers the invariants, public surfaces, component boundaries and test layers. Every section of the spec comes back to it.
- `assets/spec-template.md` gives the section structure.
- The existing spec in `docs/specs/`, if there is one. Usually design-feature has already written the top half.
- The code the feature touches: the `api/*/v1alpha1/*_types.go` files, the relevant controller or runtime package, and the README sections it changes. Cite real types, functions and files. A spec that names `internal/engine/compile.go` is checkable. One that says "update the engine" isn't.

## 0. Is there a decision?

Check that the problem and chosen approach are settled. Either `docs/specs/NNNN-*.md` has a Decision, or the user stated one clearly.

- **If there is a decision**, keep the top half. Tighten it only if the code reveals it's wrong, and if so, say so: don't silently change it.
- **If there's no decision but the approach is obvious** (one sane option), write a short Problem, Options and Decision yourself, with the rejected alternative in one line, and carry on.
- **If there's no decision and there's a real fork**, stop and suggest `design-feature`, or briefly lay out the fork and ask. Writing a detailed spec for the wrong option is the most expensive thing this skill can do.

If no spec file exists yet, create one. Follow the numbering and index rules: `docs/specs/NNNN-<slug>.md`, the next free number, and a row in `docs/specs/README.md`, creating that README if it's missing.

## 1. API

Write the actual Go type changes: fields, json tags, kubebuilder markers for validation, defaults and enums, and printer columns. Follow the style of the existing `_types.go` files. Then show the YAML a user would commit for the main use case.

- **New kinds**: give the exact `kubebuilder create api --group … --version v1alpha1 --kind …` command, with `--controller` and `--resource` set as appropriate. AGENTS.md forbids hand-creating scaffold files.
- **Prefer additive, optional fields with defaults.** For any change in meaning, write the migration in **State and upgrade**.
- Use `metav1.Condition` for status, and name each condition type and reason.
- Remember that status is coarse. Never put a value that changes every few seconds in it.

## 2. Behaviour

Describe the reconcile or runtime flow in numbered steps, naming the component (manager, runtime or hap) for each. Then cover failure modes explicitly, because this is where specs usually fall short:

- the MQTT broker is unreachable, or comes back
- the runtime restarts mid-action, or the hap pod restarts
- a referenced object is missing, or is deleted later
- a device is offline, or a value is out of range or the wrong type
- concurrent edits, or the same trigger firing twice

For each, say what the user observes: a condition, a metric, a log line, or what the Home app shows.

## 3. State and upgrade

List anything persisted, and where. Answer three scenarios:

- upgrading an existing install
- deleting the CR (what's kept, and why)
- restoring an empty cluster from Git: what comes back by itself, and what needs a backup

New state that must survive follows the established pattern: no owner reference, a predictable name and label, and an entry in README "State that must survive" and in the Pi guide's backup section.

## 4. Observability and security

- **Observability.** Use metric names with the `oikade_` prefix, and keep label sets small, because adding a label later splits series. Write log messages in Kubernetes style. List conditions and events, and the Grafana panel if one is warranted.
- **Security.** Cover the RBAC markers to add, and which Secrets are read and by whom (the hap pod stays read-only and scoped to its own Secrets). Say whether anything new is reachable on the network, given that Mosquitto is anonymous today.

## 5. Test plan

Write a table mapping each behaviour to the **layer** that proves it and a test name or description. The layers are unit, integration with the in-process broker, envtest, Kind e2e, and manual on the Pi. Push each test to the cheapest layer that really proves it. List the manual Pi checks separately, with a concrete pass condition such as "toggle in Home app → `zigbee2mqtt/x/set` seen within 1s", because HomeKit and real hardware can only be verified there.

## 6. Docs

Name the exact sections to update:

- README tables (Kinds, Capabilities, Metrics), "State that must survive", and "Not yet"
- `config/samples`
- `docs/deploy-raspberry-pi.md`

Docs are part of done.

## 7. Implementation plan

Write ordered, PR-sized steps. Each step should:

- leave `CGO_ENABLED=0 make test` green
- deliver something reviewable (API and generated code, then the controller, then the runtime, then HAP, then docs)
- name the files it touches and any CLI commands (`kubebuilder create …`, `make manifests generate`)

Put the risky or uncertain step early, so a bad assumption shows up before it's built on. If a step is really "add capability X", point at the `add-capability` skill instead of re-describing it.

## 8. Finish

- Fill **Open questions** with only the real ones, each saying who answers it and what it blocks. If there are none, write "None".
- Set Status to **Accepted** if the user agreed the design. Otherwise leave it as **Proposed**. Set `Updated:` to today. Update the index row in `docs/specs/README.md`.
- Delete any section that doesn't apply, with one line saying why. Don't leave filler.

## 9. Track the steps in Backlog

Do this only once the spec is **Accepted**. A Proposed spec stays a draft, because its steps may still change. Read `backlog instructions task-creation` first.

- **Find the parent task.** Use the spec header's `Backlog:` line, or `backlog search "<slug>" --plain`. If design-feature left a draft, promote it. If there's nothing, create the parent task with `--doc docs/specs/NNNN-<slug>.md -l spec-NNNN`. Then write its ID into the `Backlog:` line.
- **Create one subtask per step in §7, in order:**
  `backlog task create -p <parent> "Spec NNNN step N: <step title>" -d "<why this step exists>" --ac "…" --doc docs/specs/NNNN-<slug>.md -l spec-NNNN --dep <previous step's task>`
  Take the acceptance criteria from the rows in the test plan table that this step proves. Use observable outcomes, not "write function X".
- **Keep the approach in the spec, not on the task.** Don't pass `--plan`: whoever picks the task up records the plan when they start it, usually "follow step N of spec NNNN" plus anything that has changed since. Steps that are really `add-capability` work get a subtask that points to that skill.
- **If the spec already has subtasks** (you're revising it), edit them to match the new steps. Archive the ones for steps that were dropped. Don't duplicate them.

When the first subtask moves to In Progress, set the spec to **In progress**. When the parent is Done, set it to **Done**. Either way, update its row in `docs/specs/README.md` too.

Length follows the feature. A new optional field might take a page, and a new driver might take several. Don't pad.

## What to hand back

Give:

- the file path
- a three-to-five line summary: the API change, the riskiest behaviour, and the number of implementation steps
- the open questions
- the parent task ID and the subtask IDs, one per step

Suggest `plan-roadmap` to slot it in, or starting implementation with the first subtask. Don't commit unless asked.
