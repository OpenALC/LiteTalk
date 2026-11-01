package signal

// chat_recipient_scope_test.go T4（#119）：创建管线里的收件人可见性是跨主体
// 判定，不能继承发送者凭据的 scope 上限。此前 messageRecipients 带着发送者 ctx
// 调 CanInServer，靠「发消息的令牌必带 messages.write（含 ViewChannel）」这个
// 隐含前提才没出错；REST / 入站 webhook 入口直接把请求 ctx 传进来，换成
// guild.manage 之类不含 ViewChannel 的 scope 就会把其他收件人全部过滤掉。

import (
	"context"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// TestAfterMessagePersistedIgnoresSenderScope 发送者 ctx 挂一个 scope 上限不含
// ViewChannel 的 PAT（guild.manage）。收件人 bob 按自己的成员角色有 ViewChannel，
// 必须照常收到 chat.message——发送者令牌的 scope 不得收窄别人的可见性。
func TestAfterMessagePersistedIgnoresSenderScope(t *testing.T) {
	f := newChatFixture(t)
	bob := f.dial("2", "bob")

	pat := &token.Principal{
		Kind:    token.KindPAT,
		TokenID: 1,
		Scopes:  []string{token.ScopeGuildManage},
	}
	ctx := token.WithPrincipal(context.Background(), pat)
	// 自证前置：同一个 ctx 直接做权限判定确实被 scope 上限压住。没有这条，
	// 用例无法区分「代码剥离了主体」与「这个 scope 本来就不影响判定」。
	if ok, err := f.hub.permCheckQuiet(ctx, 2, f.ch.ServerID, f.ch.ID, store.ViewChannel); err != nil || ok {
		t.Fatalf("前置：带发送者 ctx 时 bob 的可见性应被 scope 上限拒绝（ok=%v err=%v）", ok, err)
	}

	msg := &store.Message{ID: 1, ChannelID: f.ch.ID, UserID: 1, Content: "t4-recipient-scope"}
	f.hub.AfterMessagePersisted(ctx, f.ch, msg, "1", "alice")
	bob.recvUntil(TypeChatMessage)
}
