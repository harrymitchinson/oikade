# add-capability skill and Raspberry Pi deploy guide

_2026-10-17 · handoff from a Claude Code session_

## Goal
Oikade v1 (a GitOps home automation operator: CRDs own config, MQTT owns live state, Prometheus/Grafana own history, HomeKit via a Go HAP bridge) is built and committed up to `d0ec556`. This session added two things around it: a step-by-step guide for running it on the user's arm64 Raspberry Pi cluster, and an `add-capability` skill so future sessions can add new device capabilities (colour temperature, leak, illuminance, …) end to end.

The user's standing constraints still apply:
- No Home Assistant anywhere.
- Homebridge is deleted for now.
- `~/Code/oikade` is unrelated. Only the name is reused, so never read or touch it.

## Status
**Done (all uncommitted)**
- `docs/deploy-raspberry-pi.md`: the full Pi cluster walkthrough, and `README.md:57` links to it. Written and reviewed by the user ("Looks all good to me"). The commands in it have not been run on real hardware.
- `.claude/skills/add-capability/SKILL.md` plus `evals/evals.json` (3 evals: colour temperature, water leak, illuminance). Iteration-1 evals scored 97% with the skill against 94% without. The user said "Looks good." After that I applied fixes: `CGO_ENABLED=0` on the lint command, and the "shared unit vs lossy kelvin" guidance under Units. Those fixes have not been re-evaluated.
- The eval worktrees are cleaned up: `git worktree list` shows only `main` and no extra branches remain. An empty `.claude/worktrees/` directory may still exist and is safe to remove.

**Not mine, leave alone**
- These are the user's own edits: `config/default/kustomization.yaml` (`../prometheus` and `../grafana` enabled), and `config/manager/kustomization.yaml` and `config/runtime/kustomization.yaml` (image `oikade:dev`). Don't revert them, and don't fold them into a commit without asking.

**Not started**
- Rebuilding the Pi cluster and doing the first real HomeKit pairing. This is the user's stated next step. mDNS doesn't work from Kind on macOS, so pairing and real Zigbee can only be verified there.

## Decisions
- **Colour temperature should stay in mireds, not kelvin.** zigbee2mqtt and HomeKit both use integer mireds, and kelvin doesn't round-trip (2700 K comes back as 2703 K). The skill now says to prefer a shared unit, or to ask the user.
- **New HAP services are appended in `internal/hap/mapping.go` `Build`, never inserted.** brutella/hap assigns instance IDs sequentially, and inserting a service remaps the user's Home app automations. Both baseline eval runs got this wrong, which is the main reason the skill exists.
- **Enum-valued capabilities (thermostat mode, lock state) need an API change** to `CapabilityKind`. The skill tells the agent to stop and discuss these rather than encode magic numbers.

## Dead ends
- `rm -rf` on eval worktrees → "Permission denied", because setup-envtest makes `bin/k8s/...` read-only. Needs `chmod -R u+w` first. The user rejected my chmod+rm call and deleted the worktrees themselves, so don't run destructive cleanup in `.claude/` without asking.
- Plain `make lint` / `make test` on this Mac fails with the cgo error `unknown architecture arm64e.x1`. Use `CGO_ENABLED=0 make test` and `CGO_ENABLED=0 GOTOOLCHAIN=go1.26.0 make lint`.

## Next steps
1. Ask whether to commit the skill (`.claude/skills/add-capability/`), `docs/deploy-raspberry-pi.md` and the `README.md` link. Keep the user's `config/*/kustomization.yaml` edits out unless told otherwise.
2. Optional: run skill-creator's description optimization on `add-capability`, or run an iteration-2 eval to confirm the post-review fixes.
3. When the user is on the Pi cluster, follow `docs/deploy-raspberry-pi.md`. Expect the first problems to be arm64 image builds, the HAP pod's `hostNetwork`/mDNS reachability, and the zigbee coordinator address. Fix the guide wherever reality differs from it.

## Open questions
- Should `.claude/planning-workspace/` and the other skills in `.claude/skills/` (`design-feature`, `handoff`, `plan-roadmap`, `write-spec`) be committed with the repo, or kept local?

## Resume prompt
```
Read .claude/handoffs/2026-10-17-add-capability-skill-and-pi-guide.md first, as it's the handoff from my last session. Oikade v1 is committed; the add-capability skill and the Raspberry Pi guide (docs/deploy-raspberry-pi.md) are finished but uncommitted, and my own config/*/kustomization.yaml edits should be left alone. Next I'm setting up the Pi cluster by following the guide, so help me through it and fix the guide wherever it's wrong.
```
