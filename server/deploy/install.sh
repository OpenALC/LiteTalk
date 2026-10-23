#!/usr/bin/env bash
# 轻音 LiteTalk 服务端一键安装（#97）。幂等：重复执行只补齐缺失项，不覆盖已有配置。
# 下载 GitHub Release 资产（含校验和验证）→ 安装二进制与升级脚本 → 生成 env 模板
#（不覆盖已有）→ 安装 systemd 单元 → enable --now。任何环节失败即停，不留半成品。
# dry-run 只打印动作，不改任何文件。
set -euo pipefail

REPO="OpenALC/LiteTalk"
DIR="/www/wwwroot/LiteTalk"
SVC_USER="www"
VERSION="latest"
ARCH=""
WITH_UPDATER=1
ALLOW_PRERELEASE=0
DRY_RUN=0

usage() {
  cat <<'EOF'
用法: install.sh [选项]
  --dir DIR          安装目录（默认 /www/wwwroot/LiteTalk）
  --user USER        服务用户（默认 www；不存在则创建系统用户）
  --version VER      版本 tag 或 latest（默认 latest）
  --arch ARCH        amd64 / arm64（默认自动探测）
  --no-updater       不安装自动更新器单元（只装主服务）
  --prerelease       允许安装 -alpha/-beta/-rc 预发布（默认只装正式版）
  --dry-run          只打印动作，不改文件
  -h, --help         显示帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR="${2:-}"; shift 2 ;;
    --user) SVC_USER="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --arch) ARCH="${2:-}"; shift 2 ;;
    --no-updater) WITH_UPDATER=0; shift ;;
    --prerelease) ALLOW_PRERELEASE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage >&2; exit 2 ;;
  esac
done

log() { echo "[install] $*"; }

