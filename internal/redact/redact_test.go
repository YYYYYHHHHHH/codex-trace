package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 以下"密钥"均为虚构样本，仅用于验证脱敏规则。
func TestTextRedactsKnownSecretShapes(t *testing.T) {
	cases := []struct {
		name, in, secret string
	}{
		{"openai", "key sk-proj-abcdefghijklmnopqrstuvwx1234 end", "abcdefghijklmnopqrstuvwx1234"},
		{"langfuse", "LANGFUSE_SECRET_KEY=sk-lf-0123456789abcdef0123", "0123456789abcdef0123"},
		{"aws", "id AKIAABCDEFGHIJKLMNOP here", "AKIAABCDEFGHIJKLMNOP"},
		{"github", "token ghp_0123456789abcdefghijklmnopqrstuvwxyz", "0123456789abcdefghijklmnopqrstuvwxyz"},
		{"github pat", "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz", "11ABCDEFG0123456789"},
		{"slack", "xoxb-1234567890-abcdefghij", "1234567890-abcdefghij"},
		{"bearer", "Authorization: Bearer abc.def.ghi-jkl", "abc.def.ghi-jkl"},
		{"jwt", "t=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcDEF123_-x", "eyJzdWIiOiIxMjM0In0"},
		{"password kv", `password = "hunter2hunter2"`, "hunter2hunter2"},
		{"api key json", `{"api_key": "abcdef0123456789"}`, "abcdef0123456789"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowFAKE\n-----END RSA PRIVATE KEY-----", "MIIEowFAKE"},
		{"truncated pem", "head key.pem\n-----BEGIN PRIVATE KEY-----\nMIIEvFAKEBODY", "MIIEvFAKEBODY"},
		{"escaped json", `{\"password\":\"correct horse battery\"}`, "correct horse battery"},
		{"json with spaces", `{"client_secret": "two words here"}`, "two words here"},
		{"aws env", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYFAKEKEY", "wJalrXUtnFEMIK7MDENGbPxRfiCYFAKEKEY"},
		{"token env", "TOKEN=abcdef123456", "abcdef123456"},
		{"cli flag", "mysql --password s3cr3tpass -h db", "s3cr3tpass"},
		{"cli flag eq", "tool --token=tok_123456789", "tok_123456789"},
		{"url userinfo", "git clone https://bob:hunter2pass@git.example.com/x.git", "hunter2pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Text(tc.in)
			if strings.Contains(out, tc.secret) {
				t.Errorf("secret survived: %q", out)
			}
			if !strings.Contains(out, Placeholder) {
				t.Errorf("expected placeholder in %q", out)
			}
		})
	}
}

func TestTextKeepsOrdinaryContent(t *testing.T) {
	for _, in := range []string{
		"ls -la && git status\n统计文件数：1",
		"token count is 42",
		"see https://example.com/docs?page=2",
		"basic information about bearer tokens",
		"tokens: 5",
	} {
		if out := Text(in); out != in {
			t.Errorf("ordinary content changed: %q -> %q", in, out)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	long := strings.Repeat("统", 100) // 每个 3 字节
	got := Truncate(long, 50)
	if !utf8.ValidString(got) {
		t.Error("truncation must keep valid UTF-8")
	}
	if !strings.Contains(got, "truncated") || len(got) > 50+40 {
		t.Errorf("unexpected truncation: %q (%d bytes)", got, len(got))
	}
}
