//go:build cgo && (windows || (linux && (amd64 || arm64)))

// WebRTC APM（webrtc-audio-processing 1.3）的 C++ 封装：AEC3 + NS + AGC2 + 高通。
//
// Go 侧只看到不透明句柄与 int 返回值；所有 C++ 对象与异常都留在这里。
// capture（处理 goroutine）与 render（混音 tick）来自不同 goroutine，
// 因此句柄内的 mutex 同时保护两个方向的 Process 调用与运行期重配置。
#include <cstdint>
#include <memory>
#include <mutex>

#include "modules/audio_processing/include/audio_processing.h"

// C 侧只看到这个不透明句柄；定义留在全局命名空间，与 extern "C" 函数签名一致。
struct lt_apm {
  std::unique_ptr<webrtc::AudioProcessing> apm;
  std::mutex mu;
};

namespace {

struct lt_apm_config {
  bool aec;
  bool ns;
  int ns_level;  // 0 low / 1 moderate / 2 high / 3 very high
  bool agc2;
  bool hpf;
};

void apply_config_locked(lt_apm *h, const lt_apm_config &c) {
  webrtc::AudioProcessing::Config cfg;
  cfg.echo_canceller.enabled = c.aec;
  // AEC 需要高通来去掉直流/低频漂移，AEC3 默认也强制开启；这里与开关一致常开。
  cfg.high_pass_filter.enabled = c.hpf;
  cfg.noise_suppression.enabled = c.ns;
  cfg.noise_suppression.level =
      static_cast<webrtc::AudioProcessing::Config::NoiseSuppression::Level>(c.ns_level);
  cfg.gain_controller2.enabled = c.agc2;
  // 头文件里 adaptive_digital.enabled 默认 false：只开外层开关会退化成
  // 固定增益 0dB + 限幅器，必须同时打开自适应部分才是真正的 AGC2。
  cfg.gain_controller2.adaptive_digital.enabled = c.agc2;
  // 残余回声检测只产出统计量，本项目不消费，关掉省一份每帧计算。
  cfg.residual_echo_detector.enabled = false;
  h->apm->ApplyConfig(cfg);
}

}  // namespace

extern "C" {

struct lt_apm *lt_apm_create(int aec, int ns, int ns_level, int agc2, int hpf) {
  try {
    std::unique_ptr<lt_apm> h(new lt_apm());
    h->apm.reset(webrtc::AudioProcessingBuilder().Create());
    if (!h->apm) {
      return nullptr;
    }
    apply_config_locked(h.get(), lt_apm_config{aec != 0, ns != 0, ns_level, agc2 != 0, hpf != 0});

    // 全链路固定 48k / mono / 10ms：与 LiteTalk 管线帧结构一致，不做重采样。
    webrtc::ProcessingConfig pc;
    pc.input_stream() = webrtc::StreamConfig(48000, 1);
    pc.output_stream() = webrtc::StreamConfig(48000, 1);
    pc.reverse_input_stream() = webrtc::StreamConfig(48000, 1);
    pc.reverse_output_stream() = webrtc::StreamConfig(48000, 1);
    if (h->apm->Initialize(pc) != webrtc::AudioProcessing::kNoError) {
      return nullptr;
    }
    return h.release();
  } catch (...) {
    return nullptr;
  }
}

void lt_apm_destroy(struct lt_apm *h) {
  if (!h) {
    return;
  }
  try {
    delete h;
  } catch (...) {
    // 析构期的异常无处可去；吞掉避免跨过 C 边界。
  }
}

int lt_apm_configure(struct lt_apm *h, int aec, int ns, int ns_level, int agc2, int hpf) {
  if (!h) {
    return -1;
  }
  try {
    std::lock_guard<std::mutex> lk(h->mu);
    apply_config_locked(h, lt_apm_config{aec != 0, ns != 0, ns_level, agc2 != 0, hpf != 0});
    return 0;
  } catch (...) {
    return -2;
  }
}

int lt_apm_process_capture(struct lt_apm *h, int16_t *frame, int samples) {
  if (!h || !frame || samples != 480) {
    return -1;
  }
  try {
    std::lock_guard<std::mutex> lk(h->mu);
    webrtc::StreamConfig sc(48000, 1);
    return h->apm->ProcessStream(frame, sc, sc, frame);
  } catch (...) {
    return -2;
  }
}

int lt_apm_process_render(struct lt_apm *h, int16_t *frame, int samples) {
  if (!h || !frame || samples != 480) {
    return -1;
  }
  try {
    std::lock_guard<std::mutex> lk(h->mu);
    webrtc::StreamConfig sc(48000, 1);
    return h->apm->ProcessReverseStream(frame, sc, sc, frame);
  } catch (...) {
    return -2;
  }
}

}  // extern "C"
