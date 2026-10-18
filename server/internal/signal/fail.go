// fail.go ICE 失败清理出口（#19）：SFU 摘除失败 peer 后经 SignalSender.SendFailed
// 回到这里，把信令层房间登记清掉并通知两端。放独立文件而不改 hub.go（T6 拥有）。
package signal

// SendFailed 实现 sfu.SignalSender（#19）：本端 PeerConnection 因 ICE 失败被 SFU
// 移除。只发给该 peer 的连接；客户端收到 rtc.failed 后销毁本地 PC 并重发
// channel.join 重建入房。SFU 侧 peer 与下行路由已在 onConnFailed 摘完，这里
// 只做信令层成员登记与广播——不能再调 sfu.LeaveIf：同一 WS 连接重入房用的是
// 同一 connID，二次清理会把刚建好的新 peer 误杀。
func (s *clientSignal) SendFailed(channelID, userID, reason string) {
	s.hub.failICE(s.c, channelID, reason)
}

// failICE 清理该连接实例在信令房间的登记并向两端下发（#19）。
// 顺序：先给其余成员广播 channel.peer_left，再给本端发 rtc.failed——客户端收到
// rtc.failed 会立刻重发 channel.join，成员摘除必须在此之前完成，否则重入被
// 「已在该频道中」拒绝。channelID 与连接当前房间不符（陈旧通知）时只发消息不摘成员。
func (h *Hub) failICE(c *client, channelID, reason string) {
	h.mu.Lock()
	var w *leaveWork
	if channelID != "" && c.roomIDLocked() == channelID {
		w = h.leaveWorkLocked(c)
	}
	h.mu.Unlock()
	if w != nil && w.removed {
		h.log.Printf("ws: %s 离开频道 %s（ICE 失败）", w.c.userID, w.roomID)
		for _, m := range w.remaining {
			h.sendJSON(m, TypeChannelPeerLeft, ChannelPeerLeftData{
				ChannelID: w.roomID,
				UserID:    w.c.userID,
			})
		}
	}
	h.sendJSON(c, TypeRTCFailed, RTCFailedData{ChannelID: channelID, Reason: reason})
}
