package signal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestWirePresenceShutdownJSON 协议逐字同步（#11/#21）：与 server/internal/signal/protocol.go
// 同名结构的 wire 字段名必须一致，改一边必须改另一边。
func TestWirePresenceShutdownJSON(t *testing.T) {
	raw, err := json.Marshal(PresenceUpdateData{UserID: "42", Online: true, TS: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"user_id":"42"`, `"online":true`, `"ts":7`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("PresenceUpdateData JSON 缺 %s: %s", key, raw)
		}
	}
	var p PresenceUpdateData
	if err := json.Unmarshal([]byte(`{"user_id":"1","online":false,"ts":9}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.UserID != "1" || p.Online || p.TS != 9 {
		t.Fatalf("反序列化不一致: %+v", p)
	}

	raw, err = json.Marshal(ServerShutdownData{Reason: "shutdown", TS: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reason":"shutdown"`) {
		t.Fatalf("ServerShutdownData JSON 字段不一致: %s", raw)
	}

	if TypePresenceUpdate != "presence.update" || TypeServerShutdown != "server.shutdown" {
		t.Fatalf("常量值漂移: %q %q", TypePresenceUpdate, TypeServerShutdown)
	}
	if ErrCodeQuotaExceeded != "quota_exceeded" {
		t.Fatalf("配额错误码应为 quota_exceeded，实际 %q", ErrCodeQuotaExceeded)
	}
}

// TestWireRTCFailedJSON 协议逐字同步（#19）：rtc.failed 常量与载荷字段名与
// server/internal/signal/protocol.go 必须一致，改一边必须改另一边。
func TestWireRTCFailedJSON(t *testing.T) {
	if TypeRTCFailed != "rtc.failed" {
		t.Fatalf("常量值漂移: %q", TypeRTCFailed)
	}
	raw, err := json.Marshal(RTCFailedData{ChannelID: "7", Reason: "ice_failed"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"channel_id":"7"`, `"reason":"ice_failed"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("RTCFailedData JSON 缺 %s: %s", key, raw)
		}
	}
	var d RTCFailedData
	if err := json.Unmarshal([]byte(`{"channel_id":"9","reason":"ice_failed"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.ChannelID != "9" || d.Reason != "ice_failed" {
		t.Fatalf("反序列化不一致: %+v", d)
	}
}

// TestWireHelloJSON 协议版本协商消息（#29）：client.hello / server.hello 的 wire 字段
// 与 server/internal/signal 生成物一致（两侧 protocol_gen.go 字节相同）。
func TestWireHelloJSON(t *testing.T) {
	raw, err := json.Marshal(ClientHelloData{Min: 1, Max: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"min":1`, `"max":2`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("ClientHelloData JSON 缺 %s: %s", key, raw)
		}
	}
	raw, err = json.Marshal(ServerHelloData{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version":2`) {
		t.Fatalf("ServerHelloData JSON 字段不一致: %s", raw)
	}
	if TypeClientHello != "client.hello" || TypeServerHello != "server.hello" {
		t.Fatalf("常量值漂移: %q %q", TypeClientHello, TypeServerHello)
	}
	if ProtocolVersionMin != 1 || ProtocolVersionMax != 2 || ProtocolVersionCurrent != 2 {
		t.Fatalf("协议版本常量漂移: %d %d %d",
			ProtocolVersionMin, ProtocolVersionMax, ProtocolVersionCurrent)
	}
	if ErrCodeProtocolUnsupported != "protocol_unsupported" {
		t.Fatalf("错误码漂移: %q", ErrCodeProtocolUnsupported)
	}
}

// TestClientHelloSentFirst 建连后立刻发 client.hello{min,max}，且在读循环开始之前
// （任何业务消息之前，#29）。老服务端会回 error{bad_message}，客户端须能容忍——
// 协商失败不触发重连风暴。
func TestClientHelloSentFirst(t *testing.T) {
	stub := newStubServer(t)
	cfg := testConfig(stub.wsURL(), "dev-alice")
	c := NewClient(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	var first Envelope
	select {
	case first = <-stub.received:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端未发送任何消息")
	}
	if first.Type != TypeClientHello {
		t.Fatalf("首条消息是 %q，want %q", first.Type, TypeClientHello)
	}
	var d ClientHelloData
	if err := first.Decode(&d); err != nil {
		t.Fatalf("decode client.hello: %v", err)
	}
	if d.Min != ProtocolVersionMin || d.Max != ProtocolVersionMax {
		t.Fatalf("client.hello = [%d,%d], want [%d,%d]",
			d.Min, d.Max, ProtocolVersionMin, ProtocolVersionMax)
	}
}

// TestClientHelloTolerant 老服务端不认识 client.hello：不回 server.hello（或回
// error{bad_message}），客户端必须保持运行、不重连风暴（#29 兼容路径）。
func TestClientHelloTolerant(t *testing.T) {
	stub := newStubServer(t)
	c := NewClient(testConfig(stub.wsURL(), "dev-alice"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case env := <-stub.received:
		if env.Type != TypeClientHello {
			t.Fatalf("首条消息是 %q", env.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("客户端未发送 client.hello")
	}
	// 服务端始终不回 server.hello：客户端必须继续运行且不进入重连循环。
	waitFor(t, "client stays connected", 3*time.Second, func() bool {
		return c.Connected()
	})
	if c.Stats.Reconnects.Load() != 0 {
		t.Fatalf("协商失败引发重连: reconnects=%d", c.Stats.Reconnects.Load())
	}
}
