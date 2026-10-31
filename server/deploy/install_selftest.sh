#!/usr/bin/env bash
# install.sh 压缩包校验的自测（#102）。CI 的 check-server 直接跑本脚本。
# 只测 verify_archive 的判定，不触网、不需要 root、不改系统：把 install.sh 以
# LITETALK_INSTALL_LIB=1 source 进来取函数，安装主流程不会执行。
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
LITETALK_INSTALL_LIB=1
# shellcheck source=install.sh
. "$HERE/install.sh"
unset LITETALK_INSTALL_LIB

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

ok() {
  pass=$((pass + 1))
  echo "ok   - $1"
}

ng() {
  fail=$((fail + 1))
  echo "FAIL - $1" >&2
}

# expect_pass <名称> <tar.gz>
expect_pass() {
  if verify_archive "$2" 2>"$WORK/err"; then
    ok "$1"
  else
    ng "$1（应通过，实际被拒：$(cat "$WORK/err")）"
  fi
}

# expect_reject <名称> <tar.gz> [期望错误里出现的子串]
expect_reject() {
  local what="$1" pkg="$2" want="${3:-}"
  if verify_archive "$pkg" 2>"$WORK/err"; then
    ng "$what（应被拒，实际通过）"
    return
  fi
  if [ -n "$want" ] && ! grep -qF -- "$want" "$WORK/err"; then
    ng "$what（拒因不符，期望含「$want」，实际：$(cat "$WORK/err")）"
    return
  fi
  ok "$what（$(cat "$WORK/err")）"
}

# mkpkg <名>：建一个干净的打包源目录，含可执行的 litetalk-server 与 update.sh
mkpkg() {
  local d="$WORK/src-$1"
  mkdir -p "$d"
  printf '#!/bin/sh\n' >"$d/litetalk-server"
  chmod 755 "$d/litetalk-server"
  printf '#!/bin/sh\n' >"$d/update.sh"
  chmod 755 "$d/update.sh"
  printf '#!/bin/sh\n' >"$d/litetalk-updater"
  chmod 755 "$d/litetalk-updater"
  printf '%s\n' "$d"
}

# ---- 1. 正常包：与 CI package-server 的实际产物同形（三个普通文件）----
d="$(mkpkg good)"
tar -C "$d" -czf "$WORK/good.tgz" litetalk-server litetalk-updater update.sh
expect_pass "正常包（三个普通文件）" "$WORK/good.tgz"
if [ "${#ARCH_NAMES[@]}" -eq 3 ]; then
  ok "校验通过后 ARCH_NAMES 列出 3 个成员"
else
  ng "ARCH_NAMES 数量 = ${#ARCH_NAMES[@]}，want 3"
fi

# ---- 2. 符号链接：名字在白名单内，但 root 解包后 install 会跟随链接 ----
d="$(mkpkg symlink)"
rm -f "$d/litetalk-server"
# Git Bash 默认把 ln -s 退化成复制，必须显式要求真符号链接；Linux 上该变量无副作用。
MSYS=winsymlinks:nativestrict ln -s /bin/sh "$d/litetalk-server"
if [ ! -L "$d/litetalk-server" ]; then
  # 造不出符号链接就没法验证这条闸：明确报失败，而不是悄悄跳过
  ng "白名单名下的符号链接（构造失败：本机 ln -s 没有产生符号链接）"
else
  tar -C "$d" -czf "$WORK/symlink.tgz" litetalk-server litetalk-updater update.sh
  expect_reject "白名单名下的符号链接" "$WORK/symlink.tgz" "符号链接"
fi

# ---- 3. 硬链接 ----
d="$(mkpkg hardlink)"
rm -f "$d/litetalk-updater"
ln "$d/update.sh" "$d/litetalk-updater"
tar -C "$d" -czf "$WORK/hardlink.tgz" litetalk-server litetalk-updater update.sh
expect_reject "硬链接条目" "$WORK/hardlink.tgz" "硬链接"

# ---- 4. FIFO：与设备节点走同一个"非常规文件"分支，且无需 root 即可构造 ----
d="$(mkpkg fifo)"
rm -f "$d/litetalk-updater"
mkfifo "$d/litetalk-updater"
# tar 打包 FIFO 不会读内容，不会挂起；用 --format 默认（gnu/posix 均可）即可
tar -C "$d" -czf "$WORK/fifo.tgz" litetalk-server litetalk-updater update.sh
expect_reject "FIFO 条目" "$WORK/fifo.tgz" "FIFO"

# ---- 5. 目录条目 ----
d="$(mkpkg direntry)"
mkdir -p "$d/assets"
echo x >"$d/assets/foo"
tar -C "$d" -czf "$WORK/direntry.tgz" litetalk-server litetalk-updater update.sh assets
expect_reject "目录条目" "$WORK/direntry.tgz" "目录"

# ---- 6. 白名单外的普通文件 ----
d="$(mkpkg extra)"
echo evil >"$d/payload.sh"
tar -C "$d" -czf "$WORK/extra.tgz" litetalk-server litetalk-updater update.sh payload.sh
expect_reject "白名单外的成员" "$WORK/extra.tgz" "非法成员"

# ---- 7. 缺少 litetalk-server ----
d="$(mkpkg noserver)"
rm -f "$d/litetalk-server"
tar -C "$d" -czf "$WORK/noserver.tgz" litetalk-updater update.sh
expect_reject "缺少 litetalk-server" "$WORK/noserver.tgz" "缺少 litetalk-server"

# ---- 8. 重复条目 ----
d="$(mkpkg dup)"
rm -f "$WORK/dup.tar" "$WORK/dup.tgz"
tar -C "$d" -cf "$WORK/dup.tar" litetalk-server litetalk-updater update.sh
tar -C "$d" -rf "$WORK/dup.tar" litetalk-server
gzip -c "$WORK/dup.tar" >"$WORK/dup.tgz"
expect_reject "重复条目" "$WORK/dup.tgz" "重复"

# ---- 9. 名字带空格：类型与名字按序号对齐，不能因空格错位 ----
d="$(mkpkg space)"
rm -f "$d/litetalk-updater"
echo x >"$d/lit etalk-updater"
tar -C "$d" -czf "$WORK/space.tgz" litetalk-server update.sh "lit etalk-updater"
expect_reject "带空格的成员名" "$WORK/space.tgz" "非法成员"

# ---- 10. --dir 含空白：必须 reject（路径会原样写进 systemd 的 ExecStart）----
# 直接跑安装脚本的参数闸：这些检查在 root/Linux 判定之前，--dry-run 又跳过 root 要求，
# 所以 Linux CI 与本机 Git Bash 都能跑到。
expect_dir_reject() {
  local what="$1" dir="$2" out rc
  # 参数闸以退出码 2 结束，set -e 会在赋值时就终止脚本；临时关掉以便读回退出码。
  set +e
  out="$(bash "$HERE/install.sh" --dry-run --dir "$dir" 2>&1)"
  rc=$?
  set -e
  if [ "$rc" -ne 2 ]; then
    ng "$what（退出码 $rc，want 2：$out）"
    return
  fi
  if ! printf '%s' "$out" | grep -q '空白'; then
    ng "$what（报错信息未说明空白字符：$out）"
    return
  fi
  ok "$what"
}

expect_dir_reject "--dir 含空格" "/opt/lite talk"
expect_dir_reject "--dir 含制表符" "/opt/lite	talk"

echo
echo "install.sh 校验自测：通过 $pass，失败 $fail"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
