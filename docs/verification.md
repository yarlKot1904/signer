# PDF Verification

The public verification endpoint is `POST /api/verify` on the `signer` service.

There is also a browser UI at `/verify.html` served by the existing static frontend.

`signer` owns the public API and token lookup. It delegates PDF signature parsing and integrity verification to `pdfsigner`, which already owns PDFBox/BouncyCastle signing logic.

## Input modes

Verify by token:

```powershell
curl.exe -s -X POST http://localhost/api/verify `
  -H "Content-Type: application/json" `
  -d "{\"token\":\"<token>\"}"
```

Verify by uploaded PDF:

```powershell
curl.exe -s -X POST http://localhost/api/verify `
  -F "pdf=@C:\path\to\signed.pdf;type=application/pdf"
```

## Response

Successful verification responses always return JSON. Unsigned PDFs and cryptographically invalid signatures still return `200` with a result payload.

Example:

```json
{
  "status": "verified",
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

Status values:

- `verified`: signature exists and integrity verification passed
- `unsigned`: no PDF signature dictionary was found
- `invalid_signature`: signature exists but integrity verification failed
- `error`: request or internal processing error

Notes:

- `certificate_trusted` is always `null` in this prototype. The system does not perform external CA trust-chain validation.
- `certificate_self_signed=true` is expected for PDFs signed by this project.

## End-to-end verification steps

1. Upload a PDF through the existing uploader UI.
2. Read the OTP from `signer` logs in prototype mode.
3. Sign via `POST /api/sign`.
4. Download the result from `/download/<token>?signed=1`.
5. Verify the signed PDF by token and by uploaded file.
6. Verify the original unsigned PDF and confirm it returns `status=unsigned`.
7. Modify the signed PDF and verify it again to confirm `integrity_valid=false`.

Optional external confirmation:

```powershell
pdfsig C:\path\to\signed.pdf
```
