package com.yarlkot1904.pdfsigner

import org.springframework.http.MediaType
import org.springframework.http.ResponseEntity
import org.springframework.web.bind.annotation.*
import org.springframework.web.multipart.MultipartFile
import java.io.IOException

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
        @RequestPart("keyPem") keyPem: String,
        @RequestPart("documentId") documentId: String
    ): ResponseEntity<ByteArray> {
        return try {
            val pdfBytes = readPdfBytes(pdf)
            val signed = signingService.signPdf(pdfBytes, certPem, keyPem, documentId)
            ResponseEntity
                .ok()
                .contentType(MediaType.APPLICATION_PDF)
                .body(signed)
        } catch (_: IOException) {
            ResponseEntity
                .badRequest()
                .contentType(MediaType.TEXT_PLAIN)
                .body("Invalid PDF".toByteArray())
        } catch (_: IllegalArgumentException) {
            ResponseEntity
                .badRequest()
                .contentType(MediaType.TEXT_PLAIN)
                .body("Invalid PDF".toByteArray())
        }
    }

    @PostMapping(
        "/verify",
        consumes = [MediaType.MULTIPART_FORM_DATA_VALUE],
        produces = [MediaType.APPLICATION_JSON_VALUE]
    )
    fun verify(
        @RequestPart("pdf") pdf: MultipartFile
    ): ResponseEntity<VerificationResult> {
        return try {
            ResponseEntity.ok(signingService.verifyPdf(readPdfBytes(pdf)))
        } catch (_: IOException) {
            ResponseEntity
                .badRequest()
                .body(VerificationResult.error("Invalid PDF"))
        } catch (_: IllegalArgumentException) {
            ResponseEntity
                .badRequest()
                .body(VerificationResult.error("Invalid PDF"))
        }
    }

    private fun readPdfBytes(pdf: MultipartFile): ByteArray {
        require(!pdf.isEmpty) { "uploaded PDF is empty" }
        val bytes = pdf.bytes
        require(signingService.isPdf(bytes)) { "invalid pdf" }
        return bytes
    }
}
