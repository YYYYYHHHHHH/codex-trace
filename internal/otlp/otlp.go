// Package otlp 定义导出用的 span 模型，并编码为 OTLP/HTTP JSON（ExportTraceServiceRequest）。
//
// 不依赖 OpenTelemetry SDK：span 是在轮次结束后根据已知时间与确定性 ID 事后构造的，
// SDK 面向"实时创建 span"的模型反而不合适。
package otlp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// spanKindInternal 是 OTLP SpanKind.SPAN_KIND_INTERNAL。
const spanKindInternal = 1

// statusCodeError 是 OTLP Status.STATUS_CODE_ERROR。
const statusCodeError = 2

type TraceID [16]byte

type SpanID [8]byte

// NewTraceID 由业务 key（root turn id）确定性地派生 trace id，重发时保持不变。
func NewTraceID(key string) TraceID {
	var id TraceID
	sum := sha256.Sum256([]byte("codex-trace:trace:" + key))
	copy(id[:], sum[:])
	return id
}

// NewSpanID 由 (种类, 业务 id) 确定性地派生 span id。
func NewSpanID(kind, key string) SpanID {
	var id SpanID
	sum := sha256.Sum256([]byte("codex-trace:" + kind + ":" + key))
	copy(id[:], sum[:])
	if id.IsZero() { // 全零 id 在 OTLP 中非法，概率可忽略但仍兜底
		id[7] = 1
	}
	return id
}

func (t TraceID) String() string { return hex.EncodeToString(t[:]) }
func (s SpanID) String() string  { return hex.EncodeToString(s[:]) }
func (s SpanID) IsZero() bool    { return s == SpanID{} }

// Attr 是一个 span / resource 属性。
type Attr struct {
	Key   string
	Value Value
}

// Value 对应 OTLP AnyValue 的一个子集。
type Value struct {
	kind valueKind
	str  string
	num  int64
	flt  float64
	flag bool
	list []string
}

type valueKind int

const (
	kindString valueKind = iota
	kindInt
	kindFloat
	kindBool
	kindStrings
)

func String(k, v string) Attr        { return Attr{k, Value{kind: kindString, str: v}} }
func Int(k string, v int64) Attr     { return Attr{k, Value{kind: kindInt, num: v}} }
func Float(k string, v float64) Attr { return Attr{k, Value{kind: kindFloat, flt: v}} }
func Bool(k string, v bool) Attr     { return Attr{k, Value{kind: kindBool, flag: v}} }

func Strings(k string, v []string) Attr {
	return Attr{k, Value{kind: kindStrings, list: append([]string(nil), v...)}}
}

// Status 只区分 OK（零值）与 ERROR。
type Status struct {
	Error   bool
	Message string
}

// Span 是一个已完成的 span。Parent 为零值表示根 span。
type Span struct {
	TraceID TraceID
	SpanID  SpanID
	Parent  SpanID
	Name    string
	Start   time.Time
	End     time.Time
	Attrs   []Attr
	Status  Status
}

// Encode 把一批 span 编码为单个 resource / scope 下的 OTLP JSON 请求体。
func Encode(resource []Attr, scopeName, scopeVersion string, spans []Span) ([]byte, error) {
	jsonSpans := make([]jsonSpan, 0, len(spans))
	for _, s := range spans {
		jsonSpans = append(jsonSpans, toJSONSpan(s))
	}
	req := jsonRequest{ResourceSpans: []jsonResourceSpans{{
		Resource: jsonResource{Attributes: toJSONAttrs(resource)},
		ScopeSpans: []jsonScopeSpans{{
			Scope: jsonScope{Name: scopeName, Version: scopeVersion},
			Spans: jsonSpans,
		}},
	}}}
	return json.Marshal(req)
}

type jsonRequest struct {
	ResourceSpans []jsonResourceSpans `json:"resourceSpans"`
}

type jsonResourceSpans struct {
	Resource   jsonResource     `json:"resource"`
	ScopeSpans []jsonScopeSpans `json:"scopeSpans"`
}

type jsonResource struct {
	Attributes []jsonAttr `json:"attributes"`
}

type jsonScopeSpans struct {
	Scope jsonScope  `json:"scope"`
	Spans []jsonSpan `json:"spans"`
}

type jsonScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type jsonSpan struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	ParentSpanID      string      `json:"parentSpanId,omitempty"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []jsonAttr  `json:"attributes"`
	Status            *jsonStatus `json:"status,omitempty"`
}

type jsonStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type jsonAttr struct {
	Key   string    `json:"key"`
	Value jsonValue `json:"value"`
}

// jsonValue 按 OTLP JSON 映射编码：int64 用十进制字符串。
type jsonValue struct {
	StringValue *string     `json:"stringValue,omitempty"`
	IntValue    *string     `json:"intValue,omitempty"`
	DoubleValue *float64    `json:"doubleValue,omitempty"`
	BoolValue   *bool       `json:"boolValue,omitempty"`
	ArrayValue  *jsonValues `json:"arrayValue,omitempty"`
}

type jsonValues struct {
	Values []jsonValue `json:"values"`
}

func toJSONSpan(s Span) jsonSpan {
	js := jsonSpan{
		TraceID:           s.TraceID.String(),
		SpanID:            s.SpanID.String(),
		Name:              s.Name,
		Kind:              spanKindInternal,
		StartTimeUnixNano: unixNano(s.Start),
		EndTimeUnixNano:   unixNano(s.End),
		Attributes:        toJSONAttrs(s.Attrs),
	}
	if !s.Parent.IsZero() {
		js.ParentSpanID = s.Parent.String()
	}
	if s.Status.Error {
		js.Status = &jsonStatus{Code: statusCodeError, Message: s.Status.Message}
	}
	return js
}

func toJSONAttrs(attrs []Attr) []jsonAttr {
	out := make([]jsonAttr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, jsonAttr{Key: a.Key, Value: toJSONValue(a.Value)})
	}
	return out
}

func toJSONValue(v Value) jsonValue {
	switch v.kind {
	case kindInt:
		s := strconv.FormatInt(v.num, 10)
		return jsonValue{IntValue: &s}
	case kindFloat:
		f := v.flt
		return jsonValue{DoubleValue: &f}
	case kindBool:
		b := v.flag
		return jsonValue{BoolValue: &b}
	case kindStrings:
		vals := make([]jsonValue, 0, len(v.list))
		for _, s := range v.list {
			s := s
			vals = append(vals, jsonValue{StringValue: &s})
		}
		return jsonValue{ArrayValue: &jsonValues{Values: vals}}
	default:
		s := v.str
		return jsonValue{StringValue: &s}
	}
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

// AsString 返回字符串值；非字符串类型返回空串。
func (v Value) AsString() string {
	if v.kind != kindString {
		return ""
	}
	return v.str
}

// AsInt 返回整数值；非整数类型返回 0。
func (v Value) AsInt() int64 {
	if v.kind != kindInt {
		return 0
	}
	return v.num
}

// AsStrings 返回字符串数组的副本；非数组类型返回 nil。
func (v Value) AsStrings() []string {
	if v.kind != kindStrings {
		return nil
	}
	return append([]string(nil), v.list...)
}
