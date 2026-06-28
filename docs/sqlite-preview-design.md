# Design: real-data previews for SQLite apps (application-landscape)

Status: **planned, not implemented.** Decided 2026-06-28 to do "preview support first"
(wire the `preview` label flow for SQLite apps) and defer an automatic migration-check
for them. This doc is the focused plan to pick up fresh.

## Goal

Let the manual **`preview` label** flow work for **application-landscape**, a SQLite app,
so a PR gets an ephemeral environment seeded with a **clone of real prod data** (the
SQLite DBs + git version history) — the same value the CNPG preview already gives
gitea/nextcloud. This is the SQLite analogue of what the operator already does for CNPG.

Non-goal (deferred): an automatic pre-merge migration-check for application-landscape.
That additionally needs a **per-PR image** (SQLite is a file, not a network endpoint, so
the runner can't connect to a clone the way the CNPG check does — the operator would have
to deploy the PR's image against the cloned PVC). Revisit later.

## Key finding — the PVC clone is already built

The operator already clones an arbitrary prod PVC into a preview:
`internal/controller/config_volume.go` → `setupConfigVolume()` does
snapshot prod PVC → cluster-scoped `VolumeSnapshotContent` → preview `VolumeSnapshot`
→ clone PVC → URL-rewrite Job → `patchDeploymentVolume()` (re-points the app's Deployment
volume at the clone). Triggered by `spec.configVolume.claimName` in a `PreviewConfig`.

**It is binary-safe for SQLite.** The URL-rewrite Job only rewrites files where
`file "$f" | grep -qiE 'text|xml|json|ascii|utf-'` (config_volume.go ~233), so binary
`*.db` SQLite files are skipped — only any text config in the volume is touched.

So getting application-landscape's `application-landscape-data` PVC (auth.db +
per-landscape `landscape.db` + lg2_s3 git checkout) into a preview is essentially a
`preview.yaml`, not new clone code.

## The real work: isolate THREE prod-write paths

application-landscape's web pod (see homelab `apps/application-landscape/deployment.yaml`)
writes to **production** in three ways. A preview MUST NOT, or it corrupts prod:

