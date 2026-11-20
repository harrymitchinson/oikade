# oikade - AI Agent Guide

## Project Structure

```
cmd/oikade/*                   oikade: every command you run, from a laptop or CI
cmd/oikade-host/*              oikade-host: the host, plus add client/NAME
*.go, hometest/*               Package oikade: what a home is written with (public API)
internal/host/*                The host
internal/engine/*              Holds, moments, arbitration, the day planner
internal/matter/*              Oikade's own Matter stack
internal/matterbridge/*        Devices as Matter endpoints
config/grafana/*               The "Oikade home" dashboard
Makefile                       Build/test commands
contract/v1/*                  The oikade/v1 plugin contract as Go types (public API)
sdk/plugin/*                   The Go plugin SDK (public API)
cmd/oikade-conformance/*       The conformance checker plugin repos run (public API)
plugins/*                      First-party plugins
test/host/*                    A real host, with simulated devices
test/journeys/*                A person's journeys through Oikade, on main and nightly
test/fixtures/simswitch/*      Simulated-relay plugin the conformance suite drives
```

## After Making Changes

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run the tests
```

**After editing the plugin contract, the SDK or the fixture** (`contract/v1`, `sdk/plugin`, `internal/conformance`, `test/fixtures/simswitch`), as well as the Go steps above:
```
make test-conformance    # Drive test/fixtures/simswitch through the oikade/v1 contract
```

The contract is `docs/plugin-contract.md`. Change it, `contract/v1`, the decoder in `internal/driver/plugin` and the conformance suite in `internal/conformance` (run by `test/conformance` and `cmd/oikade-conformance`) together. `contract/v1`, `sdk/plugin` and the package `oikade` at the root are public API: their exported signatures must not mention `internal/` types. `cmd/oikade-conformance`'s name and flags are public API too, because plugin repos run it in CI: removing or renaming one is a breaking change (`feat!:` with a `BREAKING CHANGE:` footer). An SDK for another language may live here only if it's generated from the contract (OIK-62).

**First-party plugins live in `plugins/`, and anyone else's in its own repo** (`oikade-plugin-<name>`), with its own `go.mod`, images and releases. Each plugin's README (`plugins/<name>/README.md`) is its user docs: setup, keys and what it reports. The main README only lists it. `test/fixtures/simswitch` exists only so the SDK and the conformance suite are tested end to end.

## Before Opening a PR

Run the `code-review` skill (`/code-review`) on the branch and fix or explain each finding before opening the PR.

## Testing & Development

```bash
make test                # Unit and integration tests, with a real host and simulated devices
make test-conformance    # The plugin contract
make test-race           # The tests in this process, under the race detector
make test-race-binaries  # test/host and test/cli, with the race detector in every binary (nightly in CI)
make fuzz                # Every fuzz target for 30 s (FUZZTIME), as the nightly Fuzz workflow does
make cover-binaries      # Coverage of the binaries test/host and test/cli run
make test-journeys       # From oikade init to a running host, and chip-tool (Docker, linux/arm64)
```

Tests are plain Go `testing`, with no assertion library. What more than one package's tests need, waiting for a condition, a time zone, hex and a log to read, is `internal/testkit`. A simulated home on a fake clock is `internal/simhome`, and a real host with simulated devices is `test/host`.

## Implementation rules

- **Everything that must survive lives in the host,** in `internal/store`, not in your process.
- **The host only ever crashes safe:** check that a write reaches disk before anything depends on it.
- **Structured logging:** a `*slog.Logger`, with key-value pairs.

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Host could not start Plugin"`) or omitted (`"Could not start Plugin"`)
- Past tense: `"Could not delete Device"` not `"Cannot delete Device"`
- Specify object type: `"Deleted Device"` not `"Deleted"`
- Balanced key-value pairs
- Errors under the key `err`, and a component's name under `logger`, or `part` within Matter (slog can't join names)

