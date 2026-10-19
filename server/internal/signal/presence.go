// presence 在线状态（#11）：presence:{uid} 键由 hub 写、REST 侧读。
// 写入由 register/unregister 在 h.mu 内判定的 0→1 / 1→0 边沿触发；KV SET/DEL
// 与好友推送在锁外执行，但统一经 presenceMu 串行化并重新读取连接索引的最新
// 事实，避免旧连接的 DEL 落到新连接的 SET 之后把在线用户误删成离线。
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

// syncPresence 把该 uid 的 presence 键与好友推送对齐到连接索引的当前事实。
// 由 register/unregister 锁内判定的 0→1 / 1→0 边沿触发；调用方不得持有 h.mu。
//
// 不能把边沿上算出的 online 直接落 KV：边沿判定与 KV 应用之间还有窗口，
// 断线清理与重连可能交错，按各自捕获的状态落盘会让后执行的旧状态覆盖新状态。
// 这里在 presenceMu 下重新数连接数，后到的写入总是取最新事实；presencePublished
// 记录本进程已发布的态，兜住重复推送（同设备顶替、并发首连都只推一次）。
func (h *Hub) syncPresence(uid string) {
	if h.presence == nil {
		return
	}
	h.presenceMu.Lock()
	defer h.presenceMu.Unlock()
	if h.presencePublished == nil {
		h.presencePublished = make(map[string]bool)
	}
	online := h.connCount(uid) > 0
	if h.presencePublished[uid] == online {
		return // 状态未变：不重写、不重推
	}
	ctx := context.Background()
	if online {
		if err := h.presence.Set(ctx, presenceKey(uid), "1", h.ttl()); err != nil {
			h.log.Printf("presence: SET %s 失败: %v", uid, err)
			return
		}
	} else {
		if err := h.presence.Del(ctx, presenceKey(uid)); err != nil {
			h.log.Printf("presence: DEL %s 失败: %v", uid, err)
			return
		}
	}
	if online {
		h.presencePublished[uid] = true
	} else {
		delete(h.presencePublished, uid) // 缺省即离线，避免 map 随历史用户无限增长
	}
	h.pushPresence(uid, online)
}

// renewPresence 心跳成功续期（25s 一拍，TTL 90s）。仅续期，不推送。
// 与 syncPresence 共用 presenceMu：若连接已被顶替/摘除，取消续期，避免在
// 离线 DEL 之后又把键写回来。
func (h *Hub) renewPresence(c *client) {
	if h.presence == nil {
		return
	}
	h.presenceMu.Lock()
	defer h.presenceMu.Unlock()
	if h.connByDevice(c.userID, c.dev) != c {
		return
	}
	if err := h.presence.Set(context.Background(), presenceKey(c.userID), "1", h.ttl()); err != nil {
		h.log.Printf("presence: 续期 %s 失败: %v", c.userID, err)
	}
}

// presenceOnline 读在线态：presence:{uid} 存在 = 在线；无 KV 依赖时恒离线。
// 供 room 成员快照填充（#11）；不持 h.mu。
func (h *Hub) presenceOnline(uid string) bool {
	if h.presence == nil {
		return false
	}
	_, err := h.presence.Get(context.Background(), presenceKey(uid))
	return err == nil
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
