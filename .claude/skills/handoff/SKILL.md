---
name: handoff
description: Write a dated handoff note to .claude/handoffs/ at the end of a working session so a fresh session can pick up the work cold, covering the goal, what's done and what's left, decisions and dead ends, the next steps and a ready-to-paste resume prompt. Use this whenever the user is wrapping up or about to lose the session, such as "let's stop here", "write a handoff", "I'm out of time", "context is getting full", "save where we are for next time", "we'll continue tomorrow" or "/handoff", even if they never say the word "handoff".
---

# Handoff

A session is ending and the work isn't finished. The next session will be a fresh Claude with none of this conversation, only the repo and whatever you write now. Your job is to put on paper what lives only in the conversation, so the next session doesn't redo work, reopen settled decisions or walk into the same dead ends.

The repo and git history already record *what* changed. The handoff records what they can't: why, what's still in flight, what was tried and dropped, and what to do next.

## Steps

1. **Get the date and pick a filename.** Run `date +%F`. Write to `.claude/handoffs/<date>-<topic>.md` at the repo root, where `<topic>` is a 2–5 word kebab-case slug for the work (for example `2026-10-17-matter-thermostat-mapping.md`). If that file already exists, add `-2`, `-3` and so on. Don't overwrite older handoffs, because they're the history.

2. **Look before you write.** Read the conversation back from the start, not just the last few turns. Early decisions and abandoned approaches are the easiest to lose. Run `git status --short` to see which files are actually modified. That keeps "done" honest, but it isn't a section of the note.

3. **Write the note** using the template below.

4. **Leave git alone.** Don't commit, stash, branch or add the handoff file. The user decides what happens to their working tree. If finished work is still uncommitted, say so under Status. (The work skill pushes its branch before calling this one. That's its choice, not yours.)

5. **Post it on the feature too,** if the work belongs to a feature claimed on a `claim/OIK-N` branch, so the next session can resume from the pushed `work/OIK-S` branch. Another agent picking it up may be on another machine, and the handoff file is local. Post the Status and Next steps sections with `hack/plan note OIK-N "<them>"`.

6. **Reply** with the handoff's path and the resume prompt in a fenced block, so the user can copy it straight into the next session. Keep the reply short. Everything else is in the file.

## Template

```markdown
# <Short title of the work>

_<date> · handoff from a Claude Code session_

## Goal
<What the user is trying to achieve and why, in 2–4 sentences. Include the
user's own constraints and preferences that shaped the work, such as "arm64 only"
or "don't touch the CRD". Name the Linear issue the work belongs to (OIK-N), if
there is one.>

## Status
**Done**
- <Finished items. Say how you know: "tests pass (`make test`, run after the last
  edit)", or "written but not run". Note anything that's done but uncommitted.>

**In progress**
- <Half-finished items: what state they're in and the exact file:line where you stopped.>

**Not started**
- <Remaining scope that was agreed.>

## Decisions
- <Decision>: <why>. <The alternatives considered, if that helps.>

## Dead ends
- <What was tried> → <what happened, with the exact error text if useful> →
  <why it was abandoned>. Don't retry unless <condition>.

## Next steps
1. <Concrete first action: a file, a command, what to check.>
2. ...

## Open questions
- <Things only the user can answer. Leave the section out if there are none.>

## Resume prompt
<A self-contained prompt the user can paste into a new session. See below.>
```

Leave out any section that would be empty except Goal, Status and Next steps, and don't fill a section with "N/A".

## Writing it well

**Write for a cold reader.** The next session doesn't know what "the mapping issue" or "option B" means. Name things fully: file paths (`internal/matterbridge/mapping.go:142`), types, commands, CRD fields, topic names. Quote error messages exactly instead of paraphrasing them. A short code snippet is fine when it saves the reader rediscovering something.

**Separate verified from believed.** "Tests pass" is only useful if it's true now. If the last test run came before later edits, say so. Wrong confidence in a handoff costs more than admitted uncertainty, because the next session acts on it without checking.

**Dead ends are the most valuable part.** They're invisible in the code, since the failed attempt was reverted, and repeating one wastes the most time. For each one, record what you tried, what happened and why you stopped, plus anything that would make it worth revisiting.

**Record the user's decisions as theirs.** If the user chose something or ruled something out, say so ("User wants X, not Y"), so the next session doesn't reopen it.

**Keep it tight.** Aim for something readable in two minutes. This isn't a transcript. Leave out the back-and-forth that led nowhere and isn't a real dead end, and leave out anything obvious from the code.

## The resume prompt

The user pastes this as the first message of the next session. It should:

- Point at the handoff file by its repo-relative path and ask for it to be read first.
- State the goal in one sentence and the immediate next action.
- Stand on its own, since the new session has seen nothing.

Example:

```
Read .claude/handoffs/2026-10-17-matter-thermostat-mapping.md first, as it's the handoff from my last session. We're adding thermostat setpoint support to the Matter bridge. The capability and zigbee2mqtt binding are done; next is the Thermostat cluster mapping in internal/matterbridge/mapping.go, then the unit tests. Check the "Dead ends" section before trying anything with the SystemMode attribute.
```

## When something is missing

If the session's goal is unclear, or it's genuinely ambiguous whether something was finished, write your best understanding and list it under Open questions. Don't stop to quiz the user, who is trying to leave. If the session had no meaningful unfinished work, say so in one line and still write a short note, because the user asked for one.
