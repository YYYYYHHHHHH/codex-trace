#!/bin/sh
# 交叉编译 trace-hook 到 <OUT_DIR>/<os>-<arch>/trace-hook（默认 OUT_DIR=plugins/codex-trace/bin）。
# 凭证从 BUILD_ENV（默认 ~/.config/codex-trace/build.env）读取并通过 -ldflags 注入：
#   - 文件必须属于当前用户、组 / 其他用户不可读写；
#   - 只按 KEY=VALUE 逐行解析（不会作为 shell 执行），值中不允许空白字符；
#   - 文件不存在时构建出的二进制没有内置凭证（静默不采集）。
# 脚本不会回显任何密钥。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
build_env=${BUILD_ENV:-$HOME/.config/codex-trace/build.env}
version=${VERSION:-$(cat "$root/VERSION")}
platforms=${PLATFORMS:-"darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"}
out_dir=${OUT_DIR:-$root/plugins/codex-trace/bin}
pkg=github.com/YYYYYHHHHHH/codex-trace/internal/buildinfo

fail() {
  echo "build-dist: $*" >&2
  exit 1
}

host=
public_key=
secret_key=
environment=
capture_content=
if [ -f "$build_env" ]; then
  # 可移植的属主 / 权限检查（BSD 与 GNU 的 stat 参数不同，这里用 find）
  [ -n "$(find "$build_env" -user "$(id -u)" -print)" ] || fail "${build_env} 不属于当前用户"
  loose=$(find "$build_env" \( -perm -040 -o -perm -020 -o -perm -004 -o -perm -002 \) -print)
  [ -z "$loose" ] || fail "${build_env} 对组 / 其他用户可见，请执行 chmod 600"

  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in '' | '#'*) continue ;; esac
    key=${line%%=*}
    value=${line#*=}
    case "$value" in *[[:space:]]*) fail "${key} 的值中包含空白字符" ;; esac
    case "$key" in
      CODEX_TRACE_LANGFUSE_HOST) host=$value ;;
      CODEX_TRACE_LANGFUSE_PUBLIC_KEY) public_key=$value ;;
      CODEX_TRACE_LANGFUSE_SECRET_KEY) secret_key=$value ;;
      CODEX_TRACE_ENVIRONMENT) environment=$value ;;
      CODEX_TRACE_CAPTURE_CONTENT)
        case "$value" in true | false) capture_content=$value ;; *) fail "${key} 只能是 true 或 false" ;; esac
        ;;
      *) fail "未知配置项 ${key}" ;;
    esac
  done <"$build_env"
  echo "build-dist: 使用 ${build_env}（host=${host}，environment=${environment}，capture_content=${capture_content:-false}）"
else
  echo "build-dist: 未找到 ${build_env}，构建不含内置凭证的二进制"
fi

ldflags="-s -w -X $pkg.Version=$version -X $pkg.LangfuseHost=$host"
ldflags="$ldflags -X $pkg.LangfusePublicKey=$public_key -X $pkg.LangfuseSecretKey=$secret_key"
ldflags="$ldflags -X $pkg.Environment=$environment -X $pkg.CaptureContent=$capture_content"

for p in $platforms; do
  os=${p%/*}
  arch=${p#*/}
  out="$out_dir/$os-$arch/trace-hook"
  mkdir -p "$(dirname "$out")"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$out" "$root/cmd/trace-hook"
  echo "build-dist: ${os}/${arch} -> ${out} ($(du -h "$out" | cut -f1))"
done
