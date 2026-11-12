# codex-trace 交付计划

> 编写日期：2026-11-09
> 状态：**M0–M7 全部完成，真实 Langfuse 验收通过**（2026-11-10）。已发布到公开 git marketplace `YYYYYHHHHHH/codex-trace@release`（v0.1.0）。待办：客户试点。
> 使用对象：**内部客户**。
> 前置文档：[HANDOFF.md](HANDOFF.md)（背景与原始决策）、[payload-notes.md](payload-notes.md)（实测结论）

## 0. 进度（2026-11-10）

| 里程碑 | 状态 | 分支 |
|---|---|---|
| M0 采集结论 | ✅ | `hook-payload-capture` |
| M2 hook 采集端 | ✅ 进程级 p50 3ms；已修完 go / security 评审 | `hook-capture` |
| M3 关联处理器 | ✅ 真实会话黄金测试；已修完评审 | `correlation` |
| M4 上传进程 | ✅ 已修完 security 评审 | `uploader`、`uploader-hardening` |
| M5 运维命令 | ✅ | `ops-commands` |
| M6 插件打包与本机 E2E | ✅ `make e2e` 全部断言通过（假 OTLP 接收器） | `plugin-package` |
| M7 交付文档 | ✅ install / data-collection / operations / managed-deployment | `plugin-package` |
| M1 真实 Langfuse 冒烟 | ✅ `make smoke` 全部通过；结论见 [otel-mapping.md](otel-mapping.md) | `langfuse-acceptance` |
| 真实环境 E2E | ✅ `make e2e-langfuse`：真实 Codex 会话上报到自建 Langfuse v4.77.0 并读回校验，全部通过 | `langfuse-acceptance` |

分支依次叠加，最新分支包含全部提交，由你合入 `main`。`internal/` 总覆盖率 88.9%。

### 已知问题与后续

- ~~M1 需要验证~~：已验证。Langfuse v4 按 span id 去重并保留最新版本；属性映射全部生效；归属属性（session / user / environment）需每个 span 携带，已修正。详见 [otel-mapping.md](otel-mapping.md)。
- 前置 Cloudflare 会拦截部分默认 User-Agent，exporter 已显式发送 `codex-trace/<版本>`。
- 验收时 secret key 经过了对话记录，建议轮换后再构建正式发布版本。
- `PermissionRequest` 事件尚未采到真实样本（`codex exec` 无审批交互），目前只计数。
- 重放时 `permission_requests` 计数可能偏大（仅影响展示）。
- Codex 已提供 `PreModelRequest` / `PostModelRequest` / `PreCompact` / `PostCompact` / `SessionEnd` 事件：若 `PostModelRequest` 携带 token 与模型信息，generation 可直接取自 hook，减少对 rollout 的依赖。待采样确认。
- Windows 不支持（启动脚本依赖 POSIX sh）。
- 发布构建时 secret 会短暂出现在 `go build` 进程参数中（`-ldflags`），只应在受信构建机上执行。

## 1. 交付范围

**本次交付 = 在本机端到端跑通，并产出可直接给客户试点的版本。**

包含：

- Go 实现的 `trace-hook` 单二进制：hook 采集 + 后台上传 + 运维命令
- Codex 插件包与 marketplace 清单（仓库本身即 marketplace）
- macOS（arm64 / x64）、Linux（arm64 / x64）构建产物
- 本机 E2E：插件安装 → 跑 Codex 会话 → Langfuse 中调用树经自动化脚本校验通过
- **装完即用**：Langfuse 凭证在发布构建时内置进二进制，客户只需添加 marketplace、安装插件、审批一次 hook
- 文档：客户接入、运维与密钥轮换、企业托管下发说明、数据采集说明（合规用）

不包含（需要你决策或外部动作，另行安排）：

- 推送到远端 git / 发布公开 marketplace（仓库当前没有 remote）
- 客户试点本身
- Windows 支持（见第 8 节决策 D3）
- 自建网关、多租户

## 2. 相比 HANDOFF 的方案变更

