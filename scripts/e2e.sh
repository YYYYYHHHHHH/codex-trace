#!/bin/sh
# 本机端到端测试：本地假 OTLP 接收器 + 真实 Codex 会话（codex exec）。
#
# 目标（E2E_TARGET）：
#   fake（默认）    本地假 OTLP 接收器，用 fakeotlp verify 断言；
#   langfuse        真实 Langfuse，凭证取自 BUILD_ENV（默认 ~/.config/codex-trace/build.env），
#                   environment 固定为 codex-trace-e2e，用 lfsmoke verify 从 Langfuse 读回断言。
#
# 流程：启动 fakeotlp（fake 模式）→ 把插件复制到临时目录，作为独立 marketplace（codex-trace-e2e）构建并安装 →
#      跑三段会话（子 agent / resume / 打断）→ 等待上传 → fakeotlp verify 断言调用树 →
#      卸载并核对 ~/.codex/config.toml 与安装前一致。
#
# 安全措施：
#   - 仓库内的插件目录与构建产物不会被修改（测试凭证只存在于临时目录）；
#   - 本机已安装任何 codex-trace 插件时直接中止，避免干扰或误删正式安装；
#   - 清除调用者环境中的 CODEX_TRACE_* 与用户配置，保证数据只发往本地假接收器；
#   - codex exec 使用 --dangerously-bypass-hook-trust（仅本次调用有效，不写入任何信任状态）。
# 测试会话运行在临时目录，会消耗少量模型额度。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CODEX=${CODEX:-$(command -v codex 2>/dev/null || echo "$HOME/.codex/plugins/.plugin-appserver/codex-cli/bin/codex")}
CODEX_HOME_DIR=${CODEX_HOME:-$HOME/.codex}
work=$(mktemp -d "${TMPDIR:-/tmp}/codex-trace-e2e.XXXXXX")
marketplace=codex-trace-e2e
plugin=codex-trace@$marketplace

log() { printf '\n==> %s\n' "$*"; }

if grep -q 'plugins\."codex-trace@' "$CODEX_HOME_DIR/config.toml" 2>/dev/null; then
  echo "本机已安装 codex-trace 插件，为避免干扰正式安装，e2e 中止。请先卸载或换一台机器。" >&2
  exit 1
fi

# 隔离调用者的 codex-trace 配置
for v in $(env | sed -n 's/^\(CODEX_TRACE_[A-Z_]*\)=.*/\1/p'); do unset "$v"; done
export CODEX_TRACE_CONFIG="$work/no-user-config.toml"
export CODEX_TRACE_STATE_DIR="$work/state"

target=${E2E_TARGET:-fake}
user_build_env=${BUILD_ENV:-$HOME/.config/codex-trace/build.env}
case "$target" in
  fake)
    port=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
    pk=pk-lf-e2e
    sk=sk-lf-e2e-$(python3 -I -c 'import secrets; print(secrets.token_hex(8))')
    ;;
  langfuse)
    [ -f "$user_build_env" ] || { echo "E2E_TARGET=langfuse 需要 $user_build_env" >&2; exit 1; }
    ;;
  *)
    echo "未知 E2E_TARGET=${target}（可选 fake / langfuse）" >&2
    exit 1
    ;;
esac

