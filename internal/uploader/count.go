package uploader

import "encoding/json"

// countSpans 统计 OTLP 请求体中的 span 数，仅用于状态展示。
func countSpans(body []byte) int64 {
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []json.RawMessage `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 0
	}
	var n int64
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			n += int64(len(ss.Spans))
		}
	}
	return n
}