| # | 变更 | 原因 |
|---|---|---|
| C1 | 上报接口从 `/api/public/ingestion` 改为 **OTLP/HTTP JSON：`POST /api/public/otel/v1/traces`** | Langfuse 源码 `deprecations.ts`：Langfuse Cloud **2026-11-16 起**旧接口拒收 trace / observation；v4 `events_only` 模式下已拒收。OTel 端点 v3 自建版也支持，不必区分部署形态 |
| C2 | hook 只落盘原始 payload，**关联与补全全部放到上传进程** | 退出码、token、子 agent 父子关系都要读 rollout，见 payload-notes 第 5 节 |
| C3 | 默认**轮次结束时**（`Stop` / `Interrupt` / `SubagentStop`）一次性发出该轮所有 span | OTel span 发出后不可变；轮次结束时信息最全。M1 验证 Langfuse 是否按 span id 合并，若支持再考虑"边跑边发" |
| C4 | 分发与接入文档必须处理 **hook 信任审批** | 未信任的 hook 静默不执行；插件 hook 也要审批；托管配置自动受信，见 payload-notes 第 6 节 |
| C6 | **凭证内置**（2026-11-09 用户决定，方案 A）：发布构建时通过 `-ldflags` 注入 host / pk / sk，配置文件仅作可选覆盖 | 装完即用，无需客户配置 key。风险与缓解见第 3.1 节 |
| C5 | 确定性 ID 改为 OTel 格式 | trace id 16 字节、span id 8 字节：`sha256("codex-trace:" + 种类 + ":" + 原始id)` 截断 |

## 3. 架构

```text
Codex hook 事件 (stdin JSON)
  └─▶ trace-hook <event>
        ├─ 未配置 / 已暂停 → 读完 stdin，exit 0
        ├─ 写 spool/<unixnano>-<pid>.json（tmp + rename，原子）
        └─ 若上传进程未运行（flock 探测）→ 拉起 detached 的 `trace-hook upload`
              exit 0，无 stdout，目标耗时 < 10ms

trace-hook upload（单实例，空闲 60s 自动退出）
  ├─ 读 spool → 按 session_id 更新会话状态（state/<session>.json）
  ├─ 轮次结束 → 增量读 rollout（按字节偏移）补全：退出码、token、模型、耗时、子 agent 父子关系
  ├─ 生成 OTel span → 脱敏 / 截断 → 批量 gzip 发送
  ├─ 失败指数退避重试；spool 超 50MB 丢最旧并记日志
  └─ 超过 30 分钟未结束的轮次强制收尾（标 WARNING）
```

本地目录（`$XDG_STATE_HOME/codex-trace`，默认 `~/.local/state/codex-trace`，权限 700）：`spool/`、`state/`、`logs/`、`upload.lock`、`paused`。

配置优先级（高 → 低）：环境变量 `CODEX_TRACE_*` → 托管配置（MDM 下发，路径待 M5 确定）→ 用户配置 `~/.config/codex-trace/config.toml`（权限 600，过宽则拒绝读取）→ **构建时内置默认值**。

```toml
# 可选覆盖文件；正常客户不需要
langfuse_host   = "https://cloud.langfuse.com"
public_key      = "pk-lf-..."
secret_key      = "sk-lf-..."
capture_content = false        # 默认只传元数据
environment     = "production" # 写入 langfuse.environment，便于区分/清理测试数据
enabled         = true         # false 等同于 pause
```

### 3.1 内置凭证的安全约束

- **源码中不出现 key**：`make dist` 从 `~/.config/codex-trace/build.env`（权限 600，不入库）读取并通过 `-ldflags -X` 注入；未提供时构建出的二进制没有内置凭证，行为等同"未配置"（静默不采集）。
- ~~marketplace 仓库必须私有~~：内置 key 可被 `strings` 提取，拿到插件 = 拿到 project 读写权限。2026-11-10 起仓库公开，用户明确接受内置 key 可被任何人提取的风险（可读写、删除该 Langfuse project 的数据），并决定不轮换 key。
- **专用 Langfuse project**：只存 Codex 调用链；`capture_content=false` 降低泄漏后的数据敏感度。
- **轮换流程**（按需手动执行，无定时吊销）：Langfuse 新建 key → 重新 `make dist` → 发布新版本 → 客户 `codex plugin marketplace upgrade`（并重新审批 hook）→ 确认新版本已铺开后，由你决定何时吊销旧 key。写入 `docs/operations.md`。
- **出口可迁移**：上报地址与鉴权方式集中在 `internal/uploader` 的一处配置，将来改走网关（方案 B）只需换构建参数。
- `trace-hook status` / `doctor` / `preview` 的输出中 key 一律打码。

