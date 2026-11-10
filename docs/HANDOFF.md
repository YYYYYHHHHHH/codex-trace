# 工作移交：Codex 调用链远程采集（hook → Langfuse）

> 移交日期：2026-11-09
> 状态：方案已确定；真实 hook payload 已采集（见 [payload-notes.md](payload-notes.md)），下一步搭插件骨架。

## 1. 目标

在客户机器上采集 Codex 的调用链（对话轮次、工具调用、子 agent、模型调用与 token），远程汇总到 Langfuse 查看，**不依赖 magpie**。

## 2. 已确定的决策

| 决策 | 结论 | 备注 |
|---|---|---|
| 采集方式 | Codex hooks 为主，`Stop` 时增量读取 rollout JSONL 补齐模型调用明细 | hook payload 通常不含 token / 模型输入输出 |
| 分发方式 | 做成 **Codex 插件**，通过 git marketplace 分发 | 参考 Composio 插件（插件自带 `hooks/hooks.json`，用 `${PLUGIN_ROOT}` 引用脚本） |
| 上报目的地 | **直连 Langfuse**，暂不区分租户、不加自建网关 | 用户明确决定（2026-11-09） |
| 上报接口 | Langfuse batch ingestion：`POST /api/public/ingestion` | 将来可迁移到 OTLP `/api/public/otel`，只改上传进程 |
| 实现语言 | Go，编译单文件二进制（macOS arm64/x64、Linux、Windows） | 不依赖客户机器上的 Python / Node / jq |

### 直连 Langfuse 的已知代价

- 客户机器上要放 `public key` + `secret key`，`secret key` 可读取整个 project 的数据。
- 应对：单独建一个只放 Codex 调用链的 Langfuse project；准备好密钥轮换流程。

## 3. 架构

```text
Codex hook 事件
  └─▶ trace-hook（追加写本地 spool，几毫秒内 exit 0）
        └─▶ 后台上传进程（批量、重试）
              └─▶ Langfuse /api/public/ingestion
```

## 4. 插件结构（计划）

```text
codex-trace/
├── .codex-plugin/plugin.json
├── hooks/
│   ├── hooks.json
│   └── trace-hook            # Go 单文件二进制
└── skills/                   # 可选：查看采集状态等
```

`hooks/hooks.json` 计划挂载的事件（均已在本机 `~/.codex/hooks.json` 中确认当前版本支持）：

`SessionStart`、`UserPromptSubmit`、`PreToolUse`、`PostToolUse`、`SubagentStart`、`SubagentStop`、`Stop`

```json
{
  "hooks": {
    "PreToolUse": [{ "hooks": [{ "type": "command", "command": "\"${PLUGIN_ROOT}/hooks/trace-hook\" pre-tool", "timeout": 3 }] }]
  }
}
```

（其余事件同理，参数分别为 `session-start` / `prompt` / `post-tool` / `subagent-start` / `subagent-stop` / `stop`。）

## 5. 事件 → Langfuse 映射

| Hook 事件 | Langfuse 操作 |
|---|---|
| `SessionStart` | 记录 session id；Session 由 trace 的 `sessionId` 自动聚合 |
| `UserPromptSubmit` | `trace-create`，`input` = 脱敏后的 prompt |
| `PreToolUse` | `span-create`（`startTime`、`input`） |
| `PostToolUse` | 对同一 span id 发 `span-update`（`endTime`、`output`，出错设 `level: "ERROR"`） |
| `SubagentStart` / `SubagentStop` | `span-create` / `span-update`；子 agent 内的工具 span 用 `parentObservationId` 指向它 |
| `Stop` | `trace-update` 补 `output`；读取 rollout 增量，补 `generation-create`（模型、token、耗时） |

### 确定性 ID

