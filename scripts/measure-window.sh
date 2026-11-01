#!/usr/bin/env bash
# measure-window.sh — LiteTalk 窗口生命周期测量（GTK3/GTK4 基线，任务卡 W0-1 / #5）
#
# 对指定 LiteTalk 客户端二进制做可复跑的窗口生命周期与资源测量，输出机器可读 TSV
# （stdout 与 --out 文件，二者内容一致）。测量在隔离环境进行：私有 D-Bus 会话总线、
# 私有 XDG_CONFIG_HOME / XDG_CACHE_HOME、占位 StatusNotifierWatcher（WSLg 等无托盘
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
#   lifecycle  隐藏/最小化→二次启动唤醒 30 轮 + destroy/rebuild 30 轮 + 唤醒开销校准 10 轮
#   cpu        空闲 CPU（进程树，默认 60s）
#   rss        off/hibernate/destroy 三档常驻内存
#   render     WEBKIT_DISABLE_DMABUF_RENDERER=1 软件渲染下的 cold + cpu
#   leak       destroy 循环 RSS 趋势与 WebKit 孤儿进程计数（泄漏观测）
#   all        以上全部
#
# --trigger 触发按钮（前端自定义标题栏，见 client/frontend/src/components/TitleBar.vue）：
#   close  关闭按钮（生产路径 WindowClose → interceptClose → Hide/destroy）
#   min    最小化按钮（直接 win.Minimise；destroy 模式经 WindowMinimise 事件销毁）
# 托盘宿主缺失（WSLg；或 dbus ≥1.16 严格校验使 available_linux.go 的 NameHasOwner
# 双参调用报错 → trayReady 恒 false）时，close 按钮的生效路径同样是 minimise，
# 此时建议 --trigger min。指标名随触发按钮标注（hide/minimise）。
#
# 依赖：xdotool xwininfo dbus-daemon busctl python3(python3-dbus)。
# 输出：TSV（suite、iteration、metric、value、unit），失败行 value=FAIL，
#       以 # 开头的行为元信息。退出码 0=有数据，1=环境错误。
set -u

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

for t in xdotool xwininfo dbus-daemon busctl python3; do
  command -v "$t" >/dev/null 2>&1 || { echo "缺少依赖: $t" >&2; exit 1; }
done
python3 -c 'import dbus' 2>/dev/null || { echo "缺少 python3-dbus" >&2; exit 1; }

# ---------- 环境 ----------
BIN_ABS=$(readlink -f "$BINARY")
WORKDIR="${WORKDIR:-/tmp/ltmeasure-$(date +%s)-$$}"
mkdir -p "$WORKDIR/config/LiteTalk" "$WORKDIR/cache" "$WORKDIR/dbus"
export XDG_CONFIG_HOME="$WORKDIR/config"
export XDG_CACHE_HOME="$WORKDIR/cache"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
DBUS_ADDR="unix:path=$WORKDIR/dbus/session-bus"
export DBUS_SESSION_BUS_ADDRESS="$DBUS_ADDR"
SCRIPT_MAIN=""
WATCHER_PID=""
DBUS_PID=""
BASELINE_WEBPROC=0

cleanup() {
  [ -n "$SCRIPT_MAIN" ] && tree_kill "$SCRIPT_MAIN" 2>/dev/null
  pkill -f "^$BIN_ABS" 2>/dev/null
  [ -n "$WATCHER_PID" ] && kill "$WATCHER_PID" 2>/dev/null
  [ -n "$DBUS_PID" ] && kill "$DBUS_PID" 2>/dev/null
}
trap cleanup EXIT INT TERM

# ---------- 度量辅助 ----------
tree_kill() { # $1=pid：递归杀进程树
  [ -n "${1:-}" ] || return 0
  local c
  for c in $(pgrep -P "$1" 2>/dev/null); do tree_kill "$c"; done
  kill -TERM "$1" 2>/dev/null
}

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

count_comm() { # $1=进程名（argv0 基名精确匹配）。不用 pgrep -x：进程名 >15 字节会被
  # /proc/comm 截断（WebKitWebProcess=16 字节），pgrep -x 恒 0
  python3 -c '
import sys, os
want = sys.argv[1]; n = 0
for pid in os.listdir("/proc"):
    if not pid.isdigit():
        continue
    try:
        with open("/proc/%s/cmdline" % pid, "rb") as f:
            argv0 = f.read().split(b"\0")[0].decode(errors="replace")
        if os.path.basename(argv0) == want:
            n += 1
    except OSError:
        pass
print(n)' "$1"
}

