package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ---------- 未读与提及计数（#40） ----------

// UnreadChannelDTO 单个频道的未读/提及计数（与契约 UnreadChannel 对齐）。
// 计数为 0 的频道仍出现在快照里；ID 为字符串 wire（#31）。
type UnreadChannelDTO struct {
	ChannelID    ID  `json:"channel_id"`
	UnreadCount  int `json:"unread_count"`
	MentionCount int `json:"mention_count"`
}

// UnreadServerDTO 服务器维度的未读聚合（与契约 UnreadServer 对齐）：
// 只含该服务器下当前用户可见的文字频道，不含 direct/group 会话。
type UnreadServerDTO struct {
	ServerID     ID  `json:"server_id"`
	UnreadCount  int `json:"unread_count"`
	MentionCount int `json:"mention_count"`
}

// UnreadsDTO 当前用户的未读快照（与契约 Unreads 对齐）。两个数组在
// ListUnreads 里归一为非 nil，避免桥接层把 nil 序列化成 null。
type UnreadsDTO struct {
	Channels []UnreadChannelDTO `json:"channels"`
	Servers  []UnreadServerDTO  `json:"servers"`
}

// decodeUnreadChannel 解单频道计数响应。
func decodeUnreadChannel(data []byte) (*UnreadChannelDTO, error) {
	var u UnreadChannelDTO
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("decode unread channel: %w", err)
	}
	return &u, nil
}

// ListUnreads 拉当前用户的未读快照：channels 覆盖全部可见频道（含 0 值项），
// servers 覆盖已加入服务器。空数组归一为 []。
func (c *APIClient) ListUnreads() (*UnreadsDTO, error) {
	status, data, err := c.get("/unreads")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out UnreadsDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode unreads: %w", err)
	}
	if out.Channels == nil {
		out.Channels = []UnreadChannelDTO{}
	}
	if out.Servers == nil {
		out.Servers = []UnreadServerDTO{}
	}
	return &out, nil
}

// MarkChannelRead 上报频道已读到 lastMessageID，返回服务端重算后的计数。
// 水位只前进：更旧的 ID 不会让计数回退；消息不属于该频道/不存在返回
// 400 bad_read（由 decodeError 保留机器码）。
func (c *APIClient) MarkChannelRead(channelID, lastMessageID string) (*UnreadChannelDTO, error) {
	status, data, err := c.put(fmt.Sprintf("/channels/%s/read", channelID),
		map[string]string{"last_message_id": lastMessageID})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	return decodeUnreadChannel(data)
}
