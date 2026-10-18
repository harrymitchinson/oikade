**Decision:** proposed · **Work:** <DRAFT-NNNN or TASK-NNNN>

<!-- design-feature fills in everything down to "Non-goals". write-spec fills in the rest.
     Delete any section that really doesn't apply, and say why in one line rather than leaving it empty. -->

## Problem

What hurts today, for whom, and how we'd know it's fixed. Use concrete scenarios from the user's real home.

## Constraints

Which Oikade invariants this touches: the planes, public API, state that must survive, component boundaries, and hardware. See `oikade-surfaces.md`.

## Options

### A. <name>
How it works in two to five sentences. Pros. Cons. What it costs to change our minds later.

### B. <name>
...

## Decision

The option chosen and why, in a few sentences. Say what we're giving up.

## Non-goals

What this deliberately doesn't do, so reviewers don't ask.

---

## API

The exact changes to CRD types, as Go snippets with kubebuilder markers, plus an example YAML a user would commit. New kinds list the `kubebuilder create api` command. Note defaults, validation and printer columns.

## Behaviour

How it works step by step, including failure modes: the broker is down, the runtime is restarting, a device is offline, the referenced object is missing, the input is invalid. Name each condition type and reason that is set.

## State and upgrade

What persists and where. What happens on upgrade from the current version, on deleting the CR, and on a restore from Git with an empty cluster. Note any migration.

## Observability

New or changed metrics with their labels, log messages, conditions and events, and Grafana panels.

## Security

Secrets, RBAC changes (markers), and the network exposure.

## Test plan

| Behaviour | Layer | Test |
|---|---|---|

Name the manual checks on the Pi separately, with what "pass" looks like.

## Docs

The README sections and tables, the samples, the Pi guide, and the "Not yet" list to update.

## Open questions

Each question, who answers it, and what it blocks.

<!-- The implementation steps are not written here. They're the parent task's subtasks. -->
