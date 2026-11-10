// Package exporter 把 OTLP/HTTP JSON 请求体发送到 Langfuse 的 OTel 端点。
package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// TracesPath 是 Langfuse 的 OTLP traces 端点。
const TracesPath = "/api/public/otel/v1/traces"

const (
	maxResponseBytes = 64 << 10
	// MaxRetryAfter 是服务端 Retry-After 的采信上限，防止异常值让上传长期停摆。
	MaxRetryAfter   = 5 * time.Minute
	maxMessageRunes = 200
)

// Outcome 是一次发送的结论。
type Outcome int

const (
	OK        Outcome = iota
	Retry             // 网络错误、3xx、401/403/404、408、429、5xx：稍后重试（可能是凭证轮换或部署中）
	Permanent         // 其余 4xx（如 400、413、422）：请求本身有问题，重试无意义，移入 failed
)

func (o Outcome) String() string {
	return [...]string{"ok", "retry", "permanent"}[o]
}

// Result 描述一次发送。Err 中不会包含凭证。
type Result struct {
	Outcome    Outcome
	Status     int
	Rejected   int64 // OTLP partialSuccess.rejectedSpans
	Message    string
	RetryAfter time.Duration
	Err        error
}

// Sender 发送 OTLP 请求；零值不可用，请使用 New。
type Sender struct {
	endpoint string
	auth     string
	client   *http.Client
}

// New 创建 Sender。client 为 nil 时使用带超时的默认 client（遵循 HTTPS_PROXY 等环境变量）。
// 无论传入哪个 client，都不跟随重定向：重定向可能把 Authorization 头带到别处或降级为明文。
func New(host, publicKey, secretKey string, client *http.Client) *Sender {
	c := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	token := base64.StdEncoding.EncodeToString([]byte(publicKey + ":" + secretKey))
	return &Sender{
		endpoint: strings.TrimRight(host, "/") + TracesPath,
		auth:     "Basic " + token,
		client:   &c,
	}
}

// Send 以 gzip 压缩发送请求体。
func (s *Sender) Send(ctx context.Context, body []byte) Result {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return Result{Outcome: Permanent, Err: fmt.Errorf("exporter: gzip: %w", err)}
	}
	if err := zw.Close(); err != nil {
		return Result{Outcome: Permanent, Err: fmt.Errorf("exporter: gzip: %w", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, &buf)
	if err != nil {
		return Result{Outcome: Permanent, Err: fmt.Errorf("exporter: build request: %w", err)}
	}
	req.Header.Set("Authorization", s.auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := s.client.Do(req)
	if err != nil {
		return Result{Outcome: Retry, Err: fmt.Errorf("exporter: post: %w", err)}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	res := Result{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Outcome = OK
		res.Rejected, res.Message = parsePartialSuccess(respBody)
		return res
	case retryable(resp.StatusCode):
		res.Outcome = Retry
	default:
		res.Outcome = Permanent
	}
	res.Message = summarize(respBody)
	res.Err = fmt.Errorf("exporter: HTTP %d: %s", resp.StatusCode, res.Message)
	return res
}

func retryable(code int) bool {
	switch {
	case code >= 300 && code < 400, code >= 500:
		return true
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return false
}

func parsePartialSuccess(body []byte) (int64, string) {
	var r struct {
		PartialSuccess struct {
			RejectedSpans json.RawMessage `json:"rejectedSpans"`
			ErrorMessage  string          `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(body, &r) != nil {
		return 0, ""
	}
	// OTLP JSON 中 int64 以字符串编码，也兼容数字
	raw := strings.Trim(string(r.PartialSuccess.RejectedSpans), `"`)
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n, sanitize(r.PartialSuccess.ErrorMessage)
}

// summarize 把服务端响应体整理成单行说明。
func summarize(body []byte) string { return sanitize(string(body)) }

// sanitize 去掉控制字符（防止终端转义注入）、合并空白，并按字符截断。
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > maxMessageRunes {
		s = string(runes[:maxMessageRunes]) + "…"
	}
	return s
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs > 0 {
		if secs > int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), MaxRetryAfter)
	}
	return 0
}
