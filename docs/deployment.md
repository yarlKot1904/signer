# Deployment

## Supported Targets

- Docker Compose for local development
- Kubernetes for cluster deployment

Images referenced in Kubernetes manifests use GHCR.

For ArgoCD and Kubernetes, prefer immutable tags such as a release git tag or `sha-<commit>`:

- `ghcr.io/yarlkot1904/signer/uploader:deploy-2026-04-15-1`
- `ghcr.io/yarlkot1904/signer/downloader:deploy-2026-04-15-1`
- `ghcr.io/yarlkot1904/signer/mailer:deploy-2026-04-15-1`
- `ghcr.io/yarlkot1904/signer/signer:deploy-2026-04-15-1`
- `ghcr.io/yarlkot1904/signer/pdfsigner:deploy-2026-04-15-1`

The GitHub Actions workflow publishes:

- `latest` on pushes to `main`
- `sha-<12-char-commit>` on every workflow run
- the git tag name itself on tag pushes such as `deploy-2026-04-15-1`

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
- `redis-exporter`
- `postgres-exporter`
- `prometheus`
- `grafana`

Ports:

- `80`: public entry through Nginx
- `9001`: MinIO console
- `15672`: RabbitMQ management UI
- `9090`: Prometheus
- `3000`: Grafana

Compose-specific notes:

- `gateway` is only used in local Compose mode
- `minio-init` creates the `docs-storage` bucket
- secrets and connection strings are read from `.env`
- internal service ports are not published to the host by default
- application `/metrics` endpoints stay on internal port `9100`
- `pdfsigner` Prometheus metrics are exposed internally on `8091`

## Kubernetes

Kubernetes manifests live under `deploy/k8s/`:

- `00-secrets-config.yaml`
- `01-infra.yaml`
- `02-apps.yaml`
- `03-ingress.yaml`
- `04-networkpolicy.yaml`
- `05-monitoring.yaml`

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

### Monitoring components

- Prometheus
- Grafana
- Redis exporter
- PostgreSQL exporter

### Ingress

Ingress host:

- `signer.local`
- `grafana.signer.local`

Path routing:

- `/` -> `uploader-svc`
- `/download` -> `downloader-svc`
- `/view` -> `downloader-svc`
- `/api/` -> `signer-svc`

Grafana is exposed at `grafana.signer.local`. Prometheus stays internal as a ClusterIP service.

## Environment Variables

### Shared Go configuration

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `MINIO_REGION`
- `REDIS_ADDR`
- `HTTP_PORT`
- `METRICS_PORT`
- `RABBIT_URL`
- `DB_DSN`
- `PDFSIGN_URL`
- `MAILER_URL`
- `PUBLIC_BASE_URL`
- `MASTER_KEY_HEX`
- `MAILER_TRANSPORT`
- `MAILER_LOG_BODY`
- `SMTP_HOST`
- `SMTP_PORT`
- `SMTP_USERNAME`
- `SMTP_PASSWORD`
- `SMTP_FROM`
- `SMTP_TLS_MODE`
- `SMTP_SERVER_NAME`
- `HTTP_READ_HEADER_TIMEOUT`
- `HTTP_READ_TIMEOUT`
- `HTTP_WRITE_TIMEOUT`
- `HTTP_IDLE_TIMEOUT`
- `SHUTDOWN_TIMEOUT`
- `DEPENDENCY_TIMEOUT`
- `PDFSIGN_TIMEOUT`
- `UPLOAD_MAX_BYTES`
- `JSON_MAX_BYTES`
- `POSTGRES_EXPORTER_DSN`
- `GRAFANA_ADMIN_USER`
- `GRAFANA_ADMIN_PASSWORD`

### Service usage

`uploader`:

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `REDIS_ADDR`
- `HTTP_PORT`
- `METRICS_PORT`
- `RABBIT_URL`

`downloader`:

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `MINIO_REGION`
- `REDIS_ADDR`
- `HTTP_PORT`
- `METRICS_PORT`
- `DB_DSN`

`signer`:

- `MINIO_ENDPOINT`
- `MINIO_ID`
- `MINIO_SECRET`
- `MINIO_BUCKET`
- `MINIO_REGION`
- `REDIS_ADDR`
- `HTTP_PORT`
- `METRICS_PORT`
- `DB_DSN`
- `RABBIT_URL`
- `PDFSIGN_URL`
- `MAILER_URL`
- `PUBLIC_BASE_URL`
- `MASTER_KEY_HEX`

`mailer`:

- `HTTP_PORT`
- `METRICS_PORT`
- `MAILER_TRANSPORT`
- `MAILER_LOG_BODY`
- `SMTP_HOST`
- `SMTP_PORT`
- `SMTP_USERNAME`
- `SMTP_PASSWORD`
- `SMTP_FROM`
- `SMTP_TLS_MODE`
- `SMTP_SERVER_NAME`
- `DEPENDENCY_TIMEOUT`

### Mailer SMTP

The Kubernetes manifests are configured for Mail.ru SMTP delivery:

- `MAILER_TRANSPORT=smtp`
- `SMTP_HOST=smtp.mail.ru`
- `SMTP_PORT=465`
- `SMTP_TLS_MODE=implicit`
- `SMTP_SERVER_NAME` only when the TLS certificate name differs from `SMTP_HOST`

SMTP identity and credentials are secret values:

- `SMTP_USERNAME`
- `SMTP_PASSWORD`
- `SMTP_FROM`

In Kubernetes, the mailer deployment reads these values from the `mailer-smtp-secrets` Secret. That Secret is intentionally not defined in the Git manifests, so the real mailbox address and SMTP credentials do not need to be committed to the repository.

For an ArgoCD-managed cluster, create or update the live Secret outside the Git-tracked manifests:

```powershell
kubectl -n default create secret generic mailer-smtp-secrets `
  --from-literal=SMTP_USERNAME="<your-mailbox@mail.ru>" `
  --from-literal=SMTP_PASSWORD="<external-app-password>" `
  --from-literal=SMTP_FROM="Signer <your-mailbox@mail.ru>" `
  --dry-run=client -o yaml | kubectl apply -f -
```

For Mail.ru, use a password for an external application, not the normal mailbox password. ArgoCD will not overwrite this object unless it is added to the application manifests or given ArgoCD tracking labels. Commit only the non-secret SMTP settings in `signer-config`, or use an encrypted secret workflow such as External Secrets, Sealed Secrets, or SOPS if the Secret must be managed through GitOps.

`pdfsigner`:

- `PDFSIGNER_MAX_FILE_SIZE`
- `PDFSIGNER_MAX_REQUEST_SIZE`
- `PDFSIGNER_MAX_HEADER_SIZE`
- `PDFSIGNER_MANAGEMENT_PORT`

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
- `prometheus_data`
- `grafana_data`

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
- `mailer` supports log transport for prototype testing, but the Kubernetes manifests use Mail.ru SMTP and disable full body logging by default.
