# MigrationCheck

A `MigrationCheck` provisions a throwaway CNPG clone of a production database
from a snapshot, so an app's migrations can be run against **real data before
merge**. It has two modes.

## Push (legacy)

A CI runner inside the cluster creates the CR, polls `status.phase` until
`Ready`, reads `DATABASE_URL` from `status.connectionSecretName`, runs the app
itself and deletes the CR. The operator only provisions and tears down.

```yaml
apiVersion: preview.homelab.io/v1
kind: MigrationCheck
metadata:
  name: cv-12345
  namespace: migration-check
spec:
  appName: cluster-vision
  ttlSeconds: 1800
```

## Pull

With `spec.probe` the operator runs the app under test against the clone and
turns the CR into a verdict; with `spec.report` it publishes that verdict as a
GitHub check run on the PR's head commit. Nothing outside the cluster needs
credentials or API access: a Flux `ResourceSetInputProvider` creates one CR per
labelled pull request, and the operator does the rest.

```yaml
apiVersion: preview.homelab.io/v1
kind: MigrationCheck
metadata:
  name: duro-pr-118-4543533
  namespace: migration-check
spec:
  appName: duro
  ttlSeconds: 3600
  readyTimeoutSeconds: 1500
  probe:
    image: ghcr.io/fredericrous/duro-app:pr-4543533…
    port: 3000
    readyPath: /health/ready
    expect: '"status":"ready"'
    timeoutSeconds: 600
    runAsUser: 1001   # required when the image sets USER by name
    env:
      SESSION_SECRET: ci-dummy
  report:
    provider: github
    repo: fredericrous/duro-app
    revision: 4543533…
    name: Migration check (prod-data clone)
```

### How the probe runs

Once the clone is `Ready`, the operator creates a Job **in the CR's namespace**:

- a `wait-db` init container running `pg_isready` from the clone's own CNPG
  image until it succeeds six times in a row (a restored instance goes
  unready again briefly after the operator sees it Ready; the app must not
  boot into that window and cache the failure);
- an `app` init container with `restartPolicy: Always` (a native sidecar)
  running `probe.image` with its own entrypoint (or `probe.command`/`args`
  when the image's default command starts more than the server under test),
  `DATABASE_URL` injected from
  the connection Secret the operator published, plus `probe.env` as plain
  values;
- a `probe` container (the operator's `--probe-image`, curl) polling
  `http://127.0.0.1:<port><readyPath>` every 2 s for up to `timeoutSeconds`,
  exiting 0 on the first 2xx whose body contains `expect`.

The Job's exit code is the verdict; the log tail is detail in
`status.message` and in the check run output. The app's own migration logs
land there too, which is how a failing migration names itself on the PR.

### Phases

| phase | meaning | check run |
|---|---|---|
| `Pending`, `Provisioning` | clone being restored | `in_progress` |
| `Ready` | clone accepts connections (push flow parks here) | `in_progress` |
| `Running` | probe Job in flight | `in_progress` |
| `Passed` | the app booted against the clone and answered | `success` |
| `Failed` | the app did not come up: the change was judged bad | `failure` |
| `Expired` | never judged: clone did not settle within `readyTimeoutSeconds`, image could not be pulled, probe ran out of time, or the TTL left no budget | `timed_out` |

`status.reason` carries the machine-readable cause (`CloneNotReady`,
`ProbeFailed`, `ImageUnavailable`, `PodNotStarted`, `DeadlineExceeded`,
`TTLTooShort`, …). `PodNotStarted` is the kubelet refusing a container, most
often "image has non-numeric user" under `runAsNonRoot`: set `probe.runAsUser`
to the image's uid (or make the Dockerfile's `USER` numeric).
Deleting a CR before a verdict marks its check run `cancelled`.

### Security model

- **`DATABASE_URL` reaches the Job only as a `secretKeyRef` the operator
  builds** from its own connection Secret. `probe.env` accepts plain values
  only (no `valueFrom`), and the CRD rejects `DATABASE_URL` in it.
- **The operator only runs probe Jobs in a namespace labelled
  `preview.homelab.io/migration-check-jobs: "true"`.** A CR created anywhere
  else cannot make the operator run a container there.
- **The GitHub App credentials are chosen by the operator**
  (`--github-app-secret=namespace/name`, Flux's `githubAppID` /
  `githubAppInstallationID` / `githubAppPrivateKey` keys), read at call time,
  never named by a CR. `--check-run-repo-prefix` bounds which repositories a CR
  may report to.
- The probe pod mounts no service account token, runs non-root with
  `RuntimeDefault` seccomp and all capabilities dropped.

### Flags

| flag | default | purpose |
|---|---|---|
| `--probe-image` | `curlimages/curl:8.11.1` | the probe container (shared with PreviewCheck) |
| `--github-app-secret` | empty (reporting off) | `namespace/name` of the App credentials Secret |
| `--github-api-base-url` | `https://api.github.com` | GitHub API base |
| `--check-run-repo-prefix` | empty (any) | allowed `spec.report.repo` prefix, e.g. `owner/` |

Chart values: `config.githubAppSecret`, `config.githubApiBaseUrl`,
`config.checkRunRepoPrefix`.
