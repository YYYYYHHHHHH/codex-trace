// fakeotlp 是端到端测试用的本地 OTLP 接收器：
//
//	fakeotlp serve  -addr 127.0.0.1:PORT -out received.jsonl -user PK -pass SK
//	fakeotlp verify -in received.jsonl [-expect-subagent] [-expect-failed-tool] [-expect-interrupted] [-no-content]
//
// serve 模拟 Langfuse 的 /api/public/otel/v1/traces（校验 Basic 鉴权、gzip、JSON），
// 每个请求体追加一行写入 -out。verify 读回所有 span 并断言调用树结构。
package main

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: fakeotlp serve|verify [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "verify":
		os.Exit(verify(os.Args[2:], os.Stdout))
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4318", "监听地址")
	out := fs.String("out", "received.jsonl", "落盘文件")
	user := fs.String("user", "", "Basic 用户名（public key）")
	pass := fs.String("pass", "", "Basic 密码（secret key）")
	_ = fs.Parse(args)

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var mu sync.Mutex

	http.HandleFunc("/api/public/otel/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(*user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(*pass)) != 1 {
			http.Error(w, `{"message":"invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, `{"message":"expect POST application/json"}`, http.StatusBadRequest)
			return
		}
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, `{"message":"bad gzip"}`, http.StatusBadRequest)
				return
			}
			body = zr
		}
		data, err := io.ReadAll(body)
		if err != nil || !json.Valid(data) {
			http.Error(w, `{"message":"bad json"}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		_, _ = f.Write(append(compact(data), '\n'))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	log.Printf("fakeotlp listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func compact(data []byte) []byte {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return data
	}
	out, _ := json.Marshal(v)
	return out
}
