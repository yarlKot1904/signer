package logutil

import "testing"

func TestMaskEmail(t *testing.T) {
	masked := MaskEmail("alice@example.com")
	if masked != "a****@e******.com" {
		t.Fatalf("unexpected masked email: %s", masked)
	}
}

func TestMaskToken(t *testing.T) {
	masked := MaskToken("12345678-1234-1234-1234-1234567890ab")
	if masked != "123456**************************90ab" {
		t.Fatalf("unexpected masked token: %s", masked)
	}
}
