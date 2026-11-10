#!/bin/sh
# 临时采集 Codex hook payload：stdin 原样落盘。
# 约束：不向 stdout 输出任何内容、永远 exit 0。
# 用法：capture-hook.sh <EventName>

umask 077
event="${1:-unknown}"
dir="${CODEX_TRACE_CAPTURE_DIR:-$HOME/.codex-trace/capture}"
mkdir -p "$dir" 2>/dev/null || { cat >/dev/null 2>&1; exit 0; }

# macOS date 不支持 %N，用 perl 取微秒级时间戳保证文件名有序
ts=$(perl -MTime::HiRes=time -MPOSIX=strftime -e '$t=time; printf "%s.%06d", strftime("%Y%m%dT%H%M%S", localtime $t), ($t-int $t)*1e6' 2>/dev/null)
base="$dir/${ts:-$(date +%Y%m%dT%H%M%S)}-$$-${event}"

cat >"$base.json" 2>/dev/null

# 环境变量：记录全部变量名；只记录 CODEX_/PLUGIN_ 前缀且不像密钥的值
{
  echo "argv: $*"
  echo "cwd: $(pwd)"
  echo "--- names"
  env | sed 's/=.*//' | sort
  echo "--- values"
  env | grep -E '^(CODEX|PLUGIN)[A-Z_]*=' | grep -viE '(key|token|secret|auth|password)[A-Z_]*='
} >"$base.env" 2>/dev/null

exit 0
