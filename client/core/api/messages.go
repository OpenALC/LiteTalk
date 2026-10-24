package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ---------- 消息编辑 / 软删 / 回复 / @提及（#39） ----------

// ReplyPreviewDTO 回复目标预览：只含跳转与展示所需字段，不含 email。
// 目标已软删时 Content 为空串、Deleted=true；目标不存在（历史脏数据）时
// 整个 reply_preview 字段缺省（MessageDTO.ReplyPreview 为 nil）。
type ReplyPreviewDTO struct {
	ID       flexID `json:"id"`
	UserID   flexID `json:"user_id"`
	Nickname string `json:"nickname"`
	Content  string `json:"content"`
	Deleted  bool   `json:"deleted"`
}

// MentionCandidateDTO @提及候选用户：只含昵称/tag，不含 email。
type MentionCandidateDTO struct {
	UserID   flexID `json:"user_id"`
	Nickname string `json:"nickname"`
	Tag      string `json:"tag"`
}

// GetMessage 拉频道内单条消息。软删消息仍返回（content 为空串、deleted_at 有值），
// 供回复跳转；不存在/不属于该频道返回 404。
func (c *APIClient) GetMessage(channelID, messageID int64) (*MessageDTO, error) {
	status, data, err := c.get(fmt.Sprintf("/channels/%d/messages/%d", channelID, messageID))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var m MessageDTO
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return &m, nil
}

// EditMessage 编辑自己的消息，返回更新后的消息（edited_at 已置位）。
// mentions 总是上送：nil 归一为空数组——JSON 里 null 会被服务端当成「缺省」，
// 语义变成保留原提及；空数组才是「清除全部提及」。
// 非作者 403；已删除 404 message_deleted（由 decodeError 保留机器码）。
func (c *APIClient) EditMessage(channelID, messageID int64, content string, mentions []string) (*MessageDTO, error) {
	if mentions == nil {
		mentions = []string{}
	}
	status, data, err := c.patch(fmt.Sprintf("/channels/%d/messages/%d", channelID, messageID),
		map[string]any{"content": content, "mentions": mentions})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var m MessageDTO
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return &m, nil
}

// DeleteMessage 软删除消息（作者或管理员），成功返回 204。
// 重复删除 404 message_deleted；不存在/不属于该频道 404 not_found。
func (c *APIClient) DeleteMessage(channelID, messageID int64) error {
	status, data, err := c.del(fmt.Sprintf("/channels/%d/messages/%d", channelID, messageID))
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return decodeError(status, data)
	}
	return nil
}

// ListMentionCandidates 拉可被 @ 的用户（按 nickname、user_id 升序）。
// 服务器频道为对该频道有 ViewChannel 的成员，会话频道为会话成员；空结果为 []。
func (c *APIClient) ListMentionCandidates(channelID int64) ([]MentionCandidateDTO, error) {
	status, data, err := c.get(fmt.Sprintf("/channels/%d/mention-candidates", channelID))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var list []MentionCandidateDTO
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode mention candidates: %w", err)
	}
	return list, nil
}
