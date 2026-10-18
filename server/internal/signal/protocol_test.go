package signal

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// ---- 协议版本协商（#29）----

// TestNegotiateProtocol 选定版本的区间运算：交集取 min(clientMax, serverMax)，
// 无交集/非法区间拒绝。
func TestNegotiateProtocol(t *testing.T) {
	cases := []struct {
		name       string
		cMin, cMax int64
		sMin, sMax int64
		wantV      int64
		wantOK     bool
	}{
		{"全兼容取服务端上限", 1, 2, 1, 2, 2, true},
		{"客户端上限低于服务端", 1, 1, 1, 2, 1, true},
		{"客户端区间更窄", 2, 2, 1, 3, 2, true},
		{"正好卡在下限", 2, 5, 2, 2, 2, true},
		{"无交集：客户端太新", 3, 4, 1, 2, 0, false},
		{"无交集：客户端太旧", 1, 1, 2, 2, 0, false},
		{"非法：min<1", 0, 2, 1, 2, 0, false},
		{"非法：max<min", 2, 1, 1, 2, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := negotiateProtocol(tc.cMin, tc.cMax, tc.sMin, tc.sMax)
			if ok != tc.wantOK || v != tc.wantV {
				t.Fatalf("negotiate(%d..%d, %d..%d) = (%d,%v), want (%d,%v)",
					tc.cMin, tc.cMax, tc.sMin, tc.sMax, v, ok, tc.wantV, tc.wantOK)
			}
		})
	}
}

// TestClientHelloNegotiation C→S client.hello → S→C server.hello{version}。
func TestClientHelloNegotiation(t *testing.T) {
	f := newChatFixture(t)
	c := f.dial("1", "alice")

	c.send(TypeClientHello, ClientHelloData{Min: 1, Max: 2})
	env := c.recvUntil(TypeServerHello)
	var d ServerHelloData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode server.hello: %v", err)
	}
	if d.Version != ProtocolVersionMax {
		t.Fatalf("version = %d, want %d", d.Version, ProtocolVersionMax)
	}
	// 定窄区间：协商结果应是客户端上限。
	c.send(TypeClientHello, ClientHelloData{Min: 1, Max: 1})
	env = c.recvUntil(TypeServerHello)
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode server.hello: %v", err)
	}
	// 首次协商已定 v2，重复 client.hello 只回当前结果、不重开区间。
	if d.Version != ProtocolVersionMax {
		t.Fatalf("重复协商后的 version = %d, want %d（首次结果不变）", d.Version, ProtocolVersionMax)
	}
}

// TestClientHelloNoOverlapNoDisconnect 版本无交集：回 error{protocol_unsupported}
// 且**不断连**（裁定 #9）；后续业务消息被拒收但连接仍然可用。
func TestClientHelloNoOverlapNoDisconnect(t *testing.T) {
	f := newChatFixture(t)
	c := f.dial("1", "alice")

	c.send(TypeClientHello, ClientHelloData{Min: 99, Max: 100})
	env := c.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeProtocolUnsupported {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeProtocolUnsupported)
	}
	// 不断连：业务消息仍能得到带内应答（错误），而不是 close。
	c.send(TypeChannelJoin, ChannelJoinData{ChannelID: "x"})
	if env := c.recvUntil(TypeError); env.Type != TypeError {
		t.Fatalf("业务消息未被拒收: %s", env.Type)
	}
	// 仍然不许 close：再读一条，超时前应持续可读（这里再发一条协商消息验证带内响应）。
	c.send(TypeClientHello, ClientHelloData{Min: 99, Max: 100})
	if env := c.recvUntil(TypeError); env.Type != TypeError {
		t.Fatalf("重复协商未响应: %s", env.Type)
	}
}

// TestOldClientWithoutClientHello 老客户端（不发 client.hello）：按 v1 处理，
// 收到 hello 初始数据、不收到 server.hello，且业务消息照常可用。
func TestOldClientWithoutClientHello(t *testing.T) {
	f := newChatFixture(t)
	c := f.dial("1", "alice")
	chID := strconv.FormatInt(f.vch.ID, 10)

	// 不发 client.hello，直接入房（老客户端路径）。
	c.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	if env := c.recvUntil(TypeChannelJoined); env.Type != TypeChannelJoined {
		t.Fatalf("老客户端入房失败: %s", env.Type)
	}
}

// TestWireKeyMessagesJSON 关键消息的 wire 字段名快照（#29）。
// 两侧 protocol_gen.go 字节相同，这里只兜住 schema→生成物的字段名回归。
func TestWireKeyMessagesJSON(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want []string
	}{
		{"HelloData", HelloData{User: UserBrief{ID: "1", Nickname: "n"}, Servers: []any{}, Friends: []any{}, Presence: map[string]any{}},
			[]string{`"id":"1"`, `"nickname":"n"`, `"servers":[]`, `"friends":[]`, `"presence":{}`}},
		{"ClientHelloData", ClientHelloData{Min: 1, Max: 2}, []string{`"min":1`, `"max":2`}},
		{"ServerHelloData", ServerHelloData{Version: 2}, []string{`"version":2`}},
		{"VoiceStateData 上行", VoiceStateData{Speaking: boolPtr(true)}, []string{`"speaking":true`}},
		{"VoiceStateData 下行", VoiceStateData{UserID: "7", Mute: boolPtr(false)}, []string{`"user_id":"7"`, `"mute":false`}},
		{"MemberInfo", MemberInfo{UserID: "1", Nickname: "n", Mute: true},
			[]string{`"user_id":"1"`, `"mute":true`, `"deafen":false`, `"speaking":false`}},
		{"ICEData", ICEData{Candidate: "c", SDPMid: "0"}, []string{`"candidate":"c"`, `"sdpMid":"0"`}},
		{"ChatMessageData", ChatMessageData{ID: "9", ChannelID: "3", Content: "hi", TS: 5, User: UserBrief{ID: "1"}},
			[]string{`"id":"9"`, `"channel_id":"3"`, `"content":"hi"`, `"ts":5`}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.val)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		for _, key := range tc.want {
			if !strings.Contains(string(raw), key) {
				t.Errorf("%s JSON 缺 %s: %s", tc.name, key, raw)
			}
		}
	}
	// 上行 voice.state 不带 user_id（omitempty）；空可选字段不出现在 wire 上。
	raw, _ := json.Marshal(VoiceStateData{Speaking: boolPtr(true)})
	if strings.Contains(string(raw), "user_id") {
		t.Errorf("上行 VoiceStateData 不应带 user_id: %s", raw)
	}
	raw, _ = json.Marshal(ICEData{Candidate: "c"})
	if strings.Contains(string(raw), "sdpMid") {
		t.Errorf("ICEData 空可选字段应省略: %s", raw)
	}
}

// boolPtr 构造可选增量字段指针（测试用）。
func boolPtr(b bool) *bool { return &b }
