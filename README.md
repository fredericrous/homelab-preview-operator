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
| `--leader-elect` | `false` | Enable leader election |
| `--metrics-bind-address` | `:8080` | Metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Health probe endpoint |