1. **litestream sidecar** (`litestream/litestream:0.3.13`) — *autonomously* replicates
   `/data` → the **prod RGW backup bucket** on a timer. In a preview this overwrites the
   prod backup with preview data. **Highest risk** (no user action needed; it just runs).
   → **Strip the sidecar entirely in previews.** A throwaway env needs no backup, and
   stripping is zero-risk vs repointing (which depends on env-override correctness — if
   that's wrong, it writes to prod). Also strip the `ensure-litestream-bucket` initContainer.
2. **`GIT_S3_*`** (web container) — lg2_s3 version-history git pushes to the **same prod
   bucket**; editing a landscape in a preview pollutes prod git history.
   → **Repoint `GIT_S3_ENDPOINT/BUCKET/ACCESS_KEY_ID/SECRET_ACCESS_KEY`** to the preview's
   in-namespace S3 proxy (`spec.storage.enabled: true` already provisions `setupS3Proxy`).
3. **Stripe** (`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, price IDs) — real Stripe.
   → **Override with dummy values.** The app is designed to run without Stripe (clients
   construct lazily; calls just fail), so dummies are safe.

## The operator gap to close

`spec.envMapping` (api/v1) maps **one** S3 endpoint/creds set (`s3Endpoint`, `s3AccessKey`,
`s3SecretKey`, `s3Bucket`) to one set of container env var names. application-landscape has
**two** S3 consumers (`LS_*` for litestream — being stripped — and `GIT_S3_*` for git), and
the litestream sidecar must be **removed**, which `envMapping` can't express. So pick one:

- **Option A (recommended): add a small "sidecar strip + extra env" capability.**
  - Extend `PreviewConfig` with e.g. `spec.deploymentPatch.removeContainers: [litestream]`
    and `removeInitContainers: [ensure-litestream-bucket]`, applied as a strategic-merge /
    JSON6902 patch in `buildAllPatches` (kustomization_patches.go) — the preview flow already
    sets `ks.Spec.Patches`.
  - Add a generic `spec.envMapping.extraEnv: {VAR: value-with-{S3_*}-substitution}` so the
    `GIT_S3_*` vars can be pointed at the proxy (reuse the `{REDIS_HOST}`/`{NAMESPACE}` style
    substitution already in `buildHelmValuePatches`), plus the dummy Stripe vars.
  This keeps the clone/preview machinery generic and reusable for other multi-S3 / sidecar
  apps.
- **Option B: a per-app patch file.** Skip operator changes; ship the container-strip +
  env overrides as a kustomize patch in application-landscape's preview ResourceSet. Less
  reusable, but no operator code.

Lean **A** — the strip + extraEnv primitives are small and generally useful, and the user
explicitly wants to invest in the operator as the shared clone/preview substrate.

## Sketch: `apps/application-landscape/preview.yaml`

```yaml
apiVersion: preview.homelab.io/v1
kind: PreviewConfig
metadata: { name: application-landscape, namespace: application-landscape }
spec:
  deploymentType: deployment
  storage:
    enabled: true                       # provisions the in-ns S3 proxy
  configVolume:
    claimName: application-landscape-data  # clone the SQLite + git PVC
  deploymentPatch:                        # NEW (Option A)
    removeContainers: [litestream]
    removeInitContainers: [ensure-litestream-bucket]
  envMapping:
    containerNames: [web, yjs]
    appUrl: PUBLIC_BASE_URL               # if the app reads its own URL
    extraEnv:                             # NEW (Option A)
      GIT_S3_ENDPOINT: "http://s3proxy.{NAMESPACE}.svc.cluster.local:8080"
      GIT_S3_BUCKET: "preview-git"
      GIT_S3_ACCESS_KEY_ID: "{S3_ACCESS_KEY}"
      GIT_S3_SECRET_ACCESS_KEY: "{S3_SECRET_KEY}"
      STRIPE_SECRET_KEY: "sk_test_dummy"
      STRIPE_WEBHOOK_SECRET: "whsec_dummy"
```

Health/readiness for the preview: `/pricing` (the app's existing probe path).

## Prerequisites / wiring

- application-landscape is a raw `Deployment` (not Helm) under `apps/application-landscape/`
  → `deploymentType: deployment`; the auto-detect workflow (`detect-preview-app.yaml`) already
  finds any app under `apps/`.
- Namespace is ambient + waypoint (`istio.io/dataplane-mode: ambient`,
  `istio.io/use-waypoint: waypoint`) — the preview namespace clone must keep these for OIDC
  hairpin / routing (it bypasses Authelia via `security-policy-bypass-authelia.yaml`; confirm
  the preview doesn't need an OIDCClient — it appears to use its own auth, no OIDCClient CR).
- The yjs relay container also opens the SQLite DBs directly — keep it; it reads the clone.

## Verification (safety-critical — do this before trusting it)

A wrong config writes to **prod**. The first preview MUST be supervised:
1. Open a throwaway PR, add the `preview` label.
2. While it provisions, **watch the prod RGW bucket** (`application-landscape-litestream`)
   for object writes — there must be **ZERO** new objects from the preview. (e.g. periodic
   `mc ls --recursive` count, or RGW access logs.)
3. Confirm the preview pod has **no litestream container** and `GIT_S3_ENDPOINT` points at
   the in-ns `s3proxy`, and Stripe vars are dummies.
4. Confirm the preview serves `/pricing` against the cloned data (e.g. a known landscape
   from prod is visible).
5. Tear down; confirm the clone PVC + snapshot + S3 proxy are GC'd and the prod bucket is
   untouched.

Only after a clean supervised run should this be considered safe for general use.

## Effort estimate

- Operator (Option A): ~0.5–1 day — `removeContainers/removeInitContainers` patch +
  `envMapping.extraEnv` substitution + tests; release a new chart version.
- application-landscape `preview.yaml` + namespace/preview prerequisites: ~0.5 day.
- Supervised verification: ~0.5 day (the careful part).

## Related

- The CNPG migration-check (shipped) is the reference for the clone→teardown lifecycle.
- ticket-vision / kb-vision / duro / cluster-vision / social-planner have the CNPG check;
  website-builder is being added. openauth was intentionally skipped (no real migrations).
