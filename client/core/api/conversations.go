package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ---------- 私聊/群私聊会话（#38 B4：REST 桥接） ----------

// ConversationMemberDTO 会话成员（与契约 ConversationMember 对齐）。
// 只含昵称/tag，不含 email；ID 为字符串 wire（#31）。
type ConversationMemberDTO struct {
	UserID   ID        `json:"user_id"`
	Nickname string    `json:"nickname"`
	Tag      string    `json:"tag"`
	Role     string    `json:"role"` // owner | member
	JoinedAt time.Time `json:"joined_at"`
}

// ConversationDTO 私聊/群私聊会话（与契约 Conversation 对齐，无服务器频道）。
// Kind：direct（私聊）| group（群私聊）；direct 的 Name 无意义（可能为空串）。
// ID 为字符串 wire（#31）。
type ConversationDTO struct {
	ID        ID                      `json:"id"`
	Kind      string                  `json:"kind"`
	Name      string                  `json:"name"`
	Members   []ConversationMemberDTO `json:"members"`
	CreatedAt time.Time               `json:"created_at"`
}

// decodeConversation 解单个会话响应。
func decodeConversation(data []byte) (*ConversationDTO, error) {
	var c ConversationDTO
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}
	return &c, nil
}

// ListConversations 列出当前用户参与的私聊/群私聊（created_at 倒序，含成员）。
// 空结果归一为 []，避免桥接层把 nil 序列化成 null。
func (c *APIClient) ListConversations() ([]ConversationDTO, error) {
	status, data, err := c.get("/conversations")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var list []ConversationDTO
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode conversations: %w", err)
	}
	if list == nil {
		list = []ConversationDTO{}
	}
	return list, nil
}

// CreateDirectConversation 创建私聊（userID 不含自己，双方须为已接受好友）。
// 同一对用户已有 direct 会话时服务端幂等返回原会话（200），新建返回 201。
func (c *APIClient) CreateDirectConversation(userID string) (*ConversationDTO, error) {
	status, data, err := c.post("/conversations", map[string]any{
		"kind":     "direct",
		"user_ids": []string{userID},
	}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	return decodeConversation(data)
}

// CreateGroupConversation 创建群私聊（userIDs 2–49 人，不含自己；name 可空）。
// 建群者为 owner。
func (c *APIClient) CreateGroupConversation(name string, userIDs []string) (*ConversationDTO, error) {
	body := map[string]any{"kind": "group", "user_ids": userIDs}
	// name 缺省与空串不同：给出时服务端按 1–64 字符校验，空串会拒——空名不发送。
	if name != "" {
		body["name"] = name
	}
	status, data, err := c.post("/conversations", body, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	return decodeConversation(data)
}

// GetConversation 会话详情（仅参与者；非参与者与不存在同为 404）。
func (c *APIClient) GetConversation(id string) (*ConversationDTO, error) {
	status, data, err := c.get(fmt.Sprintf("/conversations/%s", id))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	return decodeConversation(data)
}

// RenameConversation 群聊改名（仅 owner；direct 返回 400 not_group）。
// 请求体是 JSON Merge Patch 的 {"name": name}。
func (c *APIClient) RenameConversation(id, name string) (*ConversationDTO, error) {
	status, data, err := c.patch(fmt.Sprintf("/conversations/%s", id),
		map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	return decodeConversation(data)
}

// DeleteConversation 删除会话（direct 任一方；group 仅 owner）。服务端硬删除，返回 204。
func (c *APIClient) DeleteConversation(id string) error {
	status, data, err := c.del(fmt.Sprintf("/conversations/%s", id))
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return decodeError(status, data)
	}
	return nil
}

// AddConversationMembers 群聊加人（仅 owner；只能添加自己的已接受好友）。
func (c *APIClient) AddConversationMembers(id string, userIDs []string) (*ConversationDTO, error) {
	status, data, err := c.post(fmt.Sprintf("/conversations/%s/members", id),
		map[string]any{"user_ids": userIDs}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	return decodeConversation(data)
}

// RemoveConversationMember 移除成员或退出群聊（owner 可移除任意成员；普通成员只能
// 移除自己）。返回更新后的会话；最后一人退出时会话被删除——服务端回 204，
// 此时返回 nil, nil。
func (c *APIClient) RemoveConversationMember(id, userID string) (*ConversationDTO, error) {
	status, data, err := c.del(fmt.Sprintf("/conversations/%s/members/%s", id, userID))
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		return decodeConversation(data)
	default:
		return nil, decodeError(status, data)
	}
}
