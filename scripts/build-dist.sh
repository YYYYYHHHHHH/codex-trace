#!/bin/sh
# 交叉编译 trace-hook 到 plugins/codex-trace/bin/<os>-<arch>/。
# 凭证从 BUILD_ENV（默认 ~/.config/codex-trace/build.env，必须 0600）读取并通过 -ldflags 注入；
# 文件不存在时构建出的二进制没有内置凭证（静默不采集）。脚本不会回显任何密钥。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
build_env=${BUILD_ENV:-$HOME/.config/codex-trace/build.env}
version=${VERSION:-$(cat "$root/VERSION")}
platforms=${PLATFORMS:-"darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"}
pkg=github.com/YYYYYHHHHHH/codex-trace/internal/buildinfo

CODEX_TRACE_LANGFUSE_HOST=
CODEX_TRACE_LANGFUSE_PUBLIC_KEY=
CODEX_TRACE_LANGFUSE_SECRET_KEY=
CODEX_TRACE_ENVIRONMENT=
if [ -f "$build_env" ]; then
  perm=$(stat -f '%Lp' "$build_env" 2>/dev/null || stat -c '%a' "$build_env")
  case "$perm" in
    600 | 400) ;;
    *)
      echo "build-dist: $build_env 的权限为 ${perm}，请执行 chmod 600" >&2
      exit 1
      ;;
  esac
  # shellcheck disable=SC1090
  . "$build_env"
  echo "build-dist: 使用 ${build_env}（host=$CODEX_TRACE_LANGFUSE_HOST, environment=${CODEX_TRACE_ENVIRONMENT}）"
else
  echo "build-dist: 未找到 ${build_env}，构建不含内置凭证的二进制"
fi

ldflags="-s -w -X $pkg.Version=$version"
ldflags="$ldflags -X $pkg.LangfuseHost=$CODEX_TRACE_LANGFUSE_HOST"
ldflags="$ldflags -X $pkg.LangfusePublicKey=$CODEX_TRACE_LANGFUSE_PUBLIC_KEY"
ldflags="$ldflags -X $pkg.LangfuseSecretKey=$CODEX_TRACE_LANGFUSE_SECRET_KEY"
ldflags="$ldflags -X $pkg.Environment=$CODEX_TRACE_ENVIRONMENT"

for p in $platforms; do
  os=${p%/*}
  arch=${p#*/}
  out="$root/plugins/codex-trace/bin/$os-$arch/trace-hook"
  mkdir -p "$(dirname "$out")"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$out" "$root/cmd/trace-hook"
  echo "build-dist: $os/$arch -> ${out#"$root"/} ($(du -h "$out" | cut -f1))"
done
