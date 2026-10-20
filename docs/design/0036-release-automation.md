# 0036: Release automation

> Approved in [#36](https://github.com/harrymitchinson/oikade/issues/36), which has the full discussion. Replaces the release section of the unreviewed 0004 draft (PR #46, closed).

## Problem

Releases were cut by hand: a tag pushed from a laptop, then `make docker-buildx` and `make build-installer` run locally. Nothing recorded what changed between two tags, and nothing stopped a tag from pointing at a commit whose image was never built.

Done looks like this: merging a release PR on GitHub produces the next tag, an image at `ghcr.io/harrymitchinson/oikade:<tag>` for `linux/arm64` and `linux/amd64`, and a GitHub Release with `install.yaml` attached and notes listing the commits since the last tag. A `kubectl apply` of that one URL, or Flux, upgrades the cluster.

## Decision

**release-please, with releases between milestones as pre-releases of the next milestone.**

- **Versions follow the milestones.** Releases on the way to v0.1.0 are `v0.1.0-alpha.1`, `v0.1.0-alpha.2`, … Shipping the milestone cuts `v0.1.0`, and releases after it are `v0.2.0-alpha.N`. No commit, breaking ones included, moves MINOR or MAJOR on its own, and `1.0.0` is a deliberate decision. Hash suffixes were ruled out because semver compares pre-release suffixes as text, so they don't sort.
- **release-please config:** one root package, `release-type: go`, `versioning: prerelease`, `prerelease: true`, `prerelease-type: alpha`, `bump-minor-pre-major: true`, `include-component-in-tag: false`. The manifest starts at `0.1.0-alpha.0`, so the first release is `v0.1.0-alpha.1`. Without `bump-minor-pre-major`, a breaking commit makes the prerelease strategy propose `1.0.0-alpha.0`. With it, a breaking commit after `alpha.1` gives `alpha.2` (checked with `release-please release-pr --dry-run`). Each milestone takes two `Release-As` footers: `Release-As: 0.1.0` to cut it, then `Release-As: 0.2.0-alpha.1` to start the next cycle.
- **One workflow, `release.yml`.** Tags created with `GITHUB_TOKEN` don't trigger other workflows, so the job that builds the image and attaches `install.yaml` runs in the same workflow, after release-please creates the Release. It needs `contents: write` and `packages: write`, nothing broader. Those tags don't trigger Tests, Lint or E2E either, so `release.yml` calls them as reusable workflows and publishes only if they pass. The tag and the Release exist before that, so a failed publish leaves a Release without its image or `install.yaml`. That's the trade-off of release-please against a hand-written workflow that tags last.
- **`install.yaml` is attached to the Release only,** never committed to `main`, so no bot pushes to `main`. Flux follows a tag, with a semver range that opts in to pre-releases (`>=0.1.0-0`).
- **The version is baked into the binary** with `-ldflags "-X main.version=<tag>"`, set by the `VERSION` build arg. `manager --version` prints it, and the manager, runtime and hap roles log it on startup. There's no `oikade_build_info` metric.
- **The timezone stays a patch.** The published `install.yaml` runs the runtime with `--timezone=UTC`, and the deploy guide shows a kustomize patch over the Release file to set yours.
- **The user merges the release PR.** It's a release-please PR, not a feature step, so `hack/plan merge` doesn't touch it. It's opened with `GITHUB_TOKEN`, so CI doesn't run on it. That's acceptable because it only changes `CHANGELOG.md` and the manifest.

Rejected: a hand-written `workflow_dispatch` workflow that computes the next version itself. It owns about 60 lines of bump logic, has no release PR to review and no `CHANGELOG.md`. Its main reason was the earlier date versions, which are gone. Patch-only versions (`v0.0.X`, then `v0.1.X`) were also rejected: `v0.1.4` reads as a fix to 0.1.0 when it carries v0.2.0 features.

## Public names

- The workflow `release.yml` ("Release"), `release-please-config.json`, `.release-please-manifest.json` and `CHANGELOG.md`.
- The image `ghcr.io/harrymitchinson/oikade:<tag>`, with no `latest` or `main` tag, because GitOps pins versions.
- The release asset `install.yaml`.
- `manager --version`, and the `version` key in each role's startup log.

## Not in scope

Plugin images, and how plugins are versioned, are [#94](https://github.com/harrymitchinson/oikade/issues/94). Also out: releasing on every merge, per-commit `sha-` images, signing, SBOMs, a Helm chart, s390x and ppc64le images, and the Zigbee2MQTT or Mosquitto images.
