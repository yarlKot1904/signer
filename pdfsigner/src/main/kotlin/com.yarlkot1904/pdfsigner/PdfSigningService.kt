package com.yarlkot1904.pdfsigner

import org.apache.pdfbox.pdmodel.PDDocument
import org.apache.pdfbox.pdmodel.PDPageContentStream
import org.apache.pdfbox.pdmodel.font.PDType1Font
import org.apache.pdfbox.pdmodel.interactive.digitalsignature.PDSignature
import org.apache.pdfbox.pdmodel.interactive.digitalsignature.SignatureInterface
import org.apache.pdfbox.pdmodel.interactive.digitalsignature.SignatureOptions
import org.bouncycastle.asn1.pkcs.PrivateKeyInfo
import org.bouncycastle.cert.jcajce.JcaX509CertificateConverter
import org.bouncycastle.cert.jcajce.JcaX509CertificateHolder
import org.bouncycastle.cms.CMSProcessableByteArray
import org.bouncycastle.cms.CMSSignedDataGenerator
import org.bouncycastle.jce.provider.BouncyCastleProvider
import org.bouncycastle.openssl.PEMKeyPair
import org.bouncycastle.openssl.PEMParser
import org.bouncycastle.operator.jcajce.JcaContentSignerBuilder
import org.bouncycastle.operator.jcajce.JcaDigestCalculatorProviderBuilder
import org.bouncycastle.cms.jcajce.JcaSignerInfoGeneratorBuilder
import org.bouncycastle.cert.X509CertificateHolder
import org.bouncycastle.util.Store
import org.bouncycastle.cert.jcajce.JcaCertStore
import org.springframework.stereotype.Service
import java.io.*
import java.security.PrivateKey
import java.security.Security
import java.security.cert.X509Certificate
import java.time.Instant
import java.util.*
import org.apache.pdfbox.cos.COSName


@Service
class PdfSigningService {

    init {
        if (Security.getProvider(BouncyCastleProvider.PROVIDER_NAME) == null) {
            Security.addProvider(BouncyCastleProvider())
        }
    }

    fun signPdf(pdfBytes: ByteArray, certPem: String, keyPem: String): ByteArray {
        val cert = parseX509FromPem(certPem)
        val key = parsePrivateKeyFromPem(keyPem)

        PDDocument.load(ByteArrayInputStream(pdfBytes)).use { doc ->
            require(doc.numberOfPages > 0) { "PDF has no pages" }

            stampLastPage(doc, cert)

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

                doc.addSignature(signature, signer, opts)
                doc.saveIncremental(out)
            }

            return out.toByteArray()
        }
    }
    private fun extractEmailFromSubject(subject: String): String? {
        val cnMatch = Regex("""CN=([^,]+)""").find(subject)?.groupValues?.getOrNull(1)?.trim()
        if (!cnMatch.isNullOrBlank() && cnMatch.contains("@")) return cnMatch

        val emailMatch = Regex("""[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}""").find(subject)?.value
        return emailMatch
    }


    private fun stampLastPage(doc: PDDocument, cert: X509Certificate) {
    val lastPageIndex = doc.numberOfPages - 1
    val page = doc.getPage(lastPageIndex)
    val box = page.mediaBox

    val email = extractEmailFromSubject(cert.subjectX500Principal.name) ?: cert.subjectX500Principal.name

    val dateStr = Instant.now().toString()

    val padding = 10f
    val blockWidth = 340f
    val blockHeight = 70f

    val x = box.width - blockWidth - 24f
    val y = 24f

    val title = "Документ подписан электронной подписью"
    val line1 = "Email: $email"
    val line2 = "Дата:  $dateStr"

    PDPageContentStream(doc, page, PDPageContentStream.AppendMode.APPEND, true, true).use { cs ->
        cs.setLineWidth(1f)
        cs.addRect(x, y, blockWidth, blockHeight)
        cs.stroke()

        cs.beginText()
        cs.setFont(PDType1Font.HELVETICA_BOLD, 11f)
        cs.newLineAtOffset(x + padding, y + blockHeight - padding - 12f)
        cs.showText(title.take(80))
        cs.endText()

        cs.beginText()
        cs.setFont(PDType1Font.HELVETICA, 10f)
        cs.newLineAtOffset(x + padding, y + blockHeight - padding - 30f)
        cs.showText(line1.take(120))
        cs.endText()

        cs.beginText()
        cs.setFont(PDType1Font.HELVETICA, 10f)
        cs.newLineAtOffset(x + padding, y + blockHeight - padding - 45f)
        cs.showText(line2.take(120))
        cs.endText()
    }
}


    private class CmsSigner(
        private val privateKey: PrivateKey,
        private val cert: X509Certificate
    ) : SignatureInterface {

        override fun sign(content: InputStream): ByteArray {
            val data = content.readBytes()

            val certHolder = JcaX509CertificateHolder(cert)
            val certs = listOf(cert)
            val certStore: Store<*> = JcaCertStore(certs)

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

            val cmsData = CMSProcessableByteArray(data)

            val signedData = gen.generate(cmsData, false)

            return signedData.encoded
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
}