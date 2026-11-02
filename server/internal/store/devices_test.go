package store

import (
	"context"
	"testing"
)

// TestPushDeviceUpsertIdempotent 设备注册幂等（#53）：同一 push_token 重复注册 =
// 更新 platform/last_seen 且不新增行；不同 token 各自成行。Mem 与 SQLite 双实现。
func TestPushDeviceUpsertIdempotent(t *testing.T) {
	for name, st := range map[string]Store{
		"mem":    NewMem(),
		"sqlite": openTestSQLiteWithMigrate(t),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			d := &PushDevice{UserID: 42, Platform: "ios", Token: "tok-aaaaaaaaaaaaaaaa"}
			created, err := st.UpsertPushDevice(ctx, d)
			if err != nil || !created {
				t.Fatalf("首插应 created=true: %v %v", created, err)
			}
			if d.ID == 0 {
				t.Fatal("首插应回填 ID")
			}
			firstSeen := d.LastSeen

			// 同 token 换 platform：更新而非新增。
			d2 := &PushDevice{UserID: 42, Platform: "android", Token: "tok-aaaaaaaaaaaaaaaa"}
			created, err = st.UpsertPushDevice(ctx, d2)
			if err != nil || created {
				t.Fatalf("重复注册应 created=false: %v %v", created, err)
			}
			if d2.ID != d.ID {
				t.Fatalf("重复注册应复用既有 ID: got %d want %d", d2.ID, d.ID)
			}
			devs, err := st.PushDevicesOfUser(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			if len(devs) != 1 {
				t.Fatalf("重复注册后应仍 1 行，实际 %d", len(devs))
			}
			if devs[0].Platform != "android" {
				t.Fatalf("重复注册应更新 platform: %q", devs[0].Platform)
			}
			if devs[0].Token != "tok-aaaaaaaaaaaaaaaa" {
				t.Fatal("更新分支必须保留明文 token（投递依赖）")
			}

			// 另一 token 各自成行；last_seen 单调不回退。
			d3 := &PushDevice{UserID: 42, Platform: "ios", Token: "tok-bbbbbbbbbbbbbbbb"}
			if _, err := st.UpsertPushDevice(ctx, d3); err != nil {
				t.Fatal(err)
			}
			devs, _ = st.PushDevicesOfUser(ctx, 42)
			if len(devs) != 2 {
				t.Fatalf("不同 token 应各自成行，实际 %d", len(devs))
			}
			if devs[0].LastSeen.Before(firstSeen) && devs[0].ID == d.ID {
				t.Fatal("last_seen 不应回退")
			}
			// 同 token 换用户：互不串号。
			d4 := &PushDevice{UserID: 43, Platform: "ios", Token: "tok-aaaaaaaaaaaaaaaa"}
			if created, err := st.UpsertPushDevice(ctx, d4); err != nil || !created {
				t.Fatalf("跨用户同 token 应各自成行: %v %v", created, err)
			}
		})
	}
}

// TestPushDeviceDeleteNotFound 删除与查询的 not-found 口径：删两次第二次 ErrNotFound；
// 按查无此 ID 返回 ErrNotFound。
func TestPushDeviceDeleteNotFound(t *testing.T) {
	for name, st := range map[string]Store{
		"mem":    NewMem(),
		"sqlite": openTestSQLiteWithMigrate(t),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			d := &PushDevice{UserID: 7, Platform: "android", Token: "tok-cccccccccccccccc"}
			if _, err := st.UpsertPushDevice(ctx, d); err != nil {
				t.Fatal(err)
			}
			if _, err := st.PushDeviceByID(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			if err := st.DeletePushDevice(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			if err := st.DeletePushDevice(ctx, d.ID); err != ErrNotFound {
				t.Fatalf("二次删除应 ErrNotFound，实际 %v", err)
			}
			if _, err := st.PushDeviceByID(ctx, d.ID); err != ErrNotFound {
				t.Fatalf("已删设备查询应 ErrNotFound，实际 %v", err)
			}
			if _, err := st.PushDeviceByID(ctx, 999999); err != ErrNotFound {
				t.Fatalf("不存在设备应 ErrNotFound，实际 %v", err)
			}
		})
	}
}

// TestPushDeviceListOrderAndIsolation 列表按 id 升序、用户间隔离。
func TestPushDeviceListOrderAndIsolation(t *testing.T) {
	st := NewMem()
	ctx := context.Background()
	for i, tok := range []string{"tok-1", "tok-2", "tok-3"} {
		d := &PushDevice{UserID: 5, Platform: "ios", Token: tok}
		if _, err := st.UpsertPushDevice(ctx, d); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	other := &PushDevice{UserID: 6, Platform: "android", Token: "tok-other"}
	if _, err := st.UpsertPushDevice(ctx, other); err != nil {
		t.Fatal(err)
	}
	devs, err := st.PushDevicesOfUser(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 {
		t.Fatalf("want 3 devices, got %d", len(devs))
	}
	for i := 1; i < len(devs); i++ {
		if devs[i-1].ID >= devs[i].ID {
			t.Fatal("列表应按 id 升序")
		}
	}
	if devs[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt 应由 Store 回填")
	}
}

// openTestSQLiteWithMigrate 建临时 SQLite 库并跑完整迁移（/devices 测试专用）。
func openTestSQLiteWithMigrate(t *testing.T) *SQLite {
	t.Helper()
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return sq
}
