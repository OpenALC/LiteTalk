package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestSearchMessagesBridge #42 B2：搜索桥接把 REST 结果映射为与 ListMessages
// 同口径的 ChatMessageView（附件 URL 绝对化），并透传 q/limit。
func TestSearchMessagesBridge(t *testing.T) {
	var gotPath, gotQ, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQ = r.URL.Path, r.URL.Query().Get("q")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"9001","channel_id":"123","user_id":"7","content":"你好",
			"created_at":"2026-10-25T09:30:00Z","sender_nickname":"甲",
			"attachments":[{"id":"501","filename":"a.txt","size":12,
				"content_type":"text/plain","width":0,"height":0,
				"url":"/api/v1/attachments/501?key=k"}]}]`))
	}))
	defer srv.Close()

	app := &App{api: newAppAPI(srv.URL)}
	out, err := app.SearchMessages("你好 世界", 20)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if gotPath != "/api/v1/search" || gotQ != "你好 世界" || gotLimit != "20" {
		t.Fatalf("请求 = %q q=%q limit=%q", gotPath, gotQ, gotLimit)
	}
	if len(out) != 1 {
		t.Fatalf("条数 = %d，期望 1", len(out))
	}
	v := out[0]
	wantTS := strconv.FormatInt(time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC).UnixMilli(), 10)
	if v.ID != "9001" || v.ChannelID != "123" || v.UserID != "7" ||
		v.Nickname != "甲" || v.Content != "你好" || v.TS != wantTS {
		t.Fatalf("视图映射错误: %+v", v)
	}
	if len(v.Attachments) != 1 {
		t.Fatalf("附件数量 = %d，期望 1", len(v.Attachments))
	}
	wantURL := srv.URL + "/api/v1/attachments/501?key=k"
	if v.Attachments[0].URL != wantURL {
		t.Fatalf("附件 URL = %q，期望 %q", v.Attachments[0].URL, wantURL)
	}

	// 空结果归一为非 nil 空切片（Wails 序列化为 []，前端不会拿到 null）。
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer emptySrv.Close()
	app2 := &App{api: newAppAPI(emptySrv.URL)}
	empty, err := app2.SearchMessages("x", 20)
	if err != nil {
		t.Fatalf("SearchMessages 空结果: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空结果应为非 nil 空切片，得到 %#v", empty)
	}
	if !reflect.DeepEqual(empty, []ChatMessageView{}) {
		t.Fatalf("空结果 = %#v", empty)
	}

	// 未连接（api 未初始化）不 panic，明确报错。
	if _, err := (&App{}).SearchMessages("x", 20); err == nil {
		t.Fatal("未连接时应报错")
	}
}
