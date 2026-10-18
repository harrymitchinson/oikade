---
id: DRAFT-0001
title: Release automation
status: Draft
assignee: []
created_date: '2026-10-18 09:54'
updated_date: '2026-10-18 10:57'
labels: []
dependencies: []
documentation:
  - doc-0004
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Releases are cut by hand (local tag, buildx, build-installer). Adopt release-please (option B, semver 0.x): release PR, CHANGELOG.md, tag, arm64 image and install.yaml Release. Not committed: the design is accepted but the Behaviour, test plan and step breakdown still need write-spec, which is blocked on the install.yaml open question.
<!-- SECTION:DESCRIPTION:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