```go
log.Info("Started host")
log.Info("Added bridged device", "device", name)
log.Error("Failed to start Plugin", "err", err, "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### CLI output

Every `oikade` and `oikade-host` command is not verbose, but never leaves you wondering (OIK-218):

- **Anything that can take over a second says what it's doing as it starts, on stderr:** `Fetching Oikade v0.2.0`, then the result, rather than five silent seconds.
- **Say what happened, not what was asked:** `device/towel-rail/on is false, not true: no line applies, so its Default wins`, not `Set device/towel-rail/on to true`.
- **An empty answer says it's empty, and what would fill it:** `No secrets. oikade set secret/NAME adds one.`, not a bare header.
- **A long-running command says it has started:** `Following home's events`, so a quiet house doesn't look hung.
- **Never pass on another tool's output that asks for a step already done:** `oikade init` hides Go's `go mod tidy` hint, which it has run, and shows Go's output only when Go fails.
- **`--json` and stdout stay clean for scripts:** progress and hints go to stderr, and `--json` keeps its shape when empty (`[]`). A verb that changes something prints `{"ok": true, "ref": …}` with what it did, or `"ok": false` and its `error` (`changedJSON`), and `forget` and `remove`, which take several, `refs`.
- **One grammar** (OIK-355): `oikade VERB [REF...] [--flags]`, each thing named by its ref, as every output prints it. Which kinds a verb takes is in `cmd/oikade/kinds.go`, which `oikade help KIND` prints, and `TestEveryVerbExitsAsTheGrammarSays` holds each verb to it: exit 2 for a command typed wrong, such as a kind it doesn't take, and 1 for the rest. A word from another tool or a command that's gone is in help.go's `instead`, naming the one to use, and `TestNothingNamesACommandThatsGone` keeps the docs to the commands there are.

#### The look

Human output shares one look (OIK-331), printed with `internal/cli`'s `Printer` (`cli.NewPrinter(os.Stdout)`, one per stream), never raw ANSI or `text/tabwriter`:

- **Plain text first.** Every line reads right without colour, piped, and in a log.
- **Colour and glyphs only on a terminal,** never with `NO_COLOR` set or `TERM=dumb`, and few of them: `Bold` for a table's header and a section's title, `Dim` for what's secondary (refs, hints, "not in your code"), `Done` (a green `✓`) for something done, `Warn` (a yellow `!`) for a warning, and `Fail` (a red `✗` and `Error:`) for a failure.
- **Fit the terminal.** A `cli.Table` keeps to a terminal's width (`COLUMNS`, or 80, when it doesn't say), and a pipe gets it in full, for grep: columns go by their `Drop` and then `Shrink` ones shorten, and `--wide` (or `--all` where it exists) shows the rest. No `-` for an empty cell: leave it blank.
- **Say it once.** A hint that applies to many rows is one `Hint` after the table, naming the command that says more.
- **Errors are one sentence and a next step:** return a `cli.Errorf`, `cli.Wrap(err, …)` or `cli.UsageErrorf`, with `.WithHint(…)` naming the command or flag that fixes it. `Error: <what went wrong, in the user's words>`, then a dim `Hint: …`. A wrapped cause, such as a Go error chain (`fork/exec …`, `mkdir …`), shows only with `--debug` or `OIKADE_DEBUG=1`. Usage errors exit 2, other failures 1.
- **Times are relative near now** (`cli.Ago`: `8 s ago`, `today 08:45`, `3 days ago`) and absolute past a week, in local time. Versions are short (`cli.ShortVersion`: `f113018`, `v0.2.0-alpha.2`).
- **Help teaches.** `oikade help` groups the commands by what you're doing, `oikade help KIND` lists the commands that take a kind, and `oikade CMD --help` (or `oikade help CMD`) says what one does, its usage, its flags in `--flag VALUE` form and an example. Each command's help is a `cli.Command` in its binary's `help.go`, and its flag set comes from that file's `newFlags`, so `-h` prints that help and a bad flag prints `Error:` and that command's usage alone. A new command or flag needs its help there: a flag's value is named in the usage line (`--module DIR`) or in `Values`. An unknown command names the one it's likely a slip for.
- **Golden tests** (`testkit.Golden`, `testdata/*.golden`, rewritten with `go test -update`) show what a command prints, plain and styled (`cli.Plain`, `cli.Styled`), at 80 and 120 columns.
- **No new dependency:** ANSI by hand beside `golang.org/x/term`.

## Publishing

```bash
export IMG=<registry>/oikade:<version>
make docker-build docker-push IMG=$IMG   # oikade-host's image, for this machine's platform
make release-snapshot                    # everything a release ships, into dist/, publishing nothing
```

