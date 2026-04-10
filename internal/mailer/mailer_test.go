package mailer

import "testing"

func TestRenderSigningOTP(t *testing.T) {
	msg, err := Render(SendRequest{
		Template:  TemplateSigningOTP,
		Recipient: "user@example.com",
		Variables: map[string]string{
			"code":         "123456",
			"sign_url":     "http://localhost/sign.html?token=abc",
			"download_url": "http://localhost/download/abc",
			"view_url":     "http://localhost/view/abc",
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if msg.Subject != "Signer OTP code" {
		t.Fatalf("unexpected subject: %s", msg.Subject)
	}
	if msg.Metadata["has_sign_url"] != "true" {
		t.Fatalf("expected has_sign_url metadata to be true")
	}
	if msg.Body == "" {
		t.Fatal("expected message body to be rendered")
	}
}

func TestRenderSigningOTPMissingVariable(t *testing.T) {
	_, err := Render(SendRequest{
		Template:  TemplateSigningOTP,
		Recipient: "user@example.com",
		Variables: map[string]string{
			"code": "123456",
		},
	})
	if err == nil {
		t.Fatal("expected missing variable error")
	}
}
