# codex-trace 接入指南（内部客户）

codex-trace 是一个 Codex 插件：安装并审批后，自动把你的 Codex 调用链上报到团队的 Langfuse，用于排查问题和统计用量。

## 会采集什么

默认**只上报元数据**，不上传 prompt、工具输入输出与模型回复原文：

- 每轮对话：开始 / 结束时间、模型、推理强度、是否被打断、工具调用次数
- 每次工具调用：工具名、耗时、退出码（失败标红）、输入 / 输出的字节数
- 子 agent：类型、与发起它的工具调用的父子关系
- 每次模型调用：token 用量（输入、缓存命中、输出、推理）
- 标识：会话 id、哈希后的用户与机器标识、Codex 版本、工作目录名（不含完整路径）

完整字段清单见 [data-collection.md](data-collection.md)。

## 接入（两步）

### 1. 添加 marketplace 并安装插件

```sh
codex plugin marketplace add YYYYYHHHHHH/codex-trace@release
codex plugin add codex-trace@codex-trace
```

仓库是私有的：需要先获得 `YYYYYHHHHHH/codex-trace` 的读权限，并确保本机 git 能访问 GitHub 私有仓库（例如已执行 `gh auth login`，或配置了 SSH key；SSH 方式可写 `git@github.com:YYYYYHHHHHH/codex-trace.git@release`）。

升级到新版本：`codex plugin marketplace upgrade`，然后在 Codex 中重新审批 hook。

#### 只装了 Codex 桌面端（没有 `codex` 命令）

桌面端与 CLI 共用 `~/.codex/config.toml`，在任一处装好的插件两边都生效。任选一种方式：

**方式 A：用桌面端自带的 CLI（推荐）**

桌面端会在本机放一份 CLI，直接用它执行上面两条命令：

```sh
CODEX=~/.codex/plugins/.plugin-appserver/codex-cli/bin/codex
"$CODEX" plugin marketplace add YYYYYHHHHHH/codex-trace@release
"$CODEX" plugin add codex-trace@codex-trace
```

找不到这个文件时（桌面端版本不同，路径可能变化），用方式 B。

**方式 B：手动添加 marketplace，再在桌面端安装**

1. 完全退出 Codex 桌面端。
2. 在 `~/.codex/config.toml` 末尾追加：

   ```toml
   [marketplaces.codex-trace]
   source_type = "git"
   source = "https://github.com/YYYYYHHHHHH/codex-trace.git"
   ref = "release"
   ```

3. 重新打开 Codex 桌面端，在插件页面找到 "Codex Trace" 并安装。

两种方式都需要本机 git 能访问这个私有仓库（见上文）。桌面端从 Dock / 启动台打开时**不会继承终端里的环境变量**：公司网络需要代理时，请在系统层面配置代理，或从终端启动桌面端，否则上报会失败（`doctor` 中表现为"网络不通"）。

### 2. 审批 hook

Codex 对插件自带的 hook 有审批机制，**未审批的 hook 不会执行，也不会有任何提示**。

- 安装后首次启动 Codex 时，按提示审批 codex-trace 的 hooks。
- **插件升级后需要重新审批**（hook 内容变化会使之前的审批失效）。

不需要配置任何 key：凭证已内置在插件中。

## 确认已生效

用 Codex 正常跑一轮对话后：

```sh
TRACE_HOOK=$(ls -td "${CODEX_HOME:-$HOME/.codex}"/plugins/cache/*/codex-trace/*/bin/trace-hook | head -1)
"$TRACE_HOOK" doctor
```

全部为 ✓ 即表示正常。也可以直接问 Codex："codex-trace 采集状态正常吗？"（插件带有对应的 skill）。

常见问题：

| doctor 提示 | 处理 |
|---|---|
| hook 触发：从未触发 | 检查第 2 步是否审批；插件升级后需重新审批 |
| Codex 插件：未发现 hook 审批记录 | 同上 |
| Langfuse 连通性：网络不通 | 检查网络 / 代理；公司代理请确保 `HTTPS_PROXY` 对 Codex 可见 |
| Langfuse 连通性：凭证被拒绝 | 联系维护人员（凭证可能已轮换，需要升级插件） |

## 暂停与卸载

```sh
"$TRACE_HOOK" pause     # 暂停采集（已采集未上传的数据仍会上传）
"$TRACE_HOOK" resume    # 恢复
codex plugin remove codex-trace@codex-trace   # 卸载
```

卸载后本地状态目录 `~/.local/state/codex-trace` 可以直接删除。

## 平台支持

macOS（Apple Silicon / Intel）、Linux（arm64 / x64）。暂不支持 Windows。
