// audioloop 是 M1 音频管线的本地回环自检 CLI。
//
//	--list                枚举采集/播放设备后退出
//	--null                无声卡自检（合成信号源 + 计数汇），可在无声设备上运行
//	（默认）              真实声卡回环试听：麦克风 → 降噪 → Opus → 本地解码 → 扬声器
//
// 例：
//
//	audioloop --list
//	audioloop --null --duration 3
//	audioloop --duration 10 --gain 1.5 --denoise=true --capture "Microphone"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/codec"
	"github.com/OpenALC/LiteTalk/client/internal/audio"
	"github.com/gen2brain/malgo"
)

func main() {
	var (
		list     = flag.Bool("list", false, "枚举音频设备后退出")
		nullBack = flag.Bool("null", false, "无声卡自检模式（合成源，不出声）")
		duration = flag.Int("duration", 5, "运行时长（秒）")
		gain     = flag.Float64("gain", 1.0, "线性增益")
		denoise  = flag.Bool("denoise", true, "开关 RNNoise 降噪")
		capture  = flag.String("capture", "", "采集设备（ID 十六进制串或名称子串，空=默认）")
		playback = flag.String("playback", "", "播放设备（同上）")
	)
	flag.Parse()

	if *list {
		listDevices()
		return
	}

	cfg := audio.DefaultPipelineConfig()
	cfg.Loopback = true
	cfg.DSP.Gain = *gain
	cfg.DSP.Denoise = *denoise
	cfg.CaptureSelector = *capture
	cfg.PlaybackSelector = *playback
	if *nullBack {
		cfg.Backend = audio.BackendNull
	}
	cfg.DSP.OnSpeakingChange = func(b bool) {
		if b {
			fmt.Println("[speaking] ON")
		} else {
			fmt.Println("[speaking] OFF")
		}
	}

	p, err := audio.NewPipeline(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init pipeline failed:", err)
		os.Exit(1)
	}
	defer p.Close()

	if *nullBack {
		fmt.Printf("null 后端自检：%ds（合成 440Hz 正弦+噪声 → DSP → Opus → 本地解码 → 计数汇）\n", *duration)
	} else {
		fmt.Println("警告：真实声卡回环（麦克风→扬声器），不戴耳机会啸叫！请佩戴耳机。")
		fmt.Printf("回环试听：%ds，gain=%.2f denoise=%v\n", *duration, *gain, *denoise)
	}
	fmt.Printf("libopus %s\n", codec.Version())

	if err := p.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start pipeline failed:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	select {
	case <-ctx.Done():
		fmt.Println("\n收到中断，停止中…")
	case <-time.After(time.Duration(*duration) * time.Second):
	}
	p.Stop()

	printStats(p.Stats())
}

func listDevices() {
	ctx, err := audio.NewContext(func(msg string) { fmt.Fprintf(os.Stderr, "miniaudio: %s", msg) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "init audio context failed:", err)
		os.Exit(1)
	}
	defer ctx.Close()

	caps, err := ctx.Devices(malgo.Capture)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enumerate capture devices failed:", err)
		os.Exit(1)
	}
	plays, err := ctx.Devices(malgo.Playback)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enumerate playback devices failed:", err)
		os.Exit(1)
	}

	fmt.Printf("=== 采集设备（%d）===\n", len(caps))
	for i, d := range caps {
		fmt.Printf("  [%d] %s (default=%v)\n      id=%s\n", i, d.Name, d.IsDefault, d.ID)
	}
	fmt.Printf("=== 播放设备（%d）===\n", len(plays))
	for i, d := range plays {
		fmt.Printf("  [%d] %s (default=%v)\n      id=%s\n", i, d.Name, d.IsDefault, d.ID)
	}
}

func printStats(s audio.StatsSnapshot) {
	fmt.Println("=== 管线统计 ===")
	fmt.Printf("  DSP 帧（10ms）:    %d\n", s.FramesProcessed)
	fmt.Printf("  Opus 包:           encoded=%d decoded=%d (avg %.1f B ≈ %.1f kbps)\n",
		s.PacketsEncoded, s.PacketsDecoded, s.AvgPacketBytes, s.AvgPacketBytes*8*50/1000)
	fmt.Printf("  编解码错误:        enc=%d dec=%d\n", s.EncodeErrors, s.DecodeErrors)
	fmt.Printf("  采集:              callbacks=%d samples=%d dropped=%d\n",
		s.CaptureCallbacks, s.CaptureSamples, s.CaptureDropped)
	fmt.Printf("  播放:              callbacks=%d samples=%d underruns=%d dropped=%d\n",
		s.PlaybackCallbacks, s.PlaybackSamples, s.PlaybackUnderruns, s.PlaybackDropped)
	fmt.Printf("  说话状态翻转:      %d 次\n", s.SpeakingChanges)
	if s.LatencyAvgUS > 0 {
		fmt.Printf("  回环延迟估计:      avg %.1fms max %.1fms\n",
			float64(s.LatencyAvgUS)/1000, float64(s.LatencyMaxUS)/1000)
	}
}
