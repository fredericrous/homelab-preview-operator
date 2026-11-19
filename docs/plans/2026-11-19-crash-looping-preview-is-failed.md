---
canonical: homelab:docs/plans/2026-11-19-crash-looping-preview-is-failed.md
phases: [1]
status: active
---
Part of [t28: a lane bump whose preview crash-loops is failed, closed and held back](https://git.daddyshome.fr/fredericrous/homelab/src/branch/main/docs/plans/2026-11-19-crash-looping-preview-is-failed.md).

## Code review

- homelab-preview-operator: backend, lang:go (unix, tui left out: the operator's command line does not change)

## Decision log

- 2026-11-19, Phase 1 measurements on the live failing preview `preview-app-1244` (homelab #1244, kavita 0.9.1 again). Each is input → expected → actual:
  - `lastAttemptedRevision` → ends with the PR head SHA → `sha1:2048de6c…`, the PR's head. The revision binding holds.
  - Ready over 12 min → stays False, or flips to Unknown → Unknown/Progressing 15:53–16:03Z (a whole health-check wait per retry), False only between retries, and generation 3→5 with `status.observedGeneration` held at 3. So the latch is needed: `status.lastKustomizationFailure`, the CRD regenerated in both copies.
- Tests:
  - `TestKustomizationSettled*`, `TestReconcile_HealthCheckFailedAtDeadlineIsFailed`, `TestReconcile_FailureSeenBeforeAnUnknownDeadlineIsFailed` → pass → pass.
  - The same tests against main's controller → fail → 3 FAIL.
  - The latch test with the latch removed → fails → the "same generation and revision" case FAILs.