依赖：除 TOML 解析（`github.com/BurntSushi/toml`）外只用标准库；OTLP JSON 手工构造，不引入 OTel SDK。

## 4. 数据映射（OTel → Langfuse）

| 对象 | span id 来源 | parent | 关键属性 |
|---|---|---|---|
| 轮次（trace 根） | `root_turn_id` | — | `session.id`、`langfuse.trace.name="codex turn"`、`langfuse.trace.input/output`、`langfuse.trace.tags`（`interrupted` 等）、metadata（模型、cwd 目录名、Codex 版本） |
| 工具调用 | `tool_use_id` | 轮次 / 子 agent span | `langfuse.observation.type="tool"`、name=`tool_name`、input/output、`level=ERROR` + `status_message`（退出码） |
| 子 agent | 子 agent `turn_id` | spawn 调用的工具 span | `langfuse.observation.type="agent"`、name=`subagent:<agent_type>` |
| 模型调用 | `response_id` | 轮次 / 子 agent span | `langfuse.observation.type="generation"`、`model.name`、`usage_details`（input / cached / output / reasoning / total） |

Resource 属性：`service.name=codex`、Codex 版本、插件版本、机器 id（`installation_id` 哈希）、`user.id`（`creator_user_id` 哈希，缺失时用 OS 用户名哈希）。

**内容策略**：`capture_content=false` 时只传名称、长度、退出码、耗时、token；`true` 时传原文，但先脱敏（密钥 / token / 私钥 / Authorization 头等正则）再截断（单字段 10KB）。`base_instructions`、reasoning 永不上传。

generation 的开始时间取同一轮内上一条 rollout 记录的时间（近似值），结束时间取 `token_usage_record` 时间。

## 5. 里程碑

`main` 已在另一个 worktree（`~/Code/codex-trace`）检出，本 worktree 不动它：每个里程碑一个分支 `YYYYYHHHHHH/<名称>`，**依次叠在上一个里程碑分支之上**，最终由你把最后一个分支合入 `main`。

执行顺序：M0 → M2 → M3 → M4 → M5 → M6（假接收器 E2E）→ M7 →（Langfuse 信息到位后）M1 → 真实环境 E2E。

### M0 收尾采集分支
- 在 `hook-payload-capture` 分支提交当前改动（采集脚本、payload-notes、fixture、本计划）
- **验收**：分支包含全部采集成果，工作区干净

### M1 Langfuse OTel 冒烟（等 Langfuse 信息，放在 M7 之后）
- 小 Go 程序手工构造 OTLP JSON：1 个轮次 + 工具 / agent / generation 各 1，发到你的 Langfuse
- 用 `GET /api/public/v2/observations` 读回校验
- 需要验证：JSON 编码可用；observation 类型与树形正确；usage 显示正确；**同一 span id 重发是否去重/合并**（决定 C3 能否改成边跑边发、重试是否安全）
- 产出：`docs/otel-mapping.md`（实测确认的属性表）
- **验收**：Langfuse 中能看到预期的调用树，读回校验脚本通过

### M2 hook 采集端
- `cmd/trace-hook` + `internal/config`、`internal/spool`
- 测试先行：任意输入（含空、非 JSON、超大）都 exit 0 且无 stdout；未配置静默；paused 静默；原子写；fuzz 测试
- **验收**：单测 + fuzz 通过；基准测试单次 hook < 10ms

