// presence 在线状态（#11）：presence:{uid} 键由 hub 写、REST 侧读。
// 写入点：连接 register 后 SET（TTL 90s）、心跳成功续期、cleanup 且该 uid 无
// 其它连接时 DEL。DEL 必须幂等且带连接实例核对（unregister 已保证被顶替的旧
// 连接不会误清新连接的在线态）。
package signal

import (
	"context"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// PresenceStore 在线状态存储 + 好友关系查询（store.KV + store.Store 的并集子集；
// nil = 不做 presence，测试/过渡）。presence 键值形态见 presenceKey。
type PresenceStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	// FriendshipsOfUser 返回该用户参与的全部关系（两个方向）。
	FriendshipsOfUser(ctx context.Context, userID int64) ([]store.Friendship, error)
}

// defaultPresenceTTL 在线态 TTL：心跳 25s 一续期，90s 无续期即离线（§0.4）。
const defaultPresenceTTL = 90 * time.Second

// presenceKey 在线态键（§0.4）。与 api 包的同名格式手工同步——两包互不依赖，
// 改格式必须两边一起改。
func presenceKey(uid string) string { return "presence:" + uid }

// markOnline 连接建立：SET presence:{uid} 并在 0→1 变化时推在线。
// 调用方不得持有 h.mu（内含 KV I/O 与好友查询）。
func (h *Hub) markOnline(c *client) {
	if h.presence == nil {
		return
	}
	ctx := context.Background()
	if err := h.presence.Set(ctx, presenceKey(c.userID), "1", h.ttl()); err != nil {
		h.log.Printf("presence: SET %s 失败: %v", c.userID, err)
	}
	// 去抖：该 uid 已有其它连接（多设备）时状态未变，不推。
	if h.connCount(c.userID) <= 1 {
		h.pushPresence(c.userID, true)
	}
}

// markOffline 连接清理：该 uid 已无任何连接时 DEL 并推离线。
// 由 cleanup 调用（unregister 之后），调用方不得持有 h.mu。
func (h *Hub) markOffline(uid string) {
	if h.presence == nil {
		return
	}
	if h.connCount(uid) > 0 {
		return // 其它设备仍在线：状态未变
	}
	ctx := context.Background()
	if err := h.presence.Del(ctx, presenceKey(uid)); err != nil {
		h.log.Printf("presence: DEL %s 失败: %v", uid, err)
	}
	h.pushPresence(uid, false)
}

// renewPresence 心跳成功续期（25s 一拍，TTL 90s）。仅续期，不推送。
func (h *Hub) renewPresence(c *client) {
	if h.presence == nil {
		return
	}
	if err := h.presence.Set(context.Background(), presenceKey(c.userID), "1", h.ttl()); err != nil {
		h.log.Printf("presence: 续期 %s 失败: %v", c.userID, err)
	}
}

// pushPresence 把 uid 的在线状态推给其互为已接受好友的会话（#32 事件面）。
// 单向好友（pending/blocked）不推；好友数量小，逐个查关系即可。
// 目标是好友的**全部会话**（在线实时投递 + 断线续传窗口内进环），不只在线连接。
func (h *Hub) pushPresence(uid string, online bool) {
	ctx := context.Background()
	id, err := parseUID(uid)
	if err != nil {
		return // dev- 等非数字 uid 无好友关系
	}
	fs, err := h.presence.FriendshipsOfUser(ctx, id)
	if err != nil {
		h.log.Printf("presence: 查 %s 好友失败: %v", uid, err)
		return
	}
	data := PresenceUpdateData{UserID: uid, Online: online, TS: time.Now().UnixMilli()}
	uids := make(map[string]bool, len(fs))
	for _, f := range fs {
		if f.Status != store.FriendAccepted {
			continue
		}
		other := f.FriendID
		if f.FriendID == id {
			other = f.UserID
		}
		uids[formatUID(other)] = true
	}
	h.broadcastEvent(uids, TypePresenceUpdate, data)
}

// ttl presence TTL：Hub 字段可被测试缩短，零值回落默认。
func (h *Hub) ttl() time.Duration {
	if h.presenceTTL > 0 {
		return h.presenceTTL
	}
	return defaultPresenceTTL
}

// parseUID/formatUID hub 的 string uid 与 store 的 int64 uid 互转（presence 推送
// 要查好友表）。非数字 uid（dev 令牌）在 parseUID 报错，调用方直接跳过。
func parseUID(uid string) (int64, error) { return strconv.ParseInt(uid, 10, 64) }

func formatUID(id int64) string { return strconv.FormatInt(id, 10) }
