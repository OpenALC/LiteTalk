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
