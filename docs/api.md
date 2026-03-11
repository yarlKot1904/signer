# API Reference

## Public Routes

The public API is split across `uploader`, `downloader`, and `signer`.

### Upload UI

- `GET /`
  - served by `uploader`
  - returns the browser UI for email entry and PDF upload

### Tus Upload

- `POST /files/`
- `PATCH /files/<id>`
- `HEAD /files/<id>`
  - served by `uploader`
  - handled by tusd
  - accepts one PDF upload from the UI

## Downloader

### GET /download/<token>

Downloads the original file for a valid token.

Responses:

- `200` file stream
- `400` token missing
- `404` token invalid or expired
- `500` storage or database failure

Query parameters:

- `signed=1`
  - switches to signed artifact lookup
  - requires `signed_s3_key` in PostgreSQL

### GET /view/<token>

Same lookup behavior as `/download/<token>`, but uses inline `Content-Disposition`.

Responses:

- `200` file stream
- `400` token missing
- `404` token invalid or expired
- `500` storage or database failure

## Signer

### POST /api/sign

Signs a previously uploaded PDF using the OTP generated for the token.

Request:

```json
{
  "token": "uuid",
  "password": "123456"
}
```

Responses:

- `200`

```json
{
  "status": "success",
  "signed_url": "/download/<token>?signed=1"
}
```

- `400` bad JSON
- `401` invalid OTP
- `403` too many attempts or already signed
- `404` session not found
- `500` signing, storage, or downstream `pdfsigner` failure

### POST /api/verify

Verifies a signed PDF and returns structured JSON.

Input modes:

#### JSON token mode

```json
{
  "token": "uuid"
}
```

#### Multipart upload mode

- content type: `multipart/form-data`
- field name: `pdf`

Response shape:

```json
{
  "status": "verified | unsigned | invalid_signature | error",
  "signature_present": true,
  "integrity_valid": true,
  "signer_subject": "CN=user@example.com,O=CryptoSigner Demo",
  "signer_cn": "user@example.com",
  "signing_time": "2026-03-11T10:15:30Z",
  "certificate_self_signed": true,
  "certificate_trusted": null,
  "error": null
}
```

Field meaning:

- `status`
  - `verified`: signature exists and integrity check passed
  - `unsigned`: no signature found
  - `invalid_signature`: signature exists but failed integrity verification
  - `error`: request or internal processing error
- `signature_present`
  - `true` if the PDF contains an embedded signature dictionary
- `integrity_valid`
  - `true` only when the cryptographic signature validates
- `signer_subject`
  - signer certificate subject DN if available
- `signer_cn`
  - signer certificate common name if available
- `signing_time`
  - signing time from the PDF signature dictionary if present
- `certificate_self_signed`
  - `true` when the embedded signer certificate is self-signed
- `certificate_trusted`
  - always `null` in this prototype because no CA trust-store validation is performed
- `error`
  - human-readable error message when relevant

Responses:

- `200` verification result, including unsigned or invalid-signature documents
- `400` bad JSON, bad multipart payload, missing token, missing file, or malformed PDF upload
- `404` token not found, expired token, or missing signed artifact in token mode
- `500` internal storage, lookup, or downstream verification failure

Examples:

```powershell
curl.exe -s -X POST http://localhost/api/verify `
  -H "Content-Type: application/json" `
  -d "{\"token\":\"<token>\"}"
```

```powershell
curl.exe -s -X POST http://localhost/api/verify `
  -F "pdf=@C:\path\to\signed.pdf;type=application/pdf"
```

## Internal pdfsigner Routes

These routes are intended for internal service-to-service traffic.

### GET /health

Returns:

```json
{
  "status": "ok"
}
```

### POST /sign

Multipart fields:

- `pdf`: PDF bytes
- `certPem`: signer certificate PEM
- `keyPem`: signer private key PEM

Returns:

- `200` signed PDF bytes

### POST /verify

Multipart fields:

- `pdf`: PDF bytes

Returns:

- `200` verification JSON for signed, unsigned, or invalid-signature PDFs
- `400` malformed or unreadable PDF

## Error Notes

- Verification intentionally separates cryptographic validity from certificate trust.
- Self-signed certificates are expected for this prototype.
- `downloader` never creates or mutates signing state.