fake_pid=
installed=0
cleaned=0
completed=0
cleanup() {
  status=$?
  # macOS /bin/sh 在顶层展开错误（如 set -u 的未定义变量）后，EXIT trap 中的 $? 为 0；
  # 没有走到脚本末尾就一律按失败处理，避免测试失败被吞掉。
  if [ "$completed" != 1 ] && [ "$status" = 0 ]; then
    status=1
  fi
  [ "$cleaned" = 1 ] && return
  cleaned=1
  if [ "$installed" = 1 ]; then
    log "卸载插件"
    "$CODEX" plugin remove "$plugin" >/dev/null 2>&1 || echo "警告：卸载插件失败，请手动执行 codex plugin remove $plugin"
    "$CODEX" plugin marketplace remove "$marketplace" >/dev/null 2>&1 || echo "警告：移除 marketplace 失败"
    rmdir "$CODEX_HOME_DIR/plugins/cache/$marketplace" 2>/dev/null || true # Codex 卸载后留下的空目录
    if python3 -I -c 'import tomllib' 2>/dev/null; then
      python3 -I - "$work/config.toml.before" "$CODEX_HOME_DIR/config.toml" <<'PY' || status=1
import sys, tomllib
before, after = (tomllib.load(open(p, "rb")) for p in sys.argv[1:3])
if before == after:
    print("config.toml 与安装前一致")
else:
    keys = sorted(k for k in set(before) | set(after) if before.get(k) != after.get(k))
    print("警告：config.toml 与安装前不一致，差异顶层键：", keys)
    sys.exit(1)
PY
    elif cmp -s "$work/config.toml.before" "$CODEX_HOME_DIR/config.toml"; then
      echo "config.toml 与安装前一致"
    else
      echo "警告：config.toml 与安装前不一致（无 tomllib，按字节比较），备份：$work/config.toml.before"
      status=1
    fi
  fi
  [ -n "$fake_pid" ] && kill "$fake_pid" 2>/dev/null || true
  echo "工作目录（日志、收到的数据）：$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if [ "$target" = fake ]; then
  log "启动假 OTLP 接收器 127.0.0.1:$port"
  go build -o "$work/fakeotlp" "$root/tools/fakeotlp"
  "$work/fakeotlp" serve -addr "127.0.0.1:$port" -out "$work/received.jsonl" -user "$pk" -pass "$sk" >"$work/fakeotlp.log" 2>&1 &
  fake_pid=$!
fi

log "准备临时 marketplace 并构建内置测试凭证的二进制"
umask 077
mp="$work/marketplace"
mkdir -p "$mp/.agents/plugins" "$mp/plugins"
cp -R "$root/plugins/codex-trace" "$mp/plugins/"
rm -rf "$mp/plugins/codex-trace/bin/darwin-"* "$mp/plugins/codex-trace/bin/linux-"*
# 只改 marketplace 名称，插件名保持 codex-trace
python3 -I - "$root/.agents/plugins/marketplace.json" "$mp/.agents/plugins/marketplace.json" "$marketplace" <<'PY'
import json, sys
src, dst, name = sys.argv[1:4]
m = json.load(open(src))
m["name"] = name
json.dump(m, open(dst, "w"), ensure_ascii=False, indent=2)
PY
if [ "$target" = fake ]; then
  cat >"$work/build.env" <<EOF
CODEX_TRACE_LANGFUSE_HOST=http://127.0.0.1:$port
CODEX_TRACE_LANGFUSE_PUBLIC_KEY=$pk
CODEX_TRACE_LANGFUSE_SECRET_KEY=$sk
CODEX_TRACE_ENVIRONMENT=codex-trace-e2e
EOF
else
  # 复用真实凭证，只把 environment 改为 codex-trace-e2e，便于在 Langfuse 中筛选清理
  grep -v '^CODEX_TRACE_ENVIRONMENT=' "$user_build_env" >"$work/build.env"
  echo "CODEX_TRACE_ENVIRONMENT=codex-trace-e2e" >>"$work/build.env"
fi
BUILD_ENV="$work/build.env" VERSION="0.0.0-e2e" OUT_DIR="$mp/plugins/codex-trace/bin" \
  PLATFORMS="$(go env GOOS)/$(go env GOARCH)" "$root/scripts/build-dist.sh"

log "安装插件"
cp "$CODEX_HOME_DIR/config.toml" "$work/config.toml.before"
installed=1
"$CODEX" plugin marketplace add "$mp"
"$CODEX" plugin add "$plugin"
launcher=$(find "$CODEX_HOME_DIR/plugins/cache/$marketplace" -path '*/bin/trace-hook' -type f 2>/dev/null | head -1)
[ -n "$launcher" ] || { echo "未找到安装后的 trace-hook"; exit 1; }
echo "已安装：$launcher"

mkdir -p "$work/run"
cd "$work/run"
run_codex() { "$CODEX" exec --dangerously-bypass-hook-trust --skip-git-repo-check -s workspace-write "$@"; }

log "会话 1：shell、失败命令、apply_patch、子 agent"
run_codex "这是一次 hook 采集测试，请按顺序执行：1) 运行 ls -la；2) 运行 cat /nonexistent-file（预期失败，继续即可）；3) 用 apply_patch 在当前目录新建 test.txt 写入一行 hello；4) 启动一个子 agent，让它统计当前目录的文件数并汇报；5) 最后用一句话总结。" >"$work/session1.log" 2>&1

log "会话 1 第二轮（resume，显式指定会话 id）"
session_id=$(grep -h -o '"session_id":"[^"]*"' "$CODEX_TRACE_STATE_DIR"/spool/*.json "$CODEX_TRACE_STATE_DIR"/sessions/*.json 2>/dev/null | head -1 | cut -d'"' -f4)
if [ -z "$session_id" ]; then
  session_id=$(ls "$CODEX_TRACE_STATE_DIR/sessions" 2>/dev/null | head -1 | sed 's/\.json$//')
fi
[ -n "$session_id" ] || { echo "未能确定会话 1 的 session id（hook 可能没有触发）"; exit 1; }
"$CODEX" exec --dangerously-bypass-hook-trust --skip-git-repo-check -s workspace-write \
  resume "$session_id" "第二轮：运行 wc -l test.txt 并报告结果。" >"$work/session2.log" 2>&1

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

log "校验调用树（${target}）"
if [ "$target" = fake ]; then
  "$work/fakeotlp" verify -in "$work/received.jsonl" -min-turns 3 \
    -expect-subagent -expect-failed-tool -expect-interrupted -no-content
else
  sessions=$(ls "$CODEX_TRACE_STATE_DIR/sessions" | sed -n 's/\.json$//p' | paste -sd, -)
  echo "会话：$sessions"
  (cd "$root" && go run ./tools/lfsmoke verify -env "$work/build.env" -session "$sessions" -min-turns 3 \
    -expect-subagent -expect-failed-tool -expect-interrupted -no-content)
fi
completed=1
