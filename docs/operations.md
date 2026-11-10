# codex-trace 运维手册（维护人员）

## 1. 构建与发布

### 准备凭证

在构建机上创建 `~/.config/codex-trace/build.env`（**必须 `chmod 600`，不要提交到仓库**）：

```sh
CODEX_TRACE_LANGFUSE_HOST=https://<langfuse 地址>
CODEX_TRACE_LANGFUSE_PUBLIC_KEY=pk-lf-...
CODEX_TRACE_LANGFUSE_SECRET_KEY=sk-lf-...
CODEX_TRACE_ENVIRONMENT=production
```

建议为 codex-trace 单独建一个 Langfuse project，只存 Codex 调用链。

### 构建

```sh
make test      # 单元 / 集成测试（-race）
make dist      # 四平台二进制 → plugins/codex-trace/bin/<os>-<arch>/trace-hook，凭证经 -ldflags 注入
```

`make dist` 不会回显密钥；`build.env` 不存在时构建出的二进制不含凭证（安装后静默不采集）。

### 发布

1. 修改 `VERSION` 与 `plugins/codex-trace/.codex-plugin/plugin.json` 的 `version`（保持一致）。
2. `make dist`。
3. 把构建产物提交到 **`release` 分支**（`main` 只放源码，`plugins/codex-trace/bin/*-*/` 在 `main` 上被 `.gitignore` 忽略）：

   ```sh
   git switch release && git merge --no-ff main
   make dist
   git add -f plugins/codex-trace/bin && git commit -m "release: vX.Y.Z"
   git tag vX.Y.Z && git push origin release --tags
   ```

4. 客户执行 `codex plugin marketplace upgrade` 获取新版本，并**重新审批 hook**。

仓库：公开仓库 `YYYYYHHHHHH/codex-trace`，客户以 `codex plugin marketplace add YYYYYHHHHHH/codex-trace@release` 接入，无需授权。

> **已知风险（已接受）**：发布版内置的 secret key 可以从二进制中提取。2026-11-10 起仓库公开，用户明确接受内置 key 可被任何人提取的风险（可读写、删除该 Langfuse project 的数据），并决定不轮换 key。如需收回风险：在 Langfuse 中轮换 key 并重新发布，或改用只写网关（PLAN 方案 B）。

## 2. 凭证轮换

按需手动执行，没有定时吊销：

1. 在 Langfuse 中为该 project 新建一对 key。
2. 更新 `build.env`，按第 1 节重新构建并发布新版本。
3. 通知客户升级插件并重新审批 hook。
4. 确认客户已普遍升级后（可在 Langfuse 中按 `langfuse.release` 查看各版本的上报情况），由维护人员决定何时在 Langfuse 中删除旧 key。删除后仍在用旧版本的机器会收到 401，数据保留在本地 outbox 并持续重试，升级后自动补传。

紧急情况（key 泄漏）：直接在 Langfuse 删除旧 key，同时发布新版本。

## 3. 客户端排障

所有命令都在插件目录的 `bin/trace-hook` 下：

```sh
TRACE_HOOK=$(ls -td "${CODEX_HOME:-$HOME/.codex}"/plugins/cache/*/codex-trace/*/bin/trace-hook | head -1)
```

| 命令 | 用途 |
|---|---|
| `status` | 最近 hook、最近成功上传、累计计数、最近错误、下次重试、积压 |
| `doctor` | 逐项诊断：状态目录、配置来源、插件审批、hook 触发、上传错误、积压、Langfuse 连通性（`--offline` 跳过网络） |
| `preview [--raw]` | 即将上传 / 最近一次上传的内容 |
| `config show` | 生效配置及来源（密钥打码） |
| `pause` / `resume` | 暂停 / 恢复采集 |
| `upload --once` | 手动触发一次上传并等待完成 |

本地文件（默认 `~/.local/state/codex-trace/`）：

| 路径 | 内容 |
|---|---|
| `spool/` | hook 写入的原始事件（未处理） |
| `outbox/` | 已生成、待发送的 OTLP 批次 |
| `failed/` | 被服务端拒收（400 / 413 / 422 等）的批次，上限 4MB |
| `sessions/` | 每个会话的关联状态 |
| `status.json` | 上传进程状态 |
| `logs/hook.log`、`logs/upload.log` | 错误日志（1MB 轮转），不含密钥 |
| `logs/last-batch.json` | 最近一次上传的批次（0600；开启 `capture_content` 时包含脱敏后的内容） |

常见问题：

| 现象 | 原因 / 处理 |
|---|---|
| `hook 触发：从未触发` | 插件未审批或升级后未重新审批；企业客户可改用托管下发（见 [managed-deployment.md](managed-deployment.md)） |
| `上传进程：最近一次失败 HTTP 401` | 凭证已轮换，升级插件 |
| outbox 积压持续增长 | 网络 / 代理问题；`doctor` 看连通性。outbox 上限 16MB，超出丢最旧批次并计入 `dropped_batches` |
| Langfuse 中轮次带 `partial` 标签 | 轮次结束 15 秒内没等到 Codex 写完 rollout 收尾记录，token / 退出码可能不全 |
| 轮次带 `incomplete` 标签 | Codex 崩溃或被强制退出，30 分钟内没有结束事件 |

## 4. 覆盖内置配置

优先级：内置 < 用户配置 `~/.config/codex-trace/config.toml`（0600）< 环境变量 `CODEX_TRACE_*` < 托管配置（macOS `/Library/Application Support/codex-trace/config.toml`，Linux `/etc/codex-trace/config.toml`，须 root 所有且他人不可写）。

```toml
capture_content = true   # 针对某台机器开启内容采集（排查问题用）
environment = "debug"
enabled = false          # 停用
```

安全约束：任何来源覆盖了 `langfuse_host`，内置 key 就不再使用，必须另外提供 `public_key` / `secret_key`（可来自任一来源），防止内置 key 被发往其他地址。

## 5. 数据清理

- 测试数据：按 `langfuse.environment`（如 `codex-trace-dev`、`codex-trace-e2e`）在 Langfuse 中筛选删除。
- 自行写脚本调用 Langfuse API 时需设置 User-Agent：前置 Cloudflare 会拦截部分默认 UA（如 Python urllib，返回 `403 error code: 1010`）。v4 读接口为 `/api/public/v2/observations`，见 [otel-mapping.md](otel-mapping.md)。
- 客户本地：卸载插件后删除 `~/.local/state/codex-trace`。

## 6. 开发

```sh
make test       # 测试
make vet        # gofmt + go vet
make cover      # 覆盖率
make e2e        # 本机端到端：假 OTLP 接收器 + 真实 codex exec 会话（会临时安装 / 卸载插件，消耗少量模型额度）
make e2e-langfuse  # 同上，但上报到 build.env 中的真实 Langfuse（environment=codex-trace-e2e）并读回校验
make smoke      # 真实 Langfuse 冒烟：发送 fixture 会话、读回比对、重复发送检查去重（environment 取自 build.env）
```

重新采集 hook payload（Codex 升级后核对格式）：`scripts/capture-install.py install`，跑会话后 `uninstall`，见 [payload-notes.md](payload-notes.md)。
