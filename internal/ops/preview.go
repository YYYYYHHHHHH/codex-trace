package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
)

// PreviewOptions 控制 preview 输出。
type PreviewOptions struct {
	Raw bool // 输出原始 OTLP JSON
}

// Preview 展示即将上传（outbox 中最早的一批）或最近一次已上传的数据，便于用户确认上传内容。
func Preview(e Env, opts PreviewOptions) error {
	body, label, err := previewBody(e)
	if err != nil {
		return err
	}
	e.printf("%s\n\n", label)
	if opts.Raw {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err != nil {
			return fmt.Errorf("格式化 JSON: %w", err)
		}
		e.printf("%s\n", buf.String())
		return nil
	}
	return printTree(e, body)
}

func previewBody(e Env) ([]byte, string, error) {
	q := queue.New(e.Paths.OutboxDir())
	if entries, err := q.List(); err == nil && len(entries) > 0 {
		body, err := q.Read(entries[0].Name)
		if err == nil {
			return body, fmt.Sprintf("待上传（outbox 共 %d 批，显示最早一批）", len(entries)), nil
		}
	}
	body, err := os.ReadFile(e.Paths.LastBatchFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", errors.New("还没有任何待上传或已上传的数据")
	}
	if err != nil {
		return nil, "", fmt.Errorf("读取最近一批: %w", err)
	}
	return body, "最近一次已上传的批次", nil
}

type previewSpan struct {
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Attributes   []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue *string `json:"stringValue"`
			IntValue    *string `json:"intValue"`
		} `json:"value"`
	} `json:"attributes"`
}

func (s previewSpan) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key != key {
			continue
		}
		if a.Value.StringValue != nil {
			return *a.Value.StringValue
		}
		if a.Value.IntValue != nil {
			return *a.Value.IntValue
		}
	}
	return ""
}

// printTree 按父子关系缩进打印 span，并列出每个 span 携带的属性名（内容属性显示长度）。
func printTree(e Env, body []byte) error {
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []previewSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("解析批次: %w", err)
	}
	var spans []previewSpan
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			spans = append(spans, ss.Spans...)
		}
	}
	children := map[string][]previewSpan{}
	ids := map[string]bool{}
	for _, s := range spans {
		ids[s.SpanID] = true
	}
	var roots []previewSpan
	for _, s := range spans {
		if s.ParentSpanID == "" || !ids[s.ParentSpanID] {
			roots = append(roots, s)
			continue
		}
		children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
	}
	var walk func(s previewSpan, depth int)
	walk = func(s previewSpan, depth int) {
		indent := strings.Repeat("  ", depth)
		e.printf("%s- [%s] %s\n", indent, orDash(s.attr("langfuse.observation.type")), s.Name)
		e.printf("%s    %s\n", indent, describeAttrs(s))
		for _, c := range children[s.SpanID] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	e.printf("\n共 %d 个 span。查看完整内容：trace-hook preview --raw\n", len(spans))
	return nil
}

func describeAttrs(s previewSpan) string {
	var parts []string
	for _, a := range s.Attributes {
		key := strings.TrimPrefix(strings.TrimPrefix(a.Key, "langfuse.observation."), "langfuse.")
		if strings.HasSuffix(a.Key, ".input") || strings.HasSuffix(a.Key, ".output") {
			n := 0
			if a.Value.StringValue != nil {
				n = len(*a.Value.StringValue)
			}
			parts = append(parts, fmt.Sprintf("%s(%d 字节)", key, n))
			continue
		}
		parts = append(parts, key)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
