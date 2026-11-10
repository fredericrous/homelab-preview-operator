---
status: active
branch: feat/nfs-volumes
repos: [homelab-preview-operator, homelab]
adrs: []
---
# Previews for NFS media apps: radarr and sonarr

## Review panel
📄 Full reviews: [2026-11-10-nfs-media-previews.reviews.md](2026-11-10-nfs-media-previews.reviews.md)
👉 **Decide:** none — approve if the bet is a read-only real share behind a DNS-plus-namespace egress fence, opened to patch-agent only after a hand-labelled preview passes.
📍 homelab-preview-operator, homelab · plan only · next: operator worktree, Phase 1 API. Panel: backend, lang:go, platform, architect, po, lang:python, lang:typescript (all opus).
**Changed by review:** no per-mount patch (`nfs.readOnly` covers every mount); fence also allows the preview namespace and fails closed; homelab split into pin / opt-in (annotation kept) / open-to-patch-agent PRs, never a pin rollback below this release.
**Verdicts:** round 1: 6 approve-with-changes, 1 approve; round 2: backend and python approve-with-changes, typescript approve; 0 blockers. 3 carried into implementation. codex: skipped (it would send the plan to an outside vendor).

## Picture

```
 today (radarr, sonarr)              after this plan
 ----------------------              ---------------
 preview renders pvc.yaml            preview renders pvc.yaml
   media PVC --> radarr-media-pv       PersistentVolume  --> stripped
   (static PV, Bound to prod)          media/media2 PVCs --> stripped
   --> claim never binds               Deployment volume --> NAS export,
   --> pod Pending forever                               readOnly: true
 so: previewable: "false"            NetworkPolicy: egress = DNS + own ns
                                       --> no indexers, no prod qBittorrent
```

A radarr or sonarr preview used to be impossible because its media claims bind static NFS PVs that prod already holds; after this plan the preview mounts the same NAS exports read-only and reaches only DNS and its own namespace.

## Context
radarr and sonarr keep their config on Ceph block, which `configVolume` (snapshot mode) already clones. What blocks their previews is the media: `media` (/Media) and `media2` (/Media2) are PVCs bound by `volumeName` to static NFS PVs, so in a preview the claims never bind. The operator's strip list (`stripTargets` in `internal/controller/kustomization_patches.go`) has no `PersistentVolume`, so a preview would also render the cluster-scoped prod PVs into a `prune: true` Kustomization whose teardown could delete them. Today only patch-agent (sre-agent repo, `src/patch_agent/tools/scope.py` `check_previewable`) reads the `previewable: "false"` annotation; the operator ignores it.

The person chose (2026-11-10): previews see the **real share, read-only**, behind an **egress fence**, because the cloned SQLite config keeps prod's download client and indexers (the URL rewrite skips binary files); **qbittorrent stays out** (a preview would re-seed prod's torrents).

## Behaviour
New optional PreviewConfig fields, json tags `,omitempty` and no CRD defaults, so stored objects keep their config hash:

```yaml
spec:
  configVolume:
    claimName: radarr-config     # unchanged: snapshot clone of the Ceph config
  nfsVolumes:                    # prod NFS exports mounted read-only
    - claimName: media           # (configVolume.mode: nfs instead swaps in
    - claimName: media2          #  a temp dir; the Go doc and README say so)
  isolateEgress: true            # egress: kube-dns and the preview ns only
```

