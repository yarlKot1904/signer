# Architecture

## Overview

Signer is split into five application services and a small set of infrastructure dependencies.

The design keeps service boundaries strict:

- `uploader` owns uploads and token creation
- `downloader` owns file retrieval only
- `signer` owns signing orchestration, OTP validation, and public API endpoints
- `mailer` owns notification composition and delivery
- `pdfsigner` owns PDF cryptography and PDF transformation

## Service Map

### uploader

Responsibilities:

- serves the static upload UI
- accepts tus uploads at `/files/`
- stores original PDFs in MinIO
- moves uploaded objects into a `YYYY/MM/...` key layout
- creates a UUID token
- writes token metadata to Redis with a 24-hour TTL
- publishes signing tasks to RabbitMQ

Outbound dependencies:

- MinIO
- Redis
- RabbitMQ

### downloader

Responsibilities:

- serves `GET /download/<token>`
- serves `GET /view/<token>`
- switches to signed artifact mode when `?signed=1` is provided

Outbound dependencies:

- Redis for token metadata
- PostgreSQL for `signed_s3_key`
- MinIO for file bytes

### signer

Responsibilities:

- consumes RabbitMQ signing tasks
- generates OTP sessions in PostgreSQL
- calls `mailer` with OTP and document links
- calls `mailer` again with the signed-document link after successful signing
- validates OTP submissions via `POST /api/sign`
- generates RSA-2048 key pairs and self-signed X.509 certificates
- encrypts the generated private key with AES-GCM using `MASTER_KEY_HEX`
- fetches and stores PDFs in MinIO
- delegates signing and verification to `pdfsigner`
- exposes `POST /api/verify`

Outbound dependencies:

- RabbitMQ
- PostgreSQL
- Redis
- MinIO
- `mailer`
- `pdfsigner`

### mailer

Responsibilities:

- accepts internal notification requests from `signer`
- renders OTP and link messages from templates
- dispatches messages through a transport abstraction
- dispatches through SMTP when `MAILER_TRANSPORT=smtp` and SMTP settings are configured
- still supports a log transport for prototype delivery

### pdfsigner

Responsibilities:

- accepts multipart PDF signing requests
- applies a visible stamp on the last page
- creates a detached CMS PDF signature
- verifies embedded signatures in PDFs produced by this system

Libraries:

- Apache PDFBox
- BouncyCastle
- Spring Boot

## Infrastructure

### Redis

- Key pattern: `doc:<token>`
- Purpose: temporary token metadata
- TTL: 24 hours

### PostgreSQL

- Purpose: durable signing session state
- Main fields:
  - `token`
  - `email`
  - `code_hash`
  - `s3_key`
  - `attempts`
  - `is_used`
  - `notification_sent_at`
  - `encrypted_priv_key`
  - `cert_pem`
  - `signed_s3_key`
  - `signed_at`

### MinIO

- Bucket: `docs-storage`
- Original object path: `YYYY/MM/<tus-key>`
- Signed object path: `signed/YYYY/MM/<tus-key>`

### RabbitMQ

- Queue: `signer.tasks`
- Payload:

```json
{
  "token": "uuid",
  "email": "user@example.com",
  "s3_key": "2026/03/upload-key"
}
```

## Routing

### Docker Compose

The `gateway` Nginx container routes:

- `/` to `uploader`
- `/download/` to `downloader`
- `/view/` to `downloader`
- `/api/` to `signer`

### Kubernetes

Traefik Ingress routes `signer.local`:

- `/` to `uploader-svc`
- `/download` to `downloader-svc`
- `/view` to `downloader-svc`
- `/api/` to `signer-svc`

## End-to-End Signing Flow

1. User uploads one PDF through the upload UI.
2. `uploader` stores the raw tus object in MinIO.
3. `uploader` moves it to `YYYY/MM/<tus-key>`.
4. `uploader` stores token metadata in Redis under `doc:<token>`.
5. `uploader` publishes a task to `signer.tasks`.
6. `signer` worker creates a PostgreSQL signing session with a bcrypt-hashed OTP.
7. `signer` calls `mailer` to deliver the OTP and links.
8. User submits the OTP to `POST /api/sign`.
9. `signer` generates a self-signed certificate and RSA key pair.
10. `signer` calls `pdfsigner /sign`.
11. `pdfsigner` stamps and signs the PDF.
12. `signer` stores the signed PDF under `signed/<original-key>`.
13. `signer` calls `mailer` with signed download and preview links.
14. `downloader` serves the signed file through `/download/<token>?signed=1`.

## End-to-End Verification Flow

Verification by token:

1. Client calls `POST /api/verify` with `{ "token": "..." }`.
2. `signer` confirms token metadata exists in Redis.
3. `signer` reads `signed_s3_key` from PostgreSQL.
4. `signer` fetches the signed PDF from MinIO.
5. `signer` posts the PDF to `pdfsigner /verify`.
6. `pdfsigner` checks whether a signature exists, validates integrity, extracts signer details, and returns JSON.

Verification by upload:

1. Client uploads a PDF through Tus to `/verify-files/`.
2. `uploader` stores the temporary object under `verify/...` and writes `verify:<upload_token>` metadata to Redis.
3. Client calls `POST /api/verify` with `{ "upload_token": "..." }`.
4. `signer` loads the temporary object from MinIO.
5. `signer` forwards the file bytes to `pdfsigner /verify`.
6. `signer` deletes the temporary object and `.info` sidecar after verification.

## Architectural Rules

- Do not move Redis metadata into PostgreSQL without an explicit design change.
- Do not make `downloader` responsible for signing or verification state mutation.
- Do not remove RabbitMQ from the signing flow without an explicit redesign.
- Do not replace cryptographic signing with a visual-only stamp.
- Do not assume self-signed certificates are trusted by external validators.
