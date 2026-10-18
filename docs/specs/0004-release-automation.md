# 0004: Release automation

- **Status:** Accepted
- **Roadmap:** unplanned
- **Backlog:** DRAFT-0001
- **Updated:** 2026-10-18

<!-- design-feature fills in everything down to "Decision". write-spec fills in the rest.
     Delete any section that really doesn't apply, and say why in one line rather than leaving it empty. -->

## Problem

Cutting a release is all by hand. The only release so far, `2026.42.1`, was a tag pushed from a laptop. To get it onto the Pi cluster you then run `make docker-buildx` and `make build-installer` locally, push to ghcr and commit `dist/install.yaml` for Flux, following `docs/deploy-raspberry-pi.md` §6 and the upgrade section. Nothing records what changed between two tags, and nothing stops a tag from pointing at a commit whose image was never built.

Scenario: a fix to the HAP mapping merges on Wednesday. The user wants the Pis running it that evening. Today that takes about ten minutes of copied commands, it depends on whose laptop has buildx set up, and there's no list of what else went out with it.

Done looks like this: merging a release PR on GitHub produces the next semver tag, a `linux/arm64` image at `ghcr.io/harrymitchinson/oikade:<version>`, and a GitHub Release with `install.yaml` attached and notes listing the commits since the last tag. Flux, or a `kubectl apply` of that one URL, upgrades the cluster.

The request was "implement release-please". The underlying need is to stop releasing by hand. Release-please collided with the date-based `YEAR.WEEK.BUILD` scheme in use at the time, so this design first proposed a small CalVer workflow (C). The user then switched Oikade to semver, which makes release-please the natural fit (B).

## Constraints

- **Version format is a public surface.** Tags, image tags and anything Flux image automation matches on use semver, `vMAJOR.MINOR.PATCH` (README "Versioning"). While the major version is 0, a breaking change bumps MINOR, and features and fixes bump PATCH. The first semver release is `v0.1.0`. The old date tag `2026.42.1` sorts above every `0.x` under a Flux `semver` policy, so it has to be retired (the tag deleted, or the policy bounded to `<1.0.0`) before `v0.1.0` ships.
- **No Oikade runtime surfaces change.** There are no CRD, capability, metric, CEL, AID/IID or state-that-must-survive changes. The image is the one binary used by all three roles (manager, runtime, hap), so a single image per release is enough.
- **Hardware.** The Pis are arm64. The Dockerfile already cross-compiles with `CGO_ENABLED=0` and `TARGETARCH`, so a buildx build on an amd64 runner doesn't need QEMU for the Go stage. amd64 is still worth shipping for Kind. The Makefile's default `PLATFORMS` also includes s390x and ppc64le, which aren't needed.
- **CI conventions.** The existing workflows (`.github/workflows/*.yml`) pin actions by SHA, default to `permissions: {}`, grant per-job permissions and set `persist-credentials: false`. Releasing needs `contents: write` (for the tag and Release) and `packages: write` (for ghcr), and nothing broader.
- **The repo is private**, so the ghcr package is private by default. The Pi guide already covers `imagePullSecret`.
- **Tags created with `GITHUB_TOKEN` don't trigger other workflows.** A "build on tag push" workflow therefore won't fire for tags that a workflow creates. The build has to run in the same workflow that tags, or the tag has to be pushed by a person or a PAT.

## Options

### A. Release-please, with CalVer forced through `Release-As`
*Superseded by the switch to semver.*

Release-please keeps a release PR open, with a version and a CHANGELOG.md, and updates it on every push to `main`. Merging the PR creates the tag and Release, and later jobs in the same workflow build the image and attach `install.yaml`. Release-please's versioning strategies are all semver bumps ([docs](https://github.com/OrKoN/release-please/blob/main/docs/customizing.md)). From `2026.42.1` it would propose `2026.42.2` or `2026.43.0` depending on commit types, never "this week, next build". The only CalVer hook is a `Release-As: 2026.43.1` commit footer or a `release-as` value in `release-please-config.json`. Both have to be committed to `main` before each release. A custom strategy class means running release-please as a library instead of using the action.
**Pros:** a reviewable release PR and a CHANGELOG.md in the repo, and conventional commits are already in use. **Cons:** a bot commit or a hand-written footer for every release, and the version is decided when the PR updates, not when it merges. A PR opened in week 43 and merged in week 44 carries the wrong version unless someone pushes again. **Reversal cost:** low. Delete the config and the workflow. CHANGELOG.md stays as history.

