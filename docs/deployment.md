# Deployment

## Supported Targets

- Docker Compose for local development
- Kubernetes for cluster deployment

Images referenced in Kubernetes manifests use GHCR:

- `ghcr.io/yarlkot1904/signer/uploader:latest`
- `ghcr.io/yarlkot1904/signer/downloader:latest`
- `ghcr.io/yarlkot1904/signer/signer:latest`
- `ghcr.io/yarlkot1904/signer/pdfsigner:latest`

## Docker Compose

Start the stack:

```powershell
docker compose up --build
```

Compose services:

- `gateway`
- `minio`
- `minio-init`
- `redis`
- `uploader`
- `downloader`
- `signer`
- `postgres`
- `rabbitmq`
- `pdfsigner`

Ports:

- `80`: public entry through Nginx
- `8082`: direct access to `signer`
- `8090`: direct access to `pdfsigner`
- `9000`: MinIO API
- `9001`: MinIO console
- `5432`: PostgreSQL
- `5672`: RabbitMQ AMQP
- `15672`: RabbitMQ management UI

Compose-specific notes:

- `gateway` is only used in local Compose mode
- `minio-init` creates the `docs-storage` bucket
- `MASTER_KEY_HEX` must be replaced before signing

## Kubernetes

Kubernetes manifests live under `deploy/k8s/`:

- `00-secrets-config.yaml`
- `01-infra.yaml`
- `02-apps.yaml`
- `03-ingress.yaml`

### Infra components

- Redis
- MinIO
- RabbitMQ
- PostgreSQL
- `minio-init` job

### App components

- `uploader`
- `downloader`
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
- `MASTER_KEY_HEX`

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
- `MASTER_KEY_HEX`

`pdfsigner`:

- no custom application env vars are currently required

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
