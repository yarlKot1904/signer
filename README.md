# Signer

Signer is a microservice-based PDF signing platform.

It accepts one PDF upload, issues a temporary token, creates an OTP-backed signing session, applies a detached CMS signature plus a visible stamp, stores both the original and signed files, and exposes download, preview, and verification endpoints.

## Documentation

- [Architecture](docs/architecture.md)
- [API](docs/api.md)
- [Deployment](docs/deployment.md)
- [Development](docs/development.md)
- [Verification](docs/verification.md)

## Services

- `uploader` (Go): static UI, tus upload handling, MinIO storage, Redis token metadata, RabbitMQ task publish
- `downloader` (Go): download and inline preview by token, with signed-file lookup through PostgreSQL
- `signer` (Go): RabbitMQ worker plus `/api/*` endpoints for signing and verification
- `pdfsigner` (Kotlin/Spring Boot): PDFBox/BouncyCastle service for signing and verification
- `gateway` (Nginx, Docker Compose only): reverse proxy for local development

## Core Flow

1. User uploads one PDF through the `uploader` UI.
2. `uploader` stores the file in MinIO and creates a temporary token in Redis.
3. `uploader` publishes a signing task to RabbitMQ.
4. `signer` consumes the task, creates a PostgreSQL signing session, and logs the OTP in prototype mode.
5. User signs through `POST /api/sign`.
6. `signer` generates a self-signed certificate and private key, asks `pdfsigner` to sign the PDF, and stores the signed result in MinIO.
7. `downloader` serves the original or signed file by token.
8. `signer` exposes `POST /api/verify`, which delegates cryptographic verification to `pdfsigner`.

## Quick Start

Prerequisites:

- Docker and Docker Compose

Start the full stack:

```powershell
docker compose up --build
```

Open:

- App UI: `http://localhost/`
- Signer API: `http://localhost/api/`
- MinIO console: `http://localhost:9001/`
- RabbitMQ management: `http://localhost:15672/`

Important local defaults:

- MinIO user/password: `minioadmin` / `minioadmin`
- RabbitMQ user/password: `user` / `password`
- PostgreSQL DSN: `postgres://user:password@postgres:5432/signer_db`
- MinIO bucket: `docs-storage`

Before first real signing, replace the placeholder `MASTER_KEY_HEX` in `docker-compose.yml` with a 64-character hex key.

Example PowerShell command:

```powershell
[Convert]::ToHexString((1..32 | ForEach-Object { Get-Random -Minimum 0 -Maximum 256 }))
```

## Public Endpoints

- `POST /files/` via tus upload through `uploader`
- `GET /download/<token>`
- `GET /view/<token>`
- `POST /api/sign`
- `POST /api/verify`

See [docs/api.md](docs/api.md) for request and response details.

## Storage and Messaging

- Redis
  - Key pattern: `doc:<token>`
  - Stores temporary file metadata
  - TTL: 24 hours
- PostgreSQL
  - Table/model: `signing_sessions`
  - Stores OTP state and signed artifact metadata
- MinIO
  - Bucket: `docs-storage`
  - Original object key: `YYYY/MM/<tus-key>`
  - Signed object key: `signed/YYYY/MM/<tus-key>`
- RabbitMQ
  - Queue: `signer.tasks`
  - Message: `{ "token": "...", "email": "...", "s3_key": "..." }`

## Prototype Constraints

These behaviors are intentional in the current prototype:

- OTP is logged by the `signer` worker
- Certificates are self-signed and not externally trusted
- Token links are possession-based
- Redis metadata expires after 24 hours
- The private key is encrypted with AES-GCM using `MASTER_KEY_HEX`

## Repository Layout

```text
cmd/
  uploader/
  downloader/
  signer/
deploy/
  docker/
  k8s/
docs/
internal/
  config/
  infra/
pdfsigner/
static/
```

## Validation

Go:

```powershell
go test ./...
```

Kotlin compile checks:

```powershell
Set-Location .\pdfsigner
.\gradlew.bat --no-daemon compileKotlin compileTestKotlin
```

Verification examples are documented in [docs/verification.md](docs/verification.md).
