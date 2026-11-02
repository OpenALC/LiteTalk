#!/usr/bin/env bash
# measure-window.sh — LiteTalk 窗口生命周期测量（GTK3/GTK4 基线，任务卡 W0-1 / #5）
#
# 对指定 LiteTalk 客户端二进制做可复跑的窗口生命周期与资源测量，输出机器可读 TSV
# （stdout 与 --out 文件，二者内容一致）。测量在隔离环境进行：私有 D-Bus 会话总线、
# 私有 XDG 配置/缓存/数据/运行目录、占位 StatusNotifierWatcher（WSLg 等无托盘
# 宿主的会话需要它才能走「关窗=隐藏/销毁」的生产语义，见 app.go closeActionFor）。
#
# 触发方式：X11 下通过 XTest 点击前端自定义标题栏的关闭按钮（42px 宽、窗口右上角，
# 见 client/frontend/src/components/TitleBar.vue），走生产路径 WindowClose →
# interceptClose → Hide/destroyRenderProcess；唤醒经单实例二次启动（main.go
# OnSecondInstanceLaunch → ShowMainWindow → showFromTray），与托盘点击同代码路径，
# 仅多付一次进程启动开销（见 wake-calibration 套件的分解数据）。
#
# 用法：
#   measure-window.sh --binary PATH [--runs N] [--mode x11|wayland]
#                     [--suite all|cold|lifecycle|cpu|rss|render|leak]
#                     [--trigger close|min] [--cpu-seconds N] [--leak-cycles N]
#                     [--out FILE] [--workdir DIR]
#
# 套件说明：
#   cold       冷启动（2 轮预热不计入；window_mapped = 启动到窗口可见）
#   lifecycle  off/hibernate 的收起与唤醒 + 旧 destroy 请求（GTK4 验证强降，GTK3 实测销毁）
#   cpu        空闲 CPU（进程树，默认 60s）
#   rss        off/hibernate/旧 destroy 请求三档常驻内存
#   render     WEBKIT_DISABLE_DMABUF_RENDERER=1 软件渲染下的 cold + cpu
#   leak       旧 destroy 请求 20 轮 RSS 与 WebKit 进程趋势；GTK4 验证降级
#   all        以上全部
#
# --trigger 触发按钮（前端自定义标题栏，见 client/frontend/src/components/TitleBar.vue）：
#   close  关闭按钮（生产路径 WindowClose → interceptClose → Hide/destroy）
#   min    最小化按钮（直接 win.Minimise；destroy 模式经 WindowMinimise 事件销毁）
# 占位 watcher 只有总线名字，不提供真实托盘交互；close 按钮须先探测窗口是否真的隐藏。
# WSLg 的 minimise 可能是空操作，失败会明确写 FAIL，不把它计为有效周期。
#
# 依赖：xdotool xwininfo dbus-daemon busctl python3(python3-dbus)。
# 输出：TSV（suite、iteration、metric、value、unit），失败行 value=FAIL，
#       以 # 开头的行为元信息。退出码 0=套件结束且无失败，1=环境或测量失败。
set -u
umask 077

# ---------- 参数 ----------
BINARY=""
RUNS=30
MODE="x11"
SUITE="all"
TRIGGER="close"
CPU_SECONDS=60
LEAK_CYCLES=20
OUT=""
WORKDIR=""

while [ $# -gt 0 ]; do
  case "$1" in
    --binary|--runs|--mode|--suite|--trigger|--cpu-seconds|--leak-cycles|--out|--workdir)
      [ $# -ge 2 ] || { echo "参数缺少值: $1" >&2; exit 1; } ;;
  esac
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --runs) RUNS="$2"; shift 2 ;;
    --mode) MODE="$2"; shift 2 ;;
    --suite) SUITE="$2"; shift 2 ;;
    --trigger) TRIGGER="$2"; shift 2 ;;
    --cpu-seconds) CPU_SECONDS="$2"; shift 2 ;;
    --leak-cycles) LEAK_CYCLES="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --workdir) WORKDIR="$2"; shift 2 ;;
    -h|--help) sed -n '2,48p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

[ -n "$BINARY" ] || { echo "缺少 --binary" >&2; exit 1; }
[ -x "$BINARY" ] || { echo "二进制不可执行: $BINARY" >&2; exit 1; }
case "$MODE" in x11|wayland) ;; *) echo "--mode 只支持 x11|wayland" >&2; exit 1 ;; esac
case "$SUITE" in all|cold|lifecycle|cpu|rss|render|leak) ;; *) echo "--suite 非法" >&2; exit 1 ;; esac
case "$TRIGGER" in close|min) ;; *) echo "--trigger 只支持 close|min" >&2; exit 1 ;; esac
for v in RUNS CPU_SECONDS LEAK_CYCLES; do
  value=${!v}
  case "$value" in ''|*[!0-9]*) echo "$v 必须是正整数" >&2; exit 1 ;; esac
  [ "$value" -ge 1 ] && [ "$value" -le 3600 ] || { echo "$v 超出 1–3600" >&2; exit 1; }