webkit_counts() { echo "Web=$(count_comm WebKitWebProcess) Net=$(count_comm WebKitNetworkProcess)"; }

# ---------- 元信息 ----------
GTK_VER=$(ldd "$BIN_ABS" 2>/dev/null | grep -oE 'libgtk-[34]' | head -1 | grep -oE '[34]')
WEBKIT_VER=$(ldd "$BIN_ABS" 2>/dev/null | grep -oE 'libwebkit2gtk-4\.[01]|libwebkitgtk-6\.0' | head -1)
[ -n "$GTK_VER" ] || GTK_VER="unknown"

# ---------- TSV 输出（stdout 与 --out 文件） ----------
OUT_FD=1
if [ -n "$OUT" ]; then
  : >> "$OUT"
  exec 3>>"$OUT"
  OUT_FD=3
fi
emit() { # $1=suite $2=iter $3=metric $4=value $5=unit
  local line="$1"$'\t'"$2"$'\t'"$3"$'\t'"$4"$'\t'"$5"
  echo "$line"
  echo "$line" >&$OUT_FD
}
emit_meta() {
  echo "# $*"
  echo "# $*" >&$OUT_FD
}

emit_meta "binary=$BIN_ABS gtk=$GTK_VER webkit=${WEBKIT_VER:-unknown} mode=$MODE suite=$SUITE runs=$RUNS trigger=$TRIGGER"
emit_meta "host=$(uname -sr) cpus=$(nproc) date=$(date -Iseconds)"
emit_meta "note=窗口时间用 10ms 轮询探测；cpu=单核占用百分比；rss=KiB"

# ---------- D-Bus 会话总线与占位托盘宿主 ----------
if busctl --address="$DBUS_ADDR" status >/dev/null 2>&1; then
  : # 复用既有（--workdir 重跑）
else
  dbus-daemon --session --fork --address="$DBUS_ADDR" || { echo "dbus-daemon 启动失败" >&2; exit 1; }
  sleep 0.2
fi

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
python3 "$WORKDIR/fake_watcher.py" >"$WORKDIR/watcher.log" 2>&1 &
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

set_config_mode() { printf '{"hibernate_mode":"%s"}\n' "$1" > "$WORKDIR/config/LiteTalk/config.json"; }

launch_app() { # $1=附加环境变量(k=v 或 -) → 设置 SCRIPT_MAIN
  local envkv=$1
  if [ "$envkv" != "-" ]; then
    env "$envkv" "$BIN_ABS" >>"$WORKDIR/app.log" 2>&1 &
  else
    "$BIN_ABS" >>"$WORKDIR/app.log" 2>&1 &
  fi
  SCRIPT_MAIN=$!
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
  tree_kill "$SCRIPT_MAIN"
  local w
  for w in $(seq 1 100); do kill -0 "$SCRIPT_MAIN" 2>/dev/null || break; sleep 0.05; done
  kill -9 "$SCRIPT_MAIN" 2>/dev/null
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
    wait_main_window 30000 >/dev/null 2>&1
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
      emit "cold-$tag" "$i" "webprocess_present" "$webok" "bool"
    else
      fail=$((fail + 1))
      emit "cold-$tag" "$i" "window_mapped" "FAIL" "ms"
    fi
    stop_app
  done
  emit "cold-$tag" 0 "failures" "$fail" "count"
}

# ============================================================
# 套件内单轮：收起（hide/minimise）→ 二次启动唤醒（$1=轮次）
# ============================================================
cycle_hide_show() {
  local it=$1 wid wid2 t0 t1 ms sec
  local act="hide" wake="show_wake"
  [ "$TRIGGER" = "min" ] && { act="minimise"; wake="restore_wake"; }
  if ! trigger_and_wait_gone; then
    emit "hide-show" "$it" "$act" "FAIL" "ms"; return 1
  fi
  emit "hide-show" "$it" "$act" "$LAST_TRIGGER_MS" "ms"
  emit "hide-show" "$it" "${act}_tries" "$LAST_TRIGGER_TRIES" "count"
  sleep 1 # 状态稳定，不计入测量
  t0=$(now_ns)
  "$BIN_ABS" >>"$WORKDIR/app.log" 2>&1 &
  sec=$!
  if wid2=$(wait_main_window 30000); then
    t1=$(now_ns); ms=$(el_ms "$t0" "$t1")
    emit "hide-show" "$it" "$wake" "$ms" "ms"
  else
    emit "hide-show" "$it" "$wake" "FAIL" "ms"
  fi
  wait_proc_exit "$sec" 5000 || emit "hide-show" "$it" "second_instance_lingering" "yes" "bool"
  sleep 1
  return 0
}

