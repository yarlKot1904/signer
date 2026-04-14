package metrics

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	longBuckets     = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120}
	byteBuckets     = []float64{64 << 10, 128 << 10, 256 << 10, 512 << 10, 1 << 20, 2 << 20, 4 << 20, 8 << 20, 16 << 20, 32 << 20, 64 << 20}

	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_http_requests_total",
		Help: "Request volume and error rate per route.",
	}, []string{"service", "route", "method", "status_class"})
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_http_request_duration_seconds",
		Help:    "HTTP request duration per route.",
		Buckets: durationBuckets,
	}, []string{"service", "route", "method", "status_class"})
	HTTPRequestBodyBytes = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_http_request_body_bytes",
		Help:    "HTTP request body bytes per route.",
		Buckets: byteBuckets,
	}, []string{"service", "route"})
	HTTPInFlightRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "signer_http_in_flight_requests",
		Help: "In-flight HTTP requests per route.",
	}, []string{"service", "route"})

	UploadCompleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_upload_completed_total",
		Help: "Completed Tus uploads and failed finalization.",
	}, []string{"result"})
	UploadBytes = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "signer_upload_bytes",
		Help:    "Uploaded PDF size distribution.",
		Buckets: byteBuckets,
	})
	UploadFinalizeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_upload_finalize_duration_seconds",
		Help:    "Time from Tus completion to token creation and queue publish.",
		Buckets: longBuckets,
	}, []string{"result"})
	UploadS3Move = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_upload_s3_move_total",
		Help: "MinIO copy/delete outcomes during key normalization.",
	}, []string{"result"})
	TokenWrite = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_token_write_total",
		Help: "Redis token metadata write outcomes.",
	}, []string{"result"})
	TokenTTLSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "signer_token_ttl_seconds",
		Help:    "Generated Redis token TTL in seconds.",
		Buckets: []float64{3600, 21600, 43200, 82800, 86400, 90000},
	})
	RabbitMQPublish = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_rabbitmq_publish_total",
		Help: "RabbitMQ publish outcomes.",
	}, []string{"queue", "result"})
	VerifyUploadCompleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_verify_upload_completed_total",
		Help: "Verify upload completion and metadata creation outcomes.",
	}, []string{"result"})
	VerifyUploadCleanup = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_verify_upload_cleanup_total",
		Help: "Uploader cleanup of temporary verify objects and sidecars.",
	}, []string{"target", "result"})

	WorkerTasks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_worker_tasks_total",
		Help: "RabbitMQ task consumption and processing outcomes.",
	}, []string{"result"})
	OTPSessionsCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_otp_sessions_created_total",
		Help: "PostgreSQL OTP session creation outcomes.",
	}, []string{"result"})
	MailerNotifications = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_mailer_notifications_total",
		Help: "Signer-side mailer notification dispatch outcomes.",
	}, []string{"template", "result"})
	SignRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_sign_requests_total",
		Help: "Signing API outcomes.",
	}, []string{"result"})
	OTPAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_otp_attempts_total",
		Help: "OTP validation outcomes.",
	}, []string{"result"})
	SignDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_sign_duration_seconds",
		Help:    "End-to-end signing latency inside signer.",
		Buckets: longBuckets,
	}, []string{"result"})
	KeyGenerationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_key_generation_duration_seconds",
		Help:    "RSA key and self-signed certificate generation latency.",
		Buckets: longBuckets,
	}, []string{"result"})
	PDFSignerRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_pdfsigner_requests_total",
		Help: "Downstream pdfsigner request outcomes.",
	}, []string{"operation", "result"})
	PDFSignerRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_pdfsigner_request_duration_seconds",
		Help:    "Downstream pdfsigner request latency.",
		Buckets: longBuckets,
	}, []string{"operation", "result"})
	SignedPDFStore = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_signed_pdf_store_total",
		Help: "Signed PDF MinIO persistence outcomes.",
	}, []string{"result"})
	SignedDocumentRegistry = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_signed_document_registry_total",
		Help: "Signed document registry write outcomes.",
	}, []string{"result"})
	VerifyRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_verify_requests_total",
		Help: "Public verification outcomes.",
	}, []string{"mode", "status", "service_owned"})
	VerifyUploadWaitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_verify_upload_wait_duration_seconds",
		Help:    "Tus metadata wait duration for upload verification.",
		Buckets: durationBuckets,
	}, []string{"result"})
	VerifyCleanup = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_verify_cleanup_total",
		Help: "Signer cleanup of verify object and sidecar.",
	}, []string{"target", "result"})

	DownloadRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_download_requests_total",
		Help: "Original and signed download/view outcomes.",
	}, []string{"route", "signed", "result"})
	DownloadLookupDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_download_lookup_duration_seconds",
		Help:    "Redis and PostgreSQL lookup latency.",
		Buckets: durationBuckets,
	}, []string{"signed", "result"})
	DownloadS3Read = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_download_s3_read_total",
		Help: "MinIO read outcomes for downloader.",
	}, []string{"signed", "result"})
	DownloadS3ReadBytes = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_download_s3_read_bytes",
		Help:    "Served file size distribution.",
		Buckets: byteBuckets,
	}, []string{"signed"})
	SignedLookupMissing = promauto.NewCounter(prometheus.CounterOpts{
		Name: "signer_signed_lookup_missing_total",
		Help: "Signed-mode requests where signed_s3_key is absent.",
	})

	MailerSendRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_mailer_send_requests_total",
		Help: "Mailer API dispatch outcomes.",
	}, []string{"template", "transport", "result"})
	MailerRenderFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_mailer_render_failures_total",
		Help: "Mailer template validation failures.",
	}, []string{"template"})
	MailerSendDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_mailer_send_duration_seconds",
		Help:    "End-to-end mail render plus transport latency.",
		Buckets: longBuckets,
	}, []string{"template", "transport", "result"})
	MailerSMTPConnectDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_mailer_smtp_connect_duration_seconds",
		Help:    "SMTP connectivity and TLS negotiation latency.",
		Buckets: durationBuckets,
	}, []string{"tls_mode", "result"})
	MailerSMTPAuth = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_mailer_smtp_auth_total",
		Help: "SMTP authentication outcomes.",
	}, []string{"result"})
	MailerLogBodyEnabled = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "signer_mailer_log_body_enabled",
		Help: "1 when prototype mail body logging is enabled.",
	})
	MailerRenderDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_mailer_render_duration_seconds",
		Help:    "Mailer template render latency.",
		Buckets: durationBuckets,
	}, []string{"template", "result"})
	MailerMessageBytes = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_mailer_message_bytes",
		Help:    "Rendered mail message size.",
		Buckets: byteBuckets,
	}, []string{"template", "transport"})
	MailerSMTPStageTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_mailer_smtp_stage_total",
		Help: "SMTP stage outcomes.",
	}, []string{"stage", "tls_mode", "result"})
	MailerSMTPStageDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_mailer_smtp_stage_duration_seconds",
		Help:    "SMTP stage duration.",
		Buckets: durationBuckets,
	}, []string{"stage", "tls_mode", "result"})
	MailerTransportInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "signer_mailer_transport_info",
		Help: "Mailer transport configuration.",
	}, []string{"transport", "tls_mode"})
	MailerInvalidRecipient = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_mailer_invalid_recipient_total",
		Help: "Invalid mail recipient/from address outcomes.",
	}, []string{"template"})
	MailerNotificationAge = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_mailer_notification_age_seconds",
		Help:    "Notification age at mailer dispatch time.",
		Buckets: longBuckets,
	}, []string{"template", "result"})

	DependencyRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "signer_dependency_requests_total",
		Help: "Dependency request outcomes per owning service.",
	}, []string{"service", "dependency", "operation", "result"})
	DependencyRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "signer_dependency_request_duration_seconds",
		Help:    "Dependency request latency per owning service.",
		Buckets: durationBuckets,
	}, []string{"service", "dependency", "operation", "result"})
)

