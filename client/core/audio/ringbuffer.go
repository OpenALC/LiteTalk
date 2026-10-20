package audio

import "sync/atomic"

// RingBuffer 是 int16 采样的 SPSC（单生产者单消费者）无锁环形缓冲。
//
//   - 容量向上取整到 2 的幂，构造时一次性预分配，读写路径零分配；
//   - head/tail 为单调递增的 uint32 计数（2^32 是容量的整数倍，回绕安全）；
//   - 写满时 Write 只写入可容纳的部分并返回实际写入数（调用方计丢弃）；
//   - 读空时 Read 返回 0（调用方补静音）；
//   - 仅允许一个写 goroutine 和一个读 goroutine 并发使用。
type RingBuffer struct {
	buf  []int16
	mask uint32
	head atomic.Uint32 // 写位置（单调递增）
	tail atomic.Uint32 // 读位置（单调递增）
}

// NewRingBuffer 创建容量至少为 capacity 的环形缓冲（向上取整到 2 的幂）。
func NewRingBuffer(capacity int) *RingBuffer {
	n := 2
	for n < capacity {
		n <<= 1
	}
	return &RingBuffer{
		buf:  make([]int16, n),
		mask: uint32(n - 1),
	}
}

// Capacity 返回实际容量（2 的幂）。
func (r *RingBuffer) Capacity() int { return len(r.buf) }

// Available 返回当前可读采样数。
func (r *RingBuffer) Available() int {
	return int(r.head.Load() - r.tail.Load())
}

// WriteSpace 返回当前可写采样数。
func (r *RingBuffer) WriteSpace() int {
	return len(r.buf) - r.Available()
}

// Write 把 p 写入缓冲，返回实际写入的采样数（可能因满而小于 len(p)）。
// 仅限唯一的生产者调用。
func (r *RingBuffer) Write(p []int16) int {
	if len(p) == 0 {
		return 0
	}
	head := r.head.Load()
	tail := r.tail.Load()
	n := len(r.buf) - int(head-tail) // 剩余可写
	if n > len(p) {
		n = len(p)
	}
	if n <= 0 {
		return 0
	}
	idx := head & r.mask
	first := len(r.buf) - int(idx)
	if first > n {
		first = n
	}
	copy(r.buf[idx:idx+uint32(first)], p[:first])
	copy(r.buf[:n-first], p[first:n])
	// 数据落盘后再推进 head（Go 原子操作为顺序一致，消费者读到新 head 时数据必然可见）。
	r.head.Store(head + uint32(n))
	return n
}

// Read 读出最多 len(out) 个采样，返回实际读出的采样数。
// 仅限唯一的消费者调用。
func (r *RingBuffer) Read(out []int16) int {
	if len(out) == 0 {
		return 0
	}
	tail := r.tail.Load()
	head := r.head.Load()
	n := int(head - tail) // 剩余可读
	if n > len(out) {
		n = len(out)
	}
	if n <= 0 {
		return 0
	}
	idx := tail & r.mask
	first := len(r.buf) - int(idx)
	if first > n {
		first = n
	}
	copy(out[:first], r.buf[idx:idx+uint32(first)])
	copy(out[first:n], r.buf[:n-first])
	r.tail.Store(tail + uint32(n))
	return n
}

// Reset 清空缓冲。非并发安全，仅在读写双方都已停止时调用。
func (r *RingBuffer) Reset() {
	r.tail.Store(0)
	r.head.Store(0)
}
