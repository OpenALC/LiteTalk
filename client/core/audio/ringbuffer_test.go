package audio

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRingBufferBasic(t *testing.T) {
	r := NewRingBuffer(1024)
	if r.Capacity() != 1024 {
		t.Fatalf("capacity = %d, want 1024", r.Capacity())
	}
	in := make([]int16, 600)
	for i := range in {
		in[i] = int16(i)
	}
	if n := r.Write(in); n != 600 {
		t.Fatalf("Write = %d, want 600", n)
	}
	if r.Available() != 600 || r.WriteSpace() != 424 {
		t.Fatalf("Available=%d WriteSpace=%d", r.Available(), r.WriteSpace())
	}
	out := make([]int16, 600)
	if n := r.Read(out); n != 600 {
		t.Fatalf("Read = %d, want 600", n)
	}
	for i := range out {
		if out[i] != int16(i) {
			t.Fatalf("out[%d] = %d, want %d", i, out[i], i)
		}
	}
	if r.Available() != 0 {
		t.Fatalf("Available after drain = %d, want 0", r.Available())
	}
}

// TestRingBufferWrapAround 覆盖回绕路径：写-读-再写越过数组末尾。
func TestRingBufferWrapAround(t *testing.T) {
	r := NewRingBuffer(1024)
	buf := make([]int16, 800)

	// 写 800、读 600，head=800 tail=600。
	for i := range buf {
		buf[i] = int16(i % 30000)
	}
	r.Write(buf)
	got := make([]int16, 600)
	r.Read(got)
	for i := range got {
		if got[i] != int16(i%30000) {
			t.Fatalf("phase1 got[%d]=%d", i, got[i])
		}
	}

	// 再写 800：head 从 800 前进，数据在物理数组上回绕。
	for i := range buf {
		buf[i] = int16((800 + i) % 30000)
	}
	if n := r.Write(buf); n != 800 {
		t.Fatalf("phase2 Write = %d, want 800", n)
	}
	if r.Available() != 1000 {
		t.Fatalf("Available = %d, want 1000", r.Available())
	}
	got = make([]int16, 1000)
	if n := r.Read(got); n != 1000 {
		t.Fatalf("phase2 Read = %d, want 1000", n)
	}
	for i := range got {
		want := int16((600 + i) % 30000) // 第一次读剩下的 200 个 + 第二次写的 800 个
		if got[i] != want {
			t.Fatalf("phase2 got[%d]=%d, want %d", i, got[i], want)
		}
	}
}

// TestRingBufferFullEmpty 满时写返回 0、空时读返回 0、部分写正确截断。
func TestRingBufferFullEmpty(t *testing.T) {
	r := NewRingBuffer(8)
	if n := r.Read(make([]int16, 4)); n != 0 {
		t.Fatalf("empty Read = %d, want 0", n)
	}
	in := make([]int16, 10)
	for i := range in {
		in[i] = int16(i + 1)
	}
	if n := r.Write(in); n != 8 {
		t.Fatalf("Write into 8-cap = %d, want 8 (partial)", n)
	}
	if n := r.Write(in); n != 0 {
		t.Fatalf("Write into full = %d, want 0", n)
	}
	out := make([]int16, 4)
	if n := r.Read(out); n != 4 {
		t.Fatalf("Read = %d, want 4", n)
	}
	if n := r.Write(in[:4]); n != 4 {
		t.Fatalf("Write after partial drain = %d, want 4", n)
	}
	out = make([]int16, 8)
	if n := r.Read(out); n != 8 {
		t.Fatalf("final Read = %d, want 8", n)
	}
	// FIFO：先读出首轮遗留的 5,6,7,8，再读到刚写入的 1,2,3,4。
	want := []int16{5, 6, 7, 8, 1, 2, 3, 4}
	for i := range out {
		if out[i] != want[i] {
			t.Fatalf("out[%d]=%d, want %d", i, out[i], want[i])
		}
	}
}

// TestRingBufferCapacityRounding 非 2 的幂容量向上取整。
func TestRingBufferCapacityRounding(t *testing.T) {
	if c := NewRingBuffer(1000).Capacity(); c != 1024 {
		t.Fatalf("capacity = %d, want 1024", c)
	}
	if c := NewRingBuffer(16384).Capacity(); c != 16384 {
		t.Fatalf("capacity = %d, want 16384", c)
	}
}

// TestRingBufferSPSC 并发正确性：200 万采样过容量 1024 的环，序列必须完好。
func TestRingBufferSPSC(t *testing.T) {
	const total = 2_000_000
	r := NewRingBuffer(1024)

	var wg sync.WaitGroup
	wg.Add(2)

	// 生产者：递增值 0..total-1，满则让出 CPU 自旋；部分写入时下次从断点继续。
	go func() {
		defer wg.Done()
		chunk := make([]int16, 137)
		v := 0
		for v < total {
			n := len(chunk)
			if total-v < n {
				n = total - v
			}
			for i := 0; i < n; i++ {
				chunk[i] = int16((v + i) % 30000)
			}
			w := r.Write(chunk[:n])
			if w < n {
				runtime.Gosched()
			}
			v += w
		}
	}()

	// 消费者：严格校验递增序列。
	errCh := make(chan string, 1)
	go func() {
		defer wg.Done()
		chunk := make([]int16, 113)
		v := 0
		deadline := time.Now().Add(30 * time.Second)
		for v < total {
			if time.Now().After(deadline) {
				errCh <- "consumer deadline exceeded"
				return
			}
			n := r.Read(chunk)
			if n == 0 {
				runtime.Gosched()
				continue
			}
			for i := 0; i < n; i++ {
				want := int16((v + i) % 30000)
				if chunk[i] != want {
					errCh <- "sequence corrupted"
					return
				}
			}
			v += n
		}
	}()

	wg.Wait()
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
}

// TestRingBufferDiscard 丢弃只推进读指针（#117）：已排队的采样被整体跳过，
// 写者进度与容量不受影响，之后写入的数据正常可读。
func TestRingBufferDiscard(t *testing.T) {
	r := NewRingBuffer(8192)
	in := make([]int16, 960)
	for i := range in {
		in[i] = int16(i + 1)
	}
	r.Write(in)
	r.Write(in)

	if got := r.Available(); got != 1920 {
		t.Fatalf("Available = %d, want 1920", got)
	}
	r.Discard()
	if got := r.Available(); got != 0 {
		t.Fatalf("Discard 后 Available = %d, want 0", got)
	}
	back := make([]int16, 960)
	if n := r.Read(back); n != 0 {
		t.Fatalf("Discard 后 Read = %d, want 0", n)
	}

	// 丢弃后写者继续写，新数据必须原样可读（不因 tail 前跳而错位）。
	r.Write(in)
	if n := r.Read(back); n != 960 {
		t.Fatalf("Read = %d, want 960", n)
	}
	if back[0] != 1 || back[959] != 960 {
		t.Fatalf("丢弃后数据错位: first=%d last=%d", back[0], back[959])
	}
}

// BenchmarkRingBuffer 稳态 480 采样写+读：必须 0 allocs/op。
func BenchmarkRingBuffer(b *testing.B) {
	r := NewRingBuffer(16384)
	in := make([]int16, 480)
	out := make([]int16, 480)
	// 预填一半，保持稳态非空非满。
	r.Write(make([]int16, 8192))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := r.Write(in); n != 480 {
			b.Fatalf("Write = %d", n)
		}
		if n := r.Read(out); n != 480 {
			b.Fatalf("Read = %d", n)
		}
	}
}
