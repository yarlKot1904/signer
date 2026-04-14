# Metrics

This document defines the recommended application metrics for Signer. Use Prometheus naming and keep labels low-cardinality.

Do not label metrics with email addresses, tokens, S3 object keys, OTP codes, request bodies, or document UUIDs. Use logs with masked values for tracing those flows.

## Common Labels

Use these labels consistently where they apply:

- `service`: `uploader`, `downloader`, `signer`, `mailer`, or `pdfsigner`
- `route`: stable route template such as `/api/sign`, `/download/{token}`, or `/files/`
- `method`: HTTP method
- `status_class`: `2xx`, `4xx`, `5xx`
- `result`: `success`, `error`, `timeout`, `not_found`, `invalid`, or a service-specific bounded value
- `operation`: bounded dependency operation such as `redis_get`, `s3_put`, `pdfsign`, `smtp_send`
- `template`: mail template name, currently `signing-otp` or `signed-document`
- `mode`: verification mode, currently `token` or `upload`

## HTTP Metrics

These should exist on every HTTP service.

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_http_requests_total` | Counter | `service`, `route`, `method`, `status_class` | Request volume and error rate per public/internal route. |
| `signer_http_request_duration_seconds` | Histogram | `service`, `route`, `method`, `status_class` | API latency and SLO tracking. |
| `signer_http_request_body_bytes` | Histogram | `service`, `route` | Detect unexpectedly large uploads or JSON requests. |
| `signer_http_in_flight_requests` | Gauge | `service`, `route` | Identify saturation and slow downstream calls. |

## Uploader

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_upload_completed_total` | Counter | `result` | Count completed Tus uploads and failed finalization. |
| `signer_upload_bytes` | Histogram | none | Track PDF size distribution against `UPLOAD_MAX_BYTES`. |
| `signer_upload_finalize_duration_seconds` | Histogram | `result` | Time from Tus completion to token creation and queue publish. |
| `signer_upload_s3_move_total` | Counter | `result` | Detect MinIO copy/delete failures during key normalization. |
| `signer_token_write_total` | Counter | `result` | Redis `doc:<token>` write success/failure. |
| `signer_token_ttl_seconds` | Histogram | none | Confirm generated token TTL remains near 24 hours. |
| `signer_rabbitmq_publish_total` | Counter | `queue`, `result` | Signing task publish success/failure. |
| `signer_verify_upload_completed_total` | Counter | `result` | Verify upload completion and metadata creation. |
| `signer_verify_upload_cleanup_total` | Counter | `target`, `result` | Cleanup of temporary verify objects and `.info` sidecars. |

## Signer

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_worker_tasks_total` | Counter | `result` | RabbitMQ task consumption and processing outcome. |
| `signer_otp_sessions_created_total` | Counter | `result` | PostgreSQL OTP session creation health. |
| `signer_mailer_notifications_total` | Counter | `template`, `result` | Mailer dispatch outcome from the signer perspective. |
| `signer_sign_requests_total` | Counter | `result` | Signing API outcomes such as success, invalid_code, too_many_attempts, already_signed, and not_found. |
| `signer_otp_attempts_total` | Counter | `result` | OTP validation behavior without exposing codes. |
| `signer_sign_duration_seconds` | Histogram | `result` | End-to-end signing latency inside signer. |
| `signer_key_generation_duration_seconds` | Histogram | `result` | RSA key and self-signed certificate generation latency. |
| `signer_pdfsigner_requests_total` | Counter | `operation`, `result` | Downstream `pdfsigner` request health for sign and verify operations. |
| `signer_pdfsigner_request_duration_seconds` | Histogram | `operation`, `result` | Downstream `pdfsigner` latency. |
| `signer_signed_pdf_store_total` | Counter | `result` | Persistence of `signed/<originalKey>` objects in MinIO. |
| `signer_signed_document_registry_total` | Counter | `result` | PostgreSQL signed document registry writes used by verification. |
| `signer_verify_requests_total` | Counter | `mode`, `status`, `service_owned` | Public verification outcomes. |
| `signer_verify_upload_wait_duration_seconds` | Histogram | `result` | Tus metadata wait/retry behavior for upload verification. |
| `signer_verify_cleanup_total` | Counter | `target`, `result` | Signer-side cleanup of verify object and `.info` sidecar. |

## Mailer

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_mailer_send_requests_total` | Counter | `template`, `transport`, `result` | Mailer API dispatch outcome. |
| `signer_mailer_render_failures_total` | Counter | `template` | Template validation failures. |
| `signer_mailer_send_duration_seconds` | Histogram | `template`, `transport`, `result` | End-to-end render plus transport latency. |
| `signer_mailer_smtp_connect_duration_seconds` | Histogram | `tls_mode`, `result` | SMTP connectivity and TLS negotiation health. |
| `signer_mailer_smtp_auth_total` | Counter | `result` | SMTP authentication failures without exposing usernames. |
| `signer_mailer_log_body_enabled` | Gauge | none | `1` when prototype body logging is enabled, otherwise `0`. |