- **Strip.** `PersistentVolume` joins `stripTargets` for every preview. Each `nfsVolumes` claim gets a `$patch: delete` for its PVC.
- **Read-only mount.** Before the patches are built, at both `buildAllPatches` call sites (`PrepareKustomization`, `UpdateDBCredentialPatches`), the operator resolves each claim: the prod Deployment volume that claims it, and the PVC's PV `spec.nfs`. One strategic-merge Deployment patch replaces each such volume with `nfs: {server, path, readOnly: true}` (`persistentVolumeClaim: null`). Container mounts are left as the PR renders them: `nfs.readOnly` makes the kubelet mount the export `ro`, so `/media`, `/downloads` (`subPath: Downloads`), `/media2` and `/downloads2` are all read-only.
- **Errors** (each its own message, no trailing period, ST1005): claim not used by the prod Deployment; PVC not bound; PV without `spec.nfs`; a claim in both `configVolume.claimName` and `nfsVolumes`; `nfsVolumes` in the legacy `Process` flow; `deploymentType: helm` (also a CEL `XValidation` beside the existing one).
- **Egress fence.** With `isolateEgress: true`, `PrepareKustomization` create-or-updates NetworkPolicy `preview-egress-fence` **before** it unsuspends: `podSelector: {}`, `policyTypes: [Egress]`, egress to `namespaceSelector kubernetes.io/metadata.name: kube-system` + `podSelector k8s-app: kube-dns` on UDP/TCP 53, and to `podSelector: {}` in the same namespace on every port (smoke-test Job, Redis, S3 proxy, ambient HBONE 15008). Fail closed: a create or update error returns and the Kustomization stays suspended. With `isolateEgress` false or absent, an existing fence is deleted (a config change re-runs `PrepareKustomization` through the hash re-render). NFS is mounted by the kubelet on the node, so the fence does not cut the share; ingress is untouched.
- **What the person sees.** Once radarr and sonarr drop the annotation (the last step, after the live check), patch-agent opens promotion PRs for their image bumps (`ImageUnpinned` is `mode: auto`), gated on the fenced preview's verdict.
- **Caveats (README):** inline NFS volumes take no `mountOptions` (the PV's `vers=4,hard,…` are not carried); "root folder not writable" health warnings; searches, grabs and notifications fail in a fenced preview by design; each preview rescans the library over the prod export; NetworkPolicies are additive, so a NetworkPolicy an adopter's manifests render widens the fence.

## Phases
1. **Operator API.** `api/v1/previewconfig_types.go`: `NFSVolumes []NFSVolume` (`ClaimName string`) and `IsolateEgress bool`, both `,omitempty`, the Helm CEL rule; regenerate `api/v1/zz_generated.deepcopy.go`, `config/crd/bases/preview.homelab.io_previewconfigs.yaml`, `chart/homelab-preview-operator/crds/preview.homelab.io_previewconfigs.yaml`.
2. **Operator strip + mount.** `internal/controller/kustomization_patches.go`: `PersistentVolume` in `stripTargets`, PVC delete patches, and `buildAllPatches` takes `volumePatches []kustomize.Patch` (configVolume nfs patch + nfsVolumes patch) in place of `nfsPatch`. `internal/controller/nfs_volumes.go` (new): the resolver and the patch, reusing the PVC→PV lookup extracted from `resolveNFSConfigVolume` in `internal/controller/config_volume.go`. `internal/controller/preview_handler.go`: resolve at both call sites, reject in `Process`.
3. **Operator fence.** `internal/controller/egress_fence.go` (new): ensure or delete the policy, with a `+kubebuilder:rbac` marker for `networkpolicies` get/create/update/delete (`make manifests` regenerates `config/rbac/role.yaml`); by hand in `deploy/rbac.yaml` and `chart/homelab-preview-operator/templates/clusterrole.yaml`. `cmd/main.go`: add `&networkingv1.NetworkPolicy{}` to `Client.Cache.DisableFor` beside Jobs and Pods.
4. **Operator tests + docs.** `internal/controller/nfs_volumes_test.go`: krusty render of a sample with `media` mounted twice (`subPath`) and `media2` → both volumes inline `nfs` read-only, PVCs and PVs deleted, mounts and `subPath` unchanged; a render with `configVolume.mode: nfs` plus `nfsVolumes`; each error; a spec without the new fields hashes as before; the fence spec exactly; with an `interceptor.Funcs` client failing the NetworkPolicy Create, `PrepareKustomization` errors and the Kustomization stays suspended with no patches; a second run takes the AlreadyExists path; fence deleted when the flag goes false. `internal/controller/previewcheck_envtest_test.go`: the CRD accepts the fields and rejects `nfsVolumes` with `deploymentType: helm`. `README.md`: schema, fence, caveats.
5. **Homelab PR A (pin).** `kubernetes/homelab/preview-system/preview-operator.yaml`: pin to the release.
6. **Homelab PR B (opt in, annotation kept).** `kubernetes/homelab/apps/radarr/preview.yaml`, `kubernetes/homelab/apps/sonarr/preview.yaml`: add `nfsVolumes: [media, media2]` and `isolateEgress: true`, keep `previewable: "false"` (patch-agent stays out while a hand-labelled PR proves the preview).
7. **Homelab PR C (open to patch-agent).** After the live checks pass: drop the annotation and its comment from `kubernetes/homelab/apps/radarr/preview.yaml` and `kubernetes/homelab/apps/sonarr/preview.yaml`; rewrite the comment in `kubernetes/homelab/apps/qbittorrent/preview.yaml` (keeps `previewable: "false"`: `nfsVolumes` would solve binding, but a preview would re-seed prod's torrents); `docs/operations/image-vuln-monitoring.md` drops radarr and sonarr from the refused list.

## Rollout and rollback
Operator PR (Phases 1–4) merged and released through the `tag-release` skill (next minor after 0.10.6: the CRD gains fields). Before PR A: list every preview Kustomization's `.status.inventory.entries` and confirm none holds a `_PersistentVolume` entry (0 expected; a hit means closing that preview first, or the new strip prunes the prod PV). PR B waits until `kubectl explain previewconfig.spec.nfsVolumes` resolves and the operator pod runs the new image. PR C waits for the live checks.

Roll back on: a preview verdict that is a false negative caused by the mount or the fence in the first 5 promotion PRs; any non-DNS, non-namespace egress from a `preview-pr-*` namespace; any NAS file changed by a preview pod. Revert: restore the annotation (stops patch-agent only), delete every open radarr/sonarr preview namespace and see it gone, then revert the `preview.yaml` edits. Never roll the pin back below this release: an operator without the PersistentVolume strip renders the prod PVs into a `prune: true` preview whose teardown deletes them; an operator fix ships as the next patch release.

## Non-goals
- qbittorrent previews (re-seeding hazard).
- kyoo, navidrome, nextcloud (also static NFS PVs): they can adopt `nfsVolumes` later, each its own change.
- Writable or copied media, rewriting the arr SQLite config, a per-item `readOnly`/`mode` on `nfsVolumes` (an additive field later).
- Cilium-specific policy (toFQDNs) or per-app egress allow-lists.
- The `scope.py` docstring in sre-agent naming radarr/sonarr as refused (a one-line follow-up there).

## Code review
- homelab-preview-operator: backend, lang:go, platform
- homelab: platform

## Verification
- Operator: `go build`, `go vet`, pinned `make lint` (0 issues), `go test ./api/... ./internal/...`, envtest with cached assets; `make manifests generate` leaves no diff; PR checks green, read per check.
- Before PR A: the inventory query returns 0 PersistentVolume entries.
- After PR A: the CRD schema shows `nfsVolumes`; the operator pod runs the release image.
- Live, after PR B, on a hand-labelled `preview-app:radarr` PR, then the same for sonarr:
  - the preview pod is Ready; `kubectl -n preview-pr-N get pvc` shows no `media`/`media2`; `kubectl get pv radarr-media-pv` shows no preview Kustomization label;
  - `/media`, `/downloads`, `/media2` and `/downloads2` list the real library; `touch` in each of the four fails with "Read-only file system";
  - `preview-egress-fence` exists; from the pod, `nslookup qbittorrent.qbittorrent.svc.cluster.local` resolves, a TCP connect to it and to `radarr.radarr.svc` times out, and a connect to the preview's own Service succeeds;
  - the preview URL serves the UI;
  - NAS and node network throughput over the preview's first 15 min stays within prod's baseline plus 10 Mb/s on the ~100 Mb/s uplink; the library file count under /media and /media2 and the scan duration are recorded;
  - after teardown, `radarr-media-pv` is still Bound to `radarr/media`.
- After PR C: the next radarr and sonarr image bumps get a patch-agent promotion PR carrying a preview verdict; qbittorrent is still refused.

<!-- panel: repos=homelab-preview-operator,homelab adds=contract reviewers=backend,language,platform,architect,po body-sha=98f30e6c5881 -->
