package store

import (
	"context"
	"testing"
)

// assertChannelCategoryPersist 频道分类归属原样往返（#72）：CategoryID 非空要存住、
// 空要留空。Mem 与 MySQL 共用这一段断言，两边语义漂移会直接测挂——客户端只有在
// 两边都一致时才敢「新建即归组」，不必等重启对账。
func assertChannelCategoryPersist(t *testing.T, m Store) {
	t.Helper()
	ctx := context.Background()
	s := &Server{Name: "ch-cat", OwnerID: 1, IconName: "music", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	cat := &Category{ServerID: s.ID, Name: "甲组"}
	if err := m.CreateCategory(ctx, cat); err != nil {
		t.Fatal(err)
	}

	// 带分类：读回同一分类。
	withCat := &Channel{ServerID: s.ID, Name: "开黑", Type: ChannelVoice, CategoryID: &cat.ID}
	if err := m.CreateChannel(ctx, withCat); err != nil {
		t.Fatal(err)
	}
	got, err := m.ChannelByID(ctx, withCat.ID)
	if err != nil || got.CategoryID == nil || *got.CategoryID != cat.ID {
		t.Fatalf("带分类频道往返失败: %+v %v", got, err)
	}

	// 无分类：读回必须还是 nil，不能变成 0 或悬空 id。
	noCat := &Channel{ServerID: s.ID, Name: "散装", Type: ChannelText}
	if err := m.CreateChannel(ctx, noCat); err != nil {
		t.Fatal(err)
	}
	got, err = m.ChannelByID(ctx, noCat.ID)
	if err != nil || got.CategoryID != nil {
		t.Fatalf("无分类频道应往返 nil，实际 %+v %v", got, err)
	}

	// 列表里两条按分类/无分类分开可见。
	list, err := m.ChannelsOfServer(ctx, s.ID)
	if err != nil {
		t.Fatalf("ChannelsOfServer: %v", err)
	}
	byName := map[string]*int64{}
	for i := range list {
		byName[list[i].Name] = list[i].CategoryID
	}
	if len(byName) != 2 || byName["开黑"] == nil || *byName["开黑"] != cat.ID || byName["散装"] != nil {
		t.Fatalf("列表分类归属不符: %+v", byName)
	}
}

// TestChannelCategoryPersist Mem 侧（#72）。
func TestChannelCategoryPersist(t *testing.T) {
	assertChannelCategoryPersist(t, NewMem())
}
