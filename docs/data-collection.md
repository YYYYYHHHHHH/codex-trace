# codex-trace 数据采集说明

> 供使用方合规评审。字段与代码 `internal/trace`（构造 span）、`internal/hook`（本地落盘）一致；如有出入以代码为准。

## 1. 数据流

```text
Codex hook（本机）──▶ 本地 spool（~/.local/state/codex-trace，仅本人可读）
                       └─▶ 后台上传进程：关联 Codex 本地会话记录（~/.codex/sessions）补全 token 等
                             └─▶ HTTPS 上报到团队 Langfuse（OTLP）
```

- 上报目的地只有一个：插件内置的团队 Langfuse 地址。
- 本地文件权限为 0600 / 目录 0700。
- 默认（`capture_content=false`）时，prompt、工具输入输出、模型回复**在写入本地 spool 之前就被剥离**，只保留字节数。

## 2. 默认上报的字段（`capture_content=false`）

### 每轮对话（trace 根节点）

| 字段 | 说明 |
|---|---|
| `session.id` | Codex 会话 id（随机 UUID） |
| `user.id` | Codex 账号用户 id 的**单向哈希**（无法反推）；缺失时为本机用户名的哈希 |
| `langfuse.trace.name` / `tags` | 固定为 `codex turn`；标签如 `interrupted`、`subagent`、`partial`、`incomplete` |
| 元数据 | 轮次 id、模型名、推理强度、Codex 版本、**工作目录名（仅最后一级，不含路径）**、权限模式、结束原因、耗时、首 token 延迟、工具 / 模型调用次数、prompt 与回复的**字节数** |

### 每次工具调用

| 字段 | 说明 |
|---|---|
| 名称 | 工具名，如 `Bash`、`apply_patch`、`collaborationspawn_agent` |
| 时间 | 开始 / 结束时间 |
| 结果 | 退出码、状态；失败时标为 ERROR |
| 元数据 | 调用 id、输入 / 输出的**字节数**；spawn_agent 的任务路径（如 `/root/count_files`） |

### 子 agent

| 字段 | 说明 |
|---|---|
| 名称 | `subagent:<类型>` |
| 元数据 | agent id、类型、任务路径、Codex 分配的昵称 |
| 关系 | 挂在发起它的 spawn_agent 工具调用之下 |

### 每次模型调用

| 字段 | 说明 |
|---|---|
| 模型 | 模型名、推理强度 |
| 用量 | 输入（非缓存）、缓存命中、缓存写入、输出、推理 token 与总数 |
| 元数据 | 响应 id |

### 来源信息（每批数据）

`service.name=codex`、插件版本、**机器标识的单向哈希**（来自 `~/.codex/installation_id`）。

## 3. 开启内容采集后额外上报的字段（`capture_content=true`）

需要由维护人员针对特定机器开启（用户配置或托管配置）。额外上报：

- 每轮的 prompt 与最终回复
- 每次工具调用的输入与输出

上报前会：

1. 按规则遮蔽常见密钥形态（API key、令牌、私钥、Authorization / Cookie 头、URL 中的账号密码、`password=` 等字段），见 `internal/redact`；
2. 每个字段截断到 10KB。

**模型的系统提示词、推理过程（reasoning）在任何模式下都不会上传。**

## 4. 不采集的内容

- 环境变量：hook 进程虽然继承了 Codex 的完整环境，codex-trace 只读取自身的 `CODEX_TRACE_*` 配置与代理、证书相关变量，不记录、不上传任何环境变量；后台上传进程启动时只继承这些白名单变量
- 文件内容（除非工具输出中包含且开启了内容采集）
- 完整的工作目录路径、用户名明文、账号 id 明文

## 5. 保留与删除

- **本地**：已上传的数据在本地只保留去重记录与扫描进度（24 小时后清理）；最近一批上传内容保存在 `logs/last-batch.json` 供 `trace-hook preview` 查看。卸载后可直接删除 `~/.local/state/codex-trace`。
- **服务端**：保留周期与访问权限由团队 Langfuse 的配置决定，请与维护人员约定。

## 6. 用户可控项

| 操作 | 方式 |
|---|---|
| 查看将要 / 最近上传的内容 | `trace-hook preview`（`--raw` 看原始数据） |
| 暂停 / 恢复 | `trace-hook pause` / `trace-hook resume` |
| 彻底停止 | 卸载插件 |
