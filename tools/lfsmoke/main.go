// lfsmoke 是对真实 Langfuse 的验收工具。
//
//	lfsmoke fixture [-fixture testdata/sessions/subagent-resume] [-resend]
//	    把采集到的真实会话 fixture 经 pipeline 转成 span 并发送，再读回逐个比对 id / trace / 父节点。
//	    -resend 会把同一批 span 再发一次，检查 Langfuse 是否按 span id 去重。
//	lfsmoke verify -session ID[,ID...] [-min-turns N] [-expect-subagent] [-expect-failed-tool] [-expect-interrupted] [-no-content]
//	    按会话从 Langfuse 读回真实上报的数据并校验调用树（真实环境 E2E 用）。
//
// 凭证只从 -env（默认 ~/.config/codex-trace/build.env）读取，不会打印。
//
// Langfuse v4 对同一 span id 的多次写入会在后台合并为最新版本；合并前读取可能看到多行，
// 因此读回时按 id 只保留 updatedAt 最新的一行。
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/exporter"
	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/pipeline"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
	"github.com/YYYYYHHHHHH/codex-trace/internal/trace"
)

const userAgent = "codex-trace/lfsmoke"

type creds struct{ host, pk, sk, environment string }

type obs = map[string]any

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("用法: lfsmoke fixture|verify [flags]"))
	}
	switch os.Args[1] {
	case "fixture":
		os.Exit(runFixture(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	default:
		fail(fmt.Errorf("未知子命令 %q", os.Args[1]))
	}
}

func defaultEnvFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "codex-trace", "build.env")
}

func runFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ExitOnError)
	envFile := fs.String("env", defaultEnvFile(), "凭证文件")
	fixture := fs.String("fixture", "testdata/sessions/subagent-resume", "会话 fixture 目录")
	wait := fs.Duration("wait", 90*time.Second, "等待 Langfuse 落库的最长时间")
	resend := fs.Bool("resend", false, "再发送一次同一批 span，检查去重")
	dump := fs.String("dump", "", "把读回的 observation 原始 JSON 写入该文件")
	_ = fs.Parse(args)

	c, err := loadCreds(*envFile)
	if err != nil {
		fail(err)
	}
	spans, err := buildSpans(*fixture, c.environment)
	if err != nil {
		fail(err)
	}
	traceIDs := map[string]bool{}
	for _, s := range spans {
		traceIDs[s.TraceID.String()] = true
	}
	fmt.Printf("fixture %s → %d 个 span，%d 条 trace\n", *fixture, len(spans), len(traceIDs))

	sender := exporter.New(c.host, c.pk, c.sk, nil).WithUserAgent(userAgent)
	send := func(label string) {
		body, err := otlp.Encode([]otlp.Attr{otlp.String("service.name", "codex")}, "codex-trace", "lfsmoke", spans)
		if err != nil {
			fail(err)
		}
		res := sender.Send(context.Background(), body)
		if res.Outcome != exporter.OK {
			fail(fmt.Errorf("%s 发送失败: %v", label, res.Err))
		}
		fmt.Printf("%s：HTTP %d，拒收 %d\n", label, res.Status, res.Rejected)
	}
	send("第一次发送")

	a := newAPI(c)
	query := func() ([]obs, int, error) { return a.byTraces(keys(traceIDs)) }
	all, raw, err := a.waitFor(query, len(spans), *wait)
	if err != nil {
		fail(err)
	}
	if *dump != "" {
		data, _ := json.MarshalIndent(all, "", "  ")
		_ = os.WriteFile(*dump, data, 0o600)
	}
	ck := &checker{}
	printTree(all)
	fmt.Printf("读回 %d 行，%d 个不同 id\n", raw, len(all))
	ck.check(len(all) == len(spans), "observation 数 %d = span 数 %d", len(all), len(spans))
	byID := index(all)
	mismatch := 0
	for _, s := range spans {
		o, ok := byID[s.SpanID.String()]
		wantParent := ""
		if !s.Parent.IsZero() {
			wantParent = s.Parent.String()
		}
		if !ok || str(o, "parentObservationId") != wantParent || str(o, "traceId") != s.TraceID.String() {
			mismatch++
		}
	}
	ck.check(mismatch == 0, "每个 span 都以相同 id、trace、父节点落库（不一致 %d）", mismatch)
	commonChecks(ck, all, c.environment, true)

	if *resend {
		send("第二次发送（同一批 span id）")
		time.Sleep(20 * time.Second)
		again, rawAgain, err := query()
		if err != nil {
			fail(err)
		}
		ck.check(len(again) == len(all), "重复发送后不同 id 仍为 %d（原始行 %d，合并前可能暂时多于 id 数）", len(again), rawAgain)
	}
	return ck.result()
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	envFile := fs.String("env", defaultEnvFile(), "凭证文件")
	sessions := fs.String("session", "", "逗号分隔的会话 id")
	environment := fs.String("environment", "", "期望的 environment（默认取凭证文件中的值）")
	minTurns := fs.Int("min-turns", 1, "最少的主轮次数")
	expectSubagent := fs.Bool("expect-subagent", false, "要求子 agent 挂在 spawn_agent 工具下")
	expectFailed := fs.Bool("expect-failed-tool", false, "要求存在 ERROR 的工具调用")
	expectInterrupted := fs.Bool("expect-interrupted", false, "要求存在带 interrupted 标签的轮次")
	noContent := fs.Bool("no-content", false, "要求没有上传 input / output 内容")
	wait := fs.Duration("wait", 120*time.Second, "等待 Langfuse 落库的最长时间")
	_ = fs.Parse(args)

	c, err := loadCreds(*envFile)
	if err != nil {
		fail(err)
	}
	if *environment == "" {
		*environment = c.environment
	}
	ids := strings.FieldsFunc(*sessions, func(r rune) bool { return r == ',' })
	if len(ids) == 0 {
		fail(fmt.Errorf("缺少 -session"))
	}
	a := newAPI(c)
	query := func() ([]obs, int, error) { return a.bySessions(ids) }
	// 真实环境下不知道确切 span 数：等到数量连续两次不变
	all, _, err := a.waitStable(query, *wait)
	if err != nil {
		fail(err)
	}
	printTree(all)

	ck := &checker{}
	byID := index(all)
	var roots []obs
	orphans := 0
	for _, o := range all {
		p := str(o, "parentObservationId")
		if p == "" {
			roots = append(roots, o)
			continue
		}
		if parent, ok := byID[p]; !ok || str(parent, "traceId") != str(o, "traceId") {
			orphans++
		}
	}
	ck.check(len(roots) >= *minTurns, "主轮次 %d 个（至少 %d）", len(roots), *minTurns)
	ck.check(orphans == 0, "所有子节点的父节点都存在且在同一 trace（孤儿 %d）", orphans)
	commonChecks(ck, all, *environment, *expectFailed)

	if *expectSubagent {
		linked, subs := 0, 0
		for _, o := range all {
			if str(o, "type") != "AGENT" || str(o, "parentObservationId") == "" {
				continue
			}
			subs++
			if p := byID[str(o, "parentObservationId")]; p != nil && str(p, "type") == "TOOL" && strings.HasSuffix(str(p, "name"), "spawn_agent") {
				linked++
			}
		}
		ck.check(subs > 0 && linked == subs, "子 agent 挂在 spawn_agent 工具下（%d/%d）", linked, subs)
	}
	if *expectInterrupted {
		n := 0
		for _, r := range roots {
			if strings.Contains(str(r, "tags"), "interrupted") {
				n++
			}
		}
		ck.check(n > 0, "被打断的轮次带 interrupted 标签（%d 个）", n)
	}
	if *noContent {
		leaked := 0
		for _, o := range all {
			if str(o, "input") != "" || str(o, "output") != "" {
				leaked++
			}
		}
		ck.check(leaked == 0, "没有上传 input / output 内容（%d 个 observation 带内容）", leaked)
	}
	return ck.result()
}

