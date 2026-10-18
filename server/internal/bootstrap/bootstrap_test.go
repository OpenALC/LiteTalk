package bootstrap

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

func TestCodeStableAndSecretScoped(t *testing.T) {
	a := Code("secret-a")
	if a != Code("secret-a") {
		t.Fatal("同一 secret 应恒得同一码（跨重启稳定）")
	}
	if len(a) != 32 {
		t.Fatalf("码应为 32 hex，实际 %d 位: %q", len(a), a)
	}
	if a == Code("secret-b") {
		t.Fatal("不同 secret 不应派生出同一码")
	}
	// 空密钥回落与 auth.NewJWT 一致，保证与令牌同源。
	if Code("") != Code("litetalk-dev-secret-change-me") {
		t.Fatal("空密钥应回落到开发密钥派生")
	}
}

func TestVerify(t *testing.T) {
	code := Code("secret-a")
	if !Verify("secret-a", code) {
		t.Fatal("正确码应通过")
	}
	if !Verify("secret-a", "  "+code+"  ") {
		t.Fatal("应容忍前后空白")
	}
	if Verify("secret-a", "deadbeefdeadbeefdeadbeefdeadbeef") {
		t.Fatal("错误码应拒绝")
	}
	if Verify("secret-b", code) {
		t.Fatal("换 secret 后旧码应失效")
	}
}

func TestRequiredConsumeRoundtrip(t *testing.T) {
	ctx := context.Background()
	kv := store.NewMemKV()
	if !Required(ctx, kv) {
		t.Fatal("未消费时 Required 应为 true")
	}
	if err := Consume(ctx, kv, 42); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if Required(ctx, kv) {
		t.Fatal("消费后 Required 应为 false")
	}
	// 值记 uid 便于排查。
	if v, err := kv.Get(ctx, keyConsumed); err != nil || v != "42" {
		t.Fatalf("消费标记应存 uid，实际 %q/%v", v, err)
	}
}

func TestEnsurePrintsOnce(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	kv := store.NewMemKV()
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)

	Ensure(ctx, kv, st, "secret-a", lg)
	out := buf.String()
	if !strings.Contains(out, Code("secret-a")) {
		t.Fatalf("首启应打印邀请码，日志：%q", out)
	}
	buf.Reset()
	Ensure(ctx, kv, st, "secret-a", lg)
	if buf.Len() != 0 {
		t.Fatalf("已打印过不应重复打印，日志：%q", buf.String())
	}
}

func TestEnsureSkipsWhenUsersExist(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	kv := store.NewMemKV()
	u := &store.User{Email: "a@ex.com", Nickname: "alice", Tag: "0001"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	Ensure(ctx, kv, st, "secret-a", lg)
	if buf.Len() != 0 {
		t.Fatalf("库内已有用户不应再自动打印（MemKV 重启丢标记防误亮作废码），日志：%q", buf.String())
	}
}

func TestEnsureSkipsWhenConsumed(t *testing.T) {
	ctx := context.Background()
	kv := store.NewMemKV()
	if err := Consume(ctx, kv, 7); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	Ensure(ctx, kv, store.NewMem(), "secret-a", lg)
	if buf.Len() != 0 {
		t.Fatalf("已消费不应打印，日志：%q", buf.String())
	}
}

func TestPrintAdminInvite(t *testing.T) {
	ctx := context.Background()
	kv := store.NewMemKV()
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)

	PrintAdminInvite(ctx, kv, store.NewMem(), "secret-a", lg)
	if !strings.Contains(buf.String(), Code("secret-a")) {
		t.Fatalf("-print-admin-invite 应打印派生码，日志：%q", buf.String())
	}
	// 已消费只报状态，不再亮码。
	buf.Reset()
	if err := Consume(ctx, kv, 9); err != nil {
		t.Fatal(err)
	}
	PrintAdminInvite(ctx, kv, store.NewMem(), "secret-a", lg)
	if strings.Contains(buf.String(), Code("secret-a")) {
		t.Fatalf("已消费不应再亮码，日志：%q", buf.String())
	}
	if !strings.Contains(buf.String(), "已消费") {
		t.Fatalf("已消费应报状态，日志：%q", buf.String())
	}
}

func TestAdopt(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	u := &store.User{Email: "o@ex.com", Nickname: "owner", Tag: "0001"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	srv, err := Adopt(ctx, st, u.ID)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if srv.Name != DefaultServerName || srv.OwnerID != u.ID {
		t.Fatalf("默认服务器归属不符: %+v", srv)
	}
	mem, err := st.Member(ctx, srv.ID, u.ID)
	if err != nil || mem.Role != store.RoleOwner {
		t.Fatalf("创建者应为 owner: %+v/%v", mem, err)
	}
	chs, err := st.ChannelsOfServer(ctx, srv.ID)
	if err != nil || len(chs) != 2 {
		t.Fatalf("应建文字/语音两频道: %v/%v", chs, err)
	}
	servers, err := st.ServersOfUser(ctx, u.ID)
	if err != nil || len(servers) != 1 {
		t.Fatalf("用户应能看到默认服务器: %v/%v", servers, err)
	}
}
