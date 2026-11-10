# Codex hook payload 与 rollout 实测记录

> 采集日期：2026-11-09
> Codex 版本：`codex-cli 0.167.0-alpha.3.2`（`~/.codex/plugins/.plugin-appserver/codex-cli/bin/codex`）
> 采集方式：`scripts/capture-install.py` 临时挂载 `scripts/capture-hook.sh`，`codex exec` 跑测试会话后卸载
> 脱敏样本：`testdata/hook-payloads/`

## 1. 关键结论

1. **hook 有信任机制**：未信任的 hook 被**静默跳过**（不报错）。详见第 6 节，直接影响分发方案。
2. hook payload 直接带 `session_id`、`turn_id`、`tool_use_id`，**不需要本地维护轮次计数**。
3. `session_id` 在 resume 后不变；子 agent 与父会话共享 `session_id`，但有自己的 `turn_id` 和 rollout 文件。
4. `PostToolUse.tool_response` 是纯文本，**没有退出码**；失败与否、耗时、token 都要从 rollout 补。
5. 被打断的轮次**不触发 `Stop`**，只触发 `Interrupt`；rollout 中记为 `turn_aborted`。
6. hook 进程继承 Codex 的**完整环境变量**（含其他工具的密钥），trace-hook 绝不能转储 env。
7. Codex 没有给 hook 注入 `CODEX_*` 环境变量，所需信息全部来自 stdin payload。

## 2. 公共字段（所有事件）

| 字段 | 说明 |
|---|---|
| `session_id` | 会话 id（= 主 thread id）；resume 后不变；子 agent 事件中仍为父会话 id |
| `turn_id` | 当前轮次 id；子 agent 事件中为**子 agent 自己的** turn id。`SessionStart` 无此字段 |
| `transcript_path` | 当前 thread 的 rollout 文件；子 agent 事件中指向子 agent 的 rollout |
| `cwd`、`model`、`permission_mode`、`hook_event_name` | — |

## 3. 各事件特有字段

| 事件 | 特有字段 |
|---|---|
| `SessionStart` | `source`：`startup` / `resume`（未测 `clear` / `compact`） |
| `UserPromptSubmit` | `prompt`（原文，敏感） |
| `PreToolUse` | `tool_name`、`tool_input`（对象）、`tool_use_id`；子 agent 内额外带 `agent_id`、`agent_type` |
| `PostToolUse` | 同上 + `tool_response`（字符串） |
| `SubagentStart` | `agent_id`（= 子 agent thread id）、`agent_type` |
| `SubagentStop` | `agent_id`、`agent_type`、`agent_transcript_path`、`last_assistant_message`、`stop_hook_active`；注意此处 `transcript_path` 是**父会话**的 |
| `Stop` | `last_assistant_message`、`stop_hook_active` |
| `Interrupt` | 仅公共字段 |
| `PermissionRequest` | **未采到**（`codex exec` 无审批交互），待在桌面端补采 |

`tool_name` 实测值：`Bash`、`apply_patch`、`collaborationspawn_agent`、`collaborationwait_agent`（命名空间与工具名直接拼接）。
`tool_use_id` 格式：shell / apply_patch 为 `exec-<uuid>`，其余为 `call_<hex>`。

## 4. rollout 中可补充的信息

rollout 每条记录都有 `ordinal`，可作为增量读取游标。

| 需要的信息 | rollout 记录 | 关联键 |
|---|---|---|
| 工具退出码 / 失败 | `event_msg/item_completed`，`item.type=CommandExecution` 的 `status`（`completed` / `failed`）、`exit_code`、`duration` | `item.id` = `tool_use_id` |
| 文件改动 | `item.type=FileChange` 的 `changes`、`status` | `item.id` = `tool_use_id` |
| 子 agent ↔ spawn 调用 | 父 rollout 中 `item.type=SubAgentActivity`（`kind=started`）；或 spawn 的 `tool_response.task_name` = 子 rollout `session_meta.source.subagent.thread_spawn.agent_path` | `item.id` = spawn 的 `tool_use_id`，`agent_thread_id` = `agent_id`。实现采用 task_name ↔ agent_path（只依赖 hook 与子 rollout） |
| 模型调用（generation） | `token_usage_record`：`response_id`（唯一）、`usage.{input,cached_input,cache_write_input,output,reasoning_output}_tokens` | `turn_id` |
| 模型名 / 推理强度 | `turn_context.model`、`effort` | `turn_id` |
| 轮次耗时 | `event_msg/task_complete`：`duration_ms`、`time_to_first_token_ms` | `turn_id` |
| 子 agent 的父轮次 | 子 rollout 中 `task_started.turn_attribution`：`parent_turn_id`、`root_turn_id` | 子 `turn_id` |
| 打断 | `event_msg/turn_aborted` | `turn_id` |

