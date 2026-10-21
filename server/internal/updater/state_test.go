package updater

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSuccess("v1.1.0-alpha", "d1", "abc", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFailure("v1.2.0-alpha", "d2", os.ErrPermission, time.Second); err != nil {
		t.Fatal(err)
	}

	// 重新打开：从磁盘恢复
	st2, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := st2.Get()
	if s.CurrentVersion != "v1.1.0-alpha" {
		t.Fatalf("CurrentVersion = %q（失败不应推进）", s.CurrentVersion)
	}
	if s.LastResult != "failed" || !strings.Contains(s.LastError, "permission") {
		t.Fatalf("失败记录错误: %+v", s)
	}
	if !s.HasDelivery("d1") || s.HasDelivery("d2") {
		t.Fatalf("delivery 记录错误: %+v", s.Deliveries)
	}
	// 文件权限：状态不给他人读（Windows 无 POSIX 权限位语义，只在 Unix 下断言）
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("state.json 权限过宽: %o", perm)
		}
	}
}

func TestStateDeliveryRing(t *testing.T) {
	st, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < DeliveryKeep+10; i++ {
		id := "d" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if err := st.RecordSuccess("v1.0.0", id, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	s := st.Get()
	if len(s.Deliveries) > DeliveryKeep {
		t.Fatalf("delivery 记录 %d 条, 上限 %d", len(s.Deliveries), DeliveryKeep)
	}
}

func TestStateHasDeliveryEmpty(t *testing.T) {
	var s State
	if s.HasDelivery("") {
		t.Fatal("空 delivery 不应命中")
	}
}

func TestStateReloadSeesOtherInstance(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A 持有过期内存快照，B 相当于 --once 或另一个更新进程
	if err := b.RecordSuccess("v1.1.0", "d-b", "sum", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.Get().CurrentVersion; got != "" {
		t.Fatalf("Reload 前 A 不应看到 B 的写入: %q", got)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s := a.Get()
	if s.CurrentVersion != "v1.1.0" {
		t.Fatalf("Reload 后 CurrentVersion = %q", s.CurrentVersion)
	}
	if !s.HasDelivery("d-b") {
		t.Fatalf("Reload 应带上 delivery 去重记录: %+v", s.Deliveries)
	}
}

func TestStateReloadKeepsMemoryOnFailure(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSuccess("v1.0.0", "d1", "", 0); err != nil {
		t.Fatal(err)
	}

	// 文件损坏：Reload 报错且不覆盖内存
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Reload(); err == nil {
		t.Fatal("解析失败应报错")
	}
	if got := st.Get().CurrentVersion; got != "v1.0.0" {
		t.Fatalf("解析失败不得覆盖内存: %q", got)
	}

	// 初始状态（文件不存在）：同样只报错，调用方记日志继续
	st2, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Reload(); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if st2.Get().CurrentVersion != "" {
		t.Fatal("缺失文件不得产生状态")
	}
}
