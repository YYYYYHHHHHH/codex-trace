// Package redact 在上传前对文本脱敏并截断。只在 capture_content=true 时用到。
//
// 规则是"宁可多遮"：匹配常见密钥形态与 key=value 形式的敏感字段。
// 它不能替代"默认不上传内容"的策略，只是开启内容采集后的第二道防线。
package redact

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// Placeholder 替换被遮蔽的内容。
const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	// PEM 私钥整块
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), Placeholder},
	// 带固定前缀的令牌
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), Placeholder},
	{regexp.MustCompile(`\bpk-lf-[A-Za-z0-9_-]{8,}`), Placeholder},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Placeholder},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), Placeholder},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), Placeholder},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), Placeholder},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`), Placeholder},
	// Authorization: Bearer / Basic
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 " + Placeholder},
	// key = value / "key": "value" 形式的敏感字段
	{
		regexp.MustCompile(`(?i)((?:api[_-]?key|secret(?:[_-]?key)?|access[_-]?token|auth[_-]?token|password|passwd|private[_-]?key)["']?\s*[:=]\s*["']?)[^\s"',}]{6,}`),
		"${1}" + Placeholder,
	},
}

// Text 返回脱敏后的文本。
func Text(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Truncate 把 s 截断到最多约 max 字节（保持 UTF-8 合法），并注明被截掉的字节数。
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…[truncated %d bytes]", s[:cut], len(s)-cut)
}
