package api

import (
	"context"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #37 B1：频道选项（密码/人数上限/子频道树）的校验与装配。
// 密码明文与哈希都不出 API：响应只暴露 has_password，审计只记 password_changed。

// channelPasswordMaxBytes 密码长度上限（与账号密码同上限，§安全要点）。
const channelPasswordMaxBytes = 72

// userLimitMax 频道人数上限硬顶（0 = 不限）。
const userLimitMax = 100

// hashChannelPassword 密码 → bcrypt（cost 复用 auth 的 12）；空串返回空串（无密码）。
func hashChannelPassword(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return auth.HashPassword(plain)
}

// derefString *string → string（nil 当空串）。
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// checkChannelPassword 密码仅语音频道、1–72 字节。
func checkChannelPassword(password string, chType store.ChannelType) *apiErr {
	if chType != store.ChannelVoice {
		return errf(http.StatusBadRequest, "bad_channel_option", "只有语音频道可以设置密码")
	}
	n := len(password)
	if n == 0 || n > channelPasswordMaxBytes {
		return errf(http.StatusBadRequest, "bad_password", "密码长度需为 1–72 字节")
	}
	return nil
}

// checkUserLimit 人数上限仅语音频道、0..100。
func checkUserLimit(limit int, chType store.ChannelType) *apiErr {
	if chType != store.ChannelVoice {
		return errf(http.StatusBadRequest, "bad_channel_option", "只有语音频道可以设置人数上限")
	}
	if limit < 0 || limit > userLimitMax {
		return errf(http.StatusBadRequest, "bad_channel_option", "人数上限须为 0–100")
	}
	return nil
}

// channelChildren 收集某频道的直接子频道（#37：不新增 Store 方法，走 ChannelsOfServer 过滤）。
func (s *Server) channelChildren(ctx context.Context, serverID, parentID int64) ([]store.Channel, error) {
	all, err := s.store.ChannelsOfServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	out := []store.Channel{}
	for _, c := range all {
		if c.ParentID != nil && *c.ParentID == parentID {
			out = append(out, c)
		}
	}
	return out, nil
}

// resolveParent 父级校验（#37）：父存在且同服、不是自己、父是顶层、同类型、同分类。
// explicitCat 表示分类是调用方显式选定的（含显式无分类）：显式值必须与父一致；
// 未显式（缺省）且自身无分类时跟随父分类。返回 (父频道, 定稿分类, err)；
// parentID=0 表示无父，原样返回 catID。
func (s *Server) resolveParent(ctx context.Context, serverID, selfID, parentID int64, chType store.ChannelType, catID *int64, explicitCat bool) (*store.Channel, *int64, *apiErr) {
	if parentID == 0 {
		return nil, catID, nil
	}
	if parentID == selfID {
		return nil, nil, errf(http.StatusBadRequest, "bad_parent", "不能把频道挂到自己下面")
	}
	parent, err := s.store.ChannelByID(ctx, parentID)
	if err != nil || parent.ServerID != serverID {
		// 跨服/不存在统一不出存在性。
		return nil, nil, errf(http.StatusBadRequest, "bad_parent", "父频道不存在或不属于该服务器")
	}
	if parent.ParentID != nil {
		return nil, nil, errf(http.StatusBadRequest, "bad_parent", "父频道不能是子频道（最多两级）")
	}
	if parent.Type != chType {
		return nil, nil, errf(http.StatusBadRequest, "bad_parent", "父子频道类型必须相同")
	}
	if !explicitCat && catID == nil {
		// 缺省分类 → 继承父分类（创建子频道省略 category 的语义）。
		out := catID
		if parent.CategoryID != nil {
			v := *parent.CategoryID
			out = &v
		}
		return parent, out, nil
	}
	if (catID == nil) != (parent.CategoryID == nil) || (catID != nil && *catID != *parent.CategoryID) {
		return nil, nil, errf(http.StatusBadRequest, "bad_parent", "父子频道必须同属一个分类")
	}
	return parent, catID, nil
}

// hasChildren 该频道是否已有子频道（禁改 category/type/挂父的 409 门）。
func (s *Server) hasChildren(ctx context.Context, serverID, channelID int64) (bool, error) {
	children, err := s.channelChildren(ctx, serverID, channelID)
	if err != nil {
		return false, err
	}
	return len(children) > 0, nil
}
