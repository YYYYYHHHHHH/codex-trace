#!/bin/sh
# 本机端到端测试：本地假 OTLP 接收器 + 真实 Codex 会话（codex exec）。
#
# 流程：启动 fakeotlp → 用临时测试凭证构建本平台二进制 → 通过 codex plugin 安装本仓库插件 →
#      跑三段会话（子 agent / resume / 打断）→ 等待上传 → fakeotlp verify 断言调用树 →
#      卸载插件并核对 ~/.codex/config.toml 与安装前一致。
#
# 注意：codex exec 使用 --dangerously-bypass-hook-trust（仅本次调用有效，不写入任何信任状态），
# 测试会话运行在临时目录，会消耗少量模型额度。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CODEX=${CODEX:-$(command -v codex 2>/dev/null || echo "$HOME/.codex/plugins/.plugin-appserver/codex-cli/bin/codex")}
CODEX_HOME_DIR=${CODEX_HOME:-$HOME/.codex}
work=$(mktemp -d "${TMPDIR:-/tmp}/codex-trace-e2e.XXXXXX")
marketplace=codex-trace
plugin=codex-trace@codex-trace

log() { printf '\n==> %s\n' "$*"; }

port=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
pk=pk-lf-e2e
sk=sk-lf-e2e-$(python3 -I -c 'import secrets; print(secrets.token_hex(8))')

fake_pid=
installed=0
cleanup() {
  status=$?
  if [ "$installed" = 1 ]; then
    log "卸载插件"
    "$CODEX" plugin remove "$plugin" >/dev/null 2>&1 || echo "警告：卸载插件失败，请手动执行 codex plugin remove $plugin"
    "$CODEX" plugin marketplace remove "$marketplace" >/dev/null 2>&1 || echo "警告：移除 marketplace 失败"
    rmdir "$CODEX_HOME_DIR/plugins/cache/$marketplace" 2>/dev/null || true # Codex 卸载后留下的空目录
    python3 -I - "$work/config.toml.before" "$CODEX_HOME_DIR/config.toml" <<'PY' || status=1
import sys, tomllib
before, after = (tomllib.load(open(p, "rb")) for p in sys.argv[1:3])
if before == after:
    print("config.toml 与安装前一致")
else:
    keys = sorted(set(before) ^ set(after)) or sorted(k for k in before if before.get(k) != after.get(k))
    print("警告：config.toml 与安装前不一致，差异顶层键：", keys)
    sys.exit(1)
PY
  fi
  [ -n "$fake_pid" ] && kill "$fake_pid" 2>/dev/null || true
  echo "工作目录（日志、收到的数据）：$work"
  exit "$status"
}
trap cleanup EXIT INT TERM

log "启动假 OTLP 接收器 127.0.0.1:$port"
go build -o "$work/fakeotlp" "$root/tools/fakeotlp"
"$work/fakeotlp" serve -addr "127.0.0.1:$port" -out "$work/received.jsonl" -user "$pk" -pass "$sk" >"$work/fakeotlp.log" 2>&1 &
fake_pid=$!

log "构建内置测试凭证的二进制"
umask 077
cat >"$work/build.env" <<EOF
CODEX_TRACE_LANGFUSE_HOST=http://127.0.0.1:$port
CODEX_TRACE_LANGFUSE_PUBLIC_KEY=$pk
CODEX_TRACE_LANGFUSE_SECRET_KEY=$sk
CODEX_TRACE_ENVIRONMENT=codex-trace-e2e
EOF
BUILD_ENV="$work/build.env" VERSION="0.0.0-e2e" PLATFORMS="$(go env GOOS)/$(go env GOARCH)" "$root/scripts/build-dist.sh"

log "安装插件"
cp "$CODEX_HOME_DIR/config.toml" "$work/config.toml.before"
installed=1
"$CODEX" plugin marketplace add "$root"
"$CODEX" plugin add "$plugin"
launcher=$(find "$CODEX_HOME_DIR/plugins/cache" -path '*codex-trace*' -path '*/bin/trace-hook' -type f 2>/dev/null | head -1)
[ -n "$launcher" ] || { echo "未找到安装后的 trace-hook"; exit 1; }
echo "已安装：$launcher"

export CODEX_TRACE_STATE_DIR="$work/state"
mkdir -p "$work/run"
cd "$work/run"
exec_codex() { "$CODEX" exec --dangerously-bypass-hook-trust --skip-git-repo-check -s workspace-write "$@"; }

log "会话 1：shell、失败命令、apply_patch、子 agent"
exec_codex "这是一次 hook 采集测试，请按顺序执行：1) 运行 ls -la；2) 运行 cat /nonexistent-file（预期失败，继续即可）；3) 用 apply_patch 在当前目录新建 test.txt 写入一行 hello；4) 启动一个子 agent，让它统计当前目录的文件数并汇报；5) 最后用一句话总结。" >"$work/session1.log" 2>&1

log "会话 1 第二轮（resume）"
"$CODEX" exec --dangerously-bypass-hook-trust --skip-git-repo-check -s workspace-write resume --last "第二轮：运行 wc -l test.txt 并报告结果。" >"$work/session2.log" 2>&1

log "会话 2：中途打断"
# 非交互 shell 中后台作业默认忽略 SIGINT；开启 job control，让 codex 能收到中断信号
set -m
"$CODEX" exec --dangerously-bypass-hook-trust --skip-git-repo-check -s workspace-write \
  "运行 sleep 30 然后说完成" >"$work/session3.log" 2>&1 &
pid=$!
set +m
sleep 10
kill -INT "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true

log "等待上传完成"
"$launcher" upload --once || true
i=0
pending=?
while [ $i -lt 90 ]; do
  pending=$(python3 -I - "$CODEX_TRACE_STATE_DIR" <<'PY'
import json, os, sys
d = sys.argv[1]
count = lambda sub: len([n for n in os.listdir(os.path.join(d, sub)) if n.endswith(".json")]) if os.path.isdir(os.path.join(d, sub)) else 0
sessions = os.path.join(d, "sessions")
closed = 0
if os.path.isdir(sessions):
    for n in os.listdir(sessions):
        if n.endswith(".json"):
            closed += sum(1 for t in json.load(open(os.path.join(sessions, n))).get("turns", {}).values() if t.get("close"))
print(count("spool") + count("outbox") + closed)
PY
)
  [ "$pending" = 0 ] && break
  i=$((i + 1))
  sleep 1
done
echo "剩余待处理：$pending"

log "trace-hook status"
"$launcher" status || true

log "trace-hook doctor"
"$launcher" doctor || true

log "校验调用树"
"$work/fakeotlp" verify -in "$work/received.jsonl" -min-turns 3 \
  -expect-subagent -expect-failed-tool -expect-interrupted -no-content
