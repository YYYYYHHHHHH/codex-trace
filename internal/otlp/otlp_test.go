package otlp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestIDsAreDeterministicAndDistinct(t *testing.T) {
	if NewTraceID("turn-1") != NewTraceID("turn-1") {
		t.Error("trace id must be deterministic")
	}
	if NewTraceID("turn-1") == NewTraceID("turn-2") {
		t.Error("different keys must give different trace ids")
	}
	if NewSpanID("tool", "x") == NewSpanID("turn", "x") {
		t.Error("span id must depend on kind")
	}
	if len(NewTraceID("a").String()) != 32 || len(NewSpanID("k", "a").String()) != 16 {
		t.Error("hex lengths must match OTLP (32 / 16)")
	}
	var zero SpanID
	if !zero.IsZero() || NewSpanID("k", "a").IsZero() {
		t.Error("IsZero wrong")
	}
}

func TestEncodeProducesOTLPJSON(t *testing.T) {
	start := time.Unix(1700000000, 5).UTC()
	root := Span{
		TraceID: NewTraceID("t"),
		SpanID:  NewSpanID("turn", "t"),
		Name:    "codex turn",
		Start:   start,
		End:     start.Add(time.Second),
		Attrs: []Attr{
			String("session.id", "s1"),
			Int("tokens", 42),
			Bool("flag", true),
			Float("ratio", 0.5),
			Strings("langfuse.trace.tags", []string{"codex", "interrupted"}),
		},
	}
	child := Span{
		TraceID: root.TraceID,
		SpanID:  NewSpanID("tool", "c1"),
		Parent:  root.SpanID,
		Name:    "Bash",
		Start:   start,
		End:     start,
		Status:  Status{Error: true, Message: "exit code 1"},
	}

	data, err := Encode([]Attr{String("service.name", "codex")}, "codex-trace", "1.0.0", []Span{root, child})
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Scope struct{ Name, Version string } `json:"scope"`
				Spans []map[string]any               `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	spans := doc.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 || doc.ResourceSpans[0].ScopeSpans[0].Scope.Name != "codex-trace" {
		t.Fatalf("unexpected document: %s", data)
	}
	r := spans[0]
	if r["traceId"] != root.TraceID.String() || r["spanId"] != root.SpanID.String() {
		t.Errorf("ids not hex-encoded: %v", r)
	}
	if _, ok := r["parentSpanId"]; ok {
		t.Error("root span must not have parentSpanId")
	}
	if r["startTimeUnixNano"] != "1700000000000000005" {
		t.Errorf("startTimeUnixNano = %v (must be a decimal string)", r["startTimeUnixNano"])
	}
	if r["kind"] != float64(1) {
		t.Errorf("kind = %v, want INTERNAL (1)", r["kind"])
	}
	s := string(data)
	for _, want := range []string{
		`{"key":"tokens","value":{"intValue":"42"}}`,
		`{"key":"flag","value":{"boolValue":true}}`,
		`{"key":"ratio","value":{"doubleValue":0.5}}`,
		`"arrayValue":{"values":[{"stringValue":"codex"},{"stringValue":"interrupted"}]}`,
		`"status":{"code":2,"message":"exit code 1"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if spans[1]["parentSpanId"] != root.SpanID.String() {
		t.Errorf("child parent = %v", spans[1]["parentSpanId"])
	}
}

func TestEncodeOmitsStatusForOKSpans(t *testing.T) {
	data, _ := Encode(nil, "s", "v", []Span{{TraceID: NewTraceID("t"), SpanID: NewSpanID("k", "t"), Name: "ok"}})
	if strings.Contains(string(data), `"status"`) {
		t.Errorf("OK span should not carry status: %s", data)
	}
}

func TestEncodeEmptyBatch(t *testing.T) {
	data, err := Encode(nil, "s", "v", nil)
	if err != nil || !strings.Contains(string(data), `"spans":[]`) {
		t.Errorf("empty batch = %s, %v", data, err)
	}
}

func TestValueAccessors(t *testing.T) {
	if String("k", "v").Value.AsString() != "v" || Int("k", 1).Value.AsString() != "" {
		t.Error("AsString wrong")
	}
	if Int("k", 7).Value.AsInt() != 7 || String("k", "v").Value.AsInt() != 0 {
		t.Error("AsInt wrong")
	}
	if got := Strings("k", []string{"a"}).Value.AsStrings(); len(got) != 1 || got[0] != "a" {
		t.Error("AsStrings wrong")
	}
	if String("k", "v").Value.AsStrings() != nil {
		t.Error("AsStrings on string should be nil")
	}
}
