// Package snowflake 生成时间有序的 64 位 ID（#31）。
// 布局：1 符号位 + 41 毫秒时间戳 + 10 机器 ID + 12 序列号，纪元 2026-01-01 UTC。
// 旧的 AUTO_INCREMENT 小 ID 与新 Snowflake ID 混排时时间序仍成立（旧值恒小）。
// 进程内全局一个生成器；机器 ID 取 LITETALK_SNOWFLAKE_WORKER（0–1023），缺省 0。
// 多实例部署必须给每个实例分配不同 worker，否则同毫秒可能撞 ID。
package snowflake

import (
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	// EpochMS 纪元：2026-01-01T00:00:00Z。固定常量，改它等于换 ID 空间。
	EpochMS = 1767225600000
	// WorkerBits / SeqBits / 时间戳位宽与布局（1+41+10+12 = 64）。
	workerBits  = 10
	seqBits     = 12
	timeShift   = workerBits + seqBits
	workerShift = seqBits
	maxWorker   = (1 << workerBits) - 1
	maxSeq      = (1 << seqBits) - 1
)

// Generator 一台机器上的 ID 发生器。同毫秒内序列号自增；时钟回拨时沿用
// 上一毫秒继续发号（单调不减），极端回拨不重发旧值。
type Generator struct {
	worker int64
	state  atomic.Uint64 // 高 42 位：毫秒时间戳（自 EpochMS）；低 12 位：序列号
}

// New 创建指定 worker 的生成器（测试用；生产用 Next）。
func New(worker int64) *Generator {
	if worker < 0 || worker > maxWorker {
		worker = 0
	}
	return &Generator{worker: worker}
}

// Next 生成下一个 ID。热路径零分配（两个 atomic + time.Now）。
func (g *Generator) Next() int64 {
	ms := time.Now().UnixMilli() - EpochMS
	if ms < 0 {
		ms = 0
	}
	for {
		cur := g.state.Load()
		lastMs := int64(cur >> seqBits)
		seq := int64(cur & maxSeq)
		switch {
		case ms > lastMs:
			lastMs, seq = ms, 0
		case seq < maxSeq:
			seq++
		default:
			// 同毫秒序列号耗尽：借下一毫秒继续，保证不重复不回退。
			lastMs++
			seq = 0
		}
		next := uint64(lastMs)<<seqBits | uint64(seq)
		if g.state.CompareAndSwap(cur, next) {
			return lastMs<<timeShift | g.worker<<workerShift | seq
		}
	}
}

// defaultGen 进程内全局生成器；worker 从环境变量读一次。
var defaultGen = New(workerFromEnv())

// workerFromEnv 解析 LITETALK_SNOWFLAKE_WORKER；非法值回落 0（安全默认）。
func workerFromEnv() int64 {
	v := os.Getenv("LITETALK_SNOWFLAKE_WORKER")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > maxWorker {
		return 0
	}
	return n
}

// Next 全局生成器发号。store 各 Create 显式赋 ID 用它（新行不再依赖 AUTO_INCREMENT）。
func Next() int64 { return defaultGen.Next() }
