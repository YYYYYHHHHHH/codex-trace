# 企业托管下发（草案）

> 状态：**草案，未在真实 MDM 环境验证**。依据 Codex `0.167.0-alpha.3.2` 源码整理（`codex-rs/hooks/src/engine/discovery.rs`、`codex-rs/config/src/hook_config.rs`、`codex-rs/protocol/src/hooks.rs`）。上线前请在一台测试机上验证。

## 为什么要托管下发

插件安装的 hook 需要用户在 Codex 中审批，插件升级后还要重新审批；用户忘记审批时采集会**静默停止**。

Codex 对**托管来源（managed）的 hook 自动视为受信**，不需要用户审批，也不会因升级失效；还可以配合 `allow_managed_hooks_only` 只允许托管 hook。适合由 IT 统一管理的企业客户。

## 方案

托管下发不走插件，直接分发 `trace-hook` 二进制，并通过 Codex 的 requirements 配置声明 hook。

### 1. 分发二进制

把对应平台的二进制（`make dist` 的产物，内置凭证）放到固定路径，例如：

```text
/opt/codex-trace/bin/trace-hook        # root 所有，0755
```

### 2. 声明托管 hook

Codex 从以下来源读取 requirements（任选其一）：

| 来源 | 位置 |
|---|---|
| 系统文件 | Linux / macOS：`/etc/codex/requirements.toml`；Windows：`C:\ProgramData\OpenAI\Codex\requirements.toml` |
| macOS MDM 托管偏好 | 由 MDM 下发（Codex 的 managed preferences 域） |
| 企业云端托管配置 | Codex 企业管理后台 |

`requirements.toml` 示例（事件名与插件版 `hooks.json` 一致）：

```toml
[managed_hooks]
managed_dir = "/opt/codex-trace"

[[managed_hooks.SessionStart]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook SessionStart", timeout = 3 }]

[[managed_hooks.UserPromptSubmit]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook UserPromptSubmit", timeout = 3 }]

[[managed_hooks.PreToolUse]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook PreToolUse", timeout = 3 }]

[[managed_hooks.PostToolUse]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook PostToolUse", timeout = 3 }]

[[managed_hooks.PermissionRequest]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook PermissionRequest", timeout = 3 }]

[[managed_hooks.SubagentStart]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook SubagentStart", timeout = 3 }]

[[managed_hooks.SubagentStop]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook SubagentStop", timeout = 3 }]

[[managed_hooks.Stop]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook Stop", timeout = 3 }]

[[managed_hooks.Interrupt]]
hooks = [{ type = "command", command = "/opt/codex-trace/bin/trace-hook hook Interrupt", timeout = 3 }]
```

注意：

- 托管 hook 是 **Required**：命令加载失败时 Codex 会报错，请确保二进制路径在所有机器上都存在。
- 不要同时安装插件版，否则每个事件会采集两次（数据按确定性 id 去重，但浪费资源）。
- `Interrupt` 事件的 timeout 上限为 3 秒。

### 3. 托管 codex-trace 自身的配置（可选）

如需按机器覆盖配置（例如对某些部门开启内容采集），下发：

- macOS：`/Library/Application Support/codex-trace/config.toml`
- Linux：`/etc/codex-trace/config.toml`

文件须 root 所有、他人不可写，否则会被忽略。托管配置的优先级最高，用户无法通过环境变量或个人配置覆盖。

## 验证步骤

1. 在测试机部署二进制与 `requirements.toml`。
2. 启动 Codex，确认没有出现 hook 审批提示。
3. 跑一轮对话，执行 `/opt/codex-trace/bin/trace-hook doctor`：`hook 触发` 应为 ✓（`Codex 插件` 一项提示未安装插件，托管模式下可忽略）。
4. 在 Langfuse 中确认调用树。
