package com.yarlkot1904.pdfsigner

import org.apache.pdfbox.pdmodel.PDDocument
import org.apache.pdfbox.pdmodel.PDPageContentStream
import org.apache.pdfbox.pdmodel.font.PDType1Font
import org.apache.pdfbox.pdmodel.common.PDRectangle
import org.bouncycastle.openssl.PEMParser
import org.bouncycastle.openssl.jcajce.JcaPEMKeyConverter
import org.bouncycastle.cert.X509CertificateHolder
import org.springframework.stereotype.Service
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.StringReader
import java.security.PrivateKey
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.time.Instant

@Service
class PdfSigningService {

    fun signPdf(pdfBytes: ByteArray, certPem: String, keyPem: String): ByteArray {
        val cert = parseX509(certPem)
        val key = parsePrivateKey(keyPem)

        PDDocument.load(ByteArrayInputStream(pdfBytes)).use { doc ->
            if (doc.numberOfPages == 0) {
                throw IllegalArgumentException("PDF has no pages")
            }

            val page = doc.getPage(0)
            val mediaBox: PDRectangle = page.mediaBox

            val stampText = buildString {
                append("SIGNED (demo)")
                append(" | CN=")
                append(cert.subjectX500Principal.name)
                append(" | ")
                append(Instant.now().toString())
            }

            PDPageContentStream(doc, page, PDPageContentStream.AppendMode.APPEND, true, true).use { cs ->
                cs.beginText()
                cs.setFont(PDType1Font.HELVETICA_BOLD, 10f)
                cs.newLineAtOffset(24f, 24f)
                cs.showText(stampText.take(200))
                cs.endText()
            }

            val out = ByteArrayOutputStream()
            doc.save(out)
            return out.toByteArray()
        }
    }

    private fun parseX509(pem: String): X509Certificate {
        val cf = CertificateFactory.getInstance("X.509")
        val cleaned = pem.trim().toByteArray()
        return cf.generateCertificate(ByteArrayInputStream(cleaned)) as X509Certificate
    }

    private fun parsePrivateKey(pem: String): PrivateKey {
        PEMParser(StringReader(pem)).use { parser ->
            val obj = parser.readObject()
            val converter = JcaPEMKeyConverter()
            return when (obj) {
                is org.bouncycastle.openssl.PEMKeyPair -> converter.getKeyPair(obj).private
                is org.bouncycastle.asn1.pkcs.PrivateKeyInfo -> converter.getPrivateKey(obj)
                else -> throw IllegalArgumentException("Unsupported key PEM format: ${obj?.javaClass?.name}")
            }
        }
    }
}
