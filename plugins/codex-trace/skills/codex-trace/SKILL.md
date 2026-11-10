---
name: codex-trace
description: 查看或诊断 codex-trace 调用链采集（是否在上报、最近错误、暂停 / 恢复采集、预览将上传的内容）。用户问"调用链有没有上报"、"trace 采集状态"、"暂停采集"时使用。
---

# codex-trace

codex-trace 插件把 Codex 的调用链上报到团队的 Langfuse。所有操作通过插件自带的 `trace-hook` 命令完成。

## 找到命令

```sh
TRACE_HOOK=$(ls -d "${CODEX_HOME:-$HOME/.codex}"/plugins/cache/*/codex-trace/*/bin/trace-hook 2>/dev/null | tail -1)
```

## 常用命令

| 目的 | 命令 |
|---|---|
| 查看状态（最近 hook、最近上传、积压、错误） | `"$TRACE_HOOK" status` |
| 逐项诊断（配置、hook 审批、网络、凭证） | `"$TRACE_HOOK" doctor` |
| 查看即将上传 / 最近上传的内容 | `"$TRACE_HOOK" preview` |
| 暂停 / 恢复采集 | `"$TRACE_HOOK" pause` / `"$TRACE_HOOK" resume` |
| 查看生效配置（密钥已打码） | `"$TRACE_HOOK" config show` |

## 注意

- 只运行上面列出的命令，不要读取或输出任何配置文件中的密钥。
- `doctor` 提示 hook 未审批时，告诉用户在 Codex 中审批 codex-trace 的 hooks（插件升级后需要重新审批）。
