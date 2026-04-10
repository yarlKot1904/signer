# Deployment

## Supported Targets

- Docker Compose for local development
- Kubernetes for cluster deployment

Images referenced in Kubernetes manifests use GHCR.

For ArgoCD and Kubernetes, prefer immutable tags such as a release git tag or `sha-<commit>`:

- `ghcr.io/yarlkot1904/signer/uploader:deploy-2026-04-10-5`
- `ghcr.io/yarlkot1904/signer/downloader:deploy-2026-04-10-5`
- `ghcr.io/yarlkot1904/signer/mailer:deploy-2026-04-10-5`
- `ghcr.io/yarlkot1904/signer/signer:deploy-2026-04-10-5`
- `ghcr.io/yarlkot1904/signer/pdfsigner:deploy-2026-04-10-5`

The GitHub Actions workflow publishes:

- `latest` on pushes to `main`
- `sha-<12-char-commit>` on every workflow run
- the git tag name itself on tag pushes such as `deploy-2026-04-10-5`

## Docker Compose

Start the stack:

```powershell
Copy-Item .env.example .env
docker compose up --build
```

Compose services:

- `gateway`
- `minio`
- `minio-init`
- `redis`
- `uploader`
- `downloader`
- `mailer`
- `signer`
- `postgres`
- `rabbitmq`
- `pdfsigner`

Ports:

- `80`: public entry through Nginx
- `9001`: MinIO console
- `15672`: RabbitMQ management UI

Compose-specific notes:

- `gateway` is only used in local Compose mode
- `minio-init` creates the `docs-storage` bucket
- secrets and connection strings are read from `.env`
- internal service ports are not published to the host by default

## Kubernetes

Kubernetes manifests live under `deploy/k8s/`:

- `00-secrets-config.yaml`
- `01-infra.yaml`
- `02-apps.yaml`
- `03-ingress.yaml`
- `04-networkpolicy.yaml`

### Infra components

- Redis
- MinIO
- RabbitMQ
- PostgreSQL
- `minio-init` job

### App components

- `uploader`
- `downloader`
- `mailer`
- `signer`
- `pdfsigner`

### Ingress

Ingress host:

- `signer.local`

Path routing:

- `/` -> `uploader-svc`
- `/download` -> `downloader-svc`
- `/view` -> `downloader-svc`
- `/api/` -> `signer-svc`

## Environment Variables

### Shared Go configuration

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `MINIO_REGION`
- `REDIS_ADDR`
- `HTTP_PORT`
- `RABBIT_URL`
- `DB_DSN`
- `PDFSIGN_URL`
- `MAILER_URL`
- `PUBLIC_BASE_URL`
- `MASTER_KEY_HEX`
- `MAILER_LOG_BODY`
- `HTTP_READ_HEADER_TIMEOUT`
- `HTTP_READ_TIMEOUT`
- `HTTP_WRITE_TIMEOUT`
- `HTTP_IDLE_TIMEOUT`
- `SHUTDOWN_TIMEOUT`
- `DEPENDENCY_TIMEOUT`
- `PDFSIGN_TIMEOUT`
- `UPLOAD_MAX_BYTES`
- `JSON_MAX_BYTES`

### Service usage

`uploader`:

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `REDIS_ADDR`
- `HTTP_PORT`
- `RABBIT_URL`

`downloader`:

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `MINIO_REGION`
- `REDIS_ADDR`
- `HTTP_PORT`
- `DB_DSN`

`signer`:

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `MINIO_REGION`
- `REDIS_ADDR`
- `HTTP_PORT`
- `DB_DSN`
- `RABBIT_URL`
- `PDFSIGN_URL`
- `MAILER_URL`
- `PUBLIC_BASE_URL`
- `MASTER_KEY_HEX`

`mailer`:

- `HTTP_PORT`
- `MAILER_LOG_BODY`

`pdfsigner`:

- `PDFSIGNER_MAX_FILE_SIZE`
- `PDFSIGNER_MAX_REQUEST_SIZE`
- `PDFSIGNER_MAX_HEADER_SIZE`

### Infra bootstrap variables

MinIO:

- `MINIO_ROOT_USER`
- `MINIO_ROOT_PASSWORD`

PostgreSQL:

- `POSTGRES_USER`
- `POSTGRES_PASSWORD`
- `POSTGRES_DB`

RabbitMQ:

- `RABBITMQ_DEFAULT_USER`
- `RABBITMQ_DEFAULT_PASS`

## Persistence

Docker volumes:

- `minio_data`
- `postgres_data`

Kubernetes persistent volume claims:

- `minio-pvc`
- `postgres-pvc`

## Operational Notes

- Redis is used as temporary metadata storage, not durable document state.
- PostgreSQL stores signing session state and signed-file metadata.
- Signed PDFs do not replace original PDFs.
- `pdfsigner` exposes `/health` for readiness and liveness probes in Kubernetes.
- Replace placeholder secret values in `00-secrets-config.yaml` before applying manifests.
- Keep `02-apps.yaml` pinned to a published immutable tag or digest for ArgoCD syncs.
- `mailer` currently uses a log transport and logs full message bodies by default for prototype testing.
