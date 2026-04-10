# Development

## Prerequisites

- Go 1.24 or compatible toolchain
- Java 17 for `pdfsigner`
- Docker and Docker Compose for full-stack local runs

## Repository Layout

```text
cmd/
  uploader/
  downloader/
  mailer/
  signer/
internal/
  config/
  infra/
pdfsigner/
deploy/
  docker/
  k8s/
static/
docs/
```

## Local Build and Test Commands

Go services:

```powershell
go test ./...
```

`pdfsigner` compile:

```powershell
Set-Location .\pdfsigner
.\gradlew.bat --no-daemon compileKotlin compileTestKotlin
```

`pdfsigner` tests:

```powershell
Set-Location .\pdfsigner
.\gradlew.bat test
```

## Running the Full Stack

From the repository root:

```powershell
Copy-Item .env.example .env
docker compose up --build
```

Useful endpoints after startup:

- `http://localhost/`
- `http://localhost/api/sign`
- `http://localhost/api/verify`
- `http://localhost:9001/`
- `http://localhost:15672/`

## Manual End-to-End Scenario

1. Open `http://localhost/`.
2. Enter an email and upload one PDF.
3. Check `mailer` delivery logs for the mock notification flow.
4. If you need the full OTP and links in local debugging, temporarily set `MAILER_LOG_BODY=true` before starting the stack.
5. Submit the OTP to sign the document.
6. Download the signed PDF through `/download/<token>?signed=1`.
7. Verify the result through `/api/verify`.

## Verification Commands

By token:

```powershell
curl.exe -s -X POST http://localhost/api/verify `
  -H "Content-Type: application/json" `
  -d "{\"token\":\"<token>\"}"
```

By uploaded PDF:

```powershell
# First upload the PDF through the browser UI at /verify.html or any Tus client to /verify-files/
# and obtain the upload token used as verifyToken metadata.
curl.exe -s -X POST http://localhost/api/verify `
  -H "Content-Type: application/json" `
  -d "{\"upload_token\":\"<upload-token>\"}"
```

Optional external validation:

```powershell
pdfsig C:\path\to\signed.pdf
```

## Development Notes

- `uploader` should stay stateless apart from external services.
- `downloader` must not create or modify signing state.
- `signer` is the orchestration layer for OTP and signing/verification workflows.
- `mailer` is the internal notification layer for OTP and link delivery.
- `pdfsigner` should remain focused on PDF transformation and cryptographic operations.
- Preserve current prototype behavior unless a task explicitly asks for production hardening.
- `POST /api/verify` accepts JSON only; upload verification uses `/verify-files/` plus `upload_token`.
