package api

// bots_test.go bot 账号的 REST 侧行为（#51）：
//   - 空哈希密码登录必须拒绝（TestBotPasswordLoginRejected，fail-closed 不能只靠注释）；
//   - bot 不出现在好友候选/@提及候选/成员管理列表；
//   - bot 令牌走同一套 Bearer 鉴权，scope 收窄与吊销立即生效。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// botFixture REST 侧夹具：真人（服务器 owner）+ bot（服务器成员）+ bot 令牌。
type botFixture struct {
	h        http.Handler
	srv      *Server
	mem      *store.Mem
	access   string // 真人 JWT
	serverID int64
	chID     int64
	bot      *store.User
	plain    string // bot 令牌明文
	tokenID  int64
}

func newBotFixture(t *testing.T, scopes []string) *botFixture {
	t.Helper()
	srv, mem := newTestServer(t)
	f := &botFixture{h: srv.Handler(), srv: srv, mem: mem}

	var err error
	// 真人：注册 → 建服务器 → 建文字频道。
	f.access, _ = registerUser(t, f.h, mem, "owner@ex.com", "owner", "password123")
	sv := postJSON(t, f.h, "/api/v1/servers", f.access, map[string]any{"name": "srv", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	f.serverID = asID(sv["id"])
	ch := postJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/channels", f.serverID), f.access,
		map[string]any{"name": "general", "type": "text"}, http.StatusCreated)
	f.chID = asID(ch["id"])

	// bot：直建账号（创建入口是管理员 CLI，REST 不开）并直写成员表。
	f.bot = &store.User{Email: "restbot@bots.invalid", Nickname: "rest-bot", Tag: "9001", IsBot: true}
	if err = mem.CreateUser(context.Background(), f.bot); err != nil {
		t.Fatalf("CreateUser bot: %v", err)
	}
	if err = mem.AddMember(context.Background(), &store.ServerMember{ServerID: f.serverID, UserID: f.bot.ID, Role: store.RoleMember}); err != nil {
		t.Fatalf("AddMember bot: %v", err)
	}
	// 直写成员表绕过了 REST 的失效点：手动失效 authz 按服缓存（生产由
	// REST 加成员路径做），否则先建的快照里没有 bot。
	f.srv.authz.InvalidateServer(f.serverID)
	// bot 令牌：与 litetalkctl 同一条落库形状（kind=bot + scope 空格拼接）。
	p, err := token.NewPlain(token.KindBot)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	row := &store.APIToken{
		UserID: f.bot.ID, Name: "rest", Prefix: p.Prefix, SecretHash: p.Hash,
		Kind: token.KindBot, Scopes: strings.Join(scopes, " "),
	}
	if err = mem.CreateAPIToken(context.Background(), row); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	f.plain, f.tokenID = p.Token, row.ID
	return f
}

// idsOf 从 wire 列表抽 id/user_id 集合。
func idsOf(t *testing.T, v any) map[int64]bool {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("响应应为数组: %T", v)
	}
	out := map[int64]bool{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("数组元素应为对象: %T", item)
		}
		for _, key := range []string{"id", "user_id"} {
			if raw, ok := m[key]; ok {
				if id := asID(raw); id != 0 {
					out[id] = true
				}
			}
		}
	}
	return out
}

// TestBotPasswordLoginRejected bot 的空哈希必须天然登录不了（#51）：
// CheckPassword 对空串比对 dummyHash 后恒 false——断言在测试里，不靠注释。
func TestBotPasswordLoginRejected(t *testing.T) {
	f := newBotFixture(t, []string{token.ScopeMessagesRead})
	postJSON(t, f.h, "/api/v1/auth/login", "", map[string]string{
		"email": f.bot.Email, "password": "password123",
	}, http.StatusUnauthorized)
}

// TestBotNotInMentionCandidates 成员里的 bot 不出现在 @提及候选（#51）。
func TestBotNotInMentionCandidates(t *testing.T) {
	f := newBotFixture(t, []string{token.ScopeMessagesRead})
	out := getJSON(t, f.h, fmt.Sprintf("/api/v1/channels/%d/mention-candidates", f.chID), f.access, http.StatusOK)
	ids := idsOf(t, out)
	if ids[f.bot.ID] {
		t.Fatalf("bot %d 不应出现在提及候选: %v", f.bot.ID, ids)
	}
}

// TestBotNotInServerMemberList 成员管理列表不含 bot（#51）。
func TestBotNotInServerMemberList(t *testing.T) {
	f := newBotFixture(t, []string{token.ScopeMessagesRead})
	out := getJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/members", f.serverID), f.access, http.StatusOK)
	ids := idsOf(t, out)
	if ids[f.bot.ID] {
		t.Fatalf("bot %d 不应出现在成员列表: %v", f.bot.ID, ids)
	}
	if len(ids) == 0 {
		t.Fatal("列表不应为空（真人还在）")
	}
}

// TestBotFriendRequestRejected bot 不进好友候选：按 昵称#tag 加好友 → 与
// 「用户不存在」同形 404（不泄露名字是否被 bot 占用）。
func TestBotFriendRequestRejected(t *testing.T) {
	f := newBotFixture(t, []string{token.ScopeMessagesRead})
	postJSON(t, f.h, "/api/v1/friends/requests", f.access, map[string]any{
		"nickname": f.bot.Nickname + "#" + f.bot.Tag,
	}, http.StatusNotFound)
	// 好友列表同样不为 bot 建立任何关系行。
	rels := getJSON(t, f.h, "/api/v1/friends", f.access, http.StatusOK)
	if list, ok := rels.([]any); ok && len(list) != 0 {
		t.Fatalf("bot 好友请求被拒后不应有关系行: %v", rels)
	}
}

// TestBotTokenRESTScopeAndRevoke bot 令牌走同一套 Bearer 鉴权：
// messages.read 只读（发消息 403）；吊销后同令牌立即 401。
func TestBotTokenRESTScopeAndRevoke(t *testing.T) {
	f := newBotFixture(t, []string{token.ScopeMessagesRead})
	url := fmt.Sprintf("/api/v1/channels/%d/messages", f.chID)

	// 读放行（REST 中间件注入 Principal，authz 交集点在 T2 已收口）。
	getJSON(t, f.h, url, f.plain, http.StatusOK)
	// scope 外的写：403。
	postJSON(t, f.h, url, f.plain, map[string]any{"content": "hi"}, http.StatusForbidden)

	// 吊销（与 REST DELETE /tokens/{id} 同一落库路径）：无 Principal 缓存，立即生效。
	if err := f.mem.RevokeAPIToken(context.Background(), f.tokenID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	getJSON(t, f.h, url, f.plain, http.StatusUnauthorized)
}
