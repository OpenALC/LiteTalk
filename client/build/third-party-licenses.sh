#!/usr/bin/env bash
# 生成第三方许可随附目录（deb/rpm / 绿色版 tar / Windows 安装器与 zip 共用）。
#
# 用法：build/third-party-licenses.sh <输出目录>
# 产物：
#   <输出目录>/third-party/THIRD-PARTY.md   —— 组件、版本、许可与静态链接说明
#   <输出目录>/third-party/<组件>/COPYING … —— 各组件许可证全文
#
# 为什么是脚本而不是在 CI 里逐个 cp：三处打包（Linux tar、nfpm、Windows zip +
# NSIS）必须出同一份内容，路径写错一处就是法律上的漏发（#117）。
set -euo pipefail

out="${1:?用法: build/third-party-licenses.sh <输出目录>}"
# 脚本位于 client/build/，第三方目录在 ../third_party。
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/third_party"
dest="$out/third-party"

[ -d "$root" ] || { echo "未找到 $root" >&2; exit 1; }

rm -rf "$dest"
mkdir -p "$dest"

# 组件目录名与本脚本用到本仓库的文件一一对应；缺文件即失败，不允许静默漏发。
copy_lic() {
  local comp="$1" file="$2"
  [ -f "$root/$comp/$file" ] || { echo "缺少许可证文件: $comp/$file" >&2; exit 1; }
  mkdir -p "$dest/$comp"
  cp "$root/$comp/$file" "$dest/$comp/$file"
}

copy_lic opus COPYING
copy_lic opusfile COPYING
copy_lic ogg COPYING
copy_lic webrtc-apm COPYING
# abseil 静态合并进 libwebrtc-apm.a，其 Apache-2.0 全文必须一并分发。
copy_lic webrtc-apm LICENSE.abseil

cat > "$dest/THIRD-PARTY.md" <<'EOF'
# 第三方组件与许可

轻音 LiteTalk 客户端在发布版中静态链接下列第三方库。各组件许可证全文见
同目录下的对应子目录。

| 组件 | 版本 | 许可 | 用途 |
|---|---|---|---|
| libopus | 1.6.1（`--enable-dred`） | BSD-3-Clause | 语音编解码（含深度冗余） |
| libopusfile | 0.12 | BSD-3-Clause | Opus 音频文件解析 |
| libogg | 1.3.6 | BSD-3-Clause | Ogg 容器 |
| webrtc-audio-processing（WebRTC APM） | 1.3 | BSD-3-Clause | 回声消除 / 自动增益 / 降噪 |
| abseil | 20230125.1 | Apache-2.0 | APM 依赖（已静态合并进 APM 库） |

静态链接意味着上述代码的二进制形式包含在本产品中，因此按各许可证要求，
二进制分发必须随附版权声明与许可证全文——即本目录。

构建参数、源码包 SHA256 与各静态库产物的校验和见仓库内
`client/third_party/SHA256SUMS`。
EOF

echo "已生成 $dest"
find "$dest" -type f | sed 's/^/  /'
