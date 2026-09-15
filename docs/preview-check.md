# PreviewCheck

`PreviewCheck` turns a preview environment into a **machine-readable verdict**:
is the change in this pull request safe to merge?

It exists because nothing verified previews. The operator rendered a preview
namespace, stripped every `batch/Job`, and stopped. A caller that wanted to
merge on green — sre-agent's autonomous image-promotion loop is the first — had
nothing to read.

```yaml
apiVersion: preview.homelab.io/v1
kind: PreviewCheck
metadata:
  name: pr-4211
  namespace: preview-check
spec:
  prNumber: 4211
  app: navidrome
  image: ghcr.io/fredericrous/navidrome:0.55.0   # optional
  revision: 9f1c0ab                              # optional
```

```console
$ kubectl -n preview-check get previewcheck
NAME      APP         PR     PHASE    REASON            EXPIRES   AGE
pr-4211   navidrome   4211   Passed   AllChecksPassed   112m      8m
```

## Why the CR lives in `preview-check`, not in the preview namespace

The obvious home for a preview's verdict is the preview itself. It does not
work, in both directions:

- **The preview does not exist when the verdict is requested.** The CR is
  created the moment the PR opens. The `ResourceSetInputProvider` polls every
  minute and only sees PRs that already carry `preview-ready`, which CI adds
  *after* the PR exists. For the first few minutes there is no namespace to put
  anything in — which is also why an absent namespace is `Pending`, never a
  failure (see the phase contract below).
- **The preview is pruned when the PR closes**, taking anything inside it with
  it. A verdict that vanishes together with the thing it judged is unreadable
  exactly when a caller needs it: after the fact, to decide whether to merge,
  revert or escalate.

So the CR lives in a long-lived namespace (`preview-check` by convention) and
reaches *into* the preview namespace to do its work. That is why the check Jobs
carry **no ownerReferences** — a cross-namespace ownerRef is invalid and
Kubernetes' garbage collector reaps an object whose owner it cannot find in its
own namespace — and why the reconciler holds a **finalizer**: nothing else would
ever clean those Jobs up.

## The phase contract

Three phases are terminal, and the reconciler never leaves one. The distinction
between two of them is the whole contract:

| Phase | Meaning | What a caller does |
| --- | --- | --- |
| `Passed` | Every selected check passed or was skipped. | Merge. |
| `Failed` | The change was **judged, and judged bad**. | Close the PR. |
| `Expired` | The change was **never judged**. | Leave the PR open, cool down, escalate. |

`Failed` reasons: `PodsNotReady`, `RevisionMismatch`, `UnexpectedStatus`,
`ProbeJobFailed`, `KEVExceeded`, `EPSSExceeded`, `SmokeJobFailed`,
`AppMismatch`, `InvalidThreshold`, `KustomizationNotReady` *when the
Kustomization exists and is explicitly `Ready=False` at the deadline*, and
`NamespaceGone` *when the namespace was observed and then vanished*.

`Expired` reasons: `ScanMissing`, `EnrichmentUnavailable`, `QuotaExceeded`,
`Timeout`, `NamespaceGone` *when the namespace never rendered at all*, and
`KustomizationNotReady` when the gate simply never opened.

Two consequences worth stating out loud:

- **`Expired` is reachable long before `expiresAt`.** It fires at
  `startedAt + timeoutSeconds` (30 min by default), while `expiresAt` is
  `startedAt + ttlSeconds` (2 h). So the `EXPIRES` column on an `Expired` object
  is normally still in the future. That is intentional: the run deadline decides
  the verdict, the TTL decides how long the verdict is worth keeping.
- **`status.checks[]` has no `Expired` phase.** A check that ran out of road is
  `Failed` with the reason that stopped it; the CR phase carries the
  judged/not-judged distinction. A `trivy` check reading `Failed/ScanMissing`
  under a CR reading `Expired` is the normal shape, not a contradiction.

## The checks

They run in a **fixed order**, one step per reconcile, fail-fast on the first
failure, and a check that has finished is never re-run:

1. **readiness** — every Deployment and StatefulSet in the preview is available.
   `replicas: 0` counts as ready (a preview legitimately scales a worker to
   zero), and a generation the controller has not observed does not.
2. **http** — a `curl` Job **inside** the preview namespace requests
   `spec.httpPath` on the app's Service. The target is the preview HTTPRoute's
   first `backendRef` — literally what a human visiting the preview would reach
   — falling back to a Service named after the app, then to the single
   non-infrastructure Service (the CNPG clone's `-rw`/`-ro`/`-r` Services, Redis
   and the S3 proxy are filtered out).
   The **container decides**: it exits 0 on an accepted status and non-zero
   otherwise, so Job success *is* the verdict and the log tail is only detail.
   `spec.expectStatus` defaults to 2xx **and** 3xx so an OIDC-fronted app, which
   answers an unauthenticated probe with a redirect to the IdP, still passes.
