package snowflake

import (
	"testing"
	"time"
)

// TestNextMonotonic 同进程连续发号严格递增且为正。
func TestNextMonotonic(t *testing.T) {
	g := New(0)
	prev := int64(0)
	for i := 0; i < 10000; i++ {
		id := g.Next()
		if id <= 0 {
			t.Fatalf("ID 应为正，实际 %d", id)
		}
		if id <= prev {
			t.Fatalf("ID 应严格递增：%d -> %d", prev, id)
		}
		prev = id
	}
}

// TestLayout 位布局：41 时间戳 + 10 worker + 12 序列。
func TestLayout(t *testing.T) {
	const worker = 42
	g := New(worker)
	before := time.Now().UnixMilli()
	id := g.Next()
	after := time.Now().UnixMilli()

	ms := id >> timeShift
	w := (id >> workerShift) & maxWorker
	seq := id & maxSeq
	if w != worker {
		t.Fatalf("worker 位应为 %d，实际 %d", worker, w)
	}
	if seq != 0 {
		t.Fatalf("单次生成序列号应为 0，实际 %d", seq)
	}
	if ms < before-EpochMS || ms > after-EpochMS {
		t.Fatalf("时间戳位越界：%d 不在 [%d, %d]", ms, before-EpochMS, after-EpochMS)
	}
}

// TestSameMillisecondSeq 同毫秒内多次生成序列号递增（不撞 ID）。
func TestSameMillisecondSeq(t *testing.T) {
	g := New(0)
	ids := map[int64]bool{}
	for i := 0; i < 200; i++ {
		id := g.Next()
		if ids[id] {
			t.Fatalf("ID 重复：%d", id)
		}
		ids[id] = true
	}
}

// TestWorkerOutOfRange 非法 worker 回落 0。
func TestWorkerOutOfRange(t *testing.T) {
	for _, w := range []int64{-1, maxWorker + 1, 99999} {
		g := New(w)
		if g.worker != 0 {
			t.Fatalf("worker %d 应回落 0，实际 %d", w, g.worker)
		}
	}
}
