# 预编译第三方 C 库清单

> 本目录存放客户端音频链路的预编译 C/C++ 库与头文件。
> `opus/` 为官方 opus-1.6.1 源码构建的 Windows x86_64 / UCRT64 静态库，
> `opus-linux-amd64/`、`opus-linux-arm64/` 为同一源码的 Linux 交叉静态库；
> 三者都带 `--enable-dred`（#44）。`opusfile/`、`ogg/` 为 Windows x86_64 /
> UCRT64 预编译包，来源为清华 TUNA MSYS2 仓库；`webrtc-apm/` 为仓库内构建的
> WebRTC APM 1.3 静态库（Windows amd64 / Linux amd64 / Linux arm64 三套），
> 构建细节见各自的 README。详细构建环境说明见仓库根 docs/M1-环境前置.md
> （本地文档，不入库）。

| 目录 | 库 | 版本 | 来源包 | 许可 |
|---|---|---|---|---|
| `opus/` | libopus（含 DRED） | 1.6.1 | 官方 `opus-1.6.1.tar.gz`，`--enable-dred` | BSD-3-Clause |
| `opus-linux-amd64/` | libopus（含 DRED，Linux amd64） | 1.6.1 | 同上，`x86_64-linux-gnu` 交叉 | BSD-3-Clause |
| `opus-linux-arm64/` | libopus（含 DRED，Linux arm64） | 1.6.1 | 同上，`aarch64-linux-gnu` 交叉 | BSD-3-Clause |
| `opusfile/` | libopusfile / libopusurl | 0.12 | `mingw-w64-ucrt-x86_64-opusfile-0.12-4` | BSD-3-Clause |
| `ogg/` | libogg | 1.3.6 | `mingw-w64-ucrt-x86_64-libogg-1.3.6-1` | BSD-3-Clause |
| `webrtc-apm/` | libwebrtc-apm（WebRTC APM 1.3 + abseil） | 1.3 | 源码自建，见 `webrtc-apm/README.md` | BSD-3-Clause + Apache-2.0（abseil） |

opus 源码 SHA256：`6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1`。
构建参数：`./configure --disable-shared --enable-static --disable-doc
--disable-extra-programs --enable-dred`，Windows host 为 `x86_64-w64-mingw32`，
Linux arm64 host 为 `aarch64-linux-gnu`；产物 `strip --strip-debug`。
`opus-linux-*` 只保留 `lib/`（头文件与 Windows 共用 `opus/include/`）。

每个包的布局：

```
<pkg>/
├── include/            # 头文件
├── lib/
│   ├── lib*.a          # 静态库（发布版静态链接，单 exe 零第三方 DLL）
│   ├── lib*.dll.a      # 导入库（开发期动态链接用）
│   └── pkgconfig/*.pc  # 已改写为 ${pcfiledir} 相对路径，与机器无关
├── bin/                # 运行时 DLL（仅动态链接开发期需要）
└── COPYING             # 许可证
```

`webrtc-apm/` 例外：没有 `bin/` 与 pkgconfig，`lib/` 下按
`windows-amd64/`、`linux-amd64/`、`linux-arm64/` 分平台存放同名
`libwebrtc-apm.a`，由 `core/audio/apm_cgo.go` 的 `#cgo` 指令按目标平台选路径。

## 产物校验（#117）

`SHA256SUMS` 记录本目录每个 `.a` 的 SHA256，并在注释里写明上游版本与构建
参数。校验：

```bash
cd client/third_party && sha256sum -c SHA256SUMS
```

CI 的 `check-client-linux` 与 `check-client-windows` 各跑一次，库被替换或重建
却没同步清单就红。重建任何静态库后必须：

1. 更新 `SHA256SUMS` 对应的行（并同步注释里的版本/构建参数）；
2. 在提交信息里写明变更原因（上游升级 / 构建参数变化）。

## 分发合规（#117）

发布包静态链接上述库，二进制分发必须随附许可证全文。随附内容由
`client/build/third-party-licenses.sh` 生成并打进各产物：

| 产物 | 许可证位置 |
|---|---|
| deb / rpm | `/usr/share/doc/litetalk/third-party/` |
| Linux 绿色版 tar.gz | `share/doc/litetalk/third-party/` |
| Windows 安装器 | 安装目录下 `third-party\` |
| Windows 绿色版 zip | zip 内 `third-party\` |

各组件许可证来源：`opus/COPYING`、`opusfile/COPYING`、`ogg/COPYING`、
`webrtc-apm/COPYING`（BSD-3-Clause）与 `webrtc-apm/LICENSE.abseil`
（abseil 的 Apache-2.0 全文，静态合并进 libwebrtc-apm.a）。

## 链接方式

- **发布版（静态）**：`-Wl,-Bstatic -Wl,--start-group -lopusfile -lopus -logg -lm -Wl,--end-group -Wl,-Bdynamic -static-libgcc`，产物仅依赖 KERNEL32 + UCRT 系统 DLL
- **开发期（动态）**：默认 `-lopus -lopusfile` 走导入库，运行时需把对应 `bin/` 加入 PATH
- **Linux（#44）**：`core/codec/opus_linux.go` 按 `linux,amd64` / `linux,arm64`
  选 `opus-linux-*/lib` 静态链接（`-lopus -lm -static-libgcc`），构建机不需要
  `libopus-dev`，产物也不再动态依赖 `libopus.so`
- **WebRTC APM**：Windows 全静态（`-static-libgcc -static-libstdc++`，无第三方 DLL）；
  Linux 静态链接 APM 本身，允许动态依赖系统 `libstdc++`（`-lstdc++`），
  与打包依赖清单一致

## 注意

- `gopkg.in/hraban/opus.v2` 的 `#cgo pkg-config: opus opusfile` 需要构建机提供
  `pkg-config`（winlibs 自带 `pkgconf.exe`）并设置 `PKG_CONFIG_PATH` 指向上述 pkgconfig 目录；
  项目内的 Opus 封装（`core/codec`）使用 `${SRCDIR}` 相对路径的 #cgo 指令，不依赖 pkg-config。
