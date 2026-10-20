#!/usr/bin/env bash
# 契约破坏性变更检测（#86）：用锁版本的 oasdiff 对比「上一个 release tag」与当前
# api/openapi.yaml，再由 breakinggate 只放行允许清单中的已批准项。
#
# 基线选择：
#   1. git describe 找到最近的 v* tag，且该 tag 含 api/openapi.yaml → 用 tag 内的契约；
#   2. 否则回落到 api/oasdiff-baseline.yaml（v1.1.0-alpha 发布时仓库还没有 api/，
#      该文件是 #31 改造前的最后契约快照，见文件头说明）。
#
# 本地 dry-run（Git Bash / Linux 均可，需要 Go；要覆盖 tag 路径需完整 git 历史与 tag）：
#   bash server/api/tests/run_breaking_check.sh
#   OASDIFF_VERSION=v1.34.0 bash server/api/tests/run_breaking_check.sh
#   LITETALK_OASDIFF_DIR=/tmp/lt-oasdiff bash server/api/tests/run_breaking_check.sh
#
# 产物保留在 LITETALK_OASDIFF_DIR（缺省为临时目录）：openapi-base.yaml 与
# oasdiff-breaking.json，失败时可直接核对完整列表。
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
cd "$root"

oasdiff_version="${OASDIFF_VERSION:-v1.34.0}"
out_dir="${LITETALK_OASDIFF_DIR:-$(mktemp -d)}"
mkdir -p "$out_dir"
# Windows 的 Go/git 是原生程序：把 MSYS 的 /tmp/... 转成 Windows 路径，
# 否则 GOBIN 落到错误的盘符根目录。
if command -v cygpath >/dev/null 2>&1; then
  out_dir="$(cygpath -w "$out_dir")"
fi
json="$out_dir/oasdiff-breaking.json"

if [ "$(git rev-parse --is-shallow-repository 2>/dev/null || echo false)" = "true" ]; then
  echo "::warning::仓库是浅克隆，git describe 可能找不到 release tag；CI 需 fetch-depth: 0"
fi

# tag 推送时 HEAD 就在 tag 上：拿 tag 自己当基线等于空 diff，必须退到前一个 tag，
# 否则发布这次引入的破坏性变更完全绕过了检查。
head_tag="$(git describe --tags --exact-match --match 'v*' HEAD 2>/dev/null || true)"
describe_from="HEAD"
if [ -n "$head_tag" ]; then
  describe_from="${head_tag}^"
  echo "== 当前提交是 release tag ${head_tag}，基线取它的前一个 v* tag"
fi
tag="$(git describe --tags --abbrev=0 --match 'v*' "$describe_from" 2>/dev/null || true)"
if [ -n "$tag" ] && git cat-file -e "${tag}:api/openapi.yaml" 2>/dev/null; then
  git show "${tag}:api/openapi.yaml" > "$out_dir/openapi-base.yaml"
  base="$out_dir/openapi-base.yaml"
  desc="release tag ${tag}"
else
  base="$root/api/oasdiff-baseline.yaml"
  desc="快照 api/oasdiff-baseline.yaml（最近 release tag ${tag:-无} 不含 api/openapi.yaml）"
fi
echo "== oasdiff 基线: $desc"

# OASDIFF_BIN 供离线/已安装环境跳过下载（本地调试用）。
if [ -n "${OASDIFF_BIN:-}" ]; then
  oasdiff="$OASDIFF_BIN"
else
  bin_dir="$out_dir/bin"
  mkdir -p "$bin_dir"
  echo "== 安装 oasdiff ${oasdiff_version}（锁版本，禁止 @latest）"
  GOBIN="$bin_dir" go install "github.com/oasdiff/oasdiff@${oasdiff_version}"
  oasdiff="$bin_dir/oasdiff"
  if [ -x "${oasdiff}.exe" ]; then
    oasdiff="${oasdiff}.exe"
  fi
fi
if [ ! -x "$oasdiff" ]; then
  echo "::error::oasdiff 不可执行: $oasdiff"
  exit 2
fi

# --flatten-allof：把 allOf 分支合并后再比较，避免同一处类型变化被降级成 WARN。
"$oasdiff" breaking "$base" "$root/api/openapi.yaml" --format json --flatten-allof > "$json"
echo "== oasdiff 输出: $json"

cd "$root/server"
go run ./api/tools/breakinggate \
  -changes "$json" \
  -allowlist "$root/api/oasdiff-allowlist.txt" \
  -baseline "$desc"
