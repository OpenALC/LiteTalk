# 预编译第三方 C 库清单

> 本目录存放客户端音频链路的预编译 C/C++ 库与头文件。
> `opus/`、`opusfile/`、`ogg/` 为 Windows x86_64 / UCRT64 预编译包，来源为
> 清华 TUNA MSYS2 仓库；`webrtc-apm/` 为仓库内构建的 WebRTC APM 1.3
> 静态库（Windows amd64 / Linux amd64 / Linux arm64 三套），构建细节见
> 各自的 README。详细构建环境说明见仓库根 docs/M1-环境前置.md（本地文档，不入库）。

| 目录 | 库 | 版本 | 来源包 | 许可 |
|---|---|---|---|---|
| `opus/` | libopus | 1.6.1 | `mingw-w64-ucrt-x86_64-opus-1.6.1-1` | BSD-3-Clause |
| `opusfile/` | libopusfile / libopusurl | 0.12 | `mingw-w64-ucrt-x86_64-opusfile-0.12-4` | BSD-3-Clause |
| `ogg/` | libogg | 1.3.6 | `mingw-w64-ucrt-x86_64-libogg-1.3.6-1` | BSD-3-Clause |
| `webrtc-apm/` | libwebrtc-apm（WebRTC APM 1.3 + abseil） | 1.3 | 源码自建，见 `webrtc-apm/README.md` | BSD-3-Clause |

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

## 链接方式

- **发布版（静态）**：`-Wl,-Bstatic -Wl,--start-group -lopusfile -lopus -logg -lm -Wl,--end-group -Wl,-Bdynamic -static-libgcc`，产物仅依赖 KERNEL32 + UCRT 系统 DLL
- **开发期（动态）**：默认 `-lopus -lopusfile` 走导入库，运行时需把对应 `bin/` 加入 PATH
- **WebRTC APM**：Windows 全静态（`-static-libgcc -static-libstdc++`，无第三方 DLL）；
  Linux 静态链接 APM 本身，允许动态依赖系统 `libstdc++`（`-lstdc++`），
  与打包依赖清单一致

## 注意

- `gopkg.in/hraban/opus.v2` 的 `#cgo pkg-config: opus opusfile` 需要构建机提供
  `pkg-config`（winlibs 自带 `pkgconf.exe`）并设置 `PKG_CONFIG_PATH` 指向上述 pkgconfig 目录；
  项目内的 Opus 封装（`core/codec`）使用 `${SRCDIR}` 相对路径的 #cgo 指令，不依赖 pkg-config。
