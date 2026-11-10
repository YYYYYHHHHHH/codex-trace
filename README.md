# codex-trace

通过 Codex hooks 采集 Codex 调用链（轮次、工具调用、子 agent、模型调用与 token），以 OTLP 上报到 Langfuse，以 Codex 插件形式分发，装完即用。

## 当前状态（2026-11-09）

- ✅ 采集、关联、上传、运维命令、插件打包全部完成；本机 E2E（本地假 OTLP 接收器 + 真实 Codex 会话）通过。
- ⏳ 待 Langfuse 信息：M1 真实 Langfuse 冒烟（验证属性映射与 span 去重）与真实环境 E2E。见 [docs/PLAN.md](docs/PLAN.md)。

## 文档

| 文档 | 读者 |
|---|---|
| [docs/install.md](docs/install.md) | 客户：两步接入、确认生效、暂停与卸载 |
| [docs/data-collection.md](docs/data-collection.md) | 客户合规评审：采集字段、内容策略、保留与删除 |
| [docs/operations.md](docs/operations.md) | 维护人员：构建发布、凭证轮换、排障 |
| [docs/managed-deployment.md](docs/managed-deployment.md) | 企业 IT：托管下发 hook（免审批，草案） |
| [docs/PLAN.md](docs/PLAN.md) | 交付计划与进度 |
| [docs/payload-notes.md](docs/payload-notes.md) | Codex hook payload / rollout 实测记录 |
| [docs/HANDOFF.md](docs/HANDOFF.md) | 项目背景与原始决策 |

## 结构

```text
cmd/trace-hook/          单一可执行文件：hook 入口、后台上传进程、运维命令
internal/
  hook/                  hook 入口：stdin → spool（不联网、不输出、永远 exit 0）
  pipeline/ session/     hook 事件 + rollout 事实 → 轮次状态机
  rollout/               增量解析 ~/.codex/sessions/**/rollout-*.jsonl
  trace/ otlp/ redact/   构造 Langfuse OTel span、OTLP/JSON 编码、脱敏
  uploader/ exporter/    后台上传：outbox、退避重试、单实例
  ops/                   status / doctor / preview / pause / config show
  config/ paths/ ...     分层配置（内置 < 用户 < 环境变量 < 托管）
plugins/codex-trace/     Codex 插件（plugin.json、hooks.json、平台选择启动脚本、skill）
.agents/plugins/         marketplace 清单（仓库本身即 marketplace）
tools/fakeotlp/          E2E 用本地 OTLP 接收器与调用树校验
scripts/                 build-dist.sh（发布构建）、e2e.sh、payload 采集工具
testdata/                脱敏的真实 hook payload 与 rollout 样本
```

## 开发

```sh
make test     # go test -race
make vet      # gofmt + go vet
make cover    # 覆盖率
make dist     # 四平台发布构建（凭证来自 ~/.config/codex-trace/build.env）
make e2e      # 本机端到端测试
```
