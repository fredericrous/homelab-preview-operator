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

