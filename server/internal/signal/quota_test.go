package signal

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// untilErr 读到一条 error 帧并解码（跳过其它下行）。
func (p *pump) untilErr() ErrorData {
	p.t.Helper()
	env := p.until(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		p.t.Fatalf("decode error 帧: %v", err)
	}
	return e
}

// TestConnQuota 单 uid 连接数上限（#20）：超限拒握手 429 quota_exceeded；
// 同 uid+dev 的顶替重连不占新名额，不受限。
func TestConnQuota(t *testing.T) {
	f := newChatFixture(t)
	f.hub.maxConns = 1

	f.dial("1", "alice-desktop") // 第 1 条：放行

	// 第 2 条（异设备）：握手 429。
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?token=1:mobile:" + defaultTestSid("1")
	conn, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("超连接数应拒绝握手")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("期望握手 429，实际 %+v", resp)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), ErrCodeQuotaExceeded) {
		t.Fatalf("响应应含 quota_exceeded，实际 %s", body[:n])
	}

	// 同 uid+dev 顶替重连：净增 0，放行（dial 失败会 Fatal）。
	f.dial("1:desktop:"+defaultTestSid("1"), "alice-desktop-2")
}

// TestJoinRateLimit channel.join 单连接频控（#20）：超限 rate_limited。
func TestJoinRateLimit(t *testing.T) {
	f := newChatFixture(t)
	f.hub.joinRate = 2
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	p := newPump(t, a)
	p.until(TypeHello)

	// 3 连 join（limit=2）：第 1 次成功，第 2 次 already_in_channel（占额度），
	// 第 3 次被频控拦下（先于业务校验）。
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	p.until(TypeChannelJoined)
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	if e := p.untilErr(); e.Code != ErrCodeAlreadyInChan {
		t.Fatalf("第 2 次应 already_in_channel，实际 %+v", e)
	}
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	if e := p.untilErr(); e.Code != ErrCodeRateLimited {
		t.Fatalf("第 3 次应 rate_limited，实际 %+v", e)
	}
}

// TestOfferRateLimit rtc.offer 单连接频控（#20）：超限 rate_limited。
func TestOfferRateLimit(t *testing.T) {
	f := newChatFixture(t)
	f.hub.offerRate = 2
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	p := newPump(t, a)
	p.until(TypeHello)
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	p.until(TypeChannelJoined)

	// 3 条坏 SDP 的 offer（limit=2）：前 2 条到 SFU 报 rtc_error，第 3 条被频控拦下。
	a.send(TypeRTCOffer, SDPData{SDP: "v=0"})
	if e := p.untilErr(); e.Code != ErrCodeRTC {
		t.Fatalf("第 1 条应 rtc_error，实际 %+v", e)
	}
	a.send(TypeRTCOffer, SDPData{SDP: "v=0"})
	if e := p.untilErr(); e.Code != ErrCodeRTC {
		t.Fatalf("第 2 条应 rtc_error，实际 %+v", e)
	}
	a.send(TypeRTCOffer, SDPData{SDP: "v=0"})
	if e := p.untilErr(); e.Code != ErrCodeRateLimited {
		t.Fatalf("第 3 条应 rate_limited，实际 %+v", e)
	}
}

// TestRoomFullQuotaCode 每房人数超限统一报 quota_exceeded（§0.1.3）。
func TestRoomFullQuotaCode(t *testing.T) {
	f := newChatFixture(t)
	f.hub.maxPer = 1
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	pa := newPump(t, a)
	pa.until(TypeHello)
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	pa.until(TypeChannelJoined)

	b := f.dial("2", "bob")
	pb := newPump(t, b)
	pb.until(TypeHello)
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	if e := pb.untilErr(); e.Code != ErrCodeQuotaExceeded {
		t.Fatalf("满房应 quota_exceeded，实际 %+v", e)
	}
}
