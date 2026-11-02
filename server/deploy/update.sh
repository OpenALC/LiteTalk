#!/usr/bin/env bash
# 轻音 LiteTalk 服务端手工/兜底升级入口（#97）。
# 转发到同目录 litetalk-updater --once：与 webhook 走同一条更新流水线，避免两套实现分叉。
# 幂等：同一 tag 重复执行等价于再校验一次；dry-run 不改任何文件。
# 推荐 sudo -u <服务用户> 运行：root 生成的 state/lock 属主会让以服务用户运行的更新器
# 读不到状态、拿不到锁（EACCES）。
# 退出码：0 成功；1 更新失败（已回滚）；2 用法/参数错误；
#         75 已有更新在运行（跨进程锁被 webhook 或另一个 --once 占用）。
set -euo pipefail

usage() {
  cat <<'EOF'
用法: update.sh --tag vX.Y.Z [选项]
  --tag               目标版本（vX.Y.Z[-alpha|-beta|-rc] 或 latest）；必填
                      latest = 通道内 semver 最高的 release；不新于已装版本时只报"已是最新"
  --dry-run           只下载校验解包，不替换二进制、不重启、不写状态
  --no-restart        替换二进制但不重启服务（不推进已发布版本）
  --allow-prerelease  放行 -alpha/-beta/-rc（默认通道只收正式版）
  --timeout           healthz 断言超时秒数（默认 60）
  -h, --help          显示帮助

推荐: sudo -u www /www/wwwroot/LiteTalk/update.sh --tag vX.Y.Z
退出码: 0 成功（含 latest 已是最新）/ 1 更新失败（已回滚）/ 2 参数错误 / 75 已有更新在运行
EOF
}

TAG=""
DRY_RUN=0
NO_RESTART=0
ALLOW_PRERELEASE=0
TIMEOUT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --tag)
      TAG="${2:-}"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --no-restart)
      NO_RESTART=1
      shift
      ;;
    --allow-prerelease)
      ALLOW_PRERELEASE=1
      shift
      ;;
    --timeout)
      TIMEOUT="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "未知参数: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ -z "$TAG" ]; then
  echo "缺少 --tag（如 --tag v1.6.2-alpha 或 --tag latest）" >&2
  exit 2
fi
# tag 字符集闸：除 latest 外只放行版本规范形态，防参数注入
if [ "$TAG" != "latest" ] && ! printf '%s' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc))?$'; then
  echo "tag 不符合版本规范: $TAG" >&2
  exit 2
fi
if [ -n "$TIMEOUT" ] && ! printf '%s' "$TIMEOUT" | grep -Eq '^[0-9]+$'; then
  echo "--timeout 必须是整数秒: $TIMEOUT" >&2
  exit 2
fi

DIR="$(cd "$(dirname "$0")" && pwd)"
UPDATER="$DIR/litetalk-updater"
if [ ! -x "$UPDATER" ]; then
  echo "找不到可执行的更新器：$UPDATER（update.sh 与 litetalk-updater 必须放在同一目录）" >&2
  exit 1
fi

args=(--once --tag "$TAG")
if [ "$DRY_RUN" = 1 ]; then
  args+=(--dry-run)
fi
if [ "$NO_RESTART" = 1 ]; then
  args+=(--no-restart)
fi
if [ "$ALLOW_PRERELEASE" = 1 ]; then
  args+=(--allow-prerelease)
fi
if [ -n "$TIMEOUT" ]; then
  args+=(--timeout "${TIMEOUT}s")
fi

exec "$UPDATER" "${args[@]}"
