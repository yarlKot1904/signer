package com.yarlkot1904.pdfsigner

import com.fasterxml.jackson.databind.PropertyNamingStrategies
import com.fasterxml.jackson.databind.annotation.JsonNaming
import org.apache.pdfbox.pdmodel.PDDocument
import org.apache.pdfbox.pdmodel.PDPageContentStream
import org.apache.pdfbox.pdmodel.font.PDFont
import org.apache.pdfbox.pdmodel.font.PDType0Font
import org.apache.pdfbox.text.PDFTextStripper
import org.apache.pdfbox.pdmodel.interactive.digitalsignature.PDSignature
import org.apache.pdfbox.pdmodel.interactive.digitalsignature.SignatureInterface
import org.apache.pdfbox.pdmodel.interactive.digitalsignature.SignatureOptions
import org.bouncycastle.asn1.pkcs.PrivateKeyInfo
import org.bouncycastle.cert.X509CertificateHolder
import org.bouncycastle.cert.jcajce.JcaCertStore
import org.bouncycastle.cert.jcajce.JcaX509CertificateConverter
import org.bouncycastle.cms.CMSProcessableByteArray
import org.bouncycastle.cms.CMSSignedData
import org.bouncycastle.cms.CMSSignedDataGenerator
import org.bouncycastle.cms.jcajce.JcaSignerInfoGeneratorBuilder
import org.bouncycastle.cms.jcajce.JcaSimpleSignerInfoVerifierBuilder
import org.bouncycastle.jce.provider.BouncyCastleProvider
import org.bouncycastle.openssl.PEMKeyPair
import org.bouncycastle.openssl.PEMParser
import org.bouncycastle.operator.jcajce.JcaContentSignerBuilder
import org.bouncycastle.operator.jcajce.JcaDigestCalculatorProviderBuilder
import org.bouncycastle.util.Selector
import org.bouncycastle.util.Store
import org.slf4j.LoggerFactory
import org.springframework.stereotype.Service
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.InputStream
import java.io.StringReader
import java.security.MessageDigest
import java.security.PrivateKey
import java.security.Security
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.time.Instant
import java.util.Calendar

@JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy::class)
data class VerificationResult(
    val status: String,
    val signaturePresent: Boolean,
    val integrityValid: Boolean,
    val signerSubject: String? = null,
    val signerCn: String? = null,
    val signingTime: String? = null,
    val certificateSelfSigned: Boolean? = null,
    val certificateSha256: String? = null,
    val certificateTrusted: Boolean? = null,
    val error: String? = null
) {
    companion object {
        fun error(message: String): VerificationResult = VerificationResult(
            status = "error",
            signaturePresent = false,
            integrityValid = false,
            error = message
        )
    }
}

@Service
class PdfSigningService {
    private val logger = LoggerFactory.getLogger(PdfSigningService::class.java)

    init {
        if (Security.getProvider(BouncyCastleProvider.PROVIDER_NAME) == null) {
            Security.addProvider(BouncyCastleProvider())
        }
    }

    fun signPdf(pdfBytes: ByteArray, certPem: String, keyPem: String, documentId: String): ByteArray {
        val cert = parseX509FromPem(certPem)
        val key = parsePrivateKeyFromPem(keyPem)

        PDDocument.load(ByteArrayInputStream(pdfBytes)).use { doc ->
            require(doc.numberOfPages > 0) { "PDF has no pages" }
            logger.info(
                "Starting PDF signing: pages={}, subject={}",
                doc.numberOfPages,
                cert.subjectX500Principal.name
            )

            stampLastPage(doc, cert, documentId)
            val stampedOut = ByteArrayOutputStream()
            doc.save(stampedOut)
            val stampedPdf = stampedOut.toByteArray()
            logger.info("Stamped PDF prepared before signing: bytes={}", stampedPdf.size)

            PDDocument.load(ByteArrayInputStream(stampedPdf)).use { stampedDoc ->
                val extractedText = PDFTextStripper().apply {
                    startPage = stampedDoc.numberOfPages
                    endPage = stampedDoc.numberOfPages
                }.getText(stampedDoc)
                logger.info(
                    "Stamped PDF text probe: lastPageContainsTitle={}, lastPageContainsEmail={}",
                    extractedText.contains("Документ подписан электронной подписью"),
                        extractedText.contains(emailForLog(cert))
                )

                val signature = PDSignature().apply {
                    setFilter(PDSignature.FILTER_ADOBE_PPKLITE)
                    setSubFilter(PDSignature.SUBFILTER_ADBE_PKCS7_DETACHED)
                    setName(cert.subjectX500Principal.name)
                    setReason("Document signed")
                    setLocation("CryptoSigner")
                    setSignDate(Calendar.getInstance())
                }

                val signer = CmsSigner(key, cert)
                val out = ByteArrayOutputStream()
                SignatureOptions().use { opts ->
                    opts.preferredSignatureSize = 200_000
                    stampedDoc.addSignature(signature, signer, opts)
                    stampedDoc.saveIncremental(out)
                }

                logger.info(
                    "Finished PDF signing: pages={}, outputBytes={}",
                    stampedDoc.numberOfPages,
                    out.size()
                )
                return out.toByteArray()
            }
        }
    }

