# 预编译第三方 C 库清单

> 本目录存放客户端音频链路的预编译 C 库（Windows x86_64 / UCRT64），
> 来源均为清华 TUNA MSYS2 仓库（mingw-w64 UCRT64 包），校验与版本如下。
> 详细构建环境说明见仓库根 docs/M1-环境前置.md（本地文档，不入库）。

| 目录 | 库 | 版本 | 来源包 | 许可 |
|---|---|---|---|---|
| `opus/` | libopus | 1.6.1 | `mingw-w64-ucrt-x86_64-opus-1.6.1-1` | BSD-3-Clause |
| `opusfile/` | libopusfile / libopusurl | 0.12 | `mingw-w64-ucrt-x86_64-opusfile-0.12-4` | BSD-3-Clause |
| `ogg/` | libogg | 1.3.6 | `mingw-w64-ucrt-x86_64-libogg-1.3.6-1` | BSD-3-Clause |

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

## 链接方式

- **发布版（静态）**：`-Wl,-Bstatic -Wl,--start-group -lopusfile -lopus -logg -lm -Wl,--end-group -Wl,-Bdynamic -static-libgcc`，产物仅依赖 KERNEL32 + UCRT 系统 DLL
- **开发期（动态）**：默认 `-lopus -lopusfile` 走导入库，运行时需把对应 `bin/` 加入 PATH

## 注意

- `gopkg.in/hraban/opus.v2` 的 `#cgo pkg-config: opus opusfile` 需要构建机提供
  `pkg-config`（winlibs 自带 `pkgconf.exe`）并设置 `PKG_CONFIG_PATH` 指向上述 pkgconfig 目录；
  项目内的 Opus 封装（`core/codec`）使用 `${SRCDIR}` 相对路径的 #cgo 指令，不依赖 pkg-config。