## Downloader

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_download_requests_total` | Counter | `route`, `signed`, `result` | Original vs signed download/view outcomes. |
| `signer_download_lookup_duration_seconds` | Histogram | `signed`, `result` | Redis and PostgreSQL lookup latency. |
| `signer_download_s3_read_total` | Counter | `signed`, `result` | MinIO read success/failure. |
| `signer_download_s3_read_bytes` | Histogram | `signed` | Served file size distribution. |
| `signer_signed_lookup_missing_total` | Counter | none | Signed-mode requests where `signed_s3_key` is absent. |

## PdfSigner

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_pdfsigner_sign_requests_total` | Counter | `result` | PDF signing outcomes. |
| `signer_pdfsigner_sign_duration_seconds` | Histogram | `result` | Full PDFBox/BouncyCastle signing duration. |
| `signer_pdfsigner_stamp_duration_seconds` | Histogram | `result` | Visual stamp preparation latency. |
| `signer_pdfsigner_signature_duration_seconds` | Histogram | `result` | Detached CMS signature generation latency. |
| `signer_pdfsigner_verify_requests_total` | Counter | `status` | Verification outcomes returned to signer. |
| `signer_pdfsigner_verify_duration_seconds` | Histogram | `status` | PDF verification latency. |
| `signer_pdfsigner_pdf_pages` | Histogram | `operation` | Page count distribution for signing and verification inputs. |

## Dependency Metrics

Prefer client-side metrics in the owning service and official exporters for infrastructure internals.

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `signer_dependency_requests_total` | Counter | `service`, `dependency`, `operation`, `result` | Shared view of Redis, PostgreSQL, MinIO, RabbitMQ, mailer, and pdfsigner calls. |
| `signer_dependency_request_duration_seconds` | Histogram | `service`, `dependency`, `operation`, `result` | Downstream latency per owner service. |
| `rabbitmq_queue_messages_ready` | Gauge | `queue` | Queue backlog from the RabbitMQ exporter. |
| `rabbitmq_queue_messages_unacked` | Gauge | `queue` | Stuck or slow signer worker detection. |
| `redis_connected_clients` | Gauge | none | Redis exporter connection health. |
| `postgres_up` | Gauge | none | PostgreSQL exporter availability. |
| `minio_cluster_usage_object_total` | Gauge | `bucket` | MinIO object growth, especially temporary verify objects. |

## Suggested Alerts

- `POST /api/sign` 5xx rate is above 1% for 10 minutes.
- p95 `signer_sign_duration_seconds` is above 30 seconds for 10 minutes.
- `signer_mailer_notifications_total{result!="success"}` increases for 5 minutes.
- `signer_mailer_smtp_auth_total{result!="success"}` increases after enabling SMTP.
- RabbitMQ `signer.tasks` ready messages stay above 100 for 15 minutes.
- `signer_verify_cleanup_total{result!="success"}` increases, especially for `.info` sidecars.
- `signer_signed_lookup_missing_total` increases after successful signing events.
- `signer_pdfsigner_verify_requests_total{status="error"}` increases for 10 minutes.
