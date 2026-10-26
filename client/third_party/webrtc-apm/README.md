# WebRTC APM（webrtc-audio-processing 1.3）预编译静态库

LiteTalk 客户端用 WebRTC APM 做回声消除（AEC3）、自动增益（AGC2）、
中等强度降噪（NS moderate）与高通滤波；RNNoise 保留为可选的强降噪叠加。
本目录只存放链接期产物，不参与 Go 编译以外的任何构建步骤。

## 来源与版本

| 项 | 值 |
|---|---|
| 上游 | https://gitlab.freedesktop.org/pulseaudio/webrtc-audio-processing |
| 版本 | `v1.3`（2023-09） |
| 源码包 | `webrtc-audio-processing-v1.3.tar.gz` |
| 下载地址 | https://gitlab.freedesktop.org/pulseaudio/webrtc-audio-processing/-/archive/v1.3/webrtc-audio-processing-v1.3.tar.gz |
| SHA256 | `b04404884e705464c1a2da0d5036ae0fc02d87ed150faa7da4536c99d6966660` |
| 许可证 | webrtc-audio-processing：BSD-3-Clause（本目录 `COPYING`）；abseil：Apache-2.0（见下文） |
| abseil | 20230125.1（上游 meson wrap 拉取，静态合并进库） |

## 许可证

- webrtc-audio-processing 本体：BSD-3-Clause，全文见本目录 `COPYING`；
  其内置的 pffft/fft/rnnoise 等第三方组件保留各自 BSD 风格许可证头。
- abseil 20230125.1：Apache-2.0。vendored 头文件逐个保留了 Apache-2.0 版权头；
  许可证全文见 https://www.apache.org/licenses/LICENSE-2.0 。
- 静态库把上述代码合并为单一归档，分发二进制时 `COPYING` 与本节说明需随附。

## 目录布局

```
webrtc-apm/
├── include/webrtc-audio-processing-1/   # 上游头文件（仅 .h/.inc）+ abseil 头文件
├── lib/
│   ├── windows-amd64/libwebrtc-apm.a
│   ├── linux-amd64/libwebrtc-apm.a
│   └── linux-arm64/libwebrtc-apm.a
├── COPYING
└── README.md
```

每个 `libwebrtc-apm.a` 都是用 `ar -M` 合并后的单一静态库：webrtc 的
`modules/audio_processing`、`common_audio`、`rtc_base`、`system_wrappers`、
`third_party/{fft,pffft,rnnoise}` 与 abseil 的 base/container/... 全部打进去，
Go 侧只需 `-lwebrtc-apm` 一个库名。

## 与 Go 的对接

`client/core/audio/apm_shim.cc` 是 C++ 薄封装（不透明句柄 + `int` 返回，
异常不出 shim），`client/core/audio/apm_cgo.go` 用 `${SRCDIR}` 相对路径引用
本目录的头文件与对应平台静态库。shim 首行带与 `apm_cgo.go` 相同的 build tag，
macOS 等未接入平台不会编译它。关键编译参数：

- `-std=gnu++17`：GCC 16 的 libstdc++ 头文件在严格 ISO 模式下会因
  `__float128` 的 `Q` 后缀报错，GNU 方言可避免使用 cgo 拒绝的
  `-fext-numeric-literals`；
- `-DWEBRTC_WIN -D_WIN32` / `-DWEBRTC_LINUX -DWEBRTC_POSIX -DWEBRTC_THREAD_RR`：
  与建库时一致，保证头文件里的平台分支一致；
- `-DWEBRTC_APM_DEBUG_DUMP=0`：不启用 APM 调试转储；
- Windows 链接参数 `-lwinmm -Wl,-Bstatic -lstdc++ -lwinpthread -Wl,-Bdynamic
  -static-libgcc -static-libstdc++`，产物只依赖 KERNEL32/WINMM/UCRT 系统 DLL，
  不引入 libstdc++-6.dll / libwinpthread-1.dll；
- Linux 链接参数 `-lstdc++ -lm -lpthread`（允许动态依赖系统 libstdc++）。

macOS 等未提供静态库的平台由 `apm_stub.go` 提供 no-op 实现，
`apmSupported()` 返回 false，DSP 自动回落 RNNoise/自研 AGC 链路。

## 构建步骤

以下命令在源码树根目录执行；Windows 使用 winlibs GCC（mingw-w64），
Linux amd64 使用 WSL GCC 15，arm64 使用 `aarch64-linux-gnu-g++` 交叉编译。

### 1. 取证与打补丁

```bash
curl -LO https://gitlab.freedesktop.org/pulseaudio/webrtc-audio-processing/-/archive/v1.3/webrtc-audio-processing-v1.3.tar.gz
echo "b04404884e705464c1a2da0d5036ae0fc02d87ed150faa7da4536c99d6966660  webrtc-audio-processing-v1.3.tar.gz" | sha256sum -c
tar xf webrtc-audio-processing-v1.3.tar.gz
cd webrtc-audio-processing-v1.3
```