### M3 关联处理器
- `internal/session`（会话状态机）、`internal/rollout`（增量读取，过滤子 agent 继承的父历史）、`internal/trace`（构造 span 模型）
- 以 `testdata/hook-payloads/` + 新增脱敏 rollout fixture 做黄金测试
- 覆盖场景：普通轮次、工具失败、apply_patch、子 agent、resume、打断、Codex 崩溃导致轮次未结束、事件乱序与重复
- **验收**：黄金测试全部通过，`internal/` 覆盖率 ≥ 80%

### M4 上传进程
- `internal/otlp`（JSON 编码 + gzip）、`internal/uploader`（批量、退避、单实例锁、空闲退出、spool 上限）、`internal/redact`
- 用 `httptest` 模拟 Langfuse：401 / 429 / 5xx / 超时 / 部分失败
- **验收**：单测通过；对真实 Langfuse 跑一次 fixture 回放，树形与 M1 一致

### M5 运维命令
- `config show`（显示生效配置与来源，key 打码）、`status`（spool 积压、最近一次 hook 触发 / 上传时间、最近错误）、`doctor`（配置权限、凭证有效性、网络 / 代理、hook 是否在触发、插件 hook 是否已信任）、`preview`（打印将上传的脱敏内容）、`pause` / `resume`
- **验收**：单测通过；`doctor` 能分别识别"无内置凭证且未配置""凭证错误""网络不通""已安装但从未触发"，并显示当前凭证来源（内置 / 用户配置 / 托管 / 环境变量）

### M6 插件打包与本机 E2E
- 仓库根 `.agents/plugins/marketplace.json` → `plugins/codex-trace/`（`.codex-plugin/plugin.json`、`hooks/hooks.json`、`bin/trace-hook` 启动脚本按 `uname` 选平台二进制、`skills/codex-trace/`）
- `Makefile`：`build`、`test`、`dist`（四平台交叉编译）
- 用 `codex plugin marketplace add <本地路径>` + `codex plugin add` 安装；`codex exec --dangerously-bypass-hook-trust` 跑与采集时相同的场景
- `tools/fakeotlp`：本地假 OTLP 接收器（校验 Basic 鉴权、gzip、JSON 结构，落盘收到的 span），用 `-ldflags` 构建一个指向它的测试版二进制
- `tools/fakeotlp verify`：读回并断言树形（轮次数、工具 span 父子、失败工具 ERROR、子 agent 挂在 spawn 下、generation token 与 rollout 一致）
- 跑完卸载插件（除非你要求保留，见 D4）
- **验收**：`fakeotlp verify` 全部断言通过（真实 Langfuse 的读回校验在 M1 之后补）

### M7 交付文档与发布准备
- `docs/install.md`（客户接入：加 marketplace → 安装插件并审批 hook，完成；无需配置 key）
- `docs/operations.md`（运维命令、密钥轮换、数据清理、故障排查）
- `docs/managed-deployment.md`（企业托管配置下发草案，hook 免审批）
- `docs/data-collection.md`（采集字段清单，给客户合规评审用）
- 更新 `README.md`、`HANDOFF.md`；`make dist` 产出并按 D2 方式提交构建产物；打 `v0.1.0` 标签
- **验收**：按 `install.md` 在一个干净的 `CODEX_HOME` 中、**不创建任何配置文件**，从零接入成功并在 Langfuse 中看到数据

## 6. 自闭环执行规则

### 6.1 每个里程碑的固定流程
1. 切分支 → 测试先行（RED）→ 实现（GREEN）→ 重构
2. 质量门：`gofmt`、`go vet`、`go test -race ./...`、覆盖率 ≥ 80%（`internal/`）
3. 评审：`go-reviewer` agent；M2 / M4 / M5 额外加 `security-reviewer`；CRITICAL / HIGH 必须修完
4. 约定式提交，在 `HANDOFF.md` 更新进度

### 6.2 我会自主做的事
- 在本仓库内新建 / 修改文件、建分支、提交
- 往你的 Langfuse 写测试数据，全部打 `environment=codex-trace-dev`，便于你一键筛选删除
- 通过官方 `codex plugin` 命令在本机安装 / 卸载插件；用 `codex exec` 在 `/tmp` 下跑测试会话（会消耗少量模型额度）

