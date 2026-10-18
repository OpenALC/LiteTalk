package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// ListFriends GET /api/v1/friends
func (s *Server) ListFriends(ctx context.Context, request ListFriendsRequestObject) (ListFriendsResponseObject, error) {
	c := currentUser(ctx)
	fs, err := s.store.FriendshipsOfUser(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := ListFriends200JSONResponse{}
	for _, f := range fs {
		other := f.FriendID
		incoming := false
		if f.FriendID == c.UserID {
			other = f.UserID
			incoming = true
		}
		u, err := s.store.UserByID(ctx, other)
		if err != nil {
			continue
		}
		// 备注/分组是发起方视角的字段：被请求方看到空串。
		entry := FriendEntry{Id: idOf(f.ID), Status: FriendEntryStatus(f.Status), Incoming: incoming, User: s.userView(ctx, u)}
		if !incoming {
			entry.Remark, entry.Group = f.Remark, f.GroupName
		}
		out = append(out, entry)
	}
	return out, nil
}

// CreateFriendRequest POST /api/v1/friends/requests（按 昵称#tag 添加）
func (s *Server) CreateFriendRequest(ctx context.Context, request CreateFriendRequestRequestObject) (CreateFriendRequestResponseObject, error) {
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	nickname, tag := request.Body.Nickname, ""
	if request.Body.Tag != nil {
		tag = *request.Body.Tag
	}
	if strings.Contains(nickname, "#") {
		parts := strings.SplitN(nickname, "#", 2)
		nickname, tag = parts[0], strings.TrimSpace(parts[1])
	}
	if !validNickname(nickname) || len(tag) != 4 {
		return nil, errf(http.StatusBadRequest, "bad_target", "目标格式应为 昵称#4位tag")
	}
	target, err := s.store.UserByNicknameTag(ctx, nickname, tag)
	if err != nil {
		return nil, errf(http.StatusNotFound, "not_found", "用户不存在")
	}
	if target.ID == c.UserID {
		return nil, errf(http.StatusBadRequest, "bad_target", "不能添加自己为好友")
	}
	// 已存在任何方向的关系 → 冲突。
	if existing, err := s.store.FriendshipBetween(ctx, c.UserID, target.ID); err == nil {
		if existing.Status == store.FriendAccepted {
			return nil, errf(http.StatusConflict, "already_friends", "已经是好友")
		}
		return nil, errf(http.StatusConflict, "request_exists", "已存在好友请求")
	}
	f := &store.Friendship{
		UserID:   c.UserID,
		FriendID: target.ID,
		Status:   store.FriendPending,
	}
	if err := s.store.CreateFriendship(ctx, f); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return CreateFriendRequest201JSONResponse{
		Id: idOf(f.ID), Status: FriendEntryStatus(f.Status), Incoming: false,
		User: s.userView(ctx, target),
	}, nil
}

// AcceptFriendRequest POST /api/v1/friends/requests/{id}/accept
func (s *Server) AcceptFriendRequest(ctx context.Context, request AcceptFriendRequestRequestObject) (AcceptFriendRequestResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	f, err := s.store.FriendshipByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 只有被请求方可以接受。
	if f.FriendID != c.UserID {
		return nil, errf(http.StatusForbidden, "forbidden", "只有被请求方可以接受")
	}
	if f.Status != store.FriendPending {
		return nil, errf(http.StatusConflict, "bad_state", "请求不是待处理状态")
	}
	f.Status = store.FriendAccepted
	if err := s.store.UpdateFriendship(ctx, f); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	other, _ := s.store.UserByID(ctx, f.UserID)
	return AcceptFriendRequest200JSONResponse{
		Id: idOf(f.ID), Status: FriendEntryStatus(f.Status), Incoming: true,
		User: s.userView(ctx, other),
	}, nil
}

// DeleteFriend DELETE /api/v1/friends/{id}
func (s *Server) DeleteFriend(ctx context.Context, request DeleteFriendRequestObject) (DeleteFriendResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	f, err := s.store.FriendshipByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if f.UserID != c.UserID && f.FriendID != c.UserID {
		return nil, errf(http.StatusForbidden, "forbidden", "无权操作")
	}
	if err := s.store.DeleteFriendship(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return DeleteFriend200JSONResponse{Status: "ok"}, nil
}

// PatchFriend PATCH /api/v1/friends/{id}（设置备注/分组，v0.1.2）。
// 仅发起方向（friendships.user_id == 当前用户）可改——备注是发起方的本地视图，
// 被请求方改自己方向的行需要 user_id 是自己，而好友关系行的 user_id 只有一方。
func (s *Server) PatchFriend(ctx context.Context, request PatchFriendRequestObject) (PatchFriendResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	f, err := s.store.FriendshipByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 双方都能定位到关系行，但只有发起方持有备注/分组。
	if f.UserID != c.UserID {
		return nil, errf(http.StatusForbidden, "forbidden", "仅发起方可设置备注与分组")
	}
	// Merge Patch（#31）：缺省不改，null/空串清除，值设置。
	if req.Remark.Set {
		v := req.Remark.Val
		if n := len([]rune(v)); n > 64 {
			return nil, errf(http.StatusBadRequest, "bad_remark", "备注需为 0–64 个字符")
		}
		f.Remark = v
	}
	if req.Group.Set {
		v := req.Group.Val
		if n := len([]rune(v)); n > 32 {
			return nil, errf(http.StatusBadRequest, "bad_group", "分组名需为 0–32 个字符")
		}
		f.GroupName = v
	}
	if err := s.store.UpdateFriendship(ctx, f); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	other, _ := s.store.UserByID(ctx, f.FriendID)
	return PatchFriend200JSONResponse{
		Id: idOf(f.ID), Status: FriendEntryStatus(f.Status),
		Remark: f.Remark, Group: f.GroupName,
		User: s.userView(ctx, other),
	}, nil
}
