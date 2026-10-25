package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SearchMessages 全局消息搜索（#42）：跨服务器频道与私聊/群聊会话，
// 只返回当前用户可见频道内的消息（服务端按 ViewChannel / 会话成员过滤）。
// 结果按相关度与 ID 倒序，载荷字段与 ListMessages 的 MessageDTO 同口径
// （含附件、提及与回复预览；不含发送者 email）。
//
// q 先 trim 再发送：空白搜索服务端必回 400 bad_query，客户端直接返回空结果，
// 不把注定失败的请求发出去。limit 夹到 [1,50]，<=0 按默认 20——服务端对显式
// 越界值 fail-closed 返回 400 bad_limit，发出去的总是契约内的值。
// 空结果归一为 []（JSON null 也解成空切片），前端可安全 .length。
func (c *APIClient) SearchMessages(query string, limit int) ([]MessageDTO, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []MessageDTO{}, nil
	}
	if limit <= 0 {
		limit = 20
	} else if limit > 50 {
		limit = 50
	}
	params := url.Values{}
	params.Set("q", q)
	params.Set("limit", strconv.Itoa(limit))
	status, data, err := c.get("/search?" + params.Encode())
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var list []MessageDTO
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode search: %w", err)
	}
	if list == nil {
		list = []MessageDTO{}
	}
	return list, nil
}
