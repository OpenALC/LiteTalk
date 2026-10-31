// 频道密码 bcrypt 放大防护（#102）。
//
// join 频控（30/min/uid）按 uid 计数，挡不住「多账号 × 少量尝试」的持续猜密码：
// 每次尝试都是一次 cost 12 的 bcrypt，几十个账号轮着猜就能把 CPU 打满。
// 这里两道独立闸门：
//   - 失败窗口：按 uid + 频道只统计**失败**尝试（5 次/分钟，滑动窗），超限直接
//     拒绝且不跑 bcrypt。成功校验不计入并清空该键——正常用户输错几次、其后输对
//     不会被自己的历史错误锁死。
//   - 全局并发信号量：限制同时进行的 bcrypt 校验数，超时排队同样拒绝（fail
//     closed），保证即使大量不同账号同时攻击，CPU 占用也有天花板。
//
// 计数器在进程内而非 KV：与事件环同样的单实例假设（见 gateway.go 头注释），
// 且这里要的是「只在失败时累加」，而 KV 的 RateLimiter.Allow 每次调用都自增，
// 语义对不上（成功也会消耗配额）。
package signal

import (
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
)

const (
	// channelPwFailLimit / channelPwFailWindow 单 uid 单频道的失败窗口。
	channelPwFailLimit  = 5
	channelPwFailWindow = time.Minute
	// channelPwConcurrency 同时进行的密码校验数上限（cost 12 单次约 200ms，
	// 4 并发约占用 0.8 个核，够正常用户用、不足以打满机器）。
	channelPwConcurrency = 4
	// channelPwWait 拿不到信号量时的最长等待。超时按 rate_limited 拒绝——
	// 排在队尾的 bcrypt 只会放大延迟，不增加通过率。
	channelPwWait = 3 * time.Second
	// pwFailKeyCap 失败计数表的键数上限：超过时清掉已过期的条目，
	// 避免大量账号 × 频道的组合把 map 撑大（每键最多 limit 个时间戳）。
	pwFailKeyCap = 4096
)

// pwFailKey 失败窗口的键：uid + NUL + channelID（与 connKey 同样的分隔策略，
// 避免数字拼接触发歧义）。
type pwFailKey string

func makePwFailKey(uid, channelID string) pwFailKey {
	return pwFailKey(uid + "\x00" + channelID)
}

// pwAttemptAllowed 该 uid+频道当前是否还有失败配额（不累加，纯判定）。
// 顺带把窗口外的旧记录剪掉；键被剪空时删除，避免空键堆积。
func (h *Hub) pwAttemptAllowed(key pwFailKey, now time.Time) bool {
	h.pwMu.Lock()
	defer h.pwMu.Unlock()
	times := h.pwFails[key]
	if len(times) == 0 {
		delete(h.pwFails, key)
		return true
	}
	cut := now.Add(-channelPwFailWindow)
	kept := times[:0]
	for _, t := range times {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(h.pwFails, key)
		return true
	}
	h.pwFails[key] = kept
	return len(kept) < channelPwFailLimit
}

// pwRecordFailure 记一次失败。调用方已通过 pwAttemptAllowed 的配额判定；
// 并发下可能略超 limit，无害——超出的记录只让窗口多关一会儿。
func (h *Hub) pwRecordFailure(key pwFailKey, now time.Time) {
	h.pwMu.Lock()
	defer h.pwMu.Unlock()
	if h.pwFails == nil {
		h.pwFails = make(map[pwFailKey][]time.Time, 8)
	}
	if len(h.pwFails) >= pwFailKeyCap {
		cut := now.Add(-channelPwFailWindow)
		for k, times := range h.pwFails {
			if len(times) == 0 || !times[len(times)-1].After(cut) {
				delete(h.pwFails, k)
			}
		}
	}
	h.pwFails[key] = append(h.pwFails[key], now)
}

// pwClearFailures 校验成功后清空该键：成功不计入失败次数，也不保留历史失败，
// 否则用户改正后仍会被前几次错误挤到限流上。
func (h *Hub) pwClearFailures(key pwFailKey) {
	h.pwMu.Lock()
	delete(h.pwFails, key)
	h.pwMu.Unlock()
}

// pwVerify 在并发信号量内跑一次密码校验。拿不到许可返回 false（调用方回
// rate_limited）：宁可让攻击者被拒，也不能让他们把 goroutine 堆在 bcrypt 上。
// 调用方不得持有 h.mu（bcrypt 慢，不能串行化整个 hub）。
func (h *Hub) pwVerify(hash, plain string) (ok bool, admitted bool) {
	if h.pwSem != nil {
		select {
		case h.pwSem <- struct{}{}:
		default:
			wait := h.pwWait
			if wait <= 0 {
				wait = channelPwWait
			}
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case h.pwSem <- struct{}{}:
			case <-timer.C:
				return false, false
			}
		}
		defer func() { <-h.pwSem }()
	}
	check := h.pwCheck
	if check == nil {
		check = auth.CheckPassword
	}
	return check(hash, plain), true
}
