package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot 从测试自身位置向上找 api/signal（测试在 server/api/tools/signalgen 下）。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "api", "signal", "catalog.json")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("找不到仓库根（api/signal/catalog.json）")
	return ""
}

// TestGenerateIdempotent 生成物幂等：连跑两次字节相同，且两侧输出一致。
func TestGenerateIdempotent(t *testing.T) {
	root := repoRoot(t)
	cat, err := loadCatalog(filepath.Join(root, "api", "signal", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	comps, err := loadComponents(filepath.Join(root, "api", "signal", "components.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := loadMessages(filepath.Join(root, "api", "signal", "messages"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := emit(cat, comps, msgs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := emit(cat, comps, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("生成物不幂等：两次输出不同")
	}
	// 与仓库里已提交的生成物一致（忘记重新生成会在本地就暴露）。
	for _, rel := range []string{
		filepath.Join("server", "internal", "signal", "protocol_gen.go"),
		filepath.Join("client", "internal", "signal", "protocol_gen.go"),
	} {
		onDisk, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("读 %s: %v", rel, err)
		}
		if string(onDisk) != string(a) {
			t.Fatalf("%s 与生成物不一致——请重新运行 go run ./server/api/tools/signalgen", rel)
		}
	}
}

// TestGoName wire 字段名 → Go 字段名的映射（含 WebRTC 标准字段与缩写）。
func TestGoName(t *testing.T) {
	for wire, want := range map[string]string{
		"user_id":           "UserID",
		"channel_id":        "ChannelID",
		"target_id":         "TargetID",
		"device_type":       "DeviceType",
		"sdp":               "SDP",
		"sdpMid":            "SDPMid",
		"sdpMLineIndex":     "SDPMLineIndex",
		"usernameFragment":  "UsernameFragment",
		"ts":                "TS",
		"id":                "ID",
		"min":               "Min",
		"max":               "Max",
		"version":           "Version",
		"speaking":          "Speaking",
		"username_fragment": "UsernameFragment",
	} {
		if got := goName(wire); got != want {
			t.Errorf("goName(%q) = %q, want %q", wire, got, want)
		}
	}
}

// TestGeneratedWireNames 生成物必须包含契约统一后的关键名字（防回退成旧名）。
func TestGeneratedWireNames(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "server", "internal", "signal", "protocol_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// gofmt 对齐会插入空格，按「常量名 + wire 值」分别断言。
	for _, want := range []string{
		"TypeRTCIce", `"rtc.ice"`,
		"TypeClientHello", `"client.hello"`,
		"TypeServerHello", `"server.hello"`,
		"type MemberInfo struct",
		"type PeerLeftData struct",
		"type ICEData struct",
		"ProtocolVersionMin",
		"ProtocolVersionMax",
		"ProtocolVersionCurrent",
		"CloseCodeProtocolUnsupported",
		"ErrCodeProtocolUnsupported",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("生成物缺少 %q", want)
		}
	}
	for _, banned := range []string{"TypeRTCICE", "type Member struct", "ChannelPeerLeftData"} {
		if strings.Contains(src, banned) {
			t.Errorf("生成物不应再出现 %q", banned)
		}
	}
}