3. **trivy** — the trivy-operator `VulnerabilityReport` for the previewed image
   is re-scored against `spec.thresholds`. See below.
4. **smoke** — the app's own test, if it declares one. Absent is `Skipped`, not
   failed.

Everything is gated on the preview Kustomization having **settled**: not
suspended, `credentials-patched=true`, generation observed, `Ready=True` for
that generation, and — when `spec.revision` is set — a `lastAppliedRevision`
ending with it. No check has a side effect before that gate opens. The gate
closing *mid-check* (the operator re-suspends on a PreviewConfig hash change)
leaves in-flight checks `Running`: a re-render is not a verdict. The deadline
decides if it never re-opens.

### Trivy scope — stated, not implied

trivy-operator scans previews with `severity: CRITICAL,HIGH` and
`ignoreUnfixed: true`. The thresholds are therefore evaluated over **fixable
CRITICAL/HIGH findings only**.

A clean trivy check does **not** mean the image has no vulnerabilities. It means
it has no *fixable critical or high* finding that is known-exploited or likely
to be exploited. Low/medium findings, and anything with no fix available, are
invisible to this check by design — promoting on "no fixable critical/high
exploitable CVE" is the decision that was taken, and this is where it is
recorded.

Scoring:

- Only `CVE-` ids are enriched. GHSA/DSA/ALAS ids are published as
  `details.nonCveIds` and otherwise ignored: the intelligence has never heard of
  them, and posting them would make an image look enriched when nothing was
  looked up.
- An **empty** CVE set passes **without calling the enricher at all**. An empty
  list would earn a 400, which the ladder reads as terminal — so a perfectly
  clean image would fail closed.
- `thresholds.epssMaxPermille` is an **inclusive** maximum over
  `round(epss * 1000)`. An EPSS of exactly 0.500 passes the default of 500;
  0.501 does not.
- Unknown CVEs count as clean — but only because `stale` and `kev_total` prove
  the cache was loaded. A response with `stale: true` or `kev_total: 0` is
  `EnrichmentUnavailable`, never a pass: every id would come back unknown, and a
  vulnerable image would sail through a perfectly well-formed 200.
- No `--cve-enrichment-url` means the trivy check **fails closed**
  (`Expired/EnrichmentUnavailable`), never open.

## Smoke tests

Declared on the app's `PreviewConfig`, in the **production** namespace:

```yaml
apiVersion: preview.homelab.io/v1
kind: PreviewConfig
metadata:
  name: navidrome
  namespace: navidrome      # production, always {app, app}
spec:
  smokeTest:
    image: ghcr.io/fredericrous/navidrome-smoke:1
    command: ["/bin/sh", "-c"]
    args: ["curl -fsS \"$PREVIEW_URL/rest/ping.view\""]
    timeoutSeconds: 300
```

The operator injects `PREVIEW_URL` (the in-cluster Service URL),
`PREVIEW_NAMESPACE`, `APP_NAME` and `PR_NUMBER`. It deliberately does **not**
inject a public host URL: the edge listener for `*.preview.<domain>` demands a
client certificate, so a test reaching for it would fail in a way that looks
like an app bug.

Security properties, all deliberate:

- The `PreviewConfig` is **always** read from `{app, app}` in the production
  namespace, with `app` taken from the preview namespace's validated
  `preview-app` label — never from `spec.app`, and never from the preview
  namespace. A pull request cannot introduce or edit the test that judges it.
- There is **no `serviceAccount` field** in v1, and the Job always sets
  `automountServiceAccountToken: false`. Preview namespaces hold reflected
  credentials — a repo-write `forgejo-git-token`, `litellm-secrets` — and a
  smoke test is not a place to hand those out.
- `env` takes plain strings only. No `valueFrom`, so a smoke test cannot pull a
  Secret into its environment.
- The Job drops all capabilities, forbids privilege escalation and runs under
  the `RuntimeDefault` seccomp profile. It does *not* force `runAsUser` or a
  read-only root filesystem — that would break most real test images, and those
  protect the container rather than the cluster.

Check Jobs run **in the mesh** (no `ambient.istio.io/redirection: disabled`).
Preview namespaces carry both a ResourceSet-rendered STRICT PeerAuthentication
and a Kyverno-generated PERMISSIVE one; whichever wins, an in-mesh Job reaches
the app, while an out-of-mesh one loses to the STRICT case and would report a
mesh failure as an app failure.

## Spec reference