**坑：子 agent 的 rollout 开头复制了父会话的历史**（含父会话的 `session_meta`、`task_started`，**甚至父轮次的 `turn_aborted`**），读取时只能保留 `turn_id` 属于子 agent 的记录，否则 token 会重复计算、父轮次会被误判为打断。

敏感字段：`session_meta.creator_user_id`、`creator_account_id` 上传前需哈希；`base_instructions`、`message`、`reasoning` 默认不上传。

## 5. ID 方案（取代 HANDOFF 第 5 节的草案）

| Langfuse 对象 | id | 说明 |
|---|---|---|
| Session | `session_id` | 子 agent 自然归入同一 session |
| Trace | `root_turn_id` | 主会话事件即 `turn_id`；子 agent 事件通过子 rollout 的 `turn_attribution.root_turn_id` 解析 |
| 工具 span | `tool_use_id` | Pre 创建、Post 更新；退出码从 rollout 补 |
| 子 agent span | 子 agent 的 `turn_id` | 父 span = spawn 调用的工具 span（经 task_name ↔ agent_path 关联） |
| 子 agent 内工具 span | `tool_use_id` | `parentObservationId` = 子 agent span |
| Generation | `response_id` | 挂在所属 turn 对应的 trace / 子 agent span 下 |
| Ingestion event id | `hash(observation_id + 事件类型)` | 重试幂等 |

轮次结束：`Stop` → `trace-update`；`Interrupt` → `trace-update` 并打 `interrupted` 标签。

### 由此带来的架构调整

关联子 agent、补退出码、补 token 都需要读 rollout 和跨事件状态。因此：

- **trace-hook 保持"哑"**：只把 payload + 接收时间追加到 spool，不解析、不关联。
- **上传进程负责关联**：按 `session_id` 聚合，读 rollout 补全后再转成 Langfuse 事件。

## 6. hook 信任机制（影响分发）

来源：`openai/codex` 仓库 `codex-rs/hooks/src/engine/discovery.rs`、`plugin_trust.rs`。

- 每个 hook 按 `<来源文件>:<事件>:<组序号>:<hook序号>` 记录在 `~/.codex/config.toml` 的 `[hooks.state."…"]` 中，含 `trusted_hash`（以及插件的 `trusted_content_hash`）。
- 哈希对象是规范化后的 hook 声明（事件名 + matcher + handler，转 TOML 后计算）；hook 内容一变，状态就变为 `Modified`，需要重新信任。
- 状态为 `Untrusted` / `Modified` 的 hook **不执行**，`codex exec` 下没有任何提示。
- **插件 hook 同样需要信任**，且哈希包含插件内容指纹 —— 插件每次升级都可能需要用户重新确认。
- **managed 来源（企业托管配置 / requirements）的 hook 自动受信**，还可以通过 `allow_managed_hooks_only` 只允许托管 hook。
- `codex exec --dangerously-bypass-hook-trust` 可在单次调用中跳过信任检查（本次采集即用此参数，未写入任何信任状态）。

对分发的影响：

1. 插件路线：用户安装后要在 TUI / 桌面端审批一次 hook，升级后可能还要再审批。接入文档里必须写清楚，`doctor` 要能检测"已安装但未信任"。
2. 企业客户优先走 **managed 配置下发**（MDM），hook 免审批，体验最好，也更难被误关。
3. 不要尝试自己计算 `trusted_hash` 写进用户的 config.toml 来绕过审批 —— 这是在规避用户授权，与合规前提冲突。
