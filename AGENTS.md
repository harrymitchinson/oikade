# oikade - AI Agent Guide

## Project Structure

```
cmd/oikadectl/*                oikadectl: every command you run, from a laptop or CI
cmd/oikade-host/*              oikade-host: the host, plus client add
framework/*                    What a home is written with (public API)
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

The contract is `docs/plugin-contract.md`. Change it, `contract/v1`, the decoder in `internal/driver/plugin` and the conformance suite in `internal/conformance` (run by `test/conformance` and `cmd/oikade-conformance`) together. `contract/v1`, `sdk/plugin` and `framework` are public API: their exported signatures must not mention `internal/` types. `cmd/oikade-conformance`'s name and flags are public API too, because plugin repos run it in CI: removing or renaming one is a breaking change (`feat!:` with a `BREAKING CHANGE:` footer). An SDK for another language may live here only if it's generated from the contract (OIK-62).

**First-party plugins live in `plugins/`, and anyone else's in its own repo** (`oikade-plugin-<name>`, started from the [`oikade-plugin-template`](https://github.com/harrymitchinson/oikade-plugin-template) GitHub template), with its own `go.mod`, images and releases. `test/fixtures/simswitch` exists only so the SDK and the conformance suite are tested end to end.

## Before Opening a PR

Run the `code-review` skill (`/code-review`) on the branch and fix or explain each finding before opening the PR.

## Testing & Development

```bash
make test              # Unit and integration tests, with a real host and simulated devices
make test-conformance  # The plugin contract
make test-journeys     # From oikadectl init to a running host, and chip-tool (Docker, linux/arm64)
```

Tests are plain Go `testing`. A simulated home on a fake clock is `internal/simhome`, and a real host with simulated devices is `test/host`.

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

## Publishing

```bash
export IMG=<registry>/oikade:<version>
make docker-build docker-push IMG=$IMG   # oikade-host's image, for this machine's platform
make release-snapshot                    # everything a release ships, into dist/, publishing nothing
```

Releases are GoReleaser (`.goreleaser.yaml`), run by `.github/workflows/release.yml` on release-please's tag: `oikadectl-<os>-<arch>.tar.gz` for darwin and linux, `oikade-host-linux-<arch>.tar.gz`, `checksums.txt`, and the image for linux/arm64 and linux/amd64. The Dockerfile compiles nothing: it copies a prebuilt `oikade-host` from `<os>/<arch>/` in its context.

## Planning

How planning works is in `docs/planning.md`. In short:

- **The plan is in Linear,** team `OIK` (workspace `oikade`). GitHub has no issues: PRs, review and CI stay there. Old `#N` references in PRs, commits and docs point at deleted GitHub issues.
- **Ideas are issues in Triage.** **A feature is a top-level issue,** and each of its steps is a sub-issue, one PR each. "Blocked by" orders features.
- **Use `hack/plan`** (`hack/plan help`) for ideas, ready work, claims, steps and every change to issues, statuses, labels and projects. It acts in Linear as the Oikade Agent app and on GitHub as `oikade-agent[bot]` (`docs/planning.md`). Read Linear with its MCP tools. `.claude/settings.json` denies the GitHub writes `hack/plan` makes as the bot, raw API calls, the deletes that can't be undone, and the Linear MCP tools that change issues or merge PRs. Other `gh` writes act as the user, so prefer `hack/plan` wherever it has a command. Bookkeeping never needs a PR.
- **Designs are reviewed in the feature's issue,** in the status Design Review, and only for changes that are expensive to reverse: public API, state that must survive, component boundaries, or a new driver or dependency. **The user decides by moving it out of Design Review to Todo.** `hack/plan` counts that move only if the user made it. Never make it, never write "accepted", and never treat an agent-written design as the user's decision. The issue is the design's only record: nothing is committed for it. The repo's docs say how things work now instead (the README, `docs/architecture.md`, `docs/plugin-contract.md`), and a step that changes how something works updates them in the same PR. Agents may also build a feature that never needed a design (nothing expensive to reverse), or one whose `Design: OIK-M` line names an approved feature whose design it builds.
- **Parallel agents claim before building.** `hack/plan claim OIK-N` takes the branch `claim/OIK-N`, and fails if another agent holds it. Build each step on its own `work/OIK-S` branch, named for the step's sub-issue, in the worktree it creates, and open its PR with `hack/plan pr`. Merge each step yourself with `hack/plan merge` once its checks pass: the user reviews designs, not diffs. Never merge any other way. Nothing waits for the user by default, CI and `hack/plan` changes included. Only a PR or feature the user labelled `needs-review` waits for their Approve (a required check), and the next step stacks on it. Once they approve one, merge it with `hack/plan merge` like the rest. Agents can't change `.claude/settings.json` themselves: propose the change, and the user applies it. `hack/plan digest` lists what agents merged. `hack/plan release OIK-N` frees the claim when you stop. Agents don't run the journeys within a step: `make test` is the bar, and main's journeys are fixed forward (`docs/planning.md`).
- **Releases are Linear projects** (`v0.1.0`, …). Each one's description gives its goal, exit criteria and why it sits where it does, and the version numbers give the order. "Not doing" is in the README.
- **Skills:** `design` (whether a design is needed, options, the issue review, then the steps), `plan` (what's next, ideas, releases, "Not doing"), `work` (claim and build the next step), `add-capability` and `handoff`.
- **Shared checkout.** Other sessions share this checkout and its git index. Work in your own git worktree, and stage explicit paths only. Never `git add` a directory.

