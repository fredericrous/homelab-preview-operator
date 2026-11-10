---
status: active
branch: feat/nfs-config-volume
repos: [homelab-preview-operator]
adrs: []
---
# Preview operator: NFS-backed config volumes

## Review panel
📄 Full reviews: [2026-11-10-nfs-config-volume.reviews.md](2026-11-10-nfs-config-volume.reviews.md)
👉 **Decide:** none — approve if the bet is a Kustomization-patch emptyDir (copy off by default), not a direct Deployment patch.
📍 homelab-preview-operator · plan only, no code yet · next: worktree from origin/main, Phase 1 API. Panel: backend, lang:go, platform, unix, tui, architect, po (all opus).
**Changed by review:** the swap became a Flux-applied Kustomization patch, so it survives a reconcile; no CRD defaults, fields nested under `nfs`; seed script hardened (busybox grep, fail messages, size abort).
**Verdicts:** 7 approve-with-changes, 0 rework; 0 blockers left. Carried into implementation: ship `nfs.copy: false` as its own commit, an "after release" measure, a README note on volume renames, krusty-rendered patch test. codex: skipped, it would send the plan to an outside vendor.

## Picture

```
 today (configVolume)               after this plan (mode: nfs)
 --------------------               ---------------------------
 prod PVC --snapshot--> clone PVC   prod NFS share
        \--> rewrite Job                 |  nfs.copy: true only, read-only
         \--> Deployment on clone        v
                                    config-seed (init) --> emptyDir <-- app
 (NFS has no snapshot: fails)       (copy false: empty emptyDir, no init)
                                    both ride as a Kustomization patch
```

An NFS-backed config volume used to fail at the snapshot step; with `mode: nfs` the preview runs on a temp dir that is empty by default and seeded from the share only on request.

## Context
`configVolume` (`internal/controller/config_volume.go`, `setupConfigVolume`) only works
for Ceph block PVCs: it snapshots the prod PVC, restores it into the preview namespace as a
`rook-ceph-block` PVC, runs a URL-rewrite Job and swaps the claim in the Deployment. An
NFS-backed PVC has no snapshot support, so an app whose config lives on NFS cannot be
previewed. Goal: such an app runs in a preview on a temp dir (emptyDir). Copying the NFS
content into it is optional, never required, and the share is never cloned or written.

## Behaviour
New optional fields on `configVolume`; existing PreviewConfigs are unchanged. No field gets
a CRD default (a default would change every stored object's config hash and would trip the
CEL rule); code reads `mode: ""` as `snapshot` and applies the 256Mi default itself.

```yaml
configVolume:
  claimName: myapp-config
  mode: nfs            # snapshot (default) | nfs
  nfs:                 # allowed only with mode: nfs
    copy: false        # default false: start empty. true: seed from the share
    sizeLimit: "256Mi" # emptyDir sizeLimit
```

- One CEL rule: `has(self.nfs)` is only allowed when `mode == 'nfs'`
  (`nfs only applies to mode: nfs`). Snapshot configs without the block stay valid.
- The swap is declarative: a strategic-merge Deployment patch built by `buildAllPatches`
  (next to `generateContainerStripPatch`), so Flux applies it and a reconcile cannot revert
  it. It replaces the app's PVC volume (found by reading the production Deployment for the
  volume whose claim is `claimName`) with an `emptyDir` carrying the size limit
  (`persistentVolumeClaim: null`). No snapshot, PVC, Job, annotation or teardown. The patch
  goes in the `default:` (raw manifests) branch only; the lookup fails `deploymentType: helm`
  with `mode: nfs` as `mode: nfs supports raw-manifest Deployments only; Helm-rendered
  Deployments are not patched`.
- `nfs.copy: true` adds a `config-seed` initContainer to the same patch, mounting the NFS
  export of the prod PVC's PV read-only (`nfs.readOnly: true` on the Volume and `readOnly`
  on the mount).
  - Lookup, done before the patches are built, in `PrepareKustomization` and the
    credentials step that call `buildAllPatches`: prod PVC in the app's namespace, its
    `spec.volumeName`, the PV's `spec.nfs`. An unbound PVC and a PV without `spec.nfs` fail
    early with their own messages: `configVolume.claimName "<pvc>" is bound to PV "<pv>",
    which has no spec.nfs; nfs.copy: true needs an NFS PV. Set nfs.copy: false to start
    empty, or use mode: snapshot.` and `configVolume.claimName "<pvc>" in namespace "<ns>"
    is not bound to a PV; nfs.copy: true needs a bound NFS PV. Set nfs.copy: false to start
    empty.`
  - Script (no `apk`, no network, `alpine:3.21`): a `fail` function (message to stderr,
    `exit 1`) wraps every step and the container sets `terminationMessagePolicy:
    FallbackToLogsOnError`. It clears `/config` first (`find /config -mindepth 1 -delete`,
    so a re-run never double-rewrites), compares `du -sk /src` with the limit passed in KiB
    and fails with `source <server>:<path> is <N>Mi, over sizeLimit <L>; raise
    configVolume.nfs.sizeLimit or set nfs.copy: false`, runs `cp -a /src/. /config/`, then
    rewrites the same URLs as the snapshot Job. Text files are selected with `grep -Iq .`:
    status 1 (binary or empty) skips, status 2 or more fails the seed; a test runs the
    script in `alpine:3.21` to prove busybox grep accepts `-I`. Iteration uses
    `find /config -type f -exec sh -c '...' sh {} +`. A file root cannot read fails the
    seed with `cannot read <file>; the export may be root-squashed; set nfs.copy: false to
    start empty`.
  - Runs as root with the Job's cpu/memory requests and limits plus an `ephemeral-storage`
    request and limit, and `chown`s to the uid/gid of the app container's (else pod-level)
    `securityContext`; when none is set it runs `chmod -R a+rwX /config`.