    fun verifyPdf(pdfBytes: ByteArray): VerificationResult {
        PDDocument.load(ByteArrayInputStream(pdfBytes)).use { doc ->
            val signatures = doc.signatureDictionaries
            logger.info("Verifying PDF: pages={}, signatures={}", doc.numberOfPages, signatures.size)
            if (signatures.isEmpty()) {
                return VerificationResult(
                    status = "unsigned",
                    signaturePresent = false,
                    integrityValid = false
                )
            }

            val signature = signatures.first()
            val contents = signature.getContents(pdfBytes)
            val signedContent = signature.getSignedContent(pdfBytes)
            val cms = CMSSignedData(CMSProcessableByteArray(signedContent), contents)
            val signerInfo = cms.signerInfos.signers.firstOrNull()
                ?: return invalidSignature(signature, "No signer info present")

            @Suppress("UNCHECKED_CAST")
            val matches = cms.certificates.getMatches(signerInfo.sid as Selector<X509CertificateHolder>)
            val certHolder = matches.firstOrNull() as? X509CertificateHolder
                ?: return invalidSignature(signature, "Signer certificate not found")
            val cert = JcaX509CertificateConverter()
                .setProvider(BouncyCastleProvider.PROVIDER_NAME)
                .getCertificate(certHolder)

            val integrityValid = signerInfo.verify(
                JcaSimpleSignerInfoVerifierBuilder()
                    .setProvider(BouncyCastleProvider.PROVIDER_NAME)
                    .build(cert)
            )

            val subject = cert.subjectX500Principal.name
            val certHash = sha256Hex(cert.encoded)
            logger.info(
                "Verification result: integrityValid={}, subject={}, certSha256={}",
                integrityValid,
                subject,
                certHash
            )
            return VerificationResult(
                status = if (integrityValid) "verified" else "invalid_signature",
                signaturePresent = true,
                integrityValid = integrityValid,
                signerSubject = subject,
                signerCn = extractEmailFromSubject(subject) ?: extractCn(subject),
                signingTime = signature.signDate?.toInstant()?.toString(),
                certificateSelfSigned = isSelfSigned(cert),
                certificateSha256 = certHash,
                certificateTrusted = null,
                error = if (integrityValid) null else "Signature integrity check failed"
            )
        }
    }

    private fun extractEmailFromSubject(subject: String): String? {
        val cnMatch = Regex("""CN=([^,]+)""").find(subject)?.groupValues?.getOrNull(1)?.trim()
        if (!cnMatch.isNullOrBlank() && cnMatch.contains("@")) return cnMatch
        return Regex("""[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}""").find(subject)?.value
    }

    private fun extractCn(subject: String): String? =
        Regex("""CN=([^,]+)""").find(subject)?.groupValues?.getOrNull(1)?.trim()

    private fun emailForLog(cert: X509Certificate): String =
        extractEmailFromSubject(cert.subjectX500Principal.name) ?: cert.subjectX500Principal.name

