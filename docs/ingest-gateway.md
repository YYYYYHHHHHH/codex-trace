# 上报网关（维护人员）

> 上线日期：2026-11-11。代码在 `gateway/`（Cloudflare Worker），地址 `https://ingest.iluvai.org`。

## 1. 为什么需要网关

插件随公开仓库分发，二进制里内置的凭证任何人都能提取。Langfuse 的 project key 可读可写，没有只写权限；在 v0.2.0 默认上传对话内容之后，内置真实 key 等于把所有人的 prompt 与工具输出公开。

网关把两者分开：

| 凭证 | 存放位置 | 泄露后能做什么 |
|---|---|---|
| 网关 token（`ctgw_…`） | 内置在插件二进制中 | 只能向 Langfuse 写入 trace |
| Langfuse public / secret key | 只在 Worker secret 与维护人员本机 `langfuse.env` 中 | 读写删全部数据 |

客户端代码不需要改：网关地址作为 `langfuse_host`，`public_key` 填任意标识（`codex-trace`），`secret_key` 填网关 token，仍走 Basic Auth 与 `/api/public/otel/v1/traces`。

## 2. 网关行为

- 只接受 `POST /api/public/otel/v1/traces`；其他路径 404，其他方法 405。
- Basic Auth 的密码必须命中 `INGEST_TOKENS`（逗号分隔，可多个并存）之一，否则 401；比较前先做 SHA-256，避免时序泄露。
- 必须带 `Content-Length` 且不超过 5MB（411 / 413）。
- 通过鉴权的请求按客户端 IP 限速：每分钟 300 次（Cloudflare 按节点近似计数），超出返回 429 + `Retry-After: 60`；客户端会按 `Retry-After` 退避重试。
- 只转发 `Content-Type`、`Content-Encoding`、`User-Agent`，换上真实 Langfuse 凭证；上游响应的状态码与响应体原样返回（只保留 `Content-Type`、`Retry-After` 头），上游不可达时返回 502。
- secret 缺失时一律返回 500，不会放行。

客户端对 401 / 429 / 5xx 都会保留数据并重试，所以网关临时故障或 token 轮换期间不会丢数据。

## 3. 本机文件

都在 `~/.config/codex-trace/`，权限 600，不提交到仓库：

| 文件 | 内容 | 用途 |
|---|---|---|
| `build.env` | 网关地址、`codex-trace`、网关 token、`production`、`CODEX_TRACE_CAPTURE_CONTENT=true` | `make dist` 构建发布版 |
| `langfuse.env` | 真实 Langfuse 地址与 key（键名同 build.env） | 设置 Worker secret；`lfsmoke` / `e2e-langfuse` 读回校验 |

## 4. 部署

需要 `npx wrangler login`（OAuth，账号需有该 zone 的 Workers 权限）。

```sh
cd gateway
node --test                 # 单元测试
npx -y wrangler@4 deploy    # 部署代码与 ingest.iluvai.org 自定义域名
```

设置 secret（从本机文件经 stdin 传入，不出现在命令行与日志里）：

```sh
python3 -I - <<'PY' | npx -y wrangler@4 secret bulk
import json, os
def load(p):
    out = {}
    for line in open(os.path.expanduser(p)):
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1); out[k] = v
    return out
lf = load("~/.config/codex-trace/langfuse.env")
gw = load("~/.config/codex-trace/build.env")
print(json.dumps({
    "LANGFUSE_PUBLIC_KEY": lf["CODEX_TRACE_LANGFUSE_PUBLIC_KEY"],
    "LANGFUSE_SECRET_KEY": lf["CODEX_TRACE_LANGFUSE_SECRET_KEY"],
    "INGEST_TOKENS": gw["CODEX_TRACE_LANGFUSE_SECRET_KEY"],
}))
PY
```

## 5. 验证

```sh
# 客户端真实 exporter 经网关发送 fixture，再用真实 key 读回比对（environment 用测试值）
sed 's/^CODEX_TRACE_ENVIRONMENT=.*/CODEX_TRACE_ENVIRONMENT=codex-trace-dev/' ~/.config/codex-trace/build.env > /tmp/gw-dev.env
go run ./tools/lfsmoke fixture -env /tmp/gw-dev.env -read-env ~/.config/codex-trace/langfuse.env -resend
rm /tmp/gw-dev.env
```

另外应确认：经网关访问 `/api/public/v2/observations` 返回 404，用真实 Langfuse key 或错误 token 写入返回 401。

日志：Cloudflare 控制台 → Workers → `codex-trace-ingest` → Logs（已开启 observability），或 `npx wrangler tail codex-trace-ingest`。

## 6. 轮换

**网关 token**（被滥用写入垃圾数据时）：

1. 生成新 token 写入 `build.env`，把 `INGEST_TOKENS` 设为 `旧,新` 并重新设置 secret；
2. 发布新版本，等客户普遍升级；
3. 把 `INGEST_TOKENS` 改为只有新 token。紧急情况下可直接只保留新 token：旧版本客户端会收到 401，数据留在本地 outbox，升级后补传。

**Langfuse key**（怀疑泄露时）：在 Langfuse 新建 key → 写入 `langfuse.env` → 重新设置 Worker secret → 用第 5 节验证 → 在 Langfuse 删除旧 key。客户端不受影响，不需要发版。
