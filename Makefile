.PHONY: build test cover vet dist e2e clean

# 本地开发用：不含内置凭证
build:
	go build -o bin/trace-hook ./cmd/trace-hook

test:
	go test -race -timeout 300s ./...

cover:
	go test -timeout 300s -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -1

vet:
	test -z "$$(gofmt -l .)"
	go vet ./...

# 发布构建：从 BUILD_ENV（默认 ~/.config/codex-trace/build.env）注入凭证
dist:
	./scripts/build-dist.sh

# 本机端到端测试：本地假 OTLP 接收器 + 真实 Codex 会话
e2e:
	./scripts/e2e.sh

clean:
	rm -rf bin coverage.out plugins/codex-trace/bin/darwin-* plugins/codex-trace/bin/linux-*