Releases are GoReleaser (`.goreleaser.yaml`), run by `.github/workflows/release.yml` on release-please's tag: `oikade-<os>-<arch>.tar.gz` for darwin and linux, `oikade-host-linux-<arch>.tar.gz`, `checksums.txt`, and the image for linux/arm64 and linux/amd64. The Dockerfile compiles nothing: it copies a prebuilt `oikade-host` from `<os>/<arch>/` in its context.

## Planning

How planning works is in `docs/planning.md`. In short:

- **The plan is in `plan/`,** a folder in the main checkout that is its own git repository: a folder per status, a folder per issue in it (`plan/*/OIK-N-*/issue.md`, its comments beside it), releases in `plan/releases/` (`docs/planning.md`). Read it with your file tools. GitHub has no issues: PRs, review and CI stay there. Old `#N` references in PRs, commits and docs point at deleted GitHub issues, and `OIK-N` names an issue in `plan/`.
- **Ideas that need the user's decision or input are issues in Triage.** A bug or follow-up that needs no design goes straight to Todo, with the Bug label for a bug, so it never waits for the user. **A feature is a top-level issue,** and its steps are the checklist lines under its `## Steps`, one PR each (a sub-issue only for a step that needs its own discussion or another repo, which keeps a feature's steps in one place). "Blocked by" orders features.
- **Use `hack/plan`** (`hack/plan help`) for ideas, ready work, claims, steps and every change to issues, statuses, labels and releases: never edit `plan/` by hand. It writes to the plan as the Oikade Agent, one commit a change, and acts on GitHub as `oikade-agent[bot]` (`docs/planning.md`). `.claude/settings.json` denies the GitHub writes `hack/plan` makes as the bot, raw API calls, the deletes that can't be undone, and `hack/plan approve`, which is the user's. Other `gh` writes act as the user, so prefer `hack/plan` wherever it has a command. Bookkeeping never needs a PR.
- **Designs are reviewed in the feature's issue,** in the status Design Review, and only for changes that are expensive to reverse: public API, state that must survive, component boundaries, or a new driver or dependency. **The user decides by moving it out of Design Review to Todo,** with `hack/plan approve` or by moving its folder by hand. `hack/plan` counts that move only if the user made it. Never make it, never write "accepted", and never treat an agent-written design as the user's decision. The issue is the design's only record: nothing is committed for it. The repo's docs say how things work now instead (the README, which stays lean, the guides in `docs/`, each plugin's README in `plugins/<name>`, `docs/architecture.md`, `docs/plugin-contract.md`), and a step that changes how something works updates them in the same PR. Agents may also build a feature that never needed a design (nothing expensive to reverse), or one whose `Design: OIK-M` line names an approved feature whose design it builds.
- **Parallel agents claim before building.** `hack/plan claim OIK-N` takes the branch `claim/OIK-N`, and fails if another agent holds it. Build each step on its own branch (`work/OIKN-stepk`, or `work/OIK-S` for a sub-issue), in the worktree it creates, and open its PR with `hack/plan pr`. Merge each step yourself with `hack/plan merge` once its checks pass: the user reviews designs, not diffs. Never merge any other way. Nothing waits for the user by default, CI and `hack/plan` changes included. Only a PR or feature the user labelled `needs-review` waits for their Approve (a required check), and the next step stacks on it. Once they approve one, merge it with `hack/plan merge` like the rest. Agents can't change `.claude/settings.json` themselves: propose the change, and the user applies it. `hack/plan digest` lists what agents merged. `hack/plan release OIK-N` frees the claim when you stop. Agents don't run the journeys within a step: `make test` is the bar, and main's journeys are fixed forward (`docs/planning.md`).
- **Releases are files in `plan/releases/`** (`v0.1.0`, …). Each one's description gives its goal, exit criteria and why it sits where it does, and the version numbers give the order. "Not doing" is in the README.
- **Skills:** `design` (whether a design is needed, options, the issue review, then the steps), `plan` (what's next, ideas, releases, "Not doing"), `work` (claim and build the next step) and `handoff`. A new capability is named by the guide in `internal/capability`'s package doc.
- **Shared checkout.** Other sessions share this checkout and its git index. Work in your own git worktree, and stage explicit paths only. Never `git add` a directory.