    private fun stampLastPage(doc: PDDocument, cert: X509Certificate, documentId: String) {
        val page = doc.getPage(doc.numberOfPages - 1)
        val box = page.cropBox ?: page.mediaBox

        val email = extractEmailFromSubject(cert.subjectX500Principal.name) ?: cert.subjectX500Principal.name
        val dateStr = Instant.now().toString()

        val title = "\u0414\u043e\u043a\u0443\u043c\u0435\u043d\u0442 \u043f\u043e\u0434\u043f\u0438\u0441\u0430\u043d \u044d\u043b\u0435\u043a\u0442\u0440\u043e\u043d\u043d\u043e\u0439 \u043f\u043e\u0434\u043f\u0438\u0441\u044c\u044e"
        val line1 = "Email: $email"
        val line2 = "\u0414\u0430\u0442\u0430: $dateStr"
        val line3 = "UUID: $documentId"

        val padding = 10f
        val blockWidth = minOf(420f, box.width - 48f)
        val blockHeight = 102f

        val margin = 24f
        val x = (box.lowerLeftX + box.width - blockWidth - margin).coerceAtLeast(box.lowerLeftX + margin)
        val y = (box.lowerLeftY + margin).coerceAtLeast(box.lowerLeftY + margin)
        logger.info(
            "Stamping last page: pageIndex={}, x={}, y={}, width={}, height={}, email={}, documentId={}",
            doc.numberOfPages - 1,
            x,
            y,
            blockWidth,
            blockHeight,
            email,
            documentId
        )

        val fontRegular = loadFont(doc, "fonts/DejaVuSans.ttf")
        val fontBold = loadFont(doc, "fonts/DejaVuSans-Bold.ttf")

        PDPageContentStream(doc, page, PDPageContentStream.AppendMode.APPEND, true, true).use { cs ->
            cs.saveGraphicsState()
            cs.setStrokingColor(0, 0, 0)
            cs.setNonStrokingColor(0, 0, 0)
            cs.setLineWidth(1f)
            cs.addRect(x, y, blockWidth, blockHeight)
            cs.stroke()

            fun drawLine(text: String, font: PDFont, size: Float, dyFromTop: Float) {
                cs.beginText()
                try {
                    cs.setFont(font, size)
                    cs.newLineAtOffset(x + padding, y + blockHeight - padding - dyFromTop)
                    cs.showText(text)
                } finally {
                    cs.endText()
                }
            }

            drawLine(title, fontBold, 11f, 12f)
            drawLine(line1, fontRegular, 10f, 32f)
            drawLine(line2, fontRegular, 10f, 48f)
            drawLine(line3, fontRegular, 10f, 64f)
            cs.restoreGraphicsState()
        }
    }

    private fun loadFont(doc: PDDocument, resourcePath: String): PDType0Font {
        val stream = Thread.currentThread().contextClassLoader.getResourceAsStream(resourcePath)
            ?: throw IllegalStateException("Font resource not found: $resourcePath")
        stream.use {
            return PDType0Font.load(doc, it, true)
        }
    }

    private class CmsSigner(
        private val privateKey: PrivateKey,
        private val cert: X509Certificate
    ) : SignatureInterface {
        override fun sign(content: InputStream): ByteArray {
            val data = content.readBytes()
            val certStore: Store<*> = JcaCertStore(listOf(cert))
            val signer = JcaContentSignerBuilder("SHA256withRSA")
                .setProvider(BouncyCastleProvider.PROVIDER_NAME)
                .build(privateKey)
            val digestProvider = JcaDigestCalculatorProviderBuilder()
                .setProvider(BouncyCastleProvider.PROVIDER_NAME)
                .build()
            val signerInfoGen = JcaSignerInfoGeneratorBuilder(digestProvider)
                .build(signer, cert)

            val gen = CMSSignedDataGenerator().apply {
                addSignerInfoGenerator(signerInfoGen)
                addCertificates(certStore)
            }

            return gen.generate(CMSProcessableByteArray(data), false).encoded
        }
    }

    private fun parseX509FromPem(pem: String): X509Certificate {
        PEMParser(StringReader(pem)).use { parser ->
            val obj = parser.readObject()
            val holder = when (obj) {
                is X509CertificateHolder -> obj
                else -> throw IllegalArgumentException("Unsupported CERT PEM format: ${obj?.javaClass?.name}")
            }
            return JcaX509CertificateConverter()
                .setProvider(BouncyCastleProvider.PROVIDER_NAME)
                .getCertificate(holder)
        }
    }

    private fun parsePrivateKeyFromPem(pem: String): PrivateKey {
        PEMParser(StringReader(pem)).use { parser ->
            val obj = parser.readObject()
            val converter = org.bouncycastle.openssl.jcajce.JcaPEMKeyConverter()
                .setProvider(BouncyCastleProvider.PROVIDER_NAME)

            return when (obj) {
                is PEMKeyPair -> converter.getKeyPair(obj).private
                is PrivateKeyInfo -> converter.getPrivateKey(obj)
                else -> throw IllegalArgumentException("Unsupported KEY PEM format: ${obj?.javaClass?.name}")
            }
        }
    }

    private fun invalidSignature(signature: PDSignature, message: String): VerificationResult =
        VerificationResult(
            status = "invalid_signature",
            signaturePresent = true,
            integrityValid = false,
            signingTime = signature.signDate?.toInstant()?.toString(),
            error = message,
            certificateTrusted = null
        )

    private fun isSelfSigned(cert: X509Certificate): Boolean {
        if (cert.subjectX500Principal != cert.issuerX500Principal) {
            return false
        }

        return try {
            cert.verify(cert.publicKey)
            true
        } catch (_: CertificateException) {
            false
        } catch (_: Exception) {
            false
        }
    }

    private fun sha256Hex(data: ByteArray): String =
        MessageDigest.getInstance("SHA-256")
            .digest(data)
            .joinToString("") { "%02x".format(it) }
}
