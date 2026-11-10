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
)

// TracesPath 是 Langfuse 的 OTLP traces 端点。
const TracesPath = "/api/public/otel/v1/traces"

const maxResponseBytes = 64 << 10

// Outcome 是一次发送的结论。
type Outcome int

const (
	OK        Outcome = iota
	Retry             // 网络错误、408、429、5xx：稍后重试
	Permanent         // 其他 4xx：重试无意义，移入 failed
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
func New(host, publicKey, secretKey string, client *http.Client) *Sender {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	token := base64.StdEncoding.EncodeToString([]byte(publicKey + ":" + secretKey))
	return &Sender{
		endpoint: strings.TrimRight(host, "/") + TracesPath,
		auth:     "Basic " + token,
		client:   client,
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
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		res.Outcome = Retry
	default:
		res.Outcome = Permanent
	}
	res.Message = summarize(respBody)
	res.Err = fmt.Errorf("exporter: HTTP %d: %s", resp.StatusCode, res.Message)
	return res
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
	return n, r.PartialSuccess.ErrorMessage
}

// summarize 取响应体前 200 字节作为错误说明，并去掉换行。
func summarize(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