func StartServer(ctx context.Context, port, serviceName string, shutdownTimeout time.Duration) {
	if port == "" {
		port = "9100"
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = 15 * time.Second
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("%s metrics shutdown failed: %v", serviceName, err)
		}
	}()

	go func() {
		log.Printf("%s metrics started on :%s", serviceName, port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("%s metrics server failed: %v", serviceName, err)
		}
	}()
}

func InstrumentHandler(service, route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 {
			HTTPRequestBodyBytes.WithLabelValues(service, route).Observe(float64(r.ContentLength))
		}

		HTTPInFlightRequests.WithLabelValues(service, route).Inc()
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		statusClass := StatusClass(rec.status)
		HTTPInFlightRequests.WithLabelValues(service, route).Dec()
		HTTPRequests.WithLabelValues(service, route, r.Method, statusClass).Inc()
		HTTPRequestDuration.WithLabelValues(service, route, r.Method, statusClass).Observe(time.Since(start).Seconds())
	})
}

func InstrumentHandlerFunc(service, route string, next http.HandlerFunc) http.HandlerFunc {
	return InstrumentHandler(service, route, next).ServeHTTP
}

func StatusClass(status int) string {
	if status <= 0 {
		status = http.StatusOK
	}
	return strconv.Itoa(status/100) + "xx"
}

func ResultFromErr(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func ObserveDependency(service, dependency, operation string, start time.Time, err error) {
	result := ResultFromErr(err)
	DependencyRequests.WithLabelValues(service, dependency, operation, result).Inc()
	DependencyRequestDuration.WithLabelValues(service, dependency, operation, result).Observe(time.Since(start).Seconds())
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (r *responseRecorder) Push(target string, opts *http.PushOptions) error {
	pusher, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (r *responseRecorder) ReadFrom(reader io.Reader) (int64, error) {
	if readerFrom, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := readerFrom.ReadFrom(reader)
		r.bytes += n
		return n, err
	}
	return io.Copy(r.ResponseWriter, reader)
}