# ---- 参数与环境校验（fail-closed：形态不合法直接退出）----
case "$DIR" in
  /*) ;;
  *) echo "安装目录必须是绝对路径: $DIR" >&2; exit 2 ;;
esac
if printf '%s' "$DIR" | grep -q '\.\.'; then
  echo "安装目录不得包含 ..: $DIR" >&2; exit 2
fi
if ! printf '%s' "$SVC_USER" | grep -Eq '^[A-Za-z_][A-Za-z0-9_-]*$'; then
  echo "用户名形态非法: $SVC_USER" >&2; exit 2
fi
if [ "$(uname -s)" != "Linux" ]; then
  echo "仅支持 Linux（当前 $(uname -s)）" >&2; exit 1
fi
if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "无法识别架构 $(uname -m)，请用 --arch 指定" >&2; exit 1 ;;
  esac
fi
if [ "$ARCH" != "amd64" ] && [ "$ARCH" != "arm64" ]; then
  echo "架构只支持 amd64/arm64: $ARCH" >&2; exit 2
fi
if [ "$EUID" -ne 0 ] && [ "$DRY_RUN" = 0 ]; then
  echo "需要 root（sudo bash install.sh ...）；--dry-run 可不带 root" >&2; exit 1
fi

TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc))?$'
if [ "$VERSION" != "latest" ] && ! printf '%s' "$VERSION" | grep -Eq "$TAG_RE"; then
  echo "版本 tag 不符合规范: $VERSION" >&2; exit 2
fi

# ---- 解析 latest：用 releases 列表（releases/latest 不含预发布），按通道策略过滤 ----
if [ "$VERSION" = "latest" ]; then
  log "解析最新发布（通道: $([ "$ALLOW_PRERELEASE" = 1 ] && echo '含预发布' || echo '仅正式版')）"
  tags="$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=20" \
    | grep -o '"tag_name": *"[^"]*"' | sed 's/.*"\(v[^"]*\)"/\1/' || true)"
  VERSION=""
  for t in $tags; do
    printf '%s' "$t" | grep -Eq "$TAG_RE" || continue
    if [ "$ALLOW_PRERELEASE" != 1 ] && printf '%s' "$t" | grep -q '-'; then
      continue
    fi
    VERSION="$t"
    break
  done
  if [ -z "$VERSION" ]; then
    echo "没有符合通道策略的 release（预发布需加 --prerelease）" >&2
    exit 1
  fi
fi
# 显式指定的预发布同样要 --prerelease 放行（与更新器通道闸一致）
if [ "$ALLOW_PRERELEASE" != 1 ] && printf '%s' "$VERSION" | grep -q '-'; then
  echo "预发布 $VERSION 需要 --prerelease 显式放行" >&2
  exit 2
fi
TAG="$VERSION"
ASSET="litetalk-server-linux-$ARCH.tar.gz"
log "目标版本 $TAG（$ASSET）"

if [ "$DRY_RUN" = 1 ]; then
  log "dry-run：以下动作不会执行"
fi

run() {
  if [ "$DRY_RUN" = 1 ]; then
    echo "  将执行: $*"
  else
    "$@"
  fi
}

# ---- 下载并校验 ----
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
BASE="https://github.com/$REPO/releases/download/$TAG"
log "下载 $ASSET 与 SHA256SUMS.txt"
if [ "$DRY_RUN" = 1 ]; then
  echo "  将下载: $BASE/$ASSET"
  echo "  将下载: $BASE/SHA256SUMS.txt"
else
  curl -fsSL -o "$WORK/$ASSET" "$BASE/$ASSET"
  curl -fsSL -o "$WORK/SHA256SUMS.txt" "$BASE/SHA256SUMS.txt"
  want="$(grep -E " \*?${ASSET}\$" "$WORK/SHA256SUMS.txt" | awk '{print $1}' | head -1)"
  if [ -z "$want" ]; then
    echo "SHA256SUMS.txt 缺少 $ASSET 的校验行" >&2
    exit 1
  fi
  got="$(sha256sum "$WORK/$ASSET" | awk '{print $1}')"
  if [ "$got" != "$want" ]; then
    echo "SHA256 校验失败：下载 $got，清单 $want" >&2
    exit 1
  fi
  log "SHA256 校验通过 $got"
fi

# ---- 安全解包：成员白名单 + 拒绝路径穿越/符号链接 ----
if [ "$DRY_RUN" = 0 ]; then
  mkdir -p "$WORK/x"
  members="$(tar -tzf "$WORK/$ASSET")"
  while IFS= read -r m; do
    case "$m" in
      litetalk-server|litetalk-updater|update.sh) ;;
      *) echo "压缩包含非法成员: $m" >&2; exit 1 ;;
    esac
    if printf '%s' "$m" | grep -Eq '(^/|\.\.|->)'; then
      echo "压缩包成员路径非法: $m" >&2
      exit 1
    fi
  done <<<"$members"
  if ! printf '%s\n' "$members" | grep -qx 'litetalk-server'; then
    echo "压缩包缺少 litetalk-server" >&2
    exit 1
  fi
  tar -xzf "$WORK/$ASSET" -C "$WORK/x"
  if [ ! -f "$WORK/x/litetalk-server" ] || [ ! -x "$WORK/x/litetalk-server" ]; then
    echo "解包后 litetalk-server 缺失或不可执行" >&2
    exit 1
  fi
fi

# ---- 创建服务用户 / 目录 ----
if id -u "$SVC_USER" >/dev/null 2>&1; then
  log "服务用户 $SVC_USER 已存在"
else
  run useradd -r -s /sbin/nologin "$SVC_USER"
fi
run mkdir -p "$DIR"
run chown "$SVC_USER:$SVC_USER" "$DIR"
run chmod 755 "$DIR"

# ---- 安装二进制（root 所有、0755；目录属主是服务用户，更新器靠目录写权限原子替换）----
if [ "$DRY_RUN" = 1 ]; then
  echo "  将安装: $WORK/x/{litetalk-server,litetalk-updater,update.sh} -> $DIR/"
else
  install -o root -g root -m 0755 "$WORK/x/litetalk-server" "$DIR/litetalk-server"
  if [ -f "$WORK/x/litetalk-updater" ]; then
    install -o root -g root -m 0755 "$WORK/x/litetalk-updater" "$DIR/litetalk-updater"
  fi
  if [ -f "$WORK/x/update.sh" ]; then
    install -o root -g root -m 0755 "$WORK/x/update.sh" "$DIR/update.sh"
  fi
  log "二进制已安装到 $DIR"
fi

# ---- env 模板（不覆盖已有）----

if [ "$DRY_RUN" = 1 ]; then
  echo "  将生成（不覆盖已有）: /etc/litetalk.env"
else
  if [ ! -e /etc/litetalk.env ]; then
    cat > /etc/litetalk.env <<EOF
# 轻音 LiteTalk 服务端环境变量（install.sh 生成，按需修改）
# 完整变量说明见仓库 server/deploy/env/litetalk.env.example
LITETALK_DATA_DIR=$DIR/data
# LITETALK_DOMAIN=voice.example.com   # 必改：内置 ACME 用（不配则纯 HTTP）
EOF
    chmod 600 /etc/litetalk.env
    log "已生成 /etc/litetalk.env（未设置域名前为纯 HTTP，见 README）"
  else
    log "保留已有 /etc/litetalk.env"
  fi
fi

if [ "$WITH_UPDATER" = 1 ]; then
  if [ "$DRY_RUN" = 1 ]; then
    echo "  将生成（不覆盖已有）: /etc/litetalk-updater.env（LISTEN=:8088 明文 HTTP）、webhook secret"
  else
    if [ ! -e /etc/litetalk-updater.env ]; then
      cat > /etc/litetalk-updater.env <<EOF
# 轻音 LiteTalk 自动更新器环境变量（install.sh 生成，按需修改）
# 完整说明见 server/deploy/env/litetalk-updater.env.example
LITETALK_UPDATER_ENABLED=1
LITETALK_UPDATER_ALLOW_PRERELEASE=$ALLOW_PRERELEASE
# 8088 明文 HTTP：公网只需放行 webhook；/status 仅环回（SSH 隧道）
LITETALK_UPDATER_LISTEN=:8088
LITETALK_UPDATER_INSTALL_DIR=$DIR
LITETALK_UPDATER_STATE_DIR=/var/lib/litetalk-updater
EOF
      chmod 600 /etc/litetalk-updater.env
      log "已生成 /etc/litetalk-updater.env（secret 不写入本文件）"
    else
      log "保留已有 /etc/litetalk-updater.env"
      # 存量 env 只读检查：旧模板的 TLS 变量在 8088 模式下无效，LISTEN=:443 会被拒绝启动。
      # 生产文件不自动改（安装脚本必须幂等且不覆盖既有配置），只提示人工合并。
      stale_env=0
      if grep -Eq '^[[:space:]]*(export[[:space:]]+)?LITETALK_UPDATER_(DOMAIN|NO_TLS|TLS_CACHE)=' /etc/litetalk-updater.env; then
        log "警告: 仍有 LITETALK_UPDATER_DOMAIN/NO_TLS/TLS_CACHE，8088 模式下会被忽略"
        stale_env=1
      fi
      if grep -Eq '^[[:space:]]*(export[[:space:]]+)?LITETALK_UPDATER_LISTEN=:443([[:space:]]|$)' /etc/litetalk-updater.env; then
        log "警告: LITETALK_UPDATER_LISTEN=:443 会被拒绝启动（443 归主服务内置 ACME）"
        stale_env=1
      fi
      if [ "$stale_env" = 1 ]; then
        log "      编辑建议: 删除上述变量，改为 LITETALK_UPDATER_LISTEN=:8088，然后 systemctl restart litetalk-updater"
      fi
    fi
    # webhook secret：缺失才生成；无论新旧都把属主/权限校正到 root:$SVC_USER 0640
    #（旧版误配 0600 root:root 时更新器读不到，重跑安装即修复）。
    # 更新器以 $SVC_USER 运行且自己读本文件：other 位必须为 0，服务用户必须可读。
    if [ ! -e /etc/litetalk-updater.webhook.secret ]; then
      (umask 077; openssl rand -hex 32 > /etc/litetalk-updater.webhook.secret)
      log "已生成 webhook secret（/etc/litetalk-updater.webhook.secret，不回显）"
      log "配置 GitHub webhook 时用: sudo cat /etc/litetalk-updater.webhook.secret"
    else
      log "保留已有 /etc/litetalk-updater.webhook.secret（校正属主/权限）"
    fi
    chown "root:$SVC_USER" /etc/litetalk-updater.webhook.secret
    chmod 640 /etc/litetalk-updater.webhook.secret
    mkdir -p /var/lib/litetalk-updater
    # -R 顺带修正历史遗留的 root 属主（曾用 root 跑过 update.sh 会让 www 读不到状态/拿不到锁）
    chown -R "$SVC_USER:$SVC_USER" /var/lib/litetalk-updater
    chmod 700 /var/lib/litetalk-updater
  fi
fi

# ---- systemd 单元（路径/用户按本次安装参数渲染）----
# 不覆盖已有文件：已存在时把新模板写到 .new 供对比合并（.new 不会被 systemd/sudo 读取）。
# 更新器经 sudo 重启主服务，其单元不能开 NoNewPrivileges（与 setuid 提权冲突）；
# 主服务不用 sudo，保留 NoNewPrivileges=true。
write_conf() {
  local path="$1"
  if [ -e "$path" ]; then
    log "保留已有 $path（新模板另存 $path.new，请自行对比合并）"
    cat > "$path.new"
  else
    cat > "$path"
    log "已写入 $path"
  fi
}

if [ "$DRY_RUN" = 1 ]; then
  echo "  将安装（不覆盖已有）: /etc/systemd/system/litetalk.service（User=$SVC_USER Dir=$DIR，含 443 capability）"
  if [ "$WITH_UPDATER" = 1 ]; then
    echo "  将安装（不覆盖已有）: /etc/systemd/system/litetalk-updater.service、/etc/sudoers.d/litetalk-updater"
    echo "  更新器监听 :8088 明文 HTTP：公网只需放行 TCP 8088，/status 仅环回"
    echo "  将执行: systemctl daemon-reload && systemctl enable --now litetalk.service litetalk-updater.service"
  else
    echo "  将执行: systemctl daemon-reload && systemctl enable --now litetalk.service"
  fi
  log "dry-run 结束（未做任何修改）"
  exit 0
fi

write_conf /etc/systemd/system/litetalk.service <<EOF
[Unit]
Description=LiteTalk Server (signaling + SFU + REST)
After=network.target
# 启动失败限流必须写在 [Unit]：写在 [Service] 会被 systemd 忽略，坏配置会无限重启。
StartLimitIntervalSec=60
StartLimitBurst=10

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
WorkingDirectory=$DIR
EnvironmentFile=/etc/litetalk.env
ExecStart=$DIR/litetalk-server
Restart=always
RestartSec=3
StandardOutput=journal
StandardError=journal
# 内置 ACME（LITETALK_DOMAIN）监听 443 是特权端口：非 root 必须放行绑定 capability
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

if [ "$WITH_UPDATER" = 1 ]; then
  write_conf /etc/systemd/system/litetalk-updater.service <<EOF
[Unit]
Description=LiteTalk Updater (GitHub release webhook auto-update)
After=network-online.target litetalk.service
Wants=network-online.target
# 8088 被占用时进程会秒退：限流，避免 Restart=on-failure 打成重启风暴。
# 这两项必须写在 [Unit]，写在 [Service] 会被 systemd 忽略。
StartLimitIntervalSec=60
StartLimitBurst=10

[Service]
Type=simple
# 与主服务同用户（安装目录属主），不给 root 常驻
User=$SVC_USER
Group=$SVC_USER
WorkingDirectory=$DIR
EnvironmentFile=/etc/litetalk-updater.env
ExecStart=$DIR/litetalk-updater

# 8088 是普通端口，不需要 CAP_NET_BIND_SERVICE（443 归主服务内置 ACME）。
# 不要加 NoNewPrivileges=true：它会禁掉 setuid 提权，sudo -n systemctl restart
#（更新器重启主服务的唯一通道）将全部失败。最小权限由下面的 sudoers 保证：
# $SVC_USER 只能 restart/is-active litetalk.service（见 deploy/sudoers/litetalk-updater）。

# 基础加固：系统树只读，只放行安装目录与状态目录写入
NoNewPrivileges=false
ProtectSystem=full
ReadWritePaths=$DIR /var/lib/litetalk-updater
PrivateTmp=true

Restart=on-failure
RestartSec=5

# 日志走 journald：journalctl -u litetalk-updater -f
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

  # 存量 unit 只读检查：旧模板带 443 capability，8088 用不到且会误导排障。
  # 不自动覆盖（上面已另存 .new），只提示人工合并。先滤掉注释行：
  # 新模板的注释本身会提到 CAP_NET_BIND_SERVICE，不能当作残留。
  if grep -Ev '^[[:space:]]*(#|$)' /etc/systemd/system/litetalk-updater.service \
     | grep -Eq 'CAP_NET_BIND_SERVICE|:443'; then
    log "警告: 现有 /etc/systemd/system/litetalk-updater.service 仍含 CAP_NET_BIND_SERVICE 或 :443"
    log "      8088 是普通端口，不需要 capability；请对比同目录 .new 后手工合并并 daemon-reload"
  fi

  if [ -e /etc/sudoers.d/litetalk-updater ]; then
    log "保留已有 /etc/sudoers.d/litetalk-updater"
  else
    cat > /etc/sudoers.d/litetalk-updater <<EOF
# 轻音 LiteTalk 自动更新器最小 sudoers（#97）
$SVC_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart litetalk.service, /usr/bin/systemctl is-active litetalk.service
EOF
    chmod 440 /etc/sudoers.d/litetalk-updater
    if ! visudo -cf /etc/sudoers.d/litetalk-updater >/dev/null 2>&1; then
      echo "sudoers 校验失败，已回滚" >&2
      rm -f /etc/sudoers.d/litetalk-updater
      exit 1
    fi
    log "已写入 /etc/sudoers.d/litetalk-updater"
  fi
fi

systemctl daemon-reload
systemctl enable --now litetalk.service
if [ "$WITH_UPDATER" = 1 ]; then
  systemctl enable --now litetalk-updater.service
fi

log "安装完成。"
log "验证: curl -fsS http://127.0.0.1:8077/api/v1/healthz"
if [ "$WITH_UPDATER" = 1 ]; then
  log "自动更新接入："
  log "  1) 安全组/防火墙放行 TCP 8088（无需 DNS/证书，443 仍归主服务）"
  log "  2) GitHub 仓库 Settings → Webhooks 指向 http://<服务器IP>:8088/hooks/github-release（content_type=json）"
  log "  3) /status 只允许环回：ssh -N -L 127.0.0.1:8088:127.0.0.1:8088 <用户>@<服务器>"
  log "详见 server/deploy/README.md「自动更新」"
fi
