package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
)

func writeBatch(t *testing.T, spans ...otlp.Span) string {
	t.Helper()
	body, err := otlp.Encode(nil, "codex-trace", "test", spans)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "received.jsonl")
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tree() []otlp.Span {
	tid := otlp.NewTraceID("t")
	root := otlp.Span{TraceID: tid, SpanID: otlp.NewSpanID("turn", "t"), Name: "codex turn", Attrs: []otlp.Attr{
		otlp.String("langfuse.observation.type", "agent"),
		otlp.String("session.id", "s"),
		otlp.String("langfuse.trace.name", "codex turn"),
		otlp.Strings("langfuse.trace.tags", []string{"codex", "interrupted"}),
	}}
	spawn := otlp.Span{TraceID: tid, SpanID: otlp.NewSpanID("tool", "spawn"), Parent: root.SpanID, Name: "collaborationspawn_agent",
		Attrs: []otlp.Attr{otlp.String("langfuse.observation.type", "tool")}}
	failed := otlp.Span{TraceID: tid, SpanID: otlp.NewSpanID("tool", "cat"), Parent: root.SpanID, Name: "Bash",
		Status: otlp.Status{Error: true, Message: "exit code 1"},
		Attrs:  []otlp.Attr{otlp.String("langfuse.observation.type", "tool"), otlp.String("langfuse.observation.level", "ERROR")}}
	sub := otlp.Span{TraceID: tid, SpanID: otlp.NewSpanID("turn", "sub"), Parent: spawn.SpanID, Name: "subagent:default",
		Attrs: []otlp.Attr{otlp.String("langfuse.observation.type", "agent")}}
	gen := otlp.Span{TraceID: tid, SpanID: otlp.NewSpanID("gen", "r"), Parent: root.SpanID, Name: "m",
		Attrs: []otlp.Attr{otlp.String("langfuse.observation.type", "generation"), otlp.String("langfuse.observation.usage_details", `{"total":10}`)}}
	return []otlp.Span{root, spawn, failed, sub, gen}
}

func TestVerifyPassesOnCompleteTree(t *testing.T) {
	path := writeBatch(t, tree()...)
	var out bytes.Buffer
	code := verify([]string{"-in", path, "-expect-subagent", "-expect-failed-tool", "-expect-interrupted", "-no-content"}, &out)
	if code != 0 {
		t.Errorf("verify failed:\n%s", out.String())
	}
}

func TestVerifyDetectsProblems(t *testing.T) {
	spans := tree()
	spans[3].Parent = otlp.NewSpanID("tool", "missing") // 孤儿子 agent
	spans[0].Attrs = append(spans[0].Attrs, otlp.String("langfuse.observation.input", "secret prompt"))
	path := writeBatch(t, spans...)
	var out bytes.Buffer

	code := verify([]string{"-in", path, "-expect-subagent", "-no-content", "-min-turns", "2"}, &out)

	if code != 1 {
		t.Fatal("verify should fail")
	}
	for _, want := range []string{"孤儿 1", "主轮次 1 个", "langfuse.observation.input", "子 agent 挂在 spawn_agent 工具 span 下（0/1）"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestVerifyMissingFile(t *testing.T) {
	var out bytes.Buffer
	if code := verify([]string{"-in", filepath.Join(t.TempDir(), "none")}, &out); code != 1 {
		t.Error("missing input should fail")
	}
}