### B. Release-please as designed, and switch to semver
Adopt release-please unmodified (`release-type: go`), restarting at `v0.1.0`. Set `bump-minor-pre-major` and `bump-patch-for-minor-pre-major` so that, below 1.0, a `!` or `BREAKING CHANGE` commit bumps MINOR and `feat`/`fix` bump PATCH. Merging the release PR creates the tag and Release. Later jobs in the same workflow then build the image and attach `install.yaml`, which avoids the `GITHUB_TOKEN` tag-trigger limitation. **Pros:** the least custom code and the most standard setup. Conventional commits are already in use. The version says whether an upgrade is breaking, and there's a reviewable release PR plus a CHANGELOG.md. **Cons:** someone has to judge bump size, through commit types. The date is no longer readable from the version. A bot keeps a release PR open. **Reversal cost:** low now, because only one date tag exists and only the user deploys it. It rises once others pin `0.x` versions.

### C. A small CalVer release workflow, without release-please
*Superseded by the switch to semver. This was the original proposal.*

`hack/next-version.sh` reads `git tag`. It prints `$(date +%G.%-V).<max BUILD for that week + 1>`, or `.1` for a new week. A `workflow_dispatch` workflow, `release.yml`, runs under a `concurrency: release` group so that two runs can't pick the same number. It runs the script on `main`, then `docker buildx` for `linux/arm64,linux/amd64` to ghcr, then `make build-installer`. Then it creates the tag and a GitHub Release with `install.yaml` attached. The notes come from GitHub's generated release notes (or `git log --oneline <prev>..HEAD` grouped by conventional-commit prefix). The tag is created last, so a failed build never leaves a dangling tag. The same script works locally (`git tag $(hack/next-version.sh)`) as a fallback.
**Pros:** CalVer by construction, and the version is fixed at the moment of release. The script is about 20 lines and easy to test, and the workflow follows the existing CI pattern. **Cons:** there's no release PR to review, and no CHANGELOG.md in the repo, only Release notes. Oikade owns roughly 60 lines of YAML and shell that a tool would otherwise own. **Reversal cost:** low. The tag format is unchanged, so moving to A later means only swapping the workflow.

## Decision

**Accepted: B.** The user switched Oikade from date versions to semver `0.x` (README "Versioning"). Once the version is semver, release-please does exactly what's needed: it picks the next version from conventional commits, which are already in use. It also adds a release PR as the review gate and a CHANGELOG.md, and it leaves no custom versioning code to own. C's main reason, keeping CalVer, no longer applies. A was only ever a workaround for CalVer.

What the user would do:

```sh
# Merge the "chore(main): release 0.1.1" PR that release-please keeps up to date.
# → tag v0.1.1, ghcr.io/harrymitchinson/oikade:v0.1.1, Release with install.yaml and notes
```

Public names this introduces:
- the workflow `release.yml` (Actions name "Release")
- `release-please-config.json` and `.release-please-manifest.json`, and `CHANGELOG.md`
- the image `ghcr.io/harrymitchinson/oikade:<version>`, with no `latest` tag because GitOps pins versions
- the release asset `install.yaml`

## Non-goals

- Releasing automatically on every merge to `main`. Releases stay a deliberate action: merging the release PR.
- A `latest` or `main` image tag, image signing, SBOMs, or a Helm chart. Each of these can be added later without changing the tag format.
- Deciding when to release `1.0.0`.
- Building s390x or ppc64le images.
- Changing the Zigbee2MQTT or Mosquitto images, which are pinned separately.

---

## API

TODO (write-spec)

## Behaviour

TODO (write-spec)

## State and upgrade

TODO (write-spec)

## Observability

TODO (write-spec)

## Security

TODO (write-spec)

## Test plan

TODO (write-spec)

## Docs

TODO (write-spec)

## Implementation plan

TODO (write-spec)

## Open questions

- **Bake the version into the binary?** `-ldflags -X` would let `manager --version` and the startup log line report it. A `oikade_build_info{version}` metric would be a new public metric. The user decides. It doesn't block the workflow.
- **Commit `dist/install.yaml` to `main` on release, or only attach it to the Release?** Committing means a bot push to `main`, which needs a PAT or ruleset bypass. Attaching only means Flux follows the Release asset URL or a `GitRepository` ref on the tag. This depends on how the Pi cluster's Flux is wired today. The user decides, and it blocks the write-spec Behaviour section.
- **Retiring `2026.42.1`:** delete the Git tag (and the image tag, if one was pushed), or keep it and bound Flux policies to `<1.0.0`. It blocks the first `v0.1.0` release, not the workflow. The user decides.