# ============================================================
# 套件内单轮：destroy → 二次启动重建（$1=轮次 $2=1 记录 RSS/进程计数）
# ============================================================
cycle_destroy_rebuild() {
  local it=$1 detailed=${2:-0} wid0 wid2 t0 t1 ms sec
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
  "$BIN_ABS" >>"$WORKDIR/app.log" 2>&1 &
  sec=$!
  if wid2=$(wait_main_window 30000); then
    t1=$(now_ns); ms=$(el_ms "$t0" "$t1")
    emit "destroy-rebuild" "$it" "rebuild" "$ms" "ms"
    if [ -n "${wid0:-}" ]; then
      [ "$wid2" != "$wid0" ] && emit "destroy-rebuild" "$it" "rebuild_new_window" "yes" "bool" ||
        emit "destroy-rebuild" "$it" "rebuild_new_window" "no" "bool"
    fi
  else
    emit "destroy-rebuild" "$it" "rebuild" "FAIL" "ms"
  fi
  if [ "$detailed" = 1 ]; then
    sleep 1.5
    emit "destroy-rebuild" "$it" "rss_after_rebuild" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
    emit "destroy-rebuild" "$it" "counts_after_rebuild" "$(webkit_counts)" "kv"
  fi
  wait_proc_exit "$sec" 5000 || emit "destroy-rebuild" "$it" "second_instance_lingering" "yes" "bool"
  sleep 1
  return 0
}

# ============================================================
# 套件：lifecycle（唤醒校准 + hide/show + destroy/rebuild）
# ============================================================
suite_lifecycle() {
  local i fail
  # 唤醒开销校准：窗口可见时二次启动 exec→子进程退出 ≈ 单实例通知开销，
  # show_wake 与托盘点击真实路径的差距上限即此值
  set_config_mode off
  launch_app "-"
  wait_main_window 30000 >/dev/null 2>&1
  sleep 2
  for i in $(seq 1 10); do
    local t0 t1 sec
    t0=$(now_ns)
    "$BIN_ABS" >>"$WORKDIR/app.log" 2>&1 &
    sec=$!
    wait_proc_exit "$sec" 10000
    t1=$(now_ns)
    emit "wake-calibration" "$i" "second_instance_overhead" "$(el_ms "$t0" "$t1")" "ms"
    sleep 0.5
  done
  stop_app

  # hide/show（mode=off + 托盘可用 → 关窗=隐藏到托盘）
  # 先探测：若托盘不可用（trayReady=false → close 落到 minimise）且 minimise 在
  # 该合成器上是空操作（WSLg xwm：WM_STATE 维持 Normal、窗口保持可见），本轮
  # hide/show 无法从外部观测，记录 skip 后跳过（destroy/rebuild 仍然有效）。
  local act="hide"
  [ "$TRIGGER" = "min" ] && act="minimise"
  local hide_ok=0
  set_config_mode off
  launch_app "-"
  if wait_main_window 30000 >/dev/null 2>&1; then
    sleep 2
    if trigger_and_wait_gone; then
      # 探测成功 → 该轮已消耗，补记为第 0 轮数据后继续正式轮次
      hide_ok=1
      emit "hide-show" 0 "$act" "$LAST_TRIGGER_MS" "ms"
      emit "hide-show" 0 "${act}_tries" "$LAST_TRIGGER_TRIES" "count"
    else
      local mw
      mw=$(main_window "$SCRIPT_MAIN")
      if [ -n "$mw" ]; then
        emit_meta "hide-show=skipped ($act 后窗口仍可见：minimise 空操作或触发未生效，见 tray_name_owner 元信息)"
      fi
    fi
  fi
  if [ "$hide_ok" = 1 ]; then
    fail=0
    local i
    for i in $(seq 1 "$RUNS"); do
      cycle_hide_show "$i" || fail=$((fail + 1))
    done
    emit "hide-show" 0 "failures" "$fail" "count"
  fi
  stop_app

  # destroy/rebuild（mode=destroy → 关窗=销毁渲染进程）
  set_config_mode destroy
  launch_app "-"
  wait_main_window 30000 >/dev/null 2>&1
  sleep 2
  fail=0
  for i in $(seq 1 "$RUNS"); do
    cycle_destroy_rebuild "$i" 0 || fail=$((fail + 1))
  done
  emit "destroy-rebuild" 0 "failures" "$fail" "count"
  stop_app
}

