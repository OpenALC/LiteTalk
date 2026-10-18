package signal

import (
	"encoding/json"
	"strings"
	"testing"
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
