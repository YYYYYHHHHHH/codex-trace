// Package redact 在上传前对文本脱敏并截断。只在 capture_content=true 时用到。
//
// 规则是"宁可多遮"：匹配常见密钥形态与 key=value 形式的敏感字段。
// 它不能替代"默认不上传内容"的策略，只是开启内容采集后的第二道防线。
package redact

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Placeholder 替换被遮蔽的内容。
const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

// sensitiveKey 匹配含敏感词的字段名，如 api_key、AWS_SECRET_ACCESS_KEY、auth_token、password。
const sensitiveKey = `[A-Za-z0-9_-]*(?:api[_-]?key|secret|access[_-]?key|token|password|passwd|pwd|private[_-]?key|credential)[A-Za-z0-9_-]*`

var rules = []rule{
	// PEM 私钥：完整块，以及被截断、没有 END 行的块
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), Placeholder},
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*`), Placeholder},
	// 带固定前缀的令牌
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), Placeholder},
	{regexp.MustCompile(`\bpk-lf-[A-Za-z0-9_-]{8,}`), Placeholder},
	{regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[0-9A-Za-z]{16,}`), Placeholder},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Placeholder},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), Placeholder},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), Placeholder},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), Placeholder},
	{regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`), Placeholder},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), Placeholder},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`), Placeholder},
	// Authorization / Cookie 头：无论长度一律遮蔽，保留头名与认证方案名
	{regexp.MustCompile(`(?i)\b(authorization["']?\s*[:=]\s*["']?(?:bearer\s+|basic\s+|token\s+)?)[^\s"',;}]+`), "${1}" + Placeholder},
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie["']?\s*:\s*)[^\r\n"']+`), "${1}" + Placeholder},
	// 游离的 Bearer / Basic 凭证（要求 16 位以上，避免误伤 "basic information"）
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`), "$1 " + Placeholder},
	// URL 中的账号密码，例如 https://bob:secret@host
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Placeholder + "@"},
	// curl -u user:pass / --user user:pass
	{regexp.MustCompile(`((?:^|\s)(?:-u|--user)(?:=|\s+))[^\s:]+:\S+`), "${1}" + Placeholder},
	// 命令行参数 --password xxx / --token=xxx
	{regexp.MustCompile(`(?i)(--?(?:password|passwd|token|api-key|apikey|secret)(?:=|\s+))\S+`), "${1}" + Placeholder},
}

// 字段级规则：先识别字段名，再决定是否遮蔽值（排除 max_tokens 这类计数字段）。
var (
	// "key": "value" / "key": 123，值允许转义字符
	jsonField = regexp.MustCompile(`(?i)"(` + sensitiveKey + `)"(\s*:\s*)("(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?)`)
	// 已被转义的 JSON：\"key\": \"value\"
	escapedJSONField = regexp.MustCompile(`(?i)\\"(` + sensitiveKey + `)\\"(\s*:\s*)(\\"(?:[^\\]|\\[^"])*\\"|-?\d+)`)
	// key=value / key: value / 环境变量，值可以带引号（含空格）
	kvField = regexp.MustCompile(`(?i)\b(` + sensitiveKey + `)(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;}]+)`)
)

// Text 返回脱敏后的文本。
func Text(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	s = replaceField(s, jsonField, func(key, sep string) string { return `"` + key + `"` + sep + `"` + Placeholder + `"` })
	s = replaceField(s, escapedJSONField, func(key, sep string) string {
		return `\"` + key + `\"` + sep + `\"` + Placeholder + `\"`
	})
	s = replaceField(s, kvField, func(key, sep string) string { return key + sep + Placeholder })
	return s
}

func replaceField(s string, re *regexp.Regexp, build func(key, sep string) string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) < 3 || isCountKey(sub[1]) {
			return m
		}
		return build(sub[1], sub[2])
	})
}

// isCountKey 识别 token 计数一类的字段（max_tokens、input_tokens、token_count），它们不是密钥。
func isCountKey(key string) bool {
	k := strings.ToLower(key)
	return strings.HasSuffix(k, "tokens") || strings.Contains(k, "count") || strings.Contains(k, "limit")
}

// Truncate 把 s 截断到最多约 max 字节（保持 UTF-8 合法），并注明被截掉的字节数。
func Truncate(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…[truncated %d bytes]", s[:cut], len(s)-cut)
}
