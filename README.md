# Preview Operator

Kubernetes operator that automatically configures preview environments for applications.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                     PREVIEW SYSTEM                               │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ResourceSet (flux-operator)                                     │
│  ├── Namespace (label: preview-environment=true)                 │
│  ├── GitRepository (PR sha)                                      │
│  ├── Kustomization (label: preview-environment=true)             │
│  ├── HTTPRoute                                                   │
│  └── Basic secrets (github-token, cluster-vars)                  │
│                                                                  │
│  Preview Operator (this operator)                                │
│  └── Watches: Kustomizations with preview-environment=true       │
│      │                                                           │
│      ├── Discovers from production namespace:                    │
│      │   ├── OIDCClient → clone with preview redirect URI        │
│      │   └── postgres.cnpg.io annotations → CNPG from snapshot   │
│      │                                                           │
│      ├── Reads preview.yaml from app directory:                  │
│      │   ├── redis.enabled → create Redis instance               │
│      │   ├── storage.enabled → create s3proxy                    │
│      │   └── envMapping → patch deployment                       │
│      │                                                           │
│      └── Creates/patches resources in preview namespace          │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

## How It Works

1. **Trigger**: ResourceSet creates a Flux Kustomization with `preview-environment=true` label
2. **Discovery**: Operator watches these Kustomizations and:
   - Reads `preview.yaml` from the app's directory in git
   - Discovers OIDC config from existing OIDCClient in production namespace
   - Discovers database needs from namespace annotations
3. **Setup**: Creates required resources:
   - Preview-specific OIDCClient (cloned from production with new redirect URI)
   - CNPG Cluster from VolumeSnapshot (if app uses PostgreSQL)
   - Redis instance (if enabled in preview.yaml)
   - S3 proxy (if enabled in preview.yaml)
4. **Patch**: Updates Deployment/HelmRelease with preview-specific environment variables

## preview.yaml Schema

Apps define their preview requirements in `preview.yaml`:

```yaml
apiVersion: preview.homelab.io/v1
kind: PreviewConfig
spec:
  # How the app is deployed: "deployment" or "helm"
  deploymentType: deployment

  # Redis requirements
  redis:
    enabled: true

  # S3/object storage requirements
  storage:
    enabled: true
    bucket: myapp

  # Services to share with production (not isolated per preview)
  sharedServices:
    - litellm

  # For Deployment: env var names to inject
  envMapping:
    databaseHost: DATABASE_HOST
    redisHost: REDIS_HOST
    s3Endpoint: S3_ENDPOINT_URL
    oidcClientId: OAUTH_CLIENT_ID
    appUrl: APP_URL

  # For HelmRelease: value paths to patch
  helmValues:
    databaseHost: app.config.database.host
    redisHost: app.config.redis.host
```

### Config volumes on NFS

`configVolume` clones a Ceph block PVC by snapshot (`mode: snapshot`, the default). NFS has no
snapshots, so an NFS-backed config PVC uses `mode: nfs`: the preview runs on a temp dir
(`emptyDir`) that is empty by default and seeded from the share only on request. The share is
never cloned or written.

```yaml
spec:
  configVolume:
    claimName: myapp-config
    mode: nfs
    nfs:               # allowed only with mode: nfs
      copy: false      # default false: start empty. true: seed from the share, read-only
      sizeLimit: 256Mi # emptyDir sizeLimit (default 256Mi)
```

The swap is a patch on the preview Kustomization, so a Flux reconcile keeps it. It supports
`deploymentType: deployment` only; `helm` is refused. With `copy: true`, a `config-seed`
initContainer mounts the export of the production PVC's PV read-only, copies it into the temp
dir, rewrites the same URLs as snapshot mode and chowns the files to the app's uid/gid. A seed
failure shows as `Init:Error`; read it with `kubectl logs <pod> -c config-seed`.

Caveats:

- A root-squashed export fails the seed on any file root cannot read. Use `copy: false`.
- Every pod (re)start re-reads the whole share over the node uplink (about 90 s per GiB); the
  temp dir is ephemeral by design.
- `copy: true` puts production config in a PR preview namespace, so it must hold no secrets;
  the rewrite only touches URLs.
- An image whose non-root uid comes only from its `USER` needs `runAsUser` set in the
  Deployment for the seed to chown to it.
- The Deployment's volume is replaced under its original name. Renaming that volume in the
  production Deployment changes what the patch targets.
- The operator needs `get` (and `list`/`watch` for its cache) on `persistentvolumes`,
  cluster-wide and read-only.

## PR Comments

When a preview environment is ready, the operator posts (and later updates) a
comment carrying the preview URL on the pull request. Two forges are supported:

| Provider | API | Notes |
|----------|-----|-------|
| `github` | `https://api.github.com` | Default. Set `--git-api-base-url` for GitHub Enterprise |
| `gitea` | `<host>/api/v1` | Gitea and Forgejo (a Gitea fork serving the same API) |

For a self-hosted Forgejo, `--git-provider=gitea` is usually the only flag
needed: the API host is derived from the Flux `GitRepository` the preview
Kustomization syncs from (e.g. `https://git.example.com/owner/repo.git` →
`https://git.example.com/api/v1`). Set `--git-api-base-url` explicitly when the
API lives somewhere else.

The API token is read from the `github-token` secret in the preview namespace
(the name is historical — Forgejo tokens go in the same place), under either the
`password` or the `token` key.

## Requirements

- Flux CD (Kustomization controller, Source controller)
- OIDCClient CRD (security.homelab.io)
- CNPG Operator (if apps use PostgreSQL)
- Redis Operator (redis.redis.opstreelabs.in)

## Development

```bash
# Run locally
make run

# Build docker image
make docker-build

# Deploy to cluster
make deploy
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--preview-domain` | `daddyshome.fr` | Domain for preview URLs |
| `--github-repo` | `fredericrous/homelab` | Repository (`owner/repo`) PR comments are posted to; empty disables commenting |
| `--git-provider` | `github` | Forge API flavour: `github` or `gitea` (Forgejo speaks the Gitea API) |
| `--git-api-base-url` | _(empty)_ | Forge API base URL. Empty means `https://api.github.com` for `github`; for `gitea` it is derived from the Flux GitRepository the preview syncs from |
| `--leader-elect` | `false` | Enable leader election |
| `--metrics-bind-address` | `:8080` | Metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Health probe endpoint |