done
[ "$MODE" != wayland ] || [ "$SUITE" = all ] || { echo "Wayland 仅支持 --suite all（进程级粗测）" >&2; exit 1; }
[ -z "$OUT" ] || { [ ! -e "$OUT" ] && [ ! -L "$OUT" ]; } || { echo "--out 已存在，拒绝追加或覆盖: $OUT" >&2; exit 1; }
[ -z "$WORKDIR" ] || { [ ! -e "$WORKDIR" ] && [ ! -L "$WORKDIR" ]; } || { echo "--workdir 已存在，拒绝复用: $WORKDIR" >&2; exit 1; }

for t in xdotool xwininfo dbus-daemon busctl python3; do
  command -v "$t" >/dev/null 2>&1 || { echo "缺少依赖: $t" >&2; exit 1; }
done
python3 -c 'import dbus' 2>/dev/null || { echo "缺少 python3-dbus" >&2; exit 1; }
python3 -c 'import os, signal; assert hasattr(os, "pidfd_open") and hasattr(signal, "pidfd_send_signal")' 2>/dev/null || {
  echo "缺少 pidfd 安全清理能力" >&2; exit 1;
}

# ---------- 环境 ----------
BIN_ABS=$(readlink -f "$BINARY")
if [ -n "$WORKDIR" ]; then
  mkdir -- "$WORKDIR" || exit 1
else
  WORKDIR=$(mktemp -d /tmp/ltmeasure-XXXXXXXX) || exit 1
