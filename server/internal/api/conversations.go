package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #38 B2：私聊/群私聊 REST。会话 = 无服务器频道（scope=direct/group），消息仍走
// /channels/{id}/messages；服务器频道管理端点一律拒绝会话作用域（见 serverChannelOnly）。
//
// 语义边界：
//   - direct 恰好两人、dm_key 幂等；群聊总人数 3–50（含创建者）。
//   - 好友校验只在创建/加人时做；好友关系后来变化不删会话。
//   - 非参与者访问会话/会话消息一律 404，不泄露会话是否存在。

const (
	conversationCreateLimit = 10 // 创建会话：10 次/分/用户
	conversationMemberLimit = 20 // 加人：20 次/分/用户
	conversationRenameLimit = 10 // 改名：10 次/分/用户
	// maxGroupMembers 群聊总人数上限（含 owner）；创建时 user_ids 最多 49。
	maxGroupMembers = 50
)

// conversationScoped 是否私聊/群私聊频道。
func conversationScoped(ch *store.Channel) bool {
	return ch.Scope == store.ChannelScopeDirect || ch.Scope == store.ChannelScopeGroup
}

// conversationView 会话 wire 视图：成员只含昵称/tag/角色，不含 email。
func conversationView(ch *store.Channel, members []store.ChannelMember) Conversation {
	return Conversation{
		Id:        idOf(ch.ID),
		Kind:      ConversationKind(ch.Scope),
		Name:      ch.Name,
		Members:   conversationMemberViews(members),
		CreatedAt: timeOf(ch.CreatedAt),
	}
}

func conversationMemberViews(list []store.ChannelMember) []ConversationMember {
	out := make([]ConversationMember, 0, len(list))
	for i := range list {
		out = append(out, ConversationMember{
			UserId:   idOf(list[i].UserID),
			Nickname: list[i].Nickname,
			Tag:      list[i].Tag,
			Role:     ConversationMemberRole(list[i].Role),
			JoinedAt: timeOf(list[i].JoinedAt),
		})
	}
	return out
}

// conversationForMember 解析会话并要求当前用户是成员。非会话频道与非成员统一
// 404（不泄露会话是否存在）。
func (s *Server) conversationForMember(ctx context.Context, id, uid int64) (*store.Channel, *store.ChannelMember, *apiErr) {
	ch, err := s.store.ChannelByID(ctx, id)
	if err != nil {
		return nil, nil, s.writeStoreErr(ctx, err)
	}
	if !conversationScoped(ch) {
		return nil, nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	cm, err := s.store.ChannelMember(ctx, ch.ID, uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, errf(http.StatusNotFound, "not_found", "资源不存在")
		}
		return nil, nil, s.writeStoreErr(ctx, err)
	}
	return ch, cm, nil
}

// loadConversationView 取会话成员并转 wire 视图。
func (s *Server) loadConversationView(ctx context.Context, ch *store.Channel) (Conversation, *apiErr) {
	members, err := s.store.ChannelMembers(ctx, ch.ID)
	if err != nil {
		return Conversation{}, s.writeStoreErr(ctx, err)
	}
	return conversationView(ch, members), nil
}

// acceptedFriends 双方是否为已接受好友。反向行由 Store 归一为同一行；
// pending/blocked 一律不算好友。
func (s *Server) acceptedFriends(ctx context.Context, a, b int64) (bool, error) {
	f, err := s.store.FriendshipBetween(ctx, a, b)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return f.Status == store.FriendAccepted, nil
}

// conversationUserIDs 校验请求里的用户列表：去 0、查重、排除创建者。
func conversationUserIDs(ids []ID, creator int64) ([]int64, *apiErr) {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		uid := int64(id)
		if uid <= 0 {
			return nil, errf(http.StatusBadRequest, "bad_members", "成员 ID 不合法")
		}
		if uid == creator {
			return nil, errf(http.StatusBadRequest, "bad_members", "成员列表不能包含创建者")
		}
		if _, dup := seen[uid]; dup {
			return nil, errf(http.StatusBadRequest, "bad_members", "成员列表不能重复")
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out, nil
}

func validConversationName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 64
}