GCC 16 / Windows 兼容补丁（三处）：

1. `rtc_base/system/file_wrapper.h`：在 `<stddef.h>` 后补 `#include <stdint.h>`
   （mingw 的 `<stdio.h>` 不再间接提供 `uint64_t`）。
2. `meson.build`：C++ 参数加 `-fext-numeric-literals`；并加
   `add_project_arguments('-include', 'cstdint', language: 'cpp')`
   （abseil 模板在实例化处才用到 `uintptr_t`）。
3. `subprojects/abseil-cpp-20230125.1/absl/container/internal/container_memory.h`：
   在第一个 `#include` 前补 `#include <cstdint>`。

仓库内 vendored 头文件保持上游原样；cgo 侧由 `apm_shim.cc` 在包含 WebRTC
头之前先 `#include <cstdint>`，等价于源码构建里的 `-include cstdint`
（cgo 会拒绝 `-include` 参数）。

### 2. meson 构建

Windows（`mingw-native.ini`）：

```ini
[binaries]
c = 'C:/tools/mingw64/bin/gcc.exe'
cpp = 'C:/tools/mingw64/bin/g++.exe'
ar = 'C:/tools/mingw64/bin/ar.exe'
strip = 'C:/tools/mingw64/bin/strip.exe'

[built-in options]
c_args = ['-D__STDC_FORMAT_MACROS=1']
cpp_args = ['-D__STDC_FORMAT_MACROS=1', '-fext-numeric-literals']
c_link_args = ['-static-libgcc']
cpp_link_args = ['-static-libgcc', '-static-libstdc++']
```

```bash
meson setup build --cross-file=mingw-native.ini
meson compile -C build
```

Linux 用本机 g++；arm64 用 `aarch64-cross.ini`（`system='linux'`、
`cpu_family='aarch64'`，编译器换成 `aarch64-linux-gnu-{gcc,g++,ar,strip}`）。
Linux 构建同样需要第 2 处的 `-include cstdint`，但不需要 `-fext-numeric-literals`。

### 3. 合并静态库

用 GNU ar 的 MRI 脚本把 abseil 与 webrtc 各子库合成一个归档：

```
create libwebrtc-apm.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_base.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_container.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_crc.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_debugging.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_flags.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_hash.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_log.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_numeric.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_profiling.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_random.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_status.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_strings.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_synchronization.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_time.a
addlib subprojects/abseil-cpp-20230125.1/libabsl_types.a
addlib webrtc/api/liblibapi.a
addlib webrtc/common_audio/libcommon_audio.a
addlib webrtc/common_audio/libcommon_audio_avx.a      # x86 only
addlib webrtc/common_audio/libcommon_audio_sse2.a     # x86 only
addlib webrtc/modules/audio_processing/libwebrtc-audio-processing-1.a
addlib webrtc/modules/audio_processing/libwebrtc_audio_processing_privatearch.a
addlib webrtc/modules/third_party/fft/liblibfft.a
addlib webrtc/rtc_base/liblibbase.a
addlib webrtc/system_wrappers/libsystem_wrappers.a
addlib webrtc/third_party/pffft/liblibpffft.a
addlib webrtc/third_party/rnnoise/liblibrnnoise.a
save
end
```

```bash
ar -M < combine-apm.mri
```

arm64 的 MRI 去掉 `common_audio_avx` / `common_audio_sse2`（无 x86 优化对象）。
产物分别放到 `lib/windows-amd64/`、`lib/linux-amd64/`、`lib/linux-arm64/`。

### 4. 头文件裁剪

从构建目录复制 `webrtc/` 与 `subprojects/abseil-cpp-20230125.1/absl/` 下的
`.h`/`.inc`（保留目录结构，不复制 `.cc`/`.c`），作为
`include/webrtc-audio-processing-1/`。`#include "modules/..."` 等路径保持不变。

## 完整性自检

`client/core/audio/apm_test.go` 的 `TestAPMSmoke` 覆盖创建/连续 100 帧
capture+render；`TestAECSuppressesEcho` 用合成的延迟回声验证收敛（ERLE ≥ 6dB），
`TestAECDisabledPassesEcho` 作为直通对照组。Windows/Linux CI 均会真实运行
cgo 路径；macOS 为 stub，只有 `apmSupported()==false` 的回落行为。

## 升级注意

- 新版本必须同时更新头文件、三个平台的静态库与本文档的版本/校验和；
- 升级后重跑 `TestAECSuppressesEcho`，并复核 `go test -bench BenchmarkDSPProcess`
  的每帧开销与 0 allocs；
- `AudioProcessing::Config` 的字段随上游版本变化，`apm_shim.cc` 需按新头文件同步。
