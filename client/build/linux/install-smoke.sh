#!/usr/bin/env bash
# 四发行版容器内的安装 + 启动冒烟（#5 W3）。由 ci.yml 的 smoke-client-linux-install
# 矩阵 job 调用；容器里没有本仓库，脚本与安装包以只读卷挂进 /smoke。
#
# 口径与 check-client-linux 的「打 deb 并安装 + 启动冒烟」一致：
#   1) 用该发行版自己的包管理器安装 deb/rpm——nfpm 声明的 Depends 必须能被该
#      发行版的仓库解析（包名写错、缺额外 repo 在这里直接失败，这是 G2 的数据）；
#   2) dbus 会话 + 虚拟显示下限时启动 10 秒，超时存活且输出启动日志才算通过。
#
# 环境变量：PKG_TYPE=deb|rpm（由矩阵 job 注入）。
# 依赖闭包之外的东西（虚拟显示、dbus、字体、软渲染驱动）不是包依赖的一部分：
# 真实桌面必有、最小容器没有，属冒烟环境补齐，下面逐项注明。
set -euo pipefail

PKG_DIR=/smoke/pkgs
LOG=/tmp/litetalk-smoke.log

# shellcheck disable=SC1091
. /etc/os-release
echo "== 安装冒烟：${PRETTY_NAME:-未知发行版}（PKG_TYPE=${PKG_TYPE:?必须由矩阵 job 注入 deb 或 rpm}）=="

case "$PKG_TYPE" in
  deb)
    pkg=$(ls "$PKG_DIR"/litetalk_*.deb 2>/dev/null || true)
    [ -f "$pkg" ] || { echo "ERROR: 容器内未找到 deb 安装包：$PKG_DIR/litetalk_*.deb"; exit 1; }
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    # 冒烟环境补齐：xvfb / dbus 供启动冒烟（dbus-run-session 属 dbus-daemon 包），
    # fonts-dejavu-core 避免无字体环境下的假失败。
    apt-get install -y xvfb xauth dbus-daemon fonts-dejavu-core
    echo "== 依赖闭包：apt 从 $ID 仓库解析安装 $pkg =="
    # 直接装本地 deb：Depends 逐项从本发行版仓库解析。声明里有该发行版不存在的
    # 包名（或缺 repo）时这一步失败——正是矩阵要暴露的闭包问题，不放宽。
    apt-get install -y "$pkg"
    echo "== 实际安装的 WebKitGTK / GTK4 =="
    dpkg -l | grep -Ei 'webkit|libgtk-4' || true
    launch_wrapper=(xvfb-run -a)
    ;;
  rpm)
    pkg=$(ls "$PKG_DIR"/litetalk_*.rpm 2>/dev/null || true)
    [ -f "$pkg" ] || { echo "ERROR: 容器内未找到 rpm 安装包：$PKG_DIR/litetalk_*.rpm"; exit 1; }
    case "$ID" in
      fedora)
        if ! dnf -q makecache --refresh >/dev/null 2>&1; then
          # Fedora 40 已 EOL，官方镜像只保留在 archives；切过去后依赖闭包仍然
          # 只对 F40 自己的包集判定，不改判定标准。
          echo "Fedora $VERSION_ID 官方镜像不可达，切换到 archives.fedoraproject.org（该版本已 EOL 归档）"
          sed -i \
            -e 's|^metalink=|# metalink=|' \
            -e 's|^#baseurl=http://download.example/pub/fedora/linux|baseurl=https://archives.fedoraproject.org/pub/archive/fedora/linux|' \
            /etc/yum.repos.d/fedora.repo /etc/yum.repos.d/fedora-updates.repo
          dnf makecache --refresh
        fi
        # 冒烟环境补齐：xvfb-run 在 xorg-x11-server-Xvfb 包里（不在 Xorg 主包），
        # 加 dbus、基础字体与软渲染驱动（真实桌面必有，最小容器没有）。
        dnf install -y xorg-x11-server-Xvfb dbus-daemon dejavu-sans-fonts mesa-dri-drivers
        launch_wrapper=(xvfb-run -a)
        ;;
      rocky|almalinux|rhel|ol)
        # RHEL 10 系：WebKitGTK 6.0 与 Weston 均由 EPEL 10 提供。
        # Xvfb 不在该发行版的正式仓库；Weston headless 可直接提供 Wayland
        # 会话，避免依赖开发仓库或 xwayland-run 的嵌套显示栈。
        echo "启用 EPEL 10（webkitgtk6.0 / weston 的来源）"
        dnf install -y epel-release
        dnf install -y weston dbus-daemon dejavu-sans-fonts mesa-dri-drivers
        launch_wrapper=()
        launch_backend=wayland
        ;;
      *)
        echo "ERROR: 未覆盖的 rpm 发行版 $ID，请在脚本 case 中补充"
        exit 1
        ;;
    esac
    echo "== 依赖闭包：dnf 从 $ID 仓库解析安装 $pkg =="
    dnf install -y "$pkg"
    echo "== 实际安装的 WebKitGTK / GTK4 =="
    rpm -qa | grep -Ei 'webkit|gtk4' | sort || true
    ;;
  *)
    echo "ERROR: PKG_TYPE 只能是 deb 或 rpm，实际为 $PKG_TYPE"
    exit 1
    ;;
