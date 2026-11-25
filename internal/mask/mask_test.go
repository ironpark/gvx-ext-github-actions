package mask

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRedactorMasksRawBase64AndURLEncoded(t *testing.T) {
	r := New()
	secret := "ghp_SuperSecretToken+123/456="
	r.SetAll(map[string]string{
		"GITHUB_PAT": secret,
		"SHORT":      "ab", // ignored (<3 chars)
	})

	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	line := "token=" + secret + " b64=" + b64 + " keep=ab"
	got := r.Mask(line)
	if strings.Contains(got, secret) || strings.Contains(got, b64) {
		t.Fatalf("secret leaked in masked output: %q", got)
	}
	if !strings.Contains(got, "token=***") || !strings.Contains(got, "b64=***") || !strings.Contains(got, "keep=ab") {
		t.Fatalf("unexpected masked output: %q", got)
	}
}

func TestSanitizeLineStripsANSIAndSpoofing(t *testing.T) {
	r := New()
	r.Add("API_KEY", "sk-live-secret-999")

	raw := "\x1b[32mOK\x1b[0m \u202esecret=sk-live-secret-999\x00\r\nnext"
	got := r.SanitizeLine(raw)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\u202e") || strings.Contains(got, "\x00") || strings.Contains(got, "\n") {
		t.Fatalf("unsafe characters survived sanitize: %q", got)
	}
	if strings.Contains(got, "sk-live-secret-999") {
		t.Fatalf("secret leaked: %q", got)
	}
	if got != "OK secret=*** next" {
		t.Fatalf("got %q, want %q", got, "OK secret=*** next")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := TruncateRunes("hello world", 8); got != "hello..." {
		t.Fatalf("got %q", got)
	}
	if got := TruncateRunes("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
}