- `mode: nfs` in the legacy `Process` flow (`internal/controller/preview_handler.go`) fails
  with `mode: nfs is supported by the Kustomization flow only`.
- Logs one Info line when the patch is built: `NFS config volume patched` with app, copy
  and pv; a seed failure shows as `Init:Error` and in `kubectl logs <pod> -c config-seed`.
- Caveats for the README: a root-squashed export fails the seed on any unreadable file;
  every pod (re)start re-reads the whole share over the ~100 Mb/s node uplink (about 90 s
  per GiB; the temp dir is ephemeral by design); `nfs.copy: true` puts prod config in a PR
  preview namespace, so it must hold no secrets (the rewrite only touches URLs); an image
  whose non-root uid comes only from its `USER` needs `runAsUser` set for the seed to chown.

## Phases
1. **API.** In `api/v1/previewconfig_types.go` add `Mode` (enum, no default) and `NFS`
   (`*NFSConfigVolume` with `Copy *bool` and `SizeLimit *resource.Quantity`) to
   `ConfigVolumeConfig`, with the CEL rule above; regenerate
   `api/v1/zz_generated.deepcopy.go`,
   `config/crd/bases/preview.homelab.io_previewconfigs.yaml` and
   `chart/homelab-preview-operator/crds/preview.homelab.io_previewconfigs.yaml`.
2. **Patch.** In `internal/controller/kustomization_patches.go` add the volume-swap and
   `config-seed` strategic-merge patch to `buildAllPatches` (new argument: the resolved
   volume name and NFS source). In `internal/controller/config_volume.go` add the lookup
   (production Deployment volume name, PVC to PV to `spec.nfs`) and make `setupConfigVolume`
   skip `mode: nfs`; call the lookup from `internal/controller/preview_handler.go` at the
   two `buildAllPatches` call sites and reject nfs in `Process`. The snapshot Job script is
   untouched and pinned by a golden test.
3. **RBAC.** Add `get` on `persistentvolumes` to `config/rbac/role.yaml`,
   `deploy/rbac.yaml` and `chart/homelab-preview-operator/templates/clusterrole.yaml`
   (cluster-wide, read-only; stated in the PR). `persistentvolumeclaims` and `deployments`
   get already exist.
4. **Tests.** New `internal/controller/config_volume_nfs_test.go` and cases in
   `internal/controller/kustomization_patches_test.go` (or the existing patch tests): the
   patch rendered against a sample Deployment gives an `emptyDir` with the limit and no PVC;
   copy true gives the read-only nfs volume, mount, `config-seed` resources and
   securityContext; copy false gives no initContainer; non-NFS PV, unbound PVC and missing
   production volume each error with their own message (the unbound one never contains
   `PV ""`); nfs in `Process` errors; nfs with `deploymentType: helm` errors; the snapshot script golden test; the seed script runs
   in `alpine:3.21` (grep `-I`, over-limit abort, unreadable file, second run leaves no
   `pr-N-pr-N-`). An envtest case in `internal/controller/previewcheck_envtest_test.go`: the
   API server accepts the CRD, accepts a snapshot config without `nfs`, and rejects `nfs`
   with `mode: snapshot`; run with `make test-integration`.
5. **Docs.** `README.md`: the `configVolume` NFS example and the caveats above.

## Rollout and rollback
The regenerated chart CRD is committed and Flux applies it with the operator (HelmRelease
`crds: CreateReplace`); no hand `kubectl apply`. Revert: remove the whole `configVolume`
block from NFS-backed PreviewConfigs (dropping only `mode` sends them down the snapshot
path, which fails on NFS), then roll the operator back.

## Non-goals
- Cloning or snapshotting NFS, or writing back to the share.
- Other volume types (hostPath, CSI-NFS PVs without `spec.nfs`), and an exclude list for
  copied files.
- Changing snapshot mode, the CNPG clone path or `patchWorkload`.
- An Event on the PreviewConfig: the returned error and an error log suffice.

## Code review
- homelab-preview-operator: backend, lang:go, platform, unix (tui left out: no terminal output change)

## Verification
- `go test ./...` and `make test-integration` pass, including the cases of Phase 4.
- CRD regeneration: `git diff --exit-code` on `config/crd` and the chart `crds/` is empty.
- `amont land` passes its gates.
- Live on the homelab (NFS-backed app, PR preview): `nfs.copy: false` gives a Ready pod on
  an emptyDir; `nfs.copy: true` gives the files and rewritten URLs in `/config`, with init
  time measured (expected at or under 30 s for 256Mi) and `ephemeral-storage` use noted;
  `flux reconcile kustomization <preview> --with-source` leaves the pod on the emptyDir; a
  pod restart re-seeds with no `pr-N-pr-N-` in any file; a write to the share from the pod
  fails; the non-root app can read and write the seeded files; existing previews'
  `preview.homelab.io/config-hash` is unchanged after the CRD apply.
Work in a worktree from origin/main, one PR.

<!-- panel: repos=homelab-preview-operator adds=contract reviewers=backend,language,platform,unix,tui,architect,po body-sha=9e1448352973 -->