esac

# 启动冒烟：与 check-client-linux 同口径。WEBKIT_* / LIBGL_* 同宿主 job：
# 无 GPU 的虚拟显示下关闭 WebKit 合成/DMABUF，避免软件渲染初始化失败；
# bwrap 沙箱在无头 CI（无特权 user namespace）上建立失败会直接崩掉进程
# （bwrap: setting up uid map: Permission denied → dbus-proxy 失败 → g_error），
# 冒烟只验证安装闭包与启动存活，沙箱由用户真实桌面环境提供。
export WEBKIT_DISABLE_COMPOSITING_MODE=1
export WEBKIT_DISABLE_DMABUF_RENDERER=1
export LIBGL_ALWAYS_SOFTWARE=1
export WEBKIT_FORCE_SANDBOX=0
export WEBKIT_DISABLE_SANDBOX_THIS_IS_DANGEROUS=1
if [ "${launch_backend:-x11}" = wayland ]; then
  export XDG_RUNTIME_DIR=$(mktemp -d)
  chmod 700 "$XDG_RUNTIME_DIR"
  export WAYLAND_DISPLAY=wayland-litetalk-smoke
  export GDK_BACKEND=wayland
  weston --backend=headless --renderer=pixman --no-config \
    --socket="$WAYLAND_DISPLAY" --width=1280 --height=720 \
    --log=/tmp/litetalk-weston.log &
  weston_pid=$!
  trap 'kill "$weston_pid" 2>/dev/null || true; rm -rf "$XDG_RUNTIME_DIR"' EXIT
  for _ in $(seq 1 50); do
    [ -S "$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY" ] && break
    if ! kill -0 "$weston_pid" 2>/dev/null; then
      cat /tmp/litetalk-weston.log
      echo "ERROR: Weston headless 启动失败"
      exit 1
    fi
    sleep 0.1
  done
  [ -S "$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY" ] || {
    cat /tmp/litetalk-weston.log
    echo "ERROR: Weston headless 未创建 Wayland socket"
    exit 1
  }
else
  # xvfb-run 仅提供 X11；显式指定避免宿主的 WAYLAND_DISPLAY 干扰选择。
  export GDK_BACKEND=x11
fi

set +e
"${launch_wrapper[@]}" dbus-run-session -- timeout 10 litetalk > "$LOG" 2>&1
code=$?
set -e

echo "== 启动日志（退出码 $code，期望 124 = 限时存活）=="
cat "$LOG"
# exit 124 = timeout 到点杀进程，说明这 10 秒内 litetalk 一直存活；0 / 1 / 139
# 等一律失败。依据与宿主冒烟相同：应用启动后进入事件循环并驻留，不会自行结束。
if [ "$code" -ne 124 ]; then
  echo "ERROR: 启动冒烟失败：litetalk 退出码 $code（期望 124：限时内持续运行）"
  exit 1
fi
# 124 只证明进程没退出，再确认进入了 ServiceStartup 的启动日志，排除卡在
# GTK/WebKit 初始化之前的假阳性。
if ! grep -q "启动 (pid=" "$LOG"; then
  echo "ERROR: 启动冒烟日志缺少启动标记，进程未进入服务启动"
  exit 1
fi
echo "安装冒烟通过：${PRETTY_NAME:-$ID} 上安装包依赖解析成功且 litetalk 限时启动存活"
