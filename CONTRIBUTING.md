# Contributing

How Oikade is built, tested, versioned and released. It's for anyone changing Oikade. How work is planned and merged is in [docs/planning.md](docs/planning.md), the rules code follows (logging, CLI output, public API) are in [AGENTS.md](AGENTS.md), and how the parts work inside is in [docs/architecture.md](docs/architecture.md).

## Development

```sh
make build              # bin/oikade and bin/oikade-host
make test               # unit and integration tests, with a real host and simulated devices
make lint               # golangci-lint, which make lint-fix runs with fixes
make test-conformance   # builds the simswitch fixture and drives it through the plugin contract and the Go API
make test-race          # the tests in this process, under the race detector
make test-race-binaries # test/host and test/cli, with the race detector in every binary they start
make test-hack          # shellcheck and the tests of the hack/ scripts
make fuzz               # every fuzz target for FUZZTIME (30s) each, one at a time
make test-journeys      # from oikade init to a running host, and pairing chip-tool with it (Docker, linux/arm64 only)
make release-snapshot   # everything a release ships, into dist/, publishing nothing
```

The plugin SDK, the conformance suite and the simswitch fixture are Go, in the same module, so `make test` and `make lint` cover them. `make help` lists every target.

## CI

| Workflow | Runs | On |
|---|---|---|
| Tests (`test.yml`) | `make test`, `go mod tidy -diff`, the race detector, the conformance suite, and an arm64 home's check under emulation | PRs, main, nightly and before a release. `make test-race-binaries` runs only nightly |
| Lint (`lint.yml`) | `make lint-config`, `make lint`, and `make test-hack` | PRs, main, nightly and before a release |
| Journeys (`journeys.yml`) | `make test-journeys` on an arm64 runner, and the image build and `make release-snapshot` | Main after a merge, nightly and before a release. A PR that changes code gets only the image build and the snapshot |
| Fuzz (`fuzz.yml`) | `make fuzz`, keeping a failing input as the `crashers` artifact | Nightly, and a PR that changes `fuzz.yml` or `hack/fuzz` |
| Release (`release.yml`) | release-please, then Tests, Lint and Journeys on the release commit, then GoReleaser | Every push to main |
| Review gate (`review-gate.yml`) | "Approved by the owner", a required check | Every PR ([docs/planning.md](docs/planning.md#the-review-gate)) |

A PR or merge that changes only docs and planning files (Markdown, `docs/`, `.claude/`, `hack/plan`) skips Tests and Lint's Go jobs (`changes.yml`), but not `make test-hack`. `docs/plugin-contract.md` counts as code, because `contract/v1`'s tests check it.

## Versioning

Releases follow [semantic versioning](https://semver.org), and Git tags and image tags use the same version, for example `v0.1.0`. Oikade's public API is the package `oikade` at the module root, the `oikade` and `oikade-host` commands and their `--json`, the plugin contract and SDK, `oikade-conformance`'s name and flags, capability names, metric names and labels, and Matter endpoint IDs.

While the major version is 0, the API is still settling, and versions follow the milestones, which are the releases in the plan ([docs/planning.md](docs/planning.md)):

- Releases on the way to a milestone are pre-releases of it: `v0.1.0-alpha.1`, `v0.1.0-alpha.2`, … Any of them may break the public API, so read the release notes before upgrading.
- Shipping a milestone cuts its version (`v0.1.0`). Releases after it are `v0.2.0-alpha.N`, on the way to the next one.
- No commit moves MINOR or MAJOR on its own, breaking changes included.

`1.0.0` is a deliberate decision, not something a commit triggers.

## Releases

Releases are cut by merging the release PR that [release-please](https://github.com/googleapis/release-please) keeps open on `main`.

1. **Merging it** gives the tag and a GitHub Release with notes.
2. **`release.yml` checks the release commit:** Tests, Lint and Journeys run on it, and nothing is published unless all three pass.
3. **[GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) publishes** `oikade` for darwin and linux and `oikade-host` for linux, each on arm64 and amd64 as a `.tar.gz` (`oikade-<os>-<arch>.tar.gz`, `oikade-host-linux-<arch>.tar.gz`), and `checksums.txt`, onto that Release. It pushes the image `ghcr.io/harrymitchinson/oikade:<tag>` for linux/arm64 and linux/amd64, whose Dockerfile copies the archive's `oikade-host` rather than compiling it.

CI doesn't run on the release PR, because release-please opens it with `GITHUB_TOKEN`. That's fine, since it only changes `CHANGELOG.md` and `.release-please-manifest.json`.

Two settings are made once, by hand:

- release-please needs **Settings → Actions → General → Allow GitHub Actions to create and approve pull requests** turned on.
- The first release creates the ghcr package as private, so set its visibility to public in the package's settings.

### Shipping a milestone

1. Merge a commit whose message ends with a `Release-As: 0.1.0` footer, as its last paragraph. With a squash merge, put it last in the squashed message.
2. Merge the release PR for `v0.1.0`.
3. Before anything else merges, merge a commit with a `Release-As: 0.2.0-alpha.1` footer. Otherwise release-please's prerelease versioning proposes `v0.2.0-alpha` after a feature, or `v0.1.1-alpha` after a fix, rather than the next milestone's first alpha.
