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
// 这里在 presenceMu 下重新数连接数，后到的写入总是取最新事实。
//
// KV 写入与推送的判据分开：
//   - 在线：总是重试 SET（即使 presencePublished 已为 true）。重复写同一个值幂等，
//     而首 SET 失败后键可能只靠 renewPresence 续命；不依赖 published 去重才能
//     在下一次在线边沿补写。published 只决定是否推 online。
//   - 离线：无条件幂等 DEL。首 SET 失败后 renewPresence 可能已把键写回，此时
//     published 仍是 false，若按 published 跳过 DEL 会让 presence 键残留到 TTL；
//     published 只决定是否推 offline（好友从未收到 online 时不必推）。
func (h *Hub) syncPresence(uid string) {
	if h.presence == nil {
		return
	}
	h.presenceMu.Lock()
	defer h.presenceMu.Unlock()
	if h.presencePublished == nil {
		h.presencePublished = make(map[string]bool)
	}
	ctx := context.Background()
	if h.connCount(uid) > 0 {
		if err := h.presence.Set(ctx, presenceKey(uid), "1", h.ttl()); err != nil {
			h.log.Printf("presence: SET %s 失败: %v", uid, err)
			return
		}
		if h.presencePublished[uid] {
			return // 已推过 online：只补写 KV，不重推
		}
		h.presencePublished[uid] = true
		h.pushPresence(uid, true)
		return
	}
	// 离线边沿（#92）：先无条件清频道去抖缓存——不能依赖 KV DEL 成功，
	// 否则重连再进同一频道会被当成「状态未变」而不重推。
	// #102：待重试标记一并清掉，离线时没人再需要这次频道推送。
	delete(h.presenceChanPublished, uid)
	delete(h.presenceChanRetry, uid)
	if err := h.presence.Del(ctx, presenceKey(uid)); err != nil {
		h.log.Printf("presence: DEL %s 失败: %v", uid, err)
		return
	}
	pushed := h.presencePublished[uid]
	delete(h.presencePublished, uid) // 缺省即离线，避免 map 随历史用户无限增长
	if pushed {
		h.pushPresence(uid, false)
	}
}

