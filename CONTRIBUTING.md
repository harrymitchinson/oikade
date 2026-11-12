# Contributing

How Oikade is versioned and built. How work is planned is in [docs/planning.md](docs/planning.md).

## Versioning

Releases follow [semantic versioning](https://semver.org), and Git tags and image tags use the same version, for example `v0.1.0`. Oikade's public API is `framework`, the `oikade` and `oikade-host` commands and their `--json`, the plugin contract and SDK, capability names, metric names and labels, and Matter endpoint IDs.

While the major version is 0, the API is still settling, and versions follow the milestones, which are the releases in the plan (`docs/planning.md`):

- Releases on the way to a milestone are pre-releases of it: `v0.1.0-alpha.1`, `v0.1.0-alpha.2`, … Any of them may break the public API, so read the release notes before upgrading.
- Shipping a milestone cuts its version (`v0.1.0`). Releases after it are `v0.2.0-alpha.N`, on the way to the next one.
- No commit moves MINOR or MAJOR on its own, breaking changes included.

`1.0.0` is a deliberate decision, not something a commit triggers.

Releases are cut by merging the release PR that [release-please](https://github.com/googleapis/release-please) keeps open on `main`. That gives the tag and a GitHub Release with notes. [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) then adds `oikade` for darwin and linux, `oikade-host` for linux, each on arm64 and amd64 as a `.tar.gz`, and `checksums.txt`, and pushes the image `ghcr.io/harrymitchinson/oikade:<tag>` for arm64 and amd64. `make release-snapshot` builds all of it into `dist/` without publishing, and every PR that could change it runs that. CI doesn't run on the release PR, because it's opened with `GITHUB_TOKEN`. That's fine, since it only changes `CHANGELOG.md` and `.release-please-manifest.json`. release-please needs **Settings → Actions → General → Allow GitHub Actions to create and approve pull requests** turned on. The first release creates the ghcr package as private, so set its visibility to public once, in the package's settings.

To ship a milestone:

1. Merge a commit whose message ends with a `Release-As: 0.1.0` footer, as its last paragraph. With a squash merge, put it last in the squashed message.
2. Merge the release PR for `v0.1.0`.
3. Before anything else merges, merge a commit with a `Release-As: 0.2.0-alpha.1` footer. Otherwise release-please's prerelease versioning proposes `v0.2.0-alpha` after a feature, or `v0.1.1-alpha` after a fix, rather than the next milestone's first alpha.

## Development

```sh
make test               # unit and integration tests, with a real host and simulated devices
make lint
make test-conformance   # builds the simswitch test fixture and drives it through the plugin contract and the Go API
make test-journeys      # from oikade init to a running host, and pairing chip-tool with it (test/journeys/chip-tool, linux/arm64 only)
```

The plugin SDK, the conformance suite and the simswitch fixture are Go, in the same module, so `make test` and `make lint` cover them. How the host, rule engine and Matter bridge work inside is in [docs/architecture.md](docs/architecture.md).
