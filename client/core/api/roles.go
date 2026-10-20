package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 角色 / 频道覆盖 / 审计（#35 B4）。wire 形状与 api/openapi.yaml 的
// Role/ChannelOverwrite/AuditLogEntry 对齐；ID 为字符串（#31），权限位为十进制串。

// RoleDTO 服务器角色。permissions 为十进制权限位串（低 15 位）；
// builtin 角色（@everyone/admin/owner）不可改删。
type RoleDTO struct {
	ID          ID        `json:"id"`
	ServerID    ID        `json:"server_id"`
	Name        string    `json:"name"`
	Color       string    `json:"color"`
	Position    int       `json:"position"`
	Permissions string    `json:"permissions"`
	Builtin     bool      `json:"builtin"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RoleWrite 创建/更新角色的公共 body（name 与 permissions 必填）。
type RoleWrite struct {
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Position    *int   `json:"position,omitempty"`
	Permissions string `json:"permissions"`
}

// ChannelOverwriteDTO 频道覆盖。allow/deny 为十进制权限位串（仅频道位）。
type ChannelOverwriteDTO struct {
	ChannelID  ID        `json:"channel_id"`
	TargetType string    `json:"target_type"` // role | member
	TargetID   ID        `json:"target_id"`
	Allow      string    `json:"allow"`
	Deny       string    `json:"deny"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AuditLogDTO 审计条目。detail 为变更字段的 JSON 字符串（不含隐私数据）。
type AuditLogDTO struct {
	ID         ID        `json:"id"`
	ServerID   ID        `json:"server_id"`
	ActorID    ID        `json:"actor_id"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type,omitempty"`
	TargetID   *ID       `json:"target_id,omitempty"`
	ChannelID  *ID       `json:"channel_id,omitempty"`
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
}

// put JSON PUT（带 Bearer；全量替换类端点：成员角色、频道覆盖）。
func (c *APIClient) put(path string, body any) (int, []byte, error) {
	base := c.Base()
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPut, base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setVersionHeader(req)
	status, data, err := c.do(req, true, base)
	if err != nil {
		return 0, nil, fmt.Errorf("put %s: %w", path, err)
	}
	return status, data, nil
}

// ListRoles 服务器角色列表（成员可读，按 position、id 升序）。
func (c *APIClient) ListRoles(serverID string) ([]RoleDTO, error) {
	status, data, err := c.get(fmt.Sprintf("/servers/%s/roles", serverID))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out []RoleDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode roles: %w", err)
	}
	return out, nil
}

// CreateRole 创建自定义角色（需 ManageRoles；层级防提权在服务端）。
func (c *APIClient) CreateRole(serverID string, req RoleWrite) (*RoleDTO, error) {
	status, data, err := c.post(fmt.Sprintf("/servers/%s/roles", serverID), req, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	var out RoleDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode role: %w", err)
	}
	return &out, nil
}

// UpdateRole 更新自定义角色（builtin 403；body 缺省字段沿用现值由服务端补）。
func (c *APIClient) UpdateRole(roleID string, req RoleWrite) (*RoleDTO, error) {
	status, data, err := c.patch(fmt.Sprintf("/roles/%s", roleID), req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out RoleDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode role: %w", err)
	}
	return &out, nil
}

// DeleteRole 删除自定义角色（级联分配与角色覆盖；builtin 403）。
func (c *APIClient) DeleteRole(roleID string) error {
	status, data, err := c.del(fmt.Sprintf("/roles/%s", roleID))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// SetMemberRoles 全量替换成员角色（@everyone 与 owner 的 owner 角色由服务端强制保留）。
func (c *APIClient) SetMemberRoles(serverID, userID string, roleIDs []string) error {
	status, data, err := c.put(fmt.Sprintf("/servers/%s/members/%s/roles", serverID, userID),
		map[string][]string{"role_ids": roleIDs})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// ListChannelOverwrites 频道覆盖列表（需 ManageChannels）。
func (c *APIClient) ListChannelOverwrites(channelID string) ([]ChannelOverwriteDTO, error) {
	status, data, err := c.get(fmt.Sprintf("/channels/%s/overwrites", channelID))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out []ChannelOverwriteDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode overwrites: %w", err)
	}
	return out, nil
}

// SetChannelOverwrite upsert 频道覆盖（targetType=role|member；allow/deny 为十进制位串）。
func (c *APIClient) SetChannelOverwrite(channelID, targetType, targetID, allow, deny string) (*ChannelOverwriteDTO, error) {
	status, data, err := c.put(fmt.Sprintf("/channels/%s/overwrites/%s/%s", channelID, targetType, targetID),
		map[string]string{"allow": allow, "deny": deny})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out ChannelOverwriteDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode overwrite: %w", err)
	}
	return &out, nil
}

// DeleteChannelOverwrite 删除频道覆盖。
func (c *APIClient) DeleteChannelOverwrite(channelID, targetType, targetID string) error {
	status, data, err := c.del(fmt.Sprintf("/channels/%s/overwrites/%s/%s", channelID, targetType, targetID))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// ListAuditLog 审计日志倒序页（需 ManageServer）。before=空取最新页；
// limit 须在 1–50（越界服务端 400）。
func (c *APIClient) ListAuditLog(serverID, before string, limit int) ([]AuditLogDTO, error) {
	q := fmt.Sprintf("/servers/%s/audit-log?limit=%d", serverID, limit)
	if before != "" {
		q += "&before=" + before
	}
	status, data, err := c.get(q)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out []AuditLogDTO
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode audit log: %w", err)
	}
	return out, nil
}