> **已被实测结论取代**：最终 ID 方案、rollout 关联方式见 [payload-notes.md 第 5 节](payload-notes.md#5-id-方案取代-handoff-第-5-节的草案)。以下为原草案，仅供对照。

- trace id：`hash(session_id + turn_id)`；payload 无 turn id 时在本地维护轮次计数
- span id：工具调用自带的 call id（如 `tool_use_id` / `call_id`）
- event id：`hash(span_id + 事件类型)` —— Langfuse 按 event id 去重，重试安全

> 实测：payload 自带 `turn_id`、`tool_use_id`，无需本地轮次计数；退出码、token 需从 rollout 补。

### 请求示例

```http
POST {LANGFUSE_HOST}/api/public/ingestion
Authorization: Basic base64(public_key:secret_key)
Content-Type: application/json
```

```json
{
  "batch": [
    {
      "id": "evt-<唯一ID>",
      "timestamp": "2026-11-09T10:00:00.000Z",
      "type": "span-create",
      "body": {
        "id": "span-<tool_call_id>",
        "traceId": "trace-<session_id>-<turn>",
        "name": "tool:shell",
        "startTime": "...",
        "input": { "command": "..." }
      }
    }
  ]
}
```

返回 207 时逐条给出成功 / 失败，只重试失败条目。

## 6. hook 必须遵守的约束

- hook 内**不发网络请求**，只追加写 spool，立即退出。
- **永远 `exit 0`**，错误只写本地日志；不 deny、不改工具输入。
- **stdout 不输出任何内容**（`SessionStart` / `UserPromptSubmit` 的 stdout 可能被注入模型上下文）。
- timeout 2–3 秒。
- **未配置凭证时静默不采集**。

## 7. 客户端配置与运维命令

```toml
# ~/.config/codex-trace/config.toml（权限 600），也可由 MDM 下发
langfuse_host   = "https://langfuse.example.com"
public_key      = "pk-lf-..."
secret_key      = "sk-lf-..."
capture_content = false   # 默认不上传 prompt / 工具输出原文
```

上传进程：每批 ≤ 100 条或每 2 秒发一次；指数退避重试；spool 上限约 50MB（超出丢最旧）；单字段超过约 10KB 截断。

计划提供的命令：`login`、`status`、`pause` / `resume`、`preview`（显示脱敏后将上传的内容）、`doctor`（检查凭证、网络、代理、spool 积压、hook 是否触发）。

每条事件携带：插件版本、Codex 版本、机器 ID（可用 `~/.codex/installation_id`）、用户标识（哈希）。

## 8. 下一步

1. ✅ **抓真实 payload**（2026-11-09 完成，结论见 [payload-notes.md](payload-notes.md)，样本见 `testdata/hook-payloads/`；`PermissionRequest` 待桌面端补采）：在本机 `~/.codex/hooks.json` 临时**追加**一个只把 stdin 落盘的 hook（不要改动现有的 codeisland、orca hooks），跑一轮 Codex 后删除。确认 session id、turn id、call id、rollout 文件路径的字段名。
2. ✅ ID 方案已定（payload-notes 第 5 节）。spool 记录 = 原始 payload + 接收时间；关联逻辑放在上传进程。
3. ✅ 插件与 Go 版 `trace-hook` 已完成（2026-11-09）。上报接口改为 OTLP（Langfuse Cloud 2026-11-16 起旧 ingestion 接口停止接收 trace），凭证改为构建时内置，详见 [PLAN.md](PLAN.md) 第 2 节。
4. ✅ 本机 E2E 已通过（本地假 OTLP 接收器 + 真实 `codex exec` 会话，`make e2e`）；⏳ 真实 Langfuse 冒烟与 E2E 等 Langfuse 信息（PLAN M1）。
5. ⏳ 小范围客户试点；发布到私有 git marketplace（`release` 分支，见 [operations.md](operations.md)）。接入文档已完成：[install.md](install.md)。

## 8.1 新发现：hook 信任机制（影响分发）

Codex 对未信任的 hook **静默跳过**，插件 hook 也要用户审批，managed 配置下发的 hook 自动受信。详见 [payload-notes.md 第 6 节](payload-notes.md#6-hook-信任机制影响分发)。第 5 步的接入文档和 `doctor` 命令必须覆盖"已安装但未信任"的情况；企业客户优先走 MDM 下发 managed 配置。

## 9. 参考：本机现状（2026-11-09 检查）

- Codex 数据目录：`~/.codex/`；会话记录：`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`
- PATH 中没有 `codex` 命令（使用的是桌面端 / IDE 入口）；可用的 CLI 在 `~/.codex/plugins/.plugin-appserver/codex-cli/bin/codex`（0.167.0-alpha.3.2）
- `~/.codex/config.toml` 未配置 `[otel]`
- `~/.codex/hooks.json` 已有 codeisland-bridge 与 orca 两套 hooks —— 任何安装脚本都必须**合并**而非覆盖
- 插件自带 hooks 的参考实现：`~/.codex/plugins/cache/composio/composio/0.2.3/hooks/hooks.json`
- marketplace 配置示例：

```toml
[marketplaces.composio]
source_type = "git"
source = "https://github.com/ComposioHQ/composio-plugin-openai.git"

[plugins."composio@composio"]
enabled = true
```

## 10. 备选方案（已评估，未采用为主线）

- **Codex 内置 `[otel]` 导出**：配置简单，但以 OTel logs/events 为主，不是嵌套 span 树，需要 Collector 转换。可作为运维指标的补充。
- **只上传 rollout JSONL**：内容最全但最敏感，格式随版本变化。现方案中在 `Stop` 时增量读取作为补充。
- **网络层代理（LiteLLM / Helicone）**：只能看到模型请求，ChatGPT 账号登录时通常不可用。
- **自建接收网关**：多租户隔离、密钥安全更好；目前不需要区分租户，暂不做，将来需要时只改上传目的地。

## 11. 合规前提

采集前需取得客户明确授权，约定采集范围、保留周期、访问权限，并告知实际使用 Codex 的人员。脱敏在客户端完成。
