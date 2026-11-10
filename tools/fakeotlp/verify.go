package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type span struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Start        string `json:"startTimeUnixNano"`
	End          string `json:"endTimeUnixNano"`
	Attributes   []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue *string `json:"stringValue"`
			IntValue    *string `json:"intValue"`
			ArrayValue  *struct {
				Values []struct {
					StringValue *string `json:"stringValue"`
				} `json:"values"`
			} `json:"arrayValue"`
		} `json:"value"`
	} `json:"attributes"`
	Status *struct {
		Code int `json:"code"`
	} `json:"status"`
}

func (s span) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key != key {
			continue
		}
		switch {
		case a.Value.StringValue != nil:
			return *a.Value.StringValue
		case a.Value.IntValue != nil:
			return *a.Value.IntValue
		case a.Value.ArrayValue != nil:
			var parts []string
			for _, v := range a.Value.ArrayValue.Values {
				if v.StringValue != nil {
					parts = append(parts, *v.StringValue)
				}
			}
			return strings.Join(parts, ",")
		}
	}
	return ""
}

func (s span) typ() string { return s.attr("langfuse.observation.type") }

type checker struct {
	out      io.Writer
	failures int
}

func (c *checker) check(ok bool, format string, args ...any) {
	mark := "✓"
	if !ok {
		mark = "✗"
		c.failures++
	}
	fmt.Fprintf(c.out, "%s %s\n", mark, fmt.Sprintf(format, args...))
}

func verify(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	in := fs.String("in", "received.jsonl", "serve 落盘的文件")
	expectSubagent := fs.Bool("expect-subagent", false, "要求存在挂在 spawn_agent 工具下的子 agent")
	expectFailed := fs.Bool("expect-failed-tool", false, "要求存在 ERROR 状态的工具调用")
	expectInterrupted := fs.Bool("expect-interrupted", false, "要求存在打上 interrupted 标签的轮次")
	noContent := fs.Bool("no-content", false, "要求不包含 input / output 内容属性")
	minTurns := fs.Int("min-turns", 1, "最少的主轮次数")
	_ = fs.Parse(args)

	spans, requests, err := load(*in)
	if err != nil {
		fmt.Fprintf(out, "✗ 读取 %s 失败: %v\n", *in, err)
		return 1
	}
	c := &checker{out: out}
	byID := map[string]span{}
	dups := 0
	for _, s := range spans {
		if _, seen := byID[s.SpanID]; seen {
			dups++
		}
		byID[s.SpanID] = s
	}
	fmt.Fprintf(out, "收到 %d 个请求、%d 个 span（去重后 %d，重复 %d）\n", requests, len(spans), len(byID), dups)

	var roots, tools, gens, agents []span
	for _, s := range byID {
		switch {
		case s.typ() == "agent" && s.ParentSpanID == "":
			roots = append(roots, s)
		case s.typ() == "agent":
			agents = append(agents, s)
		case s.typ() == "tool":
			tools = append(tools, s)
		case s.typ() == "generation":
			gens = append(gens, s)
		}
	}
	c.check(len(roots) >= *minTurns, "主轮次 %d 个（至少 %d）", len(roots), *minTurns)
	c.check(len(tools) > 0, "工具调用 %d 个", len(tools))
	c.check(len(gens) > 0, "模型调用 %d 个", len(gens))

	orphans := 0
	for _, s := range byID {
		if s.ParentSpanID == "" {
			continue
		}
		p, ok := byID[s.ParentSpanID]
		if !ok || p.TraceID != s.TraceID {
			orphans++
		}
	}
	c.check(orphans == 0, "所有子 span 的父 span 都存在且在同一 trace（孤儿 %d）", orphans)

	missingSession := 0
	for _, r := range roots {
		if r.attr("session.id") == "" || r.attr("langfuse.trace.name") == "" {
			missingSession++
		}
	}
	c.check(missingSession == 0, "主轮次都带 session.id 与 trace 名称")

	badUsage := 0
	for _, g := range gens {
		var u map[string]int64
		if json.Unmarshal([]byte(g.attr("langfuse.observation.usage_details")), &u) != nil || u["total"] <= 0 {
			badUsage++
		}
	}
	c.check(badUsage == 0, "模型调用都带有效的 usage_details（异常 %d）", badUsage)

	if *expectFailed {
		failed := 0
		for _, t := range tools {
			if t.Status != nil && t.Status.Code == 2 && t.attr("langfuse.observation.level") == "ERROR" {
				failed++
			}
		}
		c.check(failed > 0, "失败的工具调用标为 ERROR（%d 个）", failed)
	}
	if *expectSubagent {
		linked := 0
		for _, a := range agents {
			p, ok := byID[a.ParentSpanID]
			if ok && p.typ() == "tool" && strings.HasSuffix(p.Name, "spawn_agent") {
				linked++
			}
		}
		c.check(linked > 0, "子 agent 挂在 spawn_agent 工具 span 下（%d/%d）", linked, len(agents))
	}
	if *expectInterrupted {
		n := 0
		for _, r := range roots {
			if strings.Contains(r.attr("langfuse.trace.tags"), "interrupted") {
				n++
			}
		}
		c.check(n > 0, "被打断的轮次带 interrupted 标签（%d 个）", n)
	}
	if *noContent {
		leaked := map[string]bool{}
		for _, s := range byID {
			for _, a := range s.Attributes {
				if strings.HasSuffix(a.Key, ".input") || strings.HasSuffix(a.Key, ".output") {
					leaked[a.Key] = true
				}
			}
		}
		keys := make([]string, 0, len(leaked))
		for k := range leaked {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		c.check(len(leaked) == 0, "未上传内容属性 %v", keys)
	}

	if c.failures > 0 {
		fmt.Fprintf(out, "\n%d 项检查失败\n", c.failures)
		return 1
	}
	fmt.Fprintln(out, "\n全部检查通过")
	return 0
}

func load(path string) ([]span, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var spans []span
	requests := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var req struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []span `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			return nil, requests, fmt.Errorf("第 %d 行: %w", requests+1, err)
		}
		requests++
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
	}
	return spans, requests, sc.Err()
}