# ============================================================
# 套件：cpu 空闲 CPU（$1=标签 $2=附加env）
# ============================================================
suite_cpu() {
  local tag=$1 envkv=$2 t0 t1 ticks0 ticks1 wall pct
  launch_app "$envkv"
  wait_main_window 30000 >/dev/null 2>&1
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
  local m s
  for m in off hibernate destroy; do
    set_config_mode "$m"
    launch_app "-"
    wait_main_window 30000 >/dev/null 2>&1
    sleep 5
    # 窗口可见态连采 3 次（间隔 2s），文档取中位数
    for s in 1 2 3; do
      emit "rss-$m" "$s" "rss_window_visible" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
      sleep 2
    done
    emit "rss-$m" 1 "process_count_visible" "$(tree_pids "$SCRIPT_MAIN" | wc -l)" "count"
    if [ "$m" = "hibernate" ]; then
      # hibernate 档：标题栏触发 App.WindowMinimize → emit sys:hibernate
      # （前端卸载业务组件；窗口状态在本机可能不变，RSS 是观测目标）
      trigger_and_wait_gone >/dev/null 2>&1 || true
      sleep 3
      for s in 1 2 3; do
        emit "rss-$m" "$s" "rss_after_hibernate" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
        sleep 2
      done
    fi
    if [ "$m" = "destroy" ]; then
      trigger_and_wait_gone >/dev/null 2>&1 || true
      wait_webproc_gone 8000 >/dev/null 2>&1
      sleep 2
      emit "rss-$m" 1 "rss_after_destroy" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
      emit "rss-$m" 1 "counts_after_destroy" "$(webkit_counts)" "kv"
    fi
    stop_app
  done
}

# ============================================================
# 套件：leak destroy 循环 RSS 趋势与孤儿进程（W0-2 交叉验证项）
# ============================================================
suite_leak() {
  set_config_mode destroy
  launch_app "-"
  wait_main_window 30000 >/dev/null 2>&1
  sleep 2
  emit "leak" 0 "rss_baseline" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
  emit "leak" 0 "counts_baseline" "$(webkit_counts)" "kv"
  local i fail=0
  for i in $(seq 1 "$LEAK_CYCLES"); do
    cycle_destroy_rebuild "$i" 1 || fail=$((fail + 1))
  done
  emit "leak" 0 "failures" "$fail" "count"
  stop_app
}

# ============================================================
# 主流程
# ============================================================
if [ "$MODE" = "wayland" ]; then
  export GDK_BACKEND=wayland
  emit_meta "wayland 模式: X11 工具无法观测窗口映射，仅进程级存活与资源粗测（窗口指标 NA）"
  launch_app "-"
  sleep 10
  if kill -0 "$SCRIPT_MAIN" 2>/dev/null; then
    emit "wayland-smoke" 1 "process_alive_10s" "yes" "bool"
    emit "wayland-smoke" 1 "wayland_fd_count" "$(ls -l /proc/$SCRIPT_MAIN/fd 2>/dev/null | grep -c wayland)" "count"
    ticks0=$(tree_cpu_ticks "$SCRIPT_MAIN"); t0=$(now_ns)
    sleep "$CPU_SECONDS"
    ticks1=$(tree_cpu_ticks "$SCRIPT_MAIN"); t1=$(now_ns)
    wall=$(( (t1 - t0) / 1000000000 )); [ "$wall" -lt 1 ] && wall=1
    emit "wayland-smoke" 1 "tree_cpu_percent" "$(( (ticks1 - ticks0) * 100 / CLK_TCK / wall ))" "percent_of_one_core"
    emit "wayland-smoke" 1 "tree_rss" "$(tree_rss_kib "$SCRIPT_MAIN")" "KiB"
  else
    emit "wayland-smoke" 1 "process_alive_10s" "no" "bool"
  fi
  stop_app
  exit 0
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
exit 0
