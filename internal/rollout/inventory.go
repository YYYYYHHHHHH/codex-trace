package rollout

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// skillsTag 标记 developer 消息中的 skill 段（根目录表 + 可用 skill 列表）。
const skillsTag = "<skills_instructions>"

var (
	skillRootLine = regexp.MustCompile("(?m)^- `(r\\d+)` = `([^`\n]+)`")
	// 名称取到第一个 ": " 为止（名称本身可含 \":\"，如 composio:composio）；文件取行尾的 (file: …)。
	skillLine  = regexp.MustCompile(`(?m)^- (\S+): .*\(file: ([^()\n]+)\)\s*$`)
	homePrefix = regexp.MustCompile(`^/(?:Users|home)/[^/]+/`)
)

// Inventory 是某个轮次的模型上下文中可用的工具与 skill，以及本轮实际读取过的 skill。
type Inventory struct {
	Tools      []string // 命名空间.工具名，如 functions.exec、mcp__cua_repl.js
	MCPServers []string // 由 mcp__<server> 命名空间得出
	Skills     []Skill
	SkillsUsed []string // 本轮工具调用参数中出现过其 SKILL.md 路径的 skill
}

// Skill 是上下文中声明的一个 skill。
type Skill struct {
	Name string
	File string // 按 skill 根目录表展开后的 SKILL.md 路径；无法展开时保留原样
}

// Empty 表示没有任何可报告的信息。
func (inv Inventory) Empty() bool {
	return len(inv.Tools) == 0 && len(inv.Skills) == 0
}

// HomeRelative 把 /Users/<name>/ 或 /home/<name>/ 开头的路径改写为 ~/，上传时不带用户名。
func HomeRelative(path string) string {
	return homePrefix.ReplaceAllString(path, "~/")
}

func buildInventory(tools []json.RawMessage, skillsText string, gens map[string]GenerationContent) Inventory {
	inv := Inventory{Tools: toolNames(tools, "")}
	seen := map[string]bool{}
	for _, name := range inv.Tools {
		ns, _, ok := strings.Cut(name, ".")
		server, isMCP := strings.CutPrefix(ns, "mcp__")
		if ok && isMCP && !seen[server] {
			seen[server] = true
			inv.MCPServers = append(inv.MCPServers, server)
		}
	}
	inv.Skills = parseSkills(skillsText)
	inv.SkillsUsed = skillsUsed(inv.Skills, gens)
	return inv
}

// toolNames 展开工具定义：命名空间下的工具记为 <命名空间>.<工具名>。
func toolNames(tools []json.RawMessage, prefix string) []string {
	var out []string
	for _, raw := range tools {
		var t struct {
			Type  string            `json:"type"`
			Name  string            `json:"name"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		name := firstNonEmpty(t.Name, t.Type)
		if len(t.Tools) > 0 {
			out = append(out, toolNames(t.Tools, prefix+name+".")...)
			continue
		}
		if name != "" {
			out = append(out, prefix+name)
		}
	}
	return out
}

// parseSkills 解析 skill 段中的可用 skill 列表，并按根目录表（`rN` = `/abs/path`）展开文件路径。
func parseSkills(text string) []Skill {
	if text == "" {
		return nil
	}
	roots := map[string]string{}
	for _, m := range skillRootLine.FindAllStringSubmatch(text, -1) {
		roots[m[1]] = strings.TrimRight(m[2], "/")
	}
	var out []Skill
	for _, m := range skillLine.FindAllStringSubmatch(text, -1) {
		file := strings.TrimSpace(m[2])
		if root, rest, ok := strings.Cut(file, "/"); ok && roots[root] != "" {
			file = roots[root] + "/" + rest
		}
		out = append(out, Skill{Name: m[1], File: file})
	}
	return out
}

// skillsUsed 返回本轮工具调用参数中出现过 SKILL.md 路径（绝对路径或 ~/ 形式）的 skill，按名称排序。
func skillsUsed(skills []Skill, gens map[string]GenerationContent) []string {
	var args strings.Builder
	for _, g := range gens {
		for _, m := range g.Output {
			for _, c := range m.ToolCalls {
				args.WriteString(c.Function.Arguments)
				args.WriteByte('\n')
			}
		}
	}
	all := args.String()
	if all == "" {
		return nil
	}
	var out []string
	for _, s := range skills {
		if !strings.HasPrefix(s.File, "/") {
			continue // 未展开的短路径太短，容易误判
		}
		if strings.Contains(all, s.File) || strings.Contains(all, HomeRelative(s.File)) {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}
