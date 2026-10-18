# Specs

Each spec records one feature's design decision and, once it's accepted, how to build it. A spec moves through **Exploring → Proposed → Accepted → In progress → Done**, or ends as **Rejected** or **Superseded by NNNN**. `design-feature` writes the top half (problem, options, decision) and leaves it Proposed until the decision is confirmed. `write-spec` fills in the rest (API, behaviour, tests, implementation plan) once it's Accepted. `plan-roadmap` places it in a milestone in `docs/ROADMAP.md`. Numbers are never reused. The work is tracked in Backlog.md: a Proposed spec is a draft, and an Accepted spec is a parent task with one subtask per implementation step. They're all labelled `spec-NNNN`, and each spec's `Backlog:` line gives its parent ID.

| # | Title | Status | Roadmap |
|---|---|---|---|
| [0001](0001-room-targeted-actions.md) | Room-targeted automations | Accepted | Pi bring-up |
| [0002](0002-plugin-drivers.md) | Plugin drivers | Accepted | Plugin drivers |