// ListConversations GET /api/v1/conversations（当前用户参与的 direct/group）
func (s *Server) ListConversations(ctx context.Context, _ ListConversationsRequestObject) (ListConversationsResponseObject, error) {
	c := currentUser(ctx)
	list, err := s.store.ListConversations(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	ids := make([]int64, 0, len(list))
	for i := range list {
		ids = append(ids, list[i].ID)
	}
	members, err := s.store.ChannelMembersOfChannels(ctx, ids)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 空结果落 [] 而非 null（契约 Conversation 数组）。
	out := make([]Conversation, 0, len(list))
	for i := range list {
		out = append(out, conversationView(&list[i], members[list[i].ID]))
	}
	return ListConversations200JSONResponse(out), nil
}

// CreateConversation POST /api/v1/conversations（direct 幂等 / group 建群）
func (s *Server) CreateConversation(ctx context.Context, request CreateConversationRequestObject) (CreateConversationResponseObject, error) {
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	kind := store.ChannelScope(request.Body.Kind)
	if kind != store.ChannelScopeDirect && kind != store.ChannelScopeGroup {
		return nil, errf(http.StatusBadRequest, "bad_kind", "kind 必须是 direct 或 group")
	}
	ids, e := conversationUserIDs(request.Body.UserIds, c.UserID)
	if e != nil {
		return nil, e
	}
	var name string
	if request.Body.Name != nil {
		name = *request.Body.Name
	}
	switch kind {
	case store.ChannelScopeDirect:
		name = "" // 私聊名无意义，不落库
		if len(ids) != 1 {
			return nil, errf(http.StatusBadRequest, "bad_members", "私聊必须恰好邀请 1 人")
		}
	case store.ChannelScopeGroup:
		if len(ids) < 2 || len(ids) > maxGroupMembers-1 {
			return nil, errf(http.StatusBadRequest, "bad_members", "群聊需邀请 2–49 人")
		}
		if name != "" && !validConversationName(name) {
			return nil, errf(http.StatusBadRequest, "bad_name", "群名需为 1–64 个字符")
		}
	}
	// direct 幂等：已有会话直接回原会话。好友关系后来的变化不影响已存在会话，
	// 因此这一步先于好友校验（弱网重复建会话不应变 403）。
	if kind == store.ChannelScopeDirect {
		existing, err := s.store.FindDirectChannel(ctx, c.UserID, ids[0])
		if err == nil {
			v, e := s.loadConversationView(ctx, existing)
			if e != nil {
				return nil, e
			}
			return CreateConversation200JSONResponse(v), nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	// 每个被邀请者都必须是创建者的已接受好友（pending/blocked 不算）。
	for _, uid := range ids {
		ok, err := s.acceptedFriends(ctx, c.UserID, uid)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		if !ok {
			return nil, errf(http.StatusForbidden, "not_friend", "只能邀请已接受好友")
		}
	}
	if e := s.allowRate(ctx, "conversation:create:"+strconv.FormatInt(c.UserID, 10),
		conversationCreateLimit, time.Minute, "创建会话过于频繁"); e != nil {
		return nil, e
	}
	ch := &store.Channel{Scope: kind, Name: name}
	// 创建者排第一：group 的首个成员记 owner（Store 契约）。
	memberIDs := append([]int64{c.UserID}, ids...)
	if err := s.store.CreateConversation(ctx, ch, memberIDs); err != nil {
		// direct 并发重复：唯一索引兜底，回读原会话按幂等 200 返回。
		if kind == store.ChannelScopeDirect && errors.Is(err, store.ErrConflict) {
			if existing, ferr := s.store.FindDirectChannel(ctx, c.UserID, ids[0]); ferr == nil {
				v, e := s.loadConversationView(ctx, existing)
				if e != nil {
					return nil, e
				}
				return CreateConversation200JSONResponse(v), nil
			}
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	v, e := s.loadConversationView(ctx, ch)
	if e != nil {
		return nil, e
	}
	return CreateConversation201JSONResponse(v), nil
}

// GetConversation GET /api/v1/conversations/{id}（仅参与者）
func (s *Server) GetConversation(ctx context.Context, request GetConversationRequestObject) (GetConversationResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	ch, _, e := s.conversationForMember(ctx, request.Id, currentUser(ctx).UserID)
	if e != nil {
		return nil, e
	}
	v, e := s.loadConversationView(ctx, ch)
	if e != nil {
		return nil, e
	}
	return GetConversation200JSONResponse(v), nil
}

// PatchConversation PATCH /api/v1/conversations/{id}（group owner 改名）
func (s *Server) PatchConversation(ctx context.Context, request PatchConversationRequestObject) (PatchConversationResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, cm, e := s.conversationForMember(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	if ch.Scope != store.ChannelScopeGroup {
		return nil, errf(http.StatusBadRequest, "not_group", "仅群聊支持改名")
	}
	if cm.Role != store.ChannelMemberOwner {
		return nil, errf(http.StatusForbidden, "forbidden", "仅群主可改名")
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	name, ok := req.Name.Get()
	if !ok || !validConversationName(name) {
		return nil, errf(http.StatusBadRequest, "bad_name", "群名需为 1–64 个字符")
	}
	if e := s.allowRate(ctx, "conversation:rename:"+strconv.FormatInt(c.UserID, 10),
		conversationRenameLimit, time.Minute, "改名过于频繁"); e != nil {
		return nil, e
	}
	ch.Name = name
	if err := s.store.UpdateChannel(ctx, ch); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	v, e := s.loadConversationView(ctx, ch)
	if e != nil {
		return nil, e
	}
	return PatchConversation200JSONResponse(v), nil
}

// DeleteConversation DELETE /api/v1/conversations/{id}
// direct 任一方可删（双方消失）；group 仅 owner；硬删消息与成员（Store 级联）。
func (s *Server) DeleteConversation(ctx context.Context, request DeleteConversationRequestObject) (DeleteConversationResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	ch, cm, e := s.conversationForMember(ctx, request.Id, currentUser(ctx).UserID)
	if e != nil {
		return nil, e
	}
	if ch.Scope == store.ChannelScopeGroup && cm.Role != store.ChannelMemberOwner {
		return nil, errf(http.StatusForbidden, "forbidden", "仅群主可解散群聊")
	}
	if err := s.store.DeleteChannel(ctx, ch.ID); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return DeleteConversation204Response{}, nil
}

// AddConversationMembers POST /api/v1/conversations/{id}/members（group owner 加人）
func (s *Server) AddConversationMembers(ctx context.Context, request AddConversationMembersRequestObject) (AddConversationMembersResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, cm, e := s.conversationForMember(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	if ch.Scope != store.ChannelScopeGroup {
		return nil, errf(http.StatusBadRequest, "not_group", "仅群聊支持加人")
	}
	if cm.Role != store.ChannelMemberOwner {
		return nil, errf(http.StatusForbidden, "forbidden", "仅群主可加人")
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	ids, e := conversationUserIDs(request.Body.UserIds, c.UserID)
	if e != nil {
		return nil, e
	}
	if len(ids) == 0 {
		return nil, errf(http.StatusBadRequest, "bad_members", "至少邀请 1 人")
	}
	existing, err := s.store.ChannelMembers(ctx, ch.ID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	have := make(map[int64]struct{}, len(existing))
	for _, m := range existing {
		have[m.UserID] = struct{}{}
	}
	for _, uid := range ids {
		if _, ok := have[uid]; ok {
			return nil, errf(http.StatusBadRequest, "bad_members", "成员已在群聊中")
		}
	}
	if len(existing)+len(ids) > maxGroupMembers {
		return nil, errf(http.StatusBadRequest, "too_many_members", "群聊人数上限为 50")
	}
	for _, uid := range ids {
		ok, err := s.acceptedFriends(ctx, c.UserID, uid)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		if !ok {
			return nil, errf(http.StatusForbidden, "not_friend", "只能邀请已接受好友")
		}
	}
	if e := s.allowRate(ctx, "conversation:members:"+strconv.FormatInt(c.UserID, 10),
		conversationMemberLimit, time.Minute, "加人过于频繁"); e != nil {
		return nil, e
	}
	for _, uid := range ids {
		if err := s.store.AddChannelMember(ctx, ch.ID, uid, store.ChannelMemberMember); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return nil, errf(http.StatusConflict, "conflict", "成员已在群聊中")
			}
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	v, e := s.loadConversationView(ctx, ch)
	if e != nil {
		return nil, e
	}
	return AddConversationMembers200JSONResponse(v), nil
}

// RemoveConversationMember DELETE /api/v1/conversations/{id}/members/{user_id}
// owner 可移除任意成员或自己退出；普通成员只能移除自己（leave）。
// owner 退出时把 owner 转给最早加入的剩余成员；最后一人退出时会话删除（204）。
func (s *Server) RemoveConversationMember(ctx context.Context, request RemoveConversationMemberRequestObject) (RemoveConversationMemberResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.UserId); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, self, e := s.conversationForMember(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	if ch.Scope != store.ChannelScopeGroup {
		return nil, errf(http.StatusBadRequest, "not_group", "私聊请直接删除会话")
	}
	targetID := request.UserId
	selfLeave := targetID == c.UserID
	if !selfLeave {
		if self.Role != store.ChannelMemberOwner {
			return nil, errf(http.StatusForbidden, "forbidden", "仅群主可移除他人")
		}
		if _, err := s.store.ChannelMember(ctx, ch.ID, targetID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errf(http.StatusNotFound, "not_found", "成员不存在")
			}
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	// owner 退出：先转让给最早加入的剩余成员（无剩余则删会话），再移除自己。
	if selfLeave && self.Role == store.ChannelMemberOwner {
		deleted, e := s.transferOwnerBeforeLeave(ctx, ch, c.UserID)
		if e != nil {
			return nil, e
		}
		if deleted {
			return RemoveConversationMember204Response{}, nil
		}
	}
	if err := s.store.RemoveChannelMember(ctx, ch.ID, targetID); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	v, e := s.loadConversationView(ctx, ch)
	if e != nil {
		return nil, e
	}
	return RemoveConversationMember200JSONResponse(v), nil
}

// transferOwnerBeforeLeave 把 owner 转给最早加入的剩余成员；没有剩余成员时
// 硬删会话并返回 deleted=true。并发移除可能让候选瞬间消失：重取成员列表重试，
// 连续失败则让客户端重试，不留下「无 owner 的群」。
func (s *Server) transferOwnerBeforeLeave(ctx context.Context, ch *store.Channel, ownerID int64) (bool, *apiErr) {
	for attempt := 0; attempt < 3; attempt++ {
		members, err := s.store.ChannelMembers(ctx, ch.ID)
		if err != nil {
			return false, s.writeStoreErr(ctx, err)
		}
		// ChannelMembers 已按 joined_at、user_id 升序。
		var next int64
		for _, m := range members {
			if m.UserID != ownerID {
				next = m.UserID
				break
			}
		}
		if next == 0 {
			if err := s.store.DeleteChannel(ctx, ch.ID); err != nil {
				return false, s.writeStoreErr(ctx, err)
			}
			return true, nil
		}
		err = s.store.SetChannelMemberRole(ctx, ch.ID, next, store.ChannelMemberOwner)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, s.writeStoreErr(ctx, err)
		}
	}
	return false, errf(http.StatusConflict, "conflict", "群成员正在变动，请重试")
}

// serverChannelOnly 服务器频道管理端点（PATCH/DELETE/覆盖/闭麦）拒绝会话作用域：
// 会话规则只能走 /conversations，不能借 /channels/{id} 绕过。统一 404，
// 不泄露会话是否存在。
func serverChannelOnly(ch *store.Channel) *apiErr {
	if ch.Scope != store.ChannelScopeServer {
		return errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	return nil
}