### 6.3 遇到以下情况停下来找你
- Langfuse 实际行为与计划冲突，需要改架构（例如 span 无法去重导致重试会产生重复数据）
- 需要你本人操作（例如在桌面端审批 hook、提供配置文件）
- 需要改 `~/.codex/` 下除插件安装 / 卸载以外的任何内容
- 范围变化，或某个里程碑的验收标准无法达成

### 6.4 安全底线
- 不打印、不提交 `secret_key`；只在构建时由 Makefile 从 `build.env` 读入，源码、日志、测试 fixture 中不出现真实 key
- 开发阶段构建的二进制内置的是**测试 project** 的 key，不用于对外分发
- 不绕过 hook 信任机制（不自行写 `trusted_hash`）；`--dangerously-bypass-hook-trust` 只在 `/tmp` 下的测试会话中使用
- 不推送到任何远端

## 7. 需要你准备的

在 `~/.config/codex-trace/build.env` 写好**测试用** Langfuse project 的凭证并执行 `chmod 600`。**不要把密钥发在对话里**。M1 开始前我会检查这个文件是否存在（只检查存在和权限，不读出内容）。

```sh
CODEX_TRACE_LANGFUSE_HOST=https://cloud.langfuse.com
CODEX_TRACE_LANGFUSE_PUBLIC_KEY=pk-lf-...
CODEX_TRACE_LANGFUSE_SECRET_KEY=sk-lf-...
CODEX_TRACE_ENVIRONMENT=codex-trace-dev
```

正式对外分发前，建议另建一个生产用 project，用它的 key 重新构建发布版本。

## 8. 待你拍板的决策（括号内为推荐默认值，review 通过即视为采纳）

| # | 决策 | 推荐 | 理由 |
|---|---|---|---|
| D1 | 内容采集默认值 | ~~`capture_content=false`~~ **已改：v0.2.0 默认开启**（2026-11-11） | 会话页需要看到对话内容；同时上线只写网关，二进制不再含可读 key |
| D2 | 构建产物如何进入 git marketplace | 单独的 `release` 分支提交 `bin/`，`main` 只放源码 | marketplace 按 git 快照分发，二进制必须在仓库里；分开放避免 `main` 历史膨胀，且内置 key 只存在于 `release` 分支的二进制中 |
| D3 | Windows | 本次不做，文档中注明 | hook 启动脚本依赖 POSIX sh，Windows 的 hook 执行方式需单独验证 |
| D4 | E2E 后本机是否保留插件 | 卸载 | 保留并在桌面端审批后，你所有真实 Codex 会话都会上传 |
| D5 | 子 agent 内容 | 与主会话同一套内容策略 | 规则统一，避免意外遗漏 |
| ~~D6~~ | 凭证方案 | **已定：A 内置 key**（2026-11-09） | 客户端语言 **已定：Go** |

## 9. 风险

| 风险 | 应对 |
|---|---|
| Langfuse 按 span id 不去重，重试会产生重复 observation | M1 先验证；若不去重，上传进程以"已确认发送"状态严格控制，只对整批失败重试 |
| Codex hook / rollout 格式随版本变化（当前为 alpha 版） | payload 与 rollout 解析容错（未知字段忽略、缺字段降级）；事件里带 Codex 版本便于排查；fixture 按版本归档 |
| 插件升级需重新审批 hook，客户可能没注意到导致静默停采 | `status` / `doctor` 显示"最近一次 hook 触发时间"；企业客户推荐托管下发 |
| macOS Gatekeeper 拦截未签名二进制 | M6 实测；git 克隆的文件通常不带 quarantine 属性，若被拦则在文档中给出处理方式，并记录签名需求 |
| 内置凭证随插件泄漏（仓库公开） | v0.2.0 起只内置网关 token（只写），真实 Langfuse key 只在网关上，见 [ingest-gateway.md](ingest-gateway.md) |
| 装完即自动上报，客户未充分知情 | 插件描述与 `install.md` 首段写明采集内容；`docs/data-collection.md` 供客户合规评审；提供 `pause` 和 `enabled=false` |
| 后台上传进程被系统或用户杀掉 | spool 持久化，下次 hook 触发时自动重新拉起，不丢数据 |
