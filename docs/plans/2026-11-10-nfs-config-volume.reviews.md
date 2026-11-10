# Full reviews: NFS-backed config volumes

## Full reviews (reference)
Round 1 (opus): backend 7 findings (1 blocking: `adds=contract`), lang:go 6, platform 7, unix 4, tui 5; all approve-with-changes, 26 accepted, 2 rejected (Event on PreviewConfig; `find -exec` in the snapshot Job).
Round 2: backend 5 (skip rule and merge-patch conflicts, then moot), lang:go 5 (CRD default vs CEL rule), platform 5 (Flux applies the CRD; envtest), unix 4 (busybox `grep -I` exit status; `fail` function), tui 3 (separate unbound-PVC message), architect 3 (Kustomization patch instead of a direct patch; no `mode` default; nested `nfs`), po 4 (same Flux revert; ship `copy: false` first; after-release measures). All approve-with-changes.
Delta: backend found one high (Helm apps get no patch and no error), applied; then resolved, nothing blocking.
Carried into implementation: two commits (`nfs.copy: false` first, then `copy: true`); an "after release" check (NFS previews Ready, `config-seed` failures, snapshot previews unaffected); README caveat that a PR renaming the volume leaves its PVC unpatched; render the patch with `sigs.k8s.io/kustomize/api/krusty`; the `sizeLimit` field is the smallest thing to drop if the contract should shrink.
Process note: the round-2 body was not saved to the ledger (the record ran after the edit).
