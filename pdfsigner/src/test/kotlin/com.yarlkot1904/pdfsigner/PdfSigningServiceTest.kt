package com.yarlkot1904.pdfsigner

import org.apache.pdfbox.pdmodel.PDDocument
import org.apache.pdfbox.pdmodel.PDPage
import org.apache.pdfbox.pdmodel.PDPageContentStream
import org.apache.pdfbox.pdmodel.font.PDType1Font
import org.bouncycastle.asn1.x500.X500Name
import org.bouncycastle.cert.jcajce.JcaX509CertificateConverter
import org.bouncycastle.cert.jcajce.JcaX509v3CertificateBuilder
import org.bouncycastle.jce.provider.BouncyCastleProvider
import org.bouncycastle.operator.jcajce.JcaContentSignerBuilder
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeAll
import org.junit.jupiter.api.Test
import java.io.ByteArrayOutputStream
import java.math.BigInteger
import java.security.KeyPairGenerator
import java.security.Security
import java.time.Instant
import java.time.temporal.ChronoUnit
import java.util.Base64
import java.util.Date

class PdfSigningServiceTest {
    private val service = PdfSigningService()

    @Test
    fun `verify signed pdf reports valid signature`() {
        val (certPem, keyPem) = createSigningMaterial("user@example.com")
        val signedPdf = service.signPdf(createPdf("Hello Signer"), certPem, keyPem)

        val result = service.verifyPdf(signedPdf)

        assertEquals("verified", result.status)
        assertTrue(result.signaturePresent)
        assertTrue(result.integrityValid)
        assertNotNull(result.signerSubject)
        assertEquals("user@example.com", result.signerCn)
        assertEquals(true, result.certificateSelfSigned)
        assertNotNull(result.signingTime)
        assertEquals(null, result.error)
    }

    @Test
    fun `verify unsigned pdf reports unsigned`() {
        val result = service.verifyPdf(createPdf("Unsigned"))

        assertEquals("unsigned", result.status)
        assertFalse(result.signaturePresent)
        assertFalse(result.integrityValid)
        assertEquals(null, result.signerSubject)
        assertEquals(null, result.signingTime)
    }

    @Test
    fun `verify tampered signed pdf reports invalid signature`() {
        val (certPem, keyPem) = createSigningMaterial("user@example.com")
        val signedPdf = service.signPdf(createPdf("Hello Signer"), certPem, keyPem)
        val tamperedPdf = tamperSignatureContents(signedPdf)

        val result = service.verifyPdf(tamperedPdf)

        assertEquals("invalid_signature", result.status)
        assertTrue(result.signaturePresent)
        assertFalse(result.integrityValid)
        assertNotNull(result.error)
    }

    companion object {
        @JvmStatic
        @BeforeAll
        fun registerProvider() {
            if (Security.getProvider(BouncyCastleProvider.PROVIDER_NAME) == null) {
                Security.addProvider(BouncyCastleProvider())
            }
        }
    }

    private fun createPdf(text: String): ByteArray {
        PDDocument().use { doc ->
            val page = PDPage()
            doc.addPage(page)
            PDPageContentStream(doc, page).use { cs ->
                cs.beginText()
                cs.setFont(PDType1Font.HELVETICA, 12f)
                cs.newLineAtOffset(72f, 720f)
                cs.showText(text)
                cs.endText()
            }

            val output = ByteArrayOutputStream()
            doc.save(output)
            return output.toByteArray()
        }
    }

    private fun createSigningMaterial(email: String): Pair<String, String> {
        val keyPair = KeyPairGenerator.getInstance("RSA").apply {
            initialize(2048)
        }.generateKeyPair()

        val subject = X500Name("CN=$email, O=CryptoSigner Demo")
        val now = Instant.now()
        val certBuilder = JcaX509v3CertificateBuilder(
            subject,
            BigInteger.valueOf(now.toEpochMilli()),
            Date.from(now.minus(5, ChronoUnit.MINUTES)),
            Date.from(now.plus(365, ChronoUnit.DAYS)),
            subject,
            keyPair.public
        )

        val signer = JcaContentSignerBuilder("SHA256withRSA")
            .setProvider(BouncyCastleProvider.PROVIDER_NAME)
            .build(keyPair.private)

        val cert = JcaX509CertificateConverter()
            .setProvider(BouncyCastleProvider.PROVIDER_NAME)
            .getCertificate(certBuilder.build(signer))

        return cert.encoded.toPem("CERTIFICATE") to keyPair.private.encoded.toPem("PRIVATE KEY")
    }

    private fun ByteArray.toPem(type: String): String {
        val encoded = Base64.getMimeEncoder(64, "\n".toByteArray()).encodeToString(this)
        return "-----BEGIN $type-----\n$encoded\n-----END $type-----\n"
    }

    private fun tamperSignatureContents(pdfBytes: ByteArray): ByteArray {
        val tampered = pdfBytes.copyOf()
        val marker = "/Contents <".toByteArray()
        val start = tampered.indexOf(marker)
        require(start >= 0) { "Could not find signature contents in PDF" }

        val hexStart = start + marker.size
        for (i in hexStart until tampered.size) {
            val value = tampered[i].toInt().toChar()
            if (value.isHexDigit()) {
                tampered[i] = if (value.equals('A', ignoreCase = true)) 'B'.code.toByte() else 'A'.code.toByte()
                return tampered
            }
        }

        error("Could not tamper signature contents")
    }

    private fun ByteArray.indexOf(other: ByteArray): Int {
        if (other.isEmpty() || size < other.size) return -1
        for (i in 0..size - other.size) {
            if (copyOfRange(i, i + other.size).contentEquals(other)) {
                return i
            }
        }
        return -1
    }

    private fun Char.isHexDigit(): Boolean =
        this in '0'..'9' || this in 'a'..'f' || this in 'A'..'F'
}
