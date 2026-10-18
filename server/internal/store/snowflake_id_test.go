package store

import (
	"context"
	"testing"
)

// TestSnowflakeIDs 新行 ID 由 Snowflake 赋值（#31）：正数、严格递增、跨实体不重复。
// 时间序是游标分页的根基——旧 AUTO_INCREMENT 小 ID 恒小于新 ID，混排仍正确。
func TestSnowflakeIDs(t *testing.T) {
	m := NewMem()
	ctx := context.Background()

	u := &User{Email: "a@ex.com", Nickname: "a", Tag: "0001"}
	if err := m.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sv := &Server{Name: "s", OwnerID: u.ID, IconName: "messages-square", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	ch := &Channel{ServerID: sv.ID, Name: "c", Type: ChannelText}
	if err := m.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	msg := &Message{ChannelID: ch.ID, UserID: u.ID, Content: "hi"}
	if err := m.CreateMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}

	ids := []int64{u.ID, sv.ID, ch.ID, msg.ID}
	seen := map[int64]bool{}
	for i, id := range ids {
		if id <= 0 {
			t.Fatalf("ID[%d] 应为正，实际 %d", i, id)
		}
		if seen[id] {
			t.Fatalf("ID %d 跨实体重复", id)
		}
		seen[id] = true
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ID 应按创建序递增：%d -> %d", ids[i-1], id)
		}
	}
}

// TestPresetID 调用方预置 ID 原样落库（测试夹具用），不被覆盖。
func TestPresetID(t *testing.T) {
	m := NewMem()
	u := &User{ID: 42, Email: "p@ex.com", Nickname: "p", Tag: "0002"}
	if err := m.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if u.ID != 42 {
		t.Fatalf("预置 ID 应保留，实际 %d", u.ID)
	}
}