// commonChecks 校验与具体数据无关的映射约定。
func commonChecks(ck *checker, all []obs, environment string, expectError bool) {
	types := map[string]int{}
	errs, gens, usageOK, envOK, sessionOK := 0, 0, 0, 0, 0
	for _, o := range all {
		types[str(o, "type")]++
		if str(o, "level") == "ERROR" {
			errs++
		}
		if str(o, "type") == "GENERATION" {
			gens++
			var u map[string]float64
			_ = json.Unmarshal([]byte(str(o, "usageDetails")), &u)
			if u["total"] > 0 && str(o, "model") != "" {
				usageOK++
			}
		}
		if environment == "" || str(o, "environment") == environment {
			envOK++
		}
		if str(o, "sessionId") != "" && str(o, "userId") != "" {
			sessionOK++
		}
	}
	ck.check(types["AGENT"] > 0 && types["TOOL"] > 0 && types["GENERATION"] > 0, "observation 类型 %v", types)
	if expectError {
		ck.check(errs > 0, "失败工具调用以 ERROR 落库（%d 个）", errs)
	}
	ck.check(gens > 0 && usageOK == gens, "generation 带模型名与 usage（%d/%d）", usageOK, gens)
	ck.check(envOK == len(all), "environment=%s（%d/%d）", environment, envOK, len(all))
	ck.check(sessionOK == len(all), "所有 observation 带 sessionId 与 userId（%d/%d）", sessionOK, len(all))
}

// loadCreds 按 KEY=VALUE 解析 build.env（与 scripts/build-dist.sh 一致，不作为 shell 执行）。
func loadCreds(path string) (creds, error) {
	f, err := os.Open(path)
	if err != nil {
		return creds{}, err
	}
	defer f.Close()
	var c creds
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		switch k {
		case "CODEX_TRACE_LANGFUSE_HOST":
			c.host = strings.TrimRight(v, "/")
		case "CODEX_TRACE_LANGFUSE_PUBLIC_KEY":
			c.pk = v
		case "CODEX_TRACE_LANGFUSE_SECRET_KEY":
			c.sk = v
		case "CODEX_TRACE_ENVIRONMENT":
			c.environment = v
		}
	}
	if c.host == "" || c.pk == "" || c.sk == "" {
		return creds{}, fmt.Errorf("%s 缺少 host / public_key / secret_key", path)
	}
	return c, sc.Err()
}

func buildSpans(dir, environment string) ([]otlp.Span, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(root, "hooks.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	store, err := os.MkdirTemp("", "lfsmoke-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(store)

	p := pipeline.New(pipeline.Options{
		Store:        session.NewStore(store),
		Policy:       trace.Policy{Environment: environment, Release: "lfsmoke"},
		AllowedRoots: []string{root},
		UserFallback: "lfsmoke",
	})
	var last time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		rec, err := event.ParseRecord([]byte(strings.ReplaceAll(sc.Text(), "ROLLOUT_DIR", root)))
		if err != nil {
			return nil, err
		}
		if err := p.Ingest(rec); err != nil {
			return nil, err
		}
		last = rec.ReceivedAt
	}
	spans := p.Flush(last.Add(time.Minute))
	if len(spans) == 0 {
		return nil, fmt.Errorf("fixture 没有产生任何 span")
	}
	return spans, nil
}

type api struct {
	c      creds
	client *http.Client
}

func newAPI(c creds) *api { return &api{c: c, client: &http.Client{Timeout: 30 * time.Second}} }

