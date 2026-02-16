package com.yarlkot1904.pdfsigner

import org.springframework.http.MediaType
import org.springframework.http.ResponseEntity
import org.springframework.web.bind.annotation.*
import org.springframework.web.multipart.MultipartFile

@RestController
class PdfSignerController(
    private val signingService: PdfSigningService
) {
    @GetMapping("/health")
    fun health(): Map<String, String> = mapOf("status" to "ok")

    /**
     * POST /sign
     * multipart/form-data:
     * - pdf: file (application/pdf)
     * - certPem: string (X.509 certificate PEM)
     * - keyPem: string (PKCS8 private key PEM)
     *
     * Returns: signed PDF bytes (application/pdf)
     */
    @PostMapping(
        "/sign",
        consumes = [MediaType.MULTIPART_FORM_DATA_VALUE],
        produces = [MediaType.APPLICATION_PDF_VALUE]
    )
    fun sign(
        @RequestPart("pdf") pdf: MultipartFile,
        @RequestPart("certPem") certPem: String,
        @RequestPart("keyPem") keyPem: String
    ): ResponseEntity<ByteArray> {
        val signed = signingService.signPdf(pdf.bytes, certPem, keyPem)
        return ResponseEntity
            .ok()
            .contentType(MediaType.APPLICATION_PDF)
            .body(signed)
    }
}
