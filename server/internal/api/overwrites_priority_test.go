package api

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

// #45：频道覆盖变更后 voice.priority 的广播口径与失败降级。

type priorityWorld struct {
	w       *permWorld
	hub     *fakeHub
	voiceID int64
	textID  int64
}

func newPriorityWorld(t *testing.T) *priorityWorld {
	t.Helper()
	w := newPermWorld(t)
	hub := &fakeHub{notIn: map[string]bool{}}
	w.srv.hub = hub
	voice := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "语音房", "type": "voice"}, http.StatusCreated)
	text := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "文字", "type": "text"}, http.StatusCreated)
	return &priorityWorld{w: w, hub: hub, voiceID: asID(voice["id"]), textID: asID(text["id"])}
}

func (p *priorityWorld) putOverwrite(chID int64, targetType string, targetID int64, allow, deny string) {
	p.w.t.Helper()
	doJSON(p.w.t, p.w.h, http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%d/overwrites/%s/%d", chID, targetType, targetID), p.w.owner,
		map[string]string{"allow": allow, "deny": deny}, http.StatusOK)
}

// TestOverwritePriorityBroadcast PUT/DELETE member 覆盖后按最终 effective 值广播。
func TestOverwritePriorityBroadcast(t *testing.T) {
	p := newPriorityWorld(t)
	memberID := meID(t, p.w.h, p.w.member)
	path := fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", p.voiceID, memberID)

	// 授予：PrioritySpeaker = 1<<4 = 16。
	p.putOverwrite(p.voiceID, "member", memberID, "16", "0")
	log := p.hub.priorityLog()
	if len(log) != 1 {
		t.Fatalf("授予后应广播一次 voice.priority，实际 %d: %+v", len(log), log)
	}
	want := priorityEvent{channelID: strconv.FormatInt(p.voiceID, 10), userID: strconv.FormatInt(memberID, 10), priority: true}
	if log[0] != want {
		t.Fatalf("授予广播 = %+v，期望 %+v", log[0], want)
	}

	// 撤销：删除 member 覆盖后回到角色授予（普通成员无该位）→ false。
	doJSON(t, p.w.h, http.MethodDelete, path, p.w.owner, nil, http.StatusOK)
	log = p.hub.priorityLog()
	if len(log) != 2 || log[1].priority || log[1].userID != want.userID || log[1].channelID != want.channelID {
		t.Fatalf("撤销广播不符: %+v", log)
	}
}

// TestOverwritePriorityOwnerBypass owner/Administrator 始终为 true：即使个人
// deny 该位，effective() 仍判 true，广播必须发 true（不是机械回显覆盖值）。
func TestOverwritePriorityOwnerBypass(t *testing.T) {
	p := newPriorityWorld(t)
	ownerID := meID(t, p.w.h, p.w.owner)
	p.putOverwrite(p.voiceID, "member", ownerID, "0", "16")
	log := p.hub.priorityLog()
	if len(log) != 1 || !log[0].priority {
		t.Fatalf("owner 的个人 deny 不应改变 effective 判定，广播 = %+v", log)
	}
}

// TestOverwritePrioritySkipsNonVoice 非语音频道与 role 目标不广播。
func TestOverwritePrioritySkipsNonVoice(t *testing.T) {
	p := newPriorityWorld(t)
	memberID := meID(t, p.w.h, p.w.member)

	p.putOverwrite(p.textID, "member", memberID, "16", "0")
	p.putOverwrite(p.voiceID, "role", p.w.everyoneID(), "16", "0")
	if got := p.hub.priorityLog(); len(got) != 0 {
		t.Fatalf("非语音/member 目标不应广播，实际 %+v", got)
	}
}

// TestOverwritePriorityHubNil 无信令面（纯 REST 测试/降级）时覆盖照常成功，
// 不得因广播缺失改成 500。
func TestOverwritePriorityHubNil(t *testing.T) {
	p := newPriorityWorld(t)
	p.w.srv.hub = nil
	memberID := meID(t, p.w.h, p.w.member)
	doJSON(t, p.w.h, http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", p.voiceID, memberID), p.w.owner,
		map[string]string{"allow": "16", "deny": "0"}, http.StatusOK)
}

// TestOverwritePriorityKeepsOtherBits 只改 PrioritySpeaker 位的调用约定由客户端
// 负责；服务端按提交的 allow/deny 整体判定，验证广播结果跟随最终值。
func TestOverwritePriorityDenyWins(t *testing.T) {
	p := newPriorityWorld(t)
	memberID := meID(t, p.w.h, p.w.member)
	// allow 与 deny 同时含该位时 deny 优先（与 authz 语义一致）→ false。
	p.putOverwrite(p.voiceID, "member", memberID, "16", "16")
	log := p.hub.priorityLog()
	if len(log) != 1 || log[0].priority {
		t.Fatalf("deny 与 allow 同时存在时应为 false，广播 = %+v", log)
	}
}
