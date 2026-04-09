package com.yarlkot1904.pdfsigner

import org.springframework.http.HttpStatus
import org.springframework.http.ResponseEntity
import org.springframework.web.bind.annotation.ExceptionHandler
import org.springframework.web.bind.annotation.RestControllerAdvice
import org.springframework.web.multipart.MaxUploadSizeExceededException
import org.springframework.web.multipart.MultipartException
import org.springframework.web.multipart.support.MissingServletRequestPartException

@RestControllerAdvice
class PdfSignerExceptionHandler {
    @ExceptionHandler(MaxUploadSizeExceededException::class)
    fun handleMaxUploadSizeExceeded(): ResponseEntity<VerificationResult> =
        ResponseEntity
            .status(HttpStatus.PAYLOAD_TOO_LARGE)
            .body(VerificationResult.error("uploaded PDF exceeds size limit"))

    @ExceptionHandler(MultipartException::class, MissingServletRequestPartException::class)
    fun handleMultipartErrors(): ResponseEntity<VerificationResult> =
        ResponseEntity
            .badRequest()
            .body(VerificationResult.error("invalid multipart request"))
}
