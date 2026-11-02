// oidcstate.go — OIDC 登录 state 的一次性存储（#54，批次 6 §7）。
// 任务书要求「Redis 存储、5 分钟 TTL、一次性消费」：这里复用服务端现有 KV 层
// （store.KV）落实——配置了 LITETALK_REDIS_ADDR 时它就是 Redis，未配置的单机
// 部署落 SQLite/进程内 KV，与刷新令牌/验证码/限流同一基础设施与同一持久性口径
// （多实例部署本就必须配 Redis，见 main.go 的启动日志）。
//
// 一次性消费用现有 CASScript 原子换标记实现：Get 读原值 → CAS(原值→consumed)。
// Redis EVAL 单线程、Mem 全局锁，两个并发回调只有一个能把 CAS 换成 1，
// state 重放与并发竞用都被拒绝。KV 为 nil（部分测试构造 api.Server 不带 KV）
// 时退进程内 TTL 表，单进程语义等价。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// OIDCState state 键下挂载的会话数据。verifier/nonce 不出服务端；
// dev/IP 在发起授权时定格，回调直接复用（设备类型不信任回调方再传）。
type OIDCState struct {
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Dev      string `json:"d"`
	IP       string `json:"ip"`
}

const (
	// OIDCStateTTL state 有效窗口：IdP 授权页停留超过 5 分钟即失效，须重新发起。
	OIDCStateTTL = 5 * time.Minute
	// oidcStateConsumedMark CAS 消费标记。键只要存在（无论值）都拒绝再次消费，
	// 标记过期后键消失同样拒绝——TTL 只影响键的残留时长，不影响语义。
	oidcStateConsumedMark = "consumed"
	// oidcStateConsumedTTL 消费标记残留时长（覆盖正常回调的重试窗口即可）。
	oidcStateConsumedTTL = 60 * time.Second
)

// state 未知/过期/已被消费三类失败折叠成同一错误：调用方一律让用户重新发起，
// 不区分具体原因（不给探测与重放尝试提供反馈面）。
var ErrOIDCState = errors.New("oidc: state unknown or already consumed")

// OIDCStates state 存取器。
type OIDCStates struct {
	kv  store.KV
	now func() time.Time

	mu  sync.Mutex
	mem map[string]oidcStateEntry // kv 为 nil 时的进程内后备
}

type oidcStateEntry struct {
	data    OIDCState
	expires time.Time
}

// NewOIDCStates 构造。kv 为 nil 时用进程内 TTL 表（仅测试/单进程降级）。
func NewOIDCStates(kv store.KV) *OIDCStates {
	return &OIDCStates{kv: kv, now: time.Now, mem: map[string]oidcStateEntry{}}
}

// Issue 生成高熵 state 并落存。任一失败都返回错误（绝不降级成可预测 state）。
func (s *OIDCStates) Issue(ctx context.Context, data OIDCState) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	blob, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	if s.kv != nil {
		if err := s.kv.Set(ctx, oidcStateKey(state), string(blob), OIDCStateTTL); err != nil {
			return "", err
		}
		return state, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// 写入频率低（登录发起），全量清扫过期项足够。
	for k, e := range s.mem {
		if now.After(e.expires) {
			delete(s.mem, k)
		}
	}
	s.mem[state] = oidcStateEntry{data: data, expires: now.Add(OIDCStateTTL)}
	return state, nil
}

// Consume 一次性取出：不存在/已过期/已被并发消费一律 ErrOIDCState。
func (s *OIDCStates) Consume(ctx context.Context, state string) (OIDCState, error) {
	if state == "" {
		return OIDCState{}, ErrOIDCState
	}
	key := oidcStateKey(state)
	if s.kv != nil {
		val, err := s.kv.Get(ctx, key)
		if err != nil {
			return OIDCState{}, ErrOIDCState
		}
		// Get→CAS：只有仍为原值的键能换成 consumed 标记。两个并发消费者都读到
		// 原值，但 CAS 在 Redis/Mem 的原子性下只有一个成功。
		res, err := s.kv.Eval(ctx, store.CASScript, []string{key},
			val, oidcStateConsumedMark, itoa(int64(oidcStateConsumedTTL/time.Second)))
		if err != nil {
			return OIDCState{}, ErrOIDCState
		}
		if n, _ := res.(int64); n != 1 {
			return OIDCState{}, ErrOIDCState
		}
		var data OIDCState
		if err := json.Unmarshal([]byte(val), &data); err != nil {
			return OIDCState{}, ErrOIDCState
		}
		return data, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.mem[state]
	if !ok || s.now().After(e.expires) {
		delete(s.mem, state)
		return OIDCState{}, ErrOIDCState
	}
	delete(s.mem, state) // 一次性：取出即作废
	return e.data, nil
}

func oidcStateKey(state string) string { return "oidc:state:" + state }