// renewPresence 心跳成功续期（25s 一拍，TTL 90s）。仅续期，不推送，也不改
// presencePublished：首 SET 失败时好友从未收到 online，续期只让 REST/成员快照
// 看到在线，断线时由 syncPresence 无条件 DEL 收掉键，不应补推一条 offline。
// 与 syncPresence 共用 presenceMu：若连接已被顶替/摘除，取消续期，避免在
// 离线 DEL 之后又把键写回来。
func (h *Hub) renewPresence(c *client) {
	if h.presence == nil {
		return
	}
	h.presenceMu.Lock()
	if h.connByDevice(c.userID, c.dev) != c {
		h.presenceMu.Unlock()
		return
	}
	if err := h.presence.Set(context.Background(), presenceKey(c.userID), "1", h.ttl()); err != nil {
		h.log.Printf("presence: 续期 %s 失败: %v", c.userID, err)
	}
	// #102：上一次频道推送有接收者因权限查询故障被跳过时，借这次心跳重算。
	// 不能在这里直接调 syncPresenceChannel——它要拿同一把 presenceMu（不可重入），
	// 先放锁再调；锁内只取标记。
	_, retry := h.presenceChanRetry[c.userID]
	h.presenceMu.Unlock()
	if retry {
		h.syncPresenceChannel(c.userID)
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

// presenceChanState 去抖缓存的语音房状态（#92）。serverID 为十进制字符串，
// "0" = dev 内置房间（推时不带频道字段）。零值 = 不在语音房。
type presenceChanState struct {
	channelID string
	serverID  string
}

// friendView 查看者对目标语音房的 ViewChannel 判定结果（#92）。
// unknown 专用于「查不出来」（authz 未注入/查询故障）：跳过该接收者，
// 不把故障当有权（泄露频道）或无权（当作无权会把故障伪装成否定事实）。
type friendView int

const (
	viewUnknown friendView = iota
	viewYes
	viewNo
)

// presenceChanPending 好友频道推送的待重试集合（#102）。
// viewers 为 nil 表示接收者集合未知（好友查询本身失败），下一次整体重算；
// 非 nil 时只补发其中仍待处理的查看者——已经收到过的好友不重复打扰。
type presenceChanPending struct {
	viewers map[string]bool
}

// syncPresenceChannel 把 uid 的语音房变化推给其互为已接受好友的会话（#92）。
// 与 syncPresence 共用 presenceMu（锁序 presenceMu → h.mu）。触发点：join 登记
// 成功、leaveRoom、KickChannel 摘除、断线 cleanup 的离房；失败路径不推。
//
//   - 最后一条连接断开时跳过：离线由 syncPresence 的 offline 载荷表达，
//     频道去抖缓存也在那个边沿被无条件清空。
//   - 与去抖缓存比较，状态未变不推；无频道只推 online=true（不带频道键）。
//   - 有频道时逐个接收者按其 ViewChannel 过滤：通过推带 channel_id/server_id
//     的载荷，不通过推 online-only，查询故障跳过该接收者并记日志。
//   - dev 非数字房间（serverID==0）一律不带频道字段；载荷永不包含频道名。
//   - #102：被跳过的接收者记进待重试集合，下一次心跳/状态变化只补发他们。
func (h *Hub) syncPresenceChannel(uid string) {
	if h.presence == nil {
		return
	}
	h.presenceMu.Lock()
	defer h.presenceMu.Unlock()
	if h.connCount(uid) == 0 {
		return
	}
	chID, sid, inRoom := h.VoiceChannelOf(uid)
	cur := presenceChanState{}
	if inRoom {
		cur = presenceChanState{channelID: chID, serverID: sid}
	}
	if h.presenceChanPublished == nil {
		h.presenceChanPublished = make(map[string]presenceChanState)
	}
	if h.presenceChanRetry == nil {
		h.presenceChanRetry = make(map[string]presenceChanPending)
	}
	prev, seen := h.presenceChanPublished[uid]
	pending, hasPending := h.presenceChanRetry[uid]
	if seen && prev == cur && !hasPending {
		return // 状态未变（同房重复 join、同房换设备等）：去抖
	}
	// 状态没变、只是补发上次被跳过的接收者：收件人候选收窄到待重试集合，
	// 已经收到过的好友不再重复推。
	partialOnly := seen && prev == cur && hasPending && pending.viewers != nil
	ctx := context.Background()
	id, err := parseUID(uid)
	if err != nil {
		return // dev 等非数字 uid 无好友关系
	}
	fs, err := h.presence.FriendshipsOfUser(ctx, id)
	if err != nil {
		h.log.Printf("presence: 查 %s 好友失败: %v", uid, err)
		// #102：接收者未知 → 整体待重试。不推进已发送状态之外的东西，
		// 下一次心跳重算。
		h.presenceChanRetry[uid] = presenceChanPending{}
		return
	}
	ts := time.Now().UnixMilli()
	onlineOnly := PresenceUpdateData{UserID: uid, Online: true, TS: ts}
	full := onlineOnly
	// dev 非数字房间（serverID==0）不推频道字段；两字段同现同缺。
	if cur.serverID != "" && cur.serverID != "0" {
		full.ChannelID, full.ServerID = cur.channelID, cur.serverID
	}
	withChan := make(map[string]bool, len(fs))
	only := make(map[string]bool, len(fs))
	nextPending := make(map[string]bool)
	for _, f := range fs {
		if f.Status != store.FriendAccepted {
			continue
		}
		other := f.FriendID
		if f.FriendID == id {
			other = f.UserID
		}
		otherUID := formatUID(other)
		if partialOnly && !pending.viewers[otherUID] {
			continue // 上一轮已送达：补发不重复打扰
		}
		if full.ChannelID == "" {
			only[otherUID] = true
			continue
		}
		switch h.friendViewChannel(ctx, other, cur) {
		case viewYes:
			withChan[otherUID] = true
		case viewNo:
			only[otherUID] = true
		default: // viewUnknown：跳过，不推，但要留待重试标记
			nextPending[otherUID] = true
		}
	}
	if len(withChan) > 0 {
		h.broadcastEvent(withChan, TypePresenceUpdate, full)
	}
	if len(only) > 0 {
		h.broadcastEvent(only, TypePresenceUpdate, onlineOnly)
	}
	// #102：仍有被跳过的查看者时保留待重试标记（只记他们），否则清掉。
	// 已发送状态照常推进——这批接收者确实收到了，只是有一部分没收到。
	if len(nextPending) > 0 {
		h.presenceChanRetry[uid] = presenceChanPending{viewers: nextPending}
	} else {
		delete(h.presenceChanRetry, uid)
	}
	h.presenceChanPublished[uid] = cur
}

// friendViewChannel 判定查看者对目标语音房是否有 ViewChannel（#92）。
// 权限门槛只看 ViewChannel，不要求 ConnectVoice；authz 未注入与查询故障同路径。
func (h *Hub) friendViewChannel(ctx context.Context, viewerID int64, ch presenceChanState) friendView {
	if h.authz == nil {
		h.log.Printf("presence: 频道推送跳过查看者 %d（权限服务未就绪）", viewerID)
		return viewUnknown
	}
	channelID, err := strconv.ParseInt(ch.channelID, 10, 64)
	if err != nil {
		return viewNo // serverID!=0 的房间必是数字频道；解析失败按无权，不出频道字段
	}
	serverID, err := strconv.ParseInt(ch.serverID, 10, 64)
	if err != nil {
		h.log.Printf("presence: 频道推送跳过查看者 %d（server_id %q 非法）", viewerID, ch.serverID)
		return viewUnknown
	}
	ok, err := h.authz.CanInServer(ctx, viewerID, serverID, channelID, store.ViewChannel)
	if err != nil {
		h.log.Printf("presence: 频道权限查询失败（查看者 %d 频道 %d）: %v", viewerID, channelID, err)
		return viewUnknown
	}
	if ok {
		return viewYes
	}
	return viewNo
}

// parseUID/formatUID hub 的 string uid 与 store 的 int64 uid 互转（presence 推送
// 要查好友表）。非数字 uid（dev 令牌）在 parseUID 报错，调用方直接跳过。
func parseUID(uid string) (int64, error) { return strconv.ParseInt(uid, 10, 64) }

func formatUID(id int64) string { return strconv.FormatInt(id, 10) }