| Field | Default | Notes |
| --- | --- | --- |
| `prNumber` | required | The preview namespace is `preview-pr-<prNumber>`. Immutable. |
| `app` | — | **Validated** against the namespace's `preview-app` label; a mismatch is `Failed/AppMismatch`. Immutable. |
| `image` | the previewed workload's container | Immutable. |
| `revision` | — | `lastAppliedRevision` must end with it. Immutable. |
| `checks` | `[readiness, http, trivy, smoke]` | Execution order is fixed regardless of how they are listed. |
| `httpPath` | `/` | Immutable. |
| `expectStatus` | 2xx + 3xx | An empty set would accept nothing; the CRD requires at least one. |
| `timeoutSeconds` | `1800` | The run deadline. Mutable — extending a running check is legitimate. |
| `ttlSeconds` | `7200` | Past it a non-terminal check becomes `Expired`; 24 h later the operator deletes the CR as a backstop. |
| `thresholds.kevMax` | `0` | |
| `thresholds.epssMaxPermille` | `500` | Inclusive maximum, in permille. |

`thresholds` carries a `{}` default on the parent **and** defaults on each leaf.
Both are needed: a typed client serialises the absent struct as `{}`, so the
parent default never fires, and without leaf defaults the thresholds would
silently be `0/0` — the strictest possible setting, failing every image. The
leaves are pointers so an explicit `epssMaxPermille: 0` (zero tolerance) stays
distinguishable from "unset".

## Operating it

Flags (chart values in brackets):

- `--probe-image` [`config.probeImage`] — the http check's image. Needs `curl`
  and a POSIX shell.
- `--cve-enrichment-url` [`config.cveEnrichmentUrl`] — cluster-vision's
  `POST /api/cve/enrichment`. Empty fails the trivy check closed.
- `--metrics-bind-address` [`metrics.port`] — now actually wired into the
  manager. Before v0.9.0 the flag was parsed and ignored, and nothing scraped
  the operator anyway; the chart ships a metrics `Service` and a `ServiceMonitor`
  (`metrics.serviceMonitor.enabled`, on by default).

Metrics, on controller-runtime's registry:

| Metric | Labels | Notes |
| --- | --- | --- |
| `previewcheck_total` | `app`, `phase`, `reason` | Terminal verdicts. Incremented exactly once, after the status write lands. |
| `previewcheck_duration_seconds` | `app` | `startedAt` to the terminal phase. |
| `previewcheck_check_total` | `app`, `check`, `phase` | Individual checks reaching a terminal phase. |
| `previewcheck_active` | — | GaugeFunc over the informer; returns 0 on a List error rather than failing the scrape. |

There is deliberately no `pr` label anywhere: PR numbers are unbounded, and a
per-PR series would leave thousands of dead time series behind.

### Manual end-to-end

Run as the caller's ServiceAccount, against a preview that already exists:

```sh
# 1. Find a rendered preview and its app.
kubectl get ns -l preview-environment=true \
  -o custom-columns=NS:.metadata.name,APP:.metadata.labels.preview-app

# 2. Request a verdict. <n> is the PR number in preview-pr-<n>.
kubectl -n preview-check apply -f - <<'EOF'
apiVersion: preview.homelab.io/v1
kind: PreviewCheck
metadata:
  name: pr-<n>
spec:
  prNumber: <n>
  app: <app>
EOF

# 3. Watch it decide. One check advances per reconcile.
kubectl -n preview-check get previewcheck pr-<n> -w

# 4. Read the detail, including which image trivy actually judged.
kubectl -n preview-check get previewcheck pr-<n> -o yaml | yq '.status'

# 5. The probe and smoke Jobs live in the PREVIEW namespace and survive 900 s
#    after finishing, so their logs are readable after a failure.
kubectl -n preview-pr-<n> get jobs -l preview.homelab.io/preview-check=pr-<n>
kubectl -n preview-pr-<n> logs job/preview-check-<uid12>-http

# 6. Deleting the CR tears the Jobs down (Background propagation, so the pods
#    go with them rather than being orphaned into the preview's quota).
kubectl -n preview-check delete previewcheck pr-<n>
```

Negative runs worth doing at least once before trusting it:

- **`NamespaceGone`** — request a check for a PR number with no preview. It must
  sit in `Pending` and only become `Expired` at the deadline. Then delete a
  preview namespace *after* a check has observed it: that one must go `Failed`.
- **`KEVExceeded`** — point `spec.image` at a tag with a known-exploited CVE.
- **`UnexpectedStatus`** — set `expectStatus: [200]` on an OIDC-fronted app: the
  redirect to the IdP must fail the check, and `status.checks[].details` must
  carry the actual status code.