fi
mkdir -p "$WORKDIR/config/LiteTalk" "$WORKDIR/cache" "$WORKDIR/data" "$WORKDIR/state" "$WORKDIR/runtime" "$WORKDIR/dbus"
export XDG_CONFIG_HOME="$WORKDIR/config"
export XDG_CACHE_HOME="$WORKDIR/cache"
export XDG_DATA_HOME="$WORKDIR/data"
export XDG_STATE_HOME="$WORKDIR/state"
export XDG_RUNTIME_DIR="$WORKDIR/runtime"
chmod 700 "$XDG_RUNTIME_DIR"
if [ "$MODE" = wayland ]; then
  export WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-/mnt/wslg/runtime-dir/wayland-0}"
  case "$WAYLAND_DISPLAY" in
    /*) ;;
    *) WAYLAND_DISPLAY="/mnt/wslg/runtime-dir/$WAYLAND_DISPLAY"; export WAYLAND_DISPLAY ;;
  esac
fi
DBUS_ADDR="unix:path=$WORKDIR/dbus/session-bus"
export DBUS_SESSION_BUS_ADDRESS="$DBUS_ADDR"
RUN_ID=$(python3 -c 'import uuid; print(uuid.uuid4().hex)')
APP_TAG="LITETALK_MEASURE_APP=$RUN_ID"
INFRA_TAG="LITETALK_MEASURE_INFRA=$RUN_ID"
SCRIPT_MAIN=""
SECOND_PID=""
SCRIPT_LOG=""
WATCHER_PID=""
DBUS_PID=""
BASELINE_WEBPROC=0

# 通过每次启动独有的环境标记和 pidfd 发信号；即使 PID 被复用，也不会误杀别的进程。
signal_tagged() { # $1=APP_TAG/INFRA_TAG $2=TERM/KILL
  python3 - "$1" "$2" <<'PY'
import os, signal, sys
tag = sys.argv[1].encode()
sig = getattr(signal, 'SIG' + sys.argv[2])
for name in os.listdir('/proc'):
    if not name.isdigit():
        continue
    fd = None
    try:
        pid = int(name)
        fd = os.pidfd_open(pid)
        with open(f'/proc/{pid}/environ', 'rb') as stream:
            if tag not in stream.read().split(b'\0'):
                continue
        signal.pidfd_send_signal(fd, sig)
    except (OSError, ProcessLookupError, PermissionError):
        pass
    finally:
        if fd is not None:
            os.close(fd)
PY
}

cleanup() {
  signal_tagged "$APP_TAG" TERM
  sleep 0.2
  signal_tagged "$APP_TAG" KILL
  signal_tagged "$INFRA_TAG" TERM
  sleep 0.1
  signal_tagged "$INFRA_TAG" KILL
}
trap cleanup EXIT

# ---------- 度量辅助 ----------

tree_pids() { # $1=根pid → 进程树全部 pid（含根）
  local root=$1 c
  echo "$root"
  for c in $(pgrep -P "$root" 2>/dev/null); do tree_pids "$c"; done
}

CLK_TCK=$(getconf CLK_TCK)
PAGE_SIZE=$(getconf PAGESIZE)
now_ns() { date +%s%N; }
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
el_ms() { echo $(( ($2 - $1) / 1000000 )); }

tree_cpu_ticks() { # $1=根pid → utime+stime 总滴答
  local p t sum=0
  for p in $(tree_pids "$1"); do
    t=$(awk '{print $14+$15}' "/proc/$p/stat" 2>/dev/null) && sum=$((sum + t))
  done
  echo "$sum"
}

tree_rss_kib() { # $1=根pid → 常驻内存合计 KiB
  local p r sum=0
  for p in $(tree_pids "$1"); do
    r=$(awk '{print $2}' "/proc/$p/statm" 2>/dev/null) && sum=$((sum + r))
  done
  echo $((sum * PAGE_SIZE / 1024))
}

count_comm() { # $1=进程名（argv0 基名精确匹配）。只统计本轮启动的进程。
  # /proc/comm 截断（WebKitWebProcess=16 字节），pgrep -x 恒 0
  python3 -c '
import sys, os
want = sys.argv[1]; tag = sys.argv[2].encode(); n = 0
for pid in os.listdir("/proc"):
    if not pid.isdigit():
        continue
    try:
        with open("/proc/%s/cmdline" % pid, "rb") as f:
            argv0 = f.read().split(b"\0")[0].decode(errors="replace")
        with open("/proc/%s/environ" % pid, "rb") as f:
            owned = tag in f.read().split(b"\0")
        if owned and os.path.basename(argv0) == want:
            n += 1
    except OSError:
        pass
print(n)' "$1" "$APP_TAG"
}

webkit_counts() { echo "Web=$(count_comm WebKitWebProcess) Net=$(count_comm WebKitNetworkProcess)"; }

# ---------- 元信息 ----------
GTK_VER=$(ldd "$BIN_ABS" 2>/dev/null | grep -oE 'libgtk-[34]' | head -1 | grep -oE '[34]')
WEBKIT_VER=$(ldd "$BIN_ABS" 2>/dev/null | grep -oE 'libwebkit2gtk-4\.[01]|libwebkitgtk-6\.0' | head -1)
[ -n "$GTK_VER" ] || GTK_VER="unknown"
case "$GTK_VER" in 3|4) ;; *) echo "无法识别二进制 GTK 依赖" >&2; exit 1 ;; esac

# ---------- TSV 输出（stdout 与 --out 文件） ----------
OUT_FD=1
FAIL_COUNT=0
if [ -n "$OUT" ]; then
  set -C
  exec 3>"$OUT" || { echo "无法创建输出文件: $OUT" >&2; exit 1; }
  set +C
  OUT_FD=3
fi
emit() { # $1=suite $2=iter $3=metric $4=value $5=unit
  local line="$1"$'\t'"$2"$'\t'"$3"$'\t'"$4"$'\t'"$5"
  [ "$4" != FAIL ] || FAIL_COUNT=$((FAIL_COUNT + 1))
  echo "$line"
  [ "$OUT_FD" = 1 ] || echo "$line" >&$OUT_FD
}
emit_meta() {
  echo "# $*"
  [ "$OUT_FD" = 1 ] || echo "# $*" >&$OUT_FD
}

emit_meta "binary=$BIN_ABS gtk=$GTK_VER webkit=${WEBKIT_VER:-unknown} mode=$MODE suite=$SUITE runs=$RUNS trigger=$TRIGGER"
emit_meta "host=$(uname -sr) cpus=$(nproc) date=$(date -Iseconds)"
emit_meta "workdir=$WORKDIR run_id=$RUN_ID dmabuf_disabled=${WEBKIT_DISABLE_DMABUF_RENDERER:-0}"
emit_meta "note=窗口时间用 10ms 轮询探测；cpu=单核占用百分比；rss=KiB"

# ---------- D-Bus 会话总线与占位托盘宿主 ----------
DBUS_PID=$(env "$INFRA_TAG" dbus-daemon --session --fork --address="$DBUS_ADDR" --print-pid=1) || {
  echo "dbus-daemon 启动失败" >&2; exit 1;
}
sleep 0.2

cat > "$WORKDIR/fake_watcher.py" <<'PYEOF'
import sys, time
import dbus
try:
    bus = dbus.SessionBus()
    bus.request_name('org.kde.StatusNotifierWatcher')
    print('watcher: name acquired', flush=True)
    while True:
        time.sleep(1)
except Exception as e:
    print('watcher error:', e, flush=True)
    sys.exit(1)
PYEOF
env "$INFRA_TAG" python3 "$WORKDIR/fake_watcher.py" >"$WORKDIR/watcher.log" 2>&1 &
WATCHER_PID=$!
WATCHER_OK=0
for i in $(seq 1 200); do
  if busctl --address="$DBUS_ADDR" call org.freedesktop.DBus /org/freedesktop/DBus \
      org.freedesktop.DBus NameHasOwner s org.kde.StatusNotifierWatcher 2>/dev/null | grep -q true; then
    WATCHER_OK=1; break
  fi
  sleep 0.05
done
if [ "$WATCHER_OK" != 1 ]; then
  echo "致命: 占位 StatusNotifierWatcher 未能占用名字" >&2
  exit 1
fi
emit_meta "tray_watcher=ready (占位进程 pid=$WATCHER_PID)"
BASELINE_WEBPROC=$(count_comm WebKitWebProcess)
emit_meta "baseline_webproc=$BASELINE_WEBPROC"
# 应用侧托盘可用性（available_linux.go 的 NameHasOwner 双参调用在本机 dbus 上的表现，
# 用与产品一致的 godbus 语义演示）：busctl 单参探测只验证占位名字本身
emit_meta "tray_name_owner=$(busctl --address="$DBUS_ADDR" call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus NameHasOwner s org.kde.StatusNotifierWatcher 2>&1)"

# ---------- 窗口操作（X11；wayland 模式返回失败） ----------
main_window() { # $1=pid → 主窗口 id（可见且面积≥200x200 中最大者），无则空
  [ "$MODE" = "x11" ] || return 1
  local w best="" area=0 bw bh
  for w in $(xdotool search --pid "$1" 2>/dev/null); do
    bw=$(xwininfo -id "$w" 2>/dev/null | awk '/^  Width:/{print $2}')
    bh=$(xwininfo -id "$w" 2>/dev/null | awk '/^  Height:/{print $2}')
    [ -n "$bw" ] && [ -n "$bh" ] || continue
    [ "$bw" -ge 200 ] && [ "$bh" -ge 200 ] || continue
    [ "$((bw * bh))" -gt "$area" ] || continue
    if xwininfo -id "$w" 2>/dev/null | grep -q "Map State: IsViewable"; then
      area=$((bw * bh)); best="$w"
    fi
  done
  [ -n "$best" ] && echo "$best"
  return 0
}

click_titlebar_button() { # $1=wid $2=从右数第几个按钮(1=关闭 3=最小化)
  local wid=$1 n=$2 geo ax ay w
  geo=$(xwininfo -id "$wid" 2>/dev/null)
  ax=$(echo "$geo" | awk '/Absolute upper-left X/{print $4}')
  ay=$(echo "$geo" | awk '/Absolute upper-left Y/{print $4}')
  w=$(echo "$geo" | awk '/^  Width:/{print $2}')
  [ -n "$ax" ] && [ -n "$w" ] || return 1
  # 布局：右内边距 6px + 按钮 42px（TitleBar.vue）：第 n 个按钮中心 = W - 27 - 42*(n-1)
  xdotool mousemove --sync $((ax + w - 27 - 42 * (n - 1))) $((ay + 19)) click 1
}

trigger_action() { # 触发一次窗口收起（close 或 min），返回点击是否成功
  local wid=$1
  # 先聚焦窗口：WSLg 首击仅用于聚焦（click-to-focus），不落控件
  xdotool windowfocus "$wid" 2>/dev/null
  sleep 0.15
  if [ "$TRIGGER" = "close" ]; then
    click_titlebar_button "$wid" 1
  else
    click_titlebar_button "$wid" 3
  fi
}

trigger_and_wait_gone() { # 点击 + 重试（WSLg 窗口自动排布可能导致点击瞬间窗口位移）。成功 0
  # 计时只计成功那一次点击（重试不计入，重试次数单独记录）
  local wid t0 attempt
  for attempt in 1 2 3; do
    wid=$(main_window "$SCRIPT_MAIN")
    [ -z "$wid" ] && return 1 # 窗口已消失，无从触发
    t0=$(now_ns)
    trigger_action "$wid" || continue
    if wait_main_gone 4000; then
      LAST_TRIGGER_MS=$(el_ms "$t0" "$(now_ns)")
      LAST_TRIGGER_T0=$t0
      LAST_TRIGGER_TRIES=$attempt
      return 0
    fi
  done
  return 1
}

wait_webproc_gone() { # $1=超时ms：WebKitWebProcess 回落到基线（destroy 完成信号）
  local deadline=$(( $(now_ms) + $1 ))
  while [ "$(count_comm WebKitWebProcess)" -gt "$BASELINE_WEBPROC" ]; do
    [ "$(now_ms)" -ge "$deadline" ] && return 1
    sleep 0.02
  done
  return 0
}

effective_mode() {
  if [ "$GTK_VER" = 4 ] && [ "$1" = destroy ]; then echo hibernate; else echo "$1"; fi
}
set_config_mode() {
  printf '{"hibernate_mode":"%s"}\n' "$1" > "$WORKDIR/config/LiteTalk/config.json"
  emit_meta "requested_mode=$1 expected_effective_mode=$(effective_mode "$1")"
}
disk_config_mode() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1],encoding="utf-8")).get("hibernate_mode",""))' \
    "$WORKDIR/config/LiteTalk/config.json"
}

launch_app() { # $1=附加环境变量(k=v 或 -) → 设置 SCRIPT_MAIN 和本次启动专属日志
  local envkv=$1
  SCRIPT_LOG="$WORKDIR/app-main-$(date +%s%N).log"
  if [ "$envkv" != "-" ]; then
    env "$APP_TAG" LITETALK_CI_SMOKE=1 "$envkv" "$BIN_ABS" >"$SCRIPT_LOG" 2>&1 &
  else
    env "$APP_TAG" LITETALK_CI_SMOKE=1 "$BIN_ABS" >"$SCRIPT_LOG" 2>&1 &
  fi
  SCRIPT_MAIN=$!
}

launch_secondary() {
  env "$APP_TAG" LITETALK_CI_SMOKE=1 "$BIN_ABS" >>"$WORKDIR/app-secondary.log" 2>&1 &
  SECOND_PID=$!
}

process_running() { # $1=pid；zombie 也视为退出
  local state
  state=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
  [ -n "$state" ] && [ "${state#Z}" = "$state" ]
}

wait_frontend_ready() { # $1=超时ms；只查本次主实例日志，不能命中前一轮
  local deadline=$(( $(now_ms) + $1 ))
  while [ "$(now_ms)" -lt "$deadline" ]; do
    grep -Fq 'litetalk-smoke:frontend-ready' "$SCRIPT_LOG" 2>/dev/null && return 0
    process_running "$SCRIPT_MAIN" || return 1
    sleep 0.05
  done
  return 1
}

wait_main_window() { # $1=超时ms → 成功时输出 wid
  local deadline=$(( $(now_ms) + $1 )) wid
  while [ "$(now_ms)" -lt "$deadline" ]; do
    wid=$(main_window "$SCRIPT_MAIN")
    [ -n "$wid" ] && { echo "$wid"; return 0; }
    sleep 0.01
  done
  return 1
}

wait_main_gone() { # $1=超时ms：主窗口连续 2 次探测不可见即成功
  local deadline=$(( $(now_ms) + $1 )) miss=0 wid
  while :; do
    wid=$(main_window "$SCRIPT_MAIN")
    if [ -z "$wid" ]; then
      miss=$((miss + 1))
      [ "$miss" -ge 2 ] && return 0
    else
      miss=0
    fi
    [ "$(now_ms)" -ge "$deadline" ] && return 1
    sleep 0.01
  done
}

wait_webproc() { # $1=超时ms：系统内出现 WebKitWebProcess
  local deadline=$(( $(now_ms) + $1 ))
  while [ "$(now_ms)" -lt "$deadline" ]; do
    [ "$(count_comm WebKitWebProcess)" -gt 0 ] && return 0
    sleep 0.05
  done
  return 1
}

stop_app() {
  [ -n "$SCRIPT_MAIN" ] || return 0
  signal_tagged "$APP_TAG" TERM
  local w
  for w in $(seq 1 50); do kill -0 "$SCRIPT_MAIN" 2>/dev/null || break; sleep 0.05; done
  signal_tagged "$APP_TAG" KILL
  wait "$SCRIPT_MAIN" 2>/dev/null || true
  SCRIPT_MAIN=""
  sleep 0.3
}

wait_proc_exit() { # $1=pid $2=超时ms
  local deadline=$(( $(now_ms) + $2 ))
  while kill -0 "$1" 2>/dev/null; do
    [ "$(now_ms)" -ge "$deadline" ] && return 1
    sleep 0.005
  done
  return 0
}

# ============================================================
# 套件：cold 冷启动（$1=标签 $2=附加env $3=轮数）
# ============================================================
suite_cold() {
  local tag=$1 envkv=$2 n=$3 i wid t0 t1 ms webok fail=0
  for i in 1 2; do # 预热（WebKit 字体/网络进程缓存），不计入
    launch_app "$envkv"
    if ! wait_main_window 30000 >/dev/null 2>&1 || ! wait_frontend_ready 30000; then
      emit "cold-$tag" 0 warmup FAIL bool
      stop_app; return 1
    fi
    stop_app
  done
  for i in $(seq 1 "$n"); do
    launch_app "$envkv"
    t0=$(now_ns)
    if wid=$(wait_main_window 30000); then
      t1=$(now_ns)
      ms=$(el_ms "$t0" "$t1")
      webok=$(wait_webproc 15000 && echo yes || echo no)
      emit "cold-$tag" "$i" "window_mapped" "$ms" "ms"
      if wait_frontend_ready 30000; then
        emit "cold-$tag" "$i" frontend_ready "$(el_ms "$t0" "$(now_ns)")" ms
      else
        emit "cold-$tag" "$i" frontend_ready FAIL ms
      fi
      if [ "$webok" = yes ]; then
        emit "cold-$tag" "$i" webprocess_present yes bool
      else
        emit "cold-$tag" "$i" webprocess_present FAIL bool
      fi
    else
      fail=$((fail + 1))
      emit "cold-$tag" "$i" "window_mapped" "FAIL" "ms"
    fi
    stop_app
  done
  emit "cold-$tag" 0 "failures" "$fail" "count"
}

# ============================================================
# 套件内单轮：收起→二次启动唤醒（$1=轮次 $2=套件名 $3=记录资源）
# ============================================================
cycle_hide_show() {
  local it=$1 suite=$2 detailed=${3:-0} wid0 wid2 t0 t1 ms sec web0 ok=1
  local act="hide" wake="show_wake"
  [ "$TRIGGER" = "min" ] && { act="minimise"; wake="restore_wake"; }
  wid0=$(main_window "$SCRIPT_MAIN")
  web0=$(count_comm WebKitWebProcess)
  [ -n "$wid0" ] || { emit "$suite" "$it" "$act" FAIL ms; return 1; }
  if [ "$suite" = destroy-downgrade ] && [ "$web0" -lt 1 ]; then
    emit "$suite" "$it" webprocess_before FAIL count; return 1
  fi
  if ! trigger_and_wait_gone; then
    emit "$suite" "$it" "$act" FAIL ms; return 1
  fi
  emit "$suite" "$it" "$act" "$LAST_TRIGGER_MS" ms
  emit "$suite" "$it" "${act}_tries" "$LAST_TRIGGER_TRIES" count
  if ! kill -0 "$SCRIPT_MAIN" 2>/dev/null; then
    emit "$suite" "$it" main_process_alive FAIL bool; return 1
  fi
  if [ "$detailed" = 1 ]; then
    emit "$suite" "$it" rss_after_hide "$(tree_rss_kib "$SCRIPT_MAIN")" KiB
    emit "$suite" "$it" counts_after_hide "$(webkit_counts)" kv
  fi
  sleep 1 # 状态稳定，不计入测量
  t0=$(now_ns)
  launch_secondary; sec=$SECOND_PID
  if wid2=$(wait_main_window 30000); then
    t1=$(now_ns); ms=$(el_ms "$t0" "$t1")
    emit "$suite" "$it" "$wake" "$ms" ms
    if [ "$wid2" = "$wid0" ]; then
      emit "$suite" "$it" window_reused yes bool
    else
      emit "$suite" "$it" window_reused FAIL bool; ok=0
    fi
  else
    emit "$suite" "$it" "$wake" FAIL ms; ok=0
  fi
  if [ "$suite" = destroy-downgrade ]; then
    if [ "$(disk_config_mode)" = destroy ]; then
      emit "$suite" "$it" config_destroy_retained yes bool
    else
      emit "$suite" "$it" config_destroy_retained FAIL bool; ok=0
    fi
    if [ "$(count_comm WebKitWebProcess)" = "$web0" ]; then
      emit "$suite" "$it" webprocess_count_stable yes bool
    else
      emit "$suite" "$it" webprocess_count_stable FAIL bool; ok=0
    fi
  fi
  if [ "$detailed" = 1 ]; then
    emit "$suite" "$it" rss_after_wake "$(tree_rss_kib "$SCRIPT_MAIN")" KiB
    emit "$suite" "$it" counts_after_wake "$(webkit_counts)" kv
  fi
  if ! wait_proc_exit "$sec" 5000; then
    emit "$suite" "$it" second_instance_lingering FAIL bool; ok=0
  fi
  sleep 1
  [ "$ok" = 1 ]
}

# ============================================================
# 套件内单轮：destroy → 二次启动重建（$1=轮次 $2=1 记录 RSS/进程计数）
# ============================================================
cycle_destroy_rebuild() {
  local it=$1 detailed=${2:-0} wid0 wid2 t0 t1 ms sec
  [ "$GTK_VER" = 3 ] || { echo "GTK4 禁止运行真实 destroy 套件" >&2; return 1; }
  if [ "$(count_comm WebKitWebProcess)" -lt 1 ]; then
    emit destroy-rebuild "$it" webprocess_before FAIL count; return 1
  fi
  wid0=$(main_window "$SCRIPT_MAIN")
  if [ -z "$wid0" ]; then
    emit "destroy-rebuild" "$it" "destroy" "FAIL" "ms"; return 1
  fi
  if ! trigger_and_wait_gone; then
    emit "destroy-rebuild" "$it" "destroy" "FAIL" "ms"; return 1
  fi
  emit "destroy-rebuild" "$it" "destroy" "$LAST_TRIGGER_MS" "ms"
  emit "destroy-rebuild" "$it" "destroy_tries" "$LAST_TRIGGER_TRIES" "count"
  # destroy 完成信号：WebKitWebProcess 回落到基线（win.Close 后渲染进程退出）
  if wait_webproc_gone 8000; then
    emit "destroy-rebuild" "$it" "webprocess_exit" "$(el_ms "$LAST_TRIGGER_T0" "$(now_ns)")" "ms"
  else
    emit "destroy-rebuild" "$it" "webprocess_exit" "FAIL" "ms"
  fi
  if [ "$detailed" = 1 ]; then
    emit "destroy-rebuild" "$it" "rss_after_destroy" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
    emit "destroy-rebuild" "$it" "counts_after_destroy" "$(webkit_counts)" "kv"
  fi
  sleep 1
  t0=$(now_ns)
  launch_secondary; sec=$SECOND_PID
  if wid2=$(wait_main_window 30000); then
    t1=$(now_ns); ms=$(el_ms "$t0" "$t1")
    emit "destroy-rebuild" "$it" "rebuild" "$ms" "ms"
    [ "$wid2" != "$wid0" ] && emit destroy-rebuild "$it" rebuild_new_window yes bool ||
      emit destroy-rebuild "$it" rebuild_new_window FAIL bool
  else
    emit "destroy-rebuild" "$it" "rebuild" "FAIL" "ms"
  fi
  if [ "$detailed" = 1 ]; then
    sleep 1.5
    emit "destroy-rebuild" "$it" "rss_after_rebuild" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
    emit "destroy-rebuild" "$it" "counts_after_rebuild" "$(webkit_counts)" "kv"
  fi
  wait_proc_exit "$sec" 5000 || emit "destroy-rebuild" "$it" "second_instance_lingering" FAIL bool
  sleep 1
  return 0
}

# ============================================================
# 套件：lifecycle（唤醒校准 + off/hibernate + 旧 destroy 请求）
# ============================================================
suite_lifecycle() {
  local i fail m suite sec
  # 唤醒开销校准：窗口可见时二次启动 exec→子进程退出 ≈ 单实例通知开销，
  # show_wake 与托盘点击真实路径的差距上限即此值
  set_config_mode off
  launch_app "-"
  if ! wait_main_window 30000 >/dev/null 2>&1 || ! wait_frontend_ready 30000; then
    emit wake-calibration 0 startup FAIL bool
    stop_app; return 1
  fi
  sleep 2
  for i in $(seq 1 10); do
    local t0 t1
    t0=$(now_ns)
    launch_secondary; sec=$SECOND_PID
    wait_proc_exit "$sec" 10000 || emit wake-calibration "$i" second_instance_lingering FAIL bool
    t1=$(now_ns)
    emit "wake-calibration" "$i" "second_instance_overhead" "$(el_ms "$t0" "$t1")" "ms"
    sleep 0.5
  done
  stop_app

  for m in off hibernate destroy; do
    suite="hide-show-$m"
    [ "$m" != destroy ] || { [ "$GTK_VER" = 4 ] && suite=destroy-downgrade || suite=destroy-rebuild; }
    set_config_mode "$m"
    launch_app "-"
    if ! wait_main_window 30000 >/dev/null 2>&1 || ! wait_frontend_ready 30000; then
      emit "$suite" 0 startup FAIL bool
      stop_app; continue
    fi
    sleep 2
    if [ "$suite" != destroy-rebuild ]; then
      # 预探测失败就记失败，避免 WSLg minimise 空操作把 30 轮都当成有效样本。
      if ! trigger_and_wait_gone; then
        emit "$suite" 0 hide_probe FAIL bool
        stop_app; continue
      fi
      launch_secondary; sec=$SECOND_PID
      if ! wait_main_window 30000 >/dev/null 2>&1; then
        emit "$suite" 0 wake_probe FAIL bool
        stop_app; continue
      fi
      wait_proc_exit "$sec" 5000 || emit "$suite" 0 second_instance_lingering FAIL bool
      if [ "$suite" = destroy-downgrade ] && [ "$(disk_config_mode)" != destroy ]; then
        emit "$suite" 0 config_destroy_retained FAIL bool
      fi
    fi
    fail=0
    for i in $(seq 1 "$RUNS"); do
      if [ "$suite" = destroy-rebuild ]; then
        cycle_destroy_rebuild "$i" 0 || fail=$((fail + 1))
      else
        cycle_hide_show "$i" "$suite" 0 || fail=$((fail + 1))
      fi
    done
    emit "$suite" 0 failures "$fail" count
    stop_app
  done
}

# ============================================================
# 套件：cpu 空闲 CPU（$1=标签 $2=附加env）
# ============================================================
suite_cpu() {
  local tag=$1 envkv=$2 t0 t1 ticks0 ticks1 wall pct
  launch_app "$envkv"
  if ! wait_main_window 30000 >/dev/null 2>&1 || ! wait_frontend_ready 30000; then
    emit "cpu-idle-$tag" 1 startup FAIL bool
    stop_app; return 1
  fi
  sleep 5 # 就绪稳定
  t0=$(now_ns); ticks0=$(tree_cpu_ticks "$SCRIPT_MAIN")
  sleep "$CPU_SECONDS"
  t1=$(now_ns); ticks1=$(tree_cpu_ticks "$SCRIPT_MAIN")
  wall=$(( (t1 - t0) / 1000000000 )); [ "$wall" -lt 1 ] && wall=1
  # 核占比 = Δtick/CLK_TCK(=CPU秒) / wall × 100
  pct=$(( (ticks1 - ticks0) * 100 / CLK_TCK / wall ))
  emit "cpu-idle-$tag" 1 "tree_cpu_percent" "$pct" "percent_of_one_core"
  emit "cpu-idle-$tag" 1 "tree_rss" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
  emit "cpu-idle-$tag" 1 "process_count" "$(tree_pids "$SCRIPT_MAIN" | wc -l)" "count"
  stop_app
}

# ============================================================
# 套件：rss 三档休眠模式
# ============================================================
suite_rss() {
  local m s suite after_metric web_before
  for m in off hibernate destroy; do
    suite="rss-$m"
    [ "$m" != destroy ] || { [ "$GTK_VER" = 4 ] && suite=rss-destroy-downgrade || suite=rss-destroy; }
    set_config_mode "$m"
    launch_app "-"
    if ! wait_main_window 30000 >/dev/null 2>&1 || ! wait_frontend_ready 30000; then
      emit "$suite" 0 startup FAIL bool
      stop_app; continue
    fi
    sleep 5
    # 窗口可见态连采 3 次（间隔 2s），文档取中位数
    for s in 1 2 3; do
      emit "$suite" "$s" rss_window_visible "$(tree_rss_kib "$SCRIPT_MAIN")" KiB
      sleep 2
    done
    emit "$suite" 1 process_count_visible "$(tree_pids "$SCRIPT_MAIN" | wc -l)" count
    web_before=$(count_comm WebKitWebProcess)
    if [ "$m" = destroy ] && [ "$web_before" -lt 1 ]; then
      emit "$suite" 1 webprocess_before FAIL count
      stop_app; continue
    fi
    if [ "$m" != off ]; then
      if ! trigger_and_wait_gone; then
        emit "$suite" 1 hide FAIL bool
        stop_app; continue
      fi
      if [ "$GTK_VER" = 3 ] && [ "$m" = destroy ]; then
        if ! wait_webproc_gone 8000; then emit "$suite" 1 webprocess_exit FAIL bool; fi
        after_metric=rss_after_destroy
      else
        after_metric=rss_after_hibernate
      fi
      sleep 3
      for s in 1 2 3; do
        emit "$suite" "$s" "$after_metric" "$(tree_rss_kib "$SCRIPT_MAIN")" KiB
        sleep 2
      done
      emit "$suite" 1 counts_after_hide "$(webkit_counts)" kv
      if [ "$suite" = rss-destroy-downgrade ]; then
        [ "$(disk_config_mode)" = destroy ] && emit "$suite" 1 config_destroy_retained yes bool ||
          emit "$suite" 1 config_destroy_retained FAIL bool
        [ "$web_before" -gt 0 ] && [ "$(count_comm WebKitWebProcess)" = "$web_before" ] &&
          emit "$suite" 1 webprocess_count_stable yes bool ||
          emit "$suite" 1 webprocess_count_stable FAIL bool
      fi
    fi
    stop_app
  done
}

# ============================================================
# 套件：旧 destroy 请求的连续窗口周期与资源趋势
# ============================================================
suite_leak() {
  local suite=destroy-rebuild
  [ "$GTK_VER" = 4 ] && suite=destroy-downgrade
  set_config_mode destroy
  launch_app "-"
  if ! wait_main_window 30000 >/dev/null 2>&1 || ! wait_frontend_ready 30000; then
    emit "$suite" 0 startup FAIL bool
    stop_app; return 1
  fi
  if [ "$(count_comm WebKitWebProcess)" -lt 1 ]; then
    emit "$suite" 0 webprocess_before FAIL count
    stop_app; return 1
  fi
  sleep 2
  emit "$suite" 0 rss_baseline "$(tree_rss_kib "$SCRIPT_MAIN")" KiB
  emit "$suite" 0 counts_baseline "$(webkit_counts)" kv
  local i fail=0
  for i in $(seq 1 "$LEAK_CYCLES"); do
    if [ "$GTK_VER" = 4 ]; then
      cycle_hide_show "$i" "$suite" 1 || fail=$((fail + 1))
    else
      cycle_destroy_rebuild "$i" 1 || fail=$((fail + 1))
    fi
  done
  emit "$suite" 0 failures "$fail" count
  stop_app
}

# ============================================================
# 主流程
# ============================================================
if [ "$MODE" = "wayland" ]; then
  export GDK_BACKEND=wayland
  emit_meta "wayland 模式: GDK_BACKEND=wayland WAYLAND_DISPLAY=$WAYLAND_DISPLAY；窗口映射时间仍为 NA"
  if [ ! -S "$WAYLAND_DISPLAY" ]; then
    emit wayland-smoke 0 display_socket FAIL bool
    exit 1
  fi
  set_config_mode off
  t0=$(now_ns)
  launch_app "-"
  if wait_frontend_ready 30000 && process_running "$SCRIPT_MAIN"; then
    emit wayland-smoke 1 frontend_ready "$(el_ms "$t0" "$(now_ns)")" ms
    sleep 5
    ticks0=$(tree_cpu_ticks "$SCRIPT_MAIN"); t0=$(now_ns)
    alive=1
    for i in $(seq 1 "$CPU_SECONDS"); do
      sleep 1
      if ! process_running "$SCRIPT_MAIN"; then
        emit wayland-smoke "$i" process_alive_during_sample FAIL bool
        alive=0; break
      fi
    done
    if [ "$alive" = 1 ]; then
      ticks1=$(tree_cpu_ticks "$SCRIPT_MAIN"); t1=$(now_ns)
      wall=$(( (t1 - t0) / 1000000000 )); [ "$wall" -lt 1 ] && wall=1
      emit "wayland-smoke" 1 "tree_cpu_percent" "$(( (ticks1 - ticks0) * 100 / CLK_TCK / wall ))" "percent_of_one_core"
      emit "wayland-smoke" 1 "tree_rss" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
    fi
  else
    emit wayland-smoke 1 frontend_ready FAIL bool
  fi
  stop_app
  [ "$FAIL_COUNT" = 0 ]
  exit $?
fi

export GDK_BACKEND=x11
case "$SUITE" in
  all)
    suite_cold "default" "-" "$RUNS"
    suite_lifecycle
    suite_cpu "default" "-"
    suite_rss
    suite_leak
    suite_cold "sw" "WEBKIT_DISABLE_DMABUF_RENDERER=1" "$RUNS"
    suite_cpu "sw" "WEBKIT_DISABLE_DMABUF_RENDERER=1"
    ;;
  cold) suite_cold "default" "-" "$RUNS" ;;
  lifecycle) suite_lifecycle ;;
  cpu) suite_cpu "default" "-" ;;
  rss) suite_rss ;;
  leak) suite_leak ;;
  render)
    suite_cold "sw" "WEBKIT_DISABLE_DMABUF_RENDERER=1" "$RUNS"
    suite_cpu "sw" "WEBKIT_DISABLE_DMABUF_RENDERER=1"
    ;;
esac

emit_meta "done suite=$SUITE binary=$BIN_ABS"
exec 3>&-
[ "$FAIL_COUNT" = 0 ]
exit $?
