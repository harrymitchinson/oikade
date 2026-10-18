# Where planning lives

design-feature, write-spec and plan-roadmap all keep their output in Backlog.md, and nowhere else. Each fact has one owner. Don't copy it anywhere else.

| Fact | Owner | How |
|---|---|---|
| A feature's design: problem, options, decision, API, behaviour, test plan | A Backlog doc of type `specification` under `backlog/docs/specs/` | `backlog doc create`, `backlog doc update --content` |
| Whether that design is decided | The doc's first line: `**Decision:** proposed`, `accepted`, `rejected` or `superseded by doc-NNNN` | Rewrite the doc |
| Whether the work is ready to build | Draft (not ready: it needs a design, or isn't committed yet) or task (ready) | `backlog draft create`, `backlog draft promote` |
| The steps to build it, their order and their acceptance criteria | Subtasks of the feature's parent task, chained with `--dep` | `backlog task create -p` |
| What each step covers (files, commands, risks) | That subtask's description, under `Scope:` | `backlog task edit -d` |
| Progress | Task status | Backlog's own workflow |
| Which milestone it's in | The task's or draft's milestone | `-m "<name>"` |
| The milestone order, each milestone's goal, exit criteria and reason for its place, the direction, and "Not doing" | The Roadmap doc, doc-0005 | `backlog doc update doc-0005 --content` |
| Ideas for later | Drafts with no milestone | `backlog draft list` |

So there's no `docs/specs/`, no `docs/ROADMAP.md`, no spec index, no status line that mirrors Backlog, no `spec-NNNN` labels, and no implementation-plan section in a design doc.

## Linking

- A task or draft points at its design with `--doc doc-NNNN` (`backlog task edit`, `backlog draft edit`). Use the bare ID, not the file path, because the file name follows the title.
- The design doc's first line also names the work, `**Work:** TASK-NNNN`, so a reader can get from the doc to the board. Set it when the draft or task is created, and change it when a draft is promoted, because its ID changes.
- Refer to other designs as `doc-NNNN`, and to their steps by task ID (`TASK-0001.03`), never as "step 3 of spec 2".

## Editing a doc

Backlog has no partial edit for docs, and nobody hand-edits files under `backlog/`. So:

1. Write the body to a file in your scratchpad. For an existing doc, start from its current body:
   `backlog doc view doc-NNNN --plain | awk 'n>=2{print} /^---$/{n++}' > <scratch>/doc.md`
2. Edit that file.
3. `backlog doc update doc-NNNN --content "$(cat <scratch>/doc.md)"`, then view the doc again to check it.

The doc's title is its frontmatter `title`, so the body starts at the Decision line, not with a `#` heading.

## Milestones

A milestone's description can't be edited after it's created, so it never holds anything that changes. Create milestones with:
`backlog milestone add "<name>" -d "Tasks and drafts in this milestone. Its goal, exit criteria and place in the order are in doc-0005 (Roadmap)."`

Read `backlog instructions task-creation` before creating tasks or drafts, and `backlog instructions task-execution` before changing them.
