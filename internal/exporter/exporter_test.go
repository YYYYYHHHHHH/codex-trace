package exporter

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type captured struct {
	path, auth, contentType, encoding string
	body                              string
}

func server(t *testing.T, status int, respBody string, header map[string]string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		got.encoding = r.Header.Get("Content-Encoding")
		zr, err := gzip.NewReader(r.Body)
		if err == nil {
			b, _ := io.ReadAll(zr)
			got.body = string(b)
		}
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestSendSuccess(t *testing.T) {
	srv, got := server(t, 200, `{}`, nil)
	s := New(srv.URL+"/", "pk-lf-a", "sk-lf-b", srv.Client())

	res := s.Send(context.Background(), []byte(`{"resourceSpans":[]}`))

	if res.Outcome != OK || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	if got.path != "/api/public/otel/v1/traces" {
		t.Errorf("path = %q", got.path)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-a:sk-lf-b"))
	if got.auth != wantAuth {
		t.Errorf("auth header = %q", got.auth)
	}
	if got.contentType != "application/json" || got.encoding != "gzip" || got.body != `{"resourceSpans":[]}` {
		t.Errorf("request = %+v", got)
	}
}

func TestSendReportsPartialSuccess(t *testing.T) {
	srv, _ := server(t, 200, `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"bad span"}}`, nil)
	res := New(srv.URL, "pk", "sk", srv.Client()).Send(context.Background(), []byte(`{}`))
	if res.Outcome != OK || res.Rejected != 2 || !strings.Contains(res.Message, "bad span") {
		t.Errorf("result = %+v", res)
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{
		{400, Permanent},
		{413, Permanent},
		{422, Permanent},
		{301, Retry},
		{307, Retry},
		{401, Retry},
		{403, Retry},
		{404, Retry},
		{408, Retry},
		{429, Retry},
		{500, Retry},
		{503, Retry},
	}
	for _, tc := range cases {
		srv, _ := server(t, tc.status, `{"message":"nope"}`, nil)
		res := New(srv.URL, "pk", "sk", srv.Client()).Send(context.Background(), []byte(`{}`))
		if res.Outcome != tc.want {
			t.Errorf("status %d: outcome = %v, want %v", tc.status, res.Outcome, tc.want)
		}
		if res.Err == nil || res.Status != tc.status {
			t.Errorf("status %d: expected error with status, got %+v", tc.status, res)
		}
	}
}

func TestSendHonorsRetryAfter(t *testing.T) {
	srv, _ := server(t, 429, ``, map[string]string{"Retry-After": "7"})
	res := New(srv.URL, "pk", "sk", srv.Client()).Send(context.Background(), []byte(`{}`))
	if res.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v", res.RetryAfter)
	}
}

func TestSendNetworkErrorIsRetryable(t *testing.T) {
	srv, _ := server(t, 200, ``, nil)
	url := srv.URL
	srv.Close()
	res := New(url, "pk", "sk", http.DefaultClient).Send(context.Background(), []byte(`{}`))
	if res.Outcome != Retry || res.Err == nil {
		t.Errorf("result = %+v", res)
	}
}

func TestSendErrorNeverContainsSecret(t *testing.T) {
	srv, _ := server(t, 401, `{"message":"invalid credentials"}`, nil)
	res := New(srv.URL, "pk-lf-public", "sk-lf-very-secret", srv.Client()).Send(context.Background(), []byte(`{}`))
	if strings.Contains(res.Err.Error(), "sk-lf-very-secret") {
		t.Error("error must not leak the secret key")
	}
}

func TestSendRespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 读完请求体，服务端才会探测到客户端断开
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res := New(srv.URL, "pk", "sk", srv.Client()).Send(ctx, []byte(`{}`))
	if res.Outcome != Retry || !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("result = %+v", res)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if d := parseRetryAfter("", now); d != 0 {
		t.Errorf("empty = %v", d)
	}
	if d := parseRetryAfter("abc", now); d != 0 {
		t.Errorf("garbage = %v", d)
	}
	if d := parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); d != 30*time.Second {
		t.Errorf("http date = %v", d)
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	res := New(srv.URL, "pk", "sk", srv.Client()).Send(context.Background(), []byte(`{}`))

	if leaked {
		t.Error("credentials must never follow a redirect")
	}
	if res.Outcome != Retry || res.Status != http.StatusTemporaryRedirect {
		t.Errorf("redirect should be reported as retryable, got %+v", res)
	}
}

func TestRetryAfterIsCapped(t *testing.T) {
	now := time.Now()
	for _, v := range []string{"9999999999", now.Add(48 * time.Hour).UTC().Format(http.TimeFormat)} {
		if d := parseRetryAfter(v, now); d != MaxRetryAfter {
			t.Errorf("Retry-After %q -> %v, want capped at %v", v, d, MaxRetryAfter)
		}
	}
	// 溢出的值被忽略，退回到客户端自己的退避策略
	if d := parseRetryAfter("99999999999999999999", now); d != 0 {
		t.Errorf("overflowing Retry-After should be ignored, got %v", d)
	}
}

func TestServerMessageIsSanitized(t *testing.T) {
	msg := "bad\x1b[31m red \u00e9" + strings.Repeat("统", 300)
	got := sanitize(msg)
	if strings.ContainsRune(got, 0x1b) || len([]rune(got)) > maxMessageRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("sanitize = %q", got)
	}
}
