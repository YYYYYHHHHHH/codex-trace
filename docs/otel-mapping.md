# OTel → Langfuse 映射实测（M1）

> 实测日期：2026-11-10
> 目标：自建 Langfuse **v4.77.0**（`https://langfuse.iluvai.org`，前置 Cloudflare），project `Default Project`
> 工具：`make smoke`（`tools/lfsmoke fixture -resend`），读回接口 `GET /api/public/v2/observations`

## 1. 结论

| 验证项 | 结果 |
|---|---|
| OTLP/HTTP JSON + gzip 写入 `/api/public/otel/v1/traces` | ✓ 200，`partialSuccess` 无拒收 |
| span id / trace id 原样作为 observation id / trace id | ✓ 20/20 一致，父子关系一致 |
| `langfuse.observation.type` → AGENT / TOOL / GENERATION | ✓ |
| 工具失败 → `level=ERROR`、`statusMessage="exit code 1"` | ✓ |
| `langfuse.observation.model.name` / `model.parameters` | ✓ `model`、`modelParameters={"reasoning_effort":"high"}` |
| `langfuse.observation.usage_details` | ✓ 原样落库；Langfuse 自动按模型价格计算 `costDetails` |
| `langfuse.observation.metadata.<key>` | ✓ 展开为扁平 metadata（`agent_path`、`exit_code`、`input_bytes` …） |
| `langfuse.trace.name` / `tags`、`langfuse.release` | ✓ 体现在 `traceName`、`tags`、`release` |
| `session.id` / `user.id` / `langfuse.environment` | ✓ **需要每个 span 各自携带**（见第 2 节） |
| 同一 span id 重复发送 | ✓ **按 id 去重，保留最新版本**（见第 3 节） |

## 2. 归属属性不继承

Langfuse v4 以 observation 为存储单位，`environment`、`sessionId`、`userId` 不会从父 span 继承：最初只在轮次 span 上设置时，工具与 generation 的 `environment` 为 `default`、`sessionId` 为空，按 environment 或会话筛选会漏掉它们。

修正：`internal/trace` 的 `sharedAttrs` 把 `session.id`、`user.id`、`langfuse.environment`、`langfuse.release` 写到轮次内每个 span 上。

## 3. 重复写入与去重

- 同一 span id 的多次写入会产生多行，随后在后台合并为一行，**保留最新写入的版本**（实测：改动属性后重发，几分钟内 40 行收敛为 20 行，保留的是新属性）。
- 合并前短时间内读取可能看到重复行；读取方应按 `id` 取 `updatedAt` 最新的一行（`lfsmoke` 即如此）。
- 对 codex-trace 的意义：崩溃重放、网络重试、`Discard` 后重新发出都安全，最终只有一份数据，无需在客户端做严格的"恰好一次"。

## 4. 接入注意事项

- **Cloudflare 浏览器完整性检查**：`Python-urllib` 的默认 User-Agent 会被直接 403（`error code: 1010`）。Go 默认 UA 可以通过，但 exporter 现在显式发送 `User-Agent: codex-trace/<版本>`，避免规则变化导致静默失败。自行写脚本调用 API 时也要设置 UA。
- **读接口**：v4 已移除 `/api/public/traces`（404），用 `/api/public/v2/observations`。默认只返回 core / basic 字段组，查看 model、usage、metadata 需 `fields=core,basic,time,io,metadata,model,usage,trace_context`。
- **偶发错误**：读接口偶发 Cloudflare 502 与 Langfuse 查询超时（422 `Request timed out`），只影响读取（`lfsmoke` 已重试）；写入路径的 5xx 由上传进程按退避重试。

## 5. 测试数据清理

验收写入的数据带 `environment`：

| environment | 来源 |
|---|---|
| `codex-trace-dev` | `make smoke`（fixture 回放，用户标识为哈希） |
| `codex-trace-e2e` | `make e2e-langfuse`（真实 Codex 测试会话，未上传内容） |

可在 Langfuse UI 中按 environment 筛选后删除。