func (a *api) list(filter url.Values) ([]obs, error) {
	q := url.Values{"limit": {"1000"}, "fields": {"core,basic,time,io,metadata,model,usage,trace_context"}}
	for k, v := range filter {
		q[k] = v
	}
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ { // 前置的 Cloudflare 偶发 502、Langfuse 偶发查询超时（422），读取时重试
		req, _ := http.NewRequest(http.MethodGet, a.c.host+"/api/public/v2/observations?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.c.pk+":"+a.c.sk)))
		req.Header.Set("User-Agent", userAgent)
		resp, err = a.client.Do(req)
		retry := err != nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusUnprocessableEntity
		if !retry || attempt == 4 {
			break
		}
		if err == nil {
			resp.Body.Close()
		}
		time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET observations: HTTP %d: %.300s", resp.StatusCode, body)
	}
	var out struct {
		Data []obs `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (a *api) byTraces(ids []string) ([]obs, int, error) { return a.collect("traceId", ids) }

// bySessions 按会话找到 trace，再按 trace 取全部 observation。
func (a *api) bySessions(ids []string) ([]obs, int, error) {
	traces := map[string]bool{}
	for _, id := range ids {
		rows, err := a.list(url.Values{"sessionId": {id}})
		if err != nil {
			return nil, 0, err
		}
		for _, o := range rows {
			traces[str(o, "traceId")] = true
		}
	}
	return a.collect("traceId", keys(traces))
}

// collect 拉取并按 id 去重（保留 updatedAt 最新的一行），返回去重结果与原始行数。
func (a *api) collect(param string, values []string) ([]obs, int, error) {
	latest := map[string]obs{}
	raw := 0
	for _, v := range values {
		rows, err := a.list(url.Values{param: {v}})
		if err != nil {
			return nil, 0, err
		}
		raw += len(rows)
		for _, o := range rows {
			id := str(o, "id")
			if prev, ok := latest[id]; !ok || str(o, "updatedAt") > str(prev, "updatedAt") {
				latest[id] = o
			}
		}
	}
	out := make([]obs, 0, len(latest))
	for _, o := range latest {
		out = append(out, o)
	}
	return out, raw, nil
}

func (a *api) waitFor(query func() ([]obs, int, error), want int, timeout time.Duration) ([]obs, int, error) {
	deadline := time.Now().Add(timeout)
	for {
		all, raw, err := query()
		if err != nil || len(all) >= want || time.Now().After(deadline) {
			return all, raw, err
		}
		time.Sleep(3 * time.Second)
	}
}

func (a *api) waitStable(query func() ([]obs, int, error), timeout time.Duration) ([]obs, int, error) {
	deadline := time.Now().Add(timeout)
	prev := -1
	for {
		all, raw, err := query()
		if err != nil {
			return nil, 0, err
		}
		if (len(all) > 0 && len(all) == prev) || time.Now().After(deadline) {
			return all, raw, nil
		}
		prev = len(all)
		time.Sleep(10 * time.Second)
	}
}

func index(all []obs) map[string]obs {
	m := make(map[string]obs, len(all))
	for _, o := range all {
		m[str(o, "id")] = o
	}
	return m
}

func printTree(all []obs) {
	byID := index(all)
	sorted := append([]obs(nil), all...)
	sort.Slice(sorted, func(i, j int) bool {
		ti, tj := str(sorted[i], "traceId"), str(sorted[j], "traceId")
		if ti != tj {
			return ti < tj
		}
		return str(sorted[i], "startTime") < str(sorted[j], "startTime")
	})
	fmt.Println("\n读回的调用树：")
	for _, o := range sorted {
		depth := 0
		for p := str(o, "parentObservationId"); p != "" && depth < 6; p = str(byID[p], "parentObservationId") {
			depth++
		}
		var extra []string
		if lvl := str(o, "level"); lvl != "" && lvl != "DEFAULT" {
			extra = append(extra, "level="+lvl)
		}
		if tags := str(o, "tags"); depth == 0 && tags != "" {
			extra = append(extra, "tags="+tags)
		}
		if u := str(o, "usageDetails"); u != "" && u != "{}" {
			var m map[string]float64
			_ = json.Unmarshal([]byte(u), &m)
			extra = append(extra, fmt.Sprintf("tokens=%.0f", m["total"]))
		}
		fmt.Printf("%s- [%s] %s %s\n", strings.Repeat("  ", depth), str(o, "type"), str(o, "name"), strings.Join(extra, " "))
	}
	fmt.Println()
}

func str(m obs, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok {
				return s
			}
			b, _ := json.Marshal(v)
			return string(b)
		}
	}
	return ""
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type checker struct{ failures int }

func (c *checker) check(ok bool, format string, args ...any) {
	mark := "✓"
	if !ok {
		mark = "✗"
		c.failures++
	}
	fmt.Printf("%s %s\n", mark, fmt.Sprintf(format, args...))
}

func (c *checker) result() int {
	if c.failures > 0 {
		fmt.Printf("\n%d 项检查失败\n", c.failures)
		return 1
	}
	fmt.Println("\n全部检查通过")
	return 0
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lfsmoke:", err)
	os.Exit(1)
}
