#!/usr/bin/env bash
# 宿主与发行版容器共用的启动冒烟判定（#5）。
# ServiceStartup 的启动日志早于 GTK/WebKit 窗口创建；只有前端通过 Wails
# 绑定调用 GetHibernateMode 后才会写出 frontend-ready。
set -euo pipefail

code=${1:?用法: assert-smoke-ready.sh <退出码> <日志文件>}
log=${2:?用法: assert-smoke-ready.sh <退出码> <日志文件>}

if [ "$code" -ne 124 ]; then
  echo "ERROR: litetalk 退出码 $code（期望 124：限时内持续运行）"
  exit 1
fi
if ! grep -Eq '=== LiteTalk [^ ]+ 启动 \(pid=[0-9]+\) ===' "$log"; then
  echo "ERROR: 缺少 ServiceStartup 启动日志"
  exit 1
fi
if ! grep -Fq 'litetalk-smoke:frontend-ready' "$log"; then
  echo "ERROR: 缺少前端就绪标记（页面未通过 Wails 绑定调用 GetHibernateMode）"
  exit 1
fi

echo '启动冒烟通过：进程持续运行，GTK/WebKit 页面已执行前端 JS 并调用 Go 绑定'
