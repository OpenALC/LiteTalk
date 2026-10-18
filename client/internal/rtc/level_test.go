package rtc

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// TestPeerAudioLevelExt RFC 6464 电平扩展全链路（#33）：协商 → SendOpus 改写
// 预分配字节 → 对端解析出电平。逐包改写必须反映新值（payload 缓冲不被拷走）。
func TestPeerAudioLevelExt(t *testing.T) {
	level := byte(27)
	peer, sfu := connectLoopback(t, func() uint8 { return level }, false)

	payload := []byte{0x01, 0x02, 0x03, 0x04}
	if err := peer.SendOpus(payload); err != nil {
		t.Fatalf("SendOpus: %v", err)
	}
	assertLevel(t, waitUplink(t, sfu), level)

	// 电平是每包就地改写的：换值后下一包必须带上新值。
	level = 53
	if err := peer.SendOpus(payload); err != nil {
		t.Fatalf("SendOpus #2: %v", err)
	}
	assertLevel(t, waitUplink(t, sfu), level)
}

// TestPeerAudioLevelExtAbsent 对端不带电平扩展时（LevelFn=nil）不写扩展。
func TestPeerAudioLevelExtAbsent(t *testing.T) {
	peer, sfu := connectLoopback(t, nil, false)
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	if err := peer.SendOpus(payload); err != nil {
		t.Fatalf("SendOpus: %v", err)
	}
	pkt := waitUplink(t, sfu)
	if pkt.Header.Extension {
		t.Fatalf("unexpected header extension: ids=%v", pkt.Header.GetExtensionIDs())
	}
}

// waitUplink 取 fakeSFU 收到的一包上行 RTP。
func waitUplink(t *testing.T, sfu *fakeSFU) *rtp.Packet {
	t.Helper()
	select {
	case pkt := <-sfu.gotRTP:
		return pkt
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting uplink packet")
		return nil
	}
}

// assertLevel 断言包带 RFC 6464 one-byte 头扩展且电平字节符合预期。
// 会话只协商 audio-level 一个扩展，元素恰好一个（ID 由 SDP 分配）。
func assertLevel(t *testing.T, pkt *rtp.Packet, level byte) {
	t.Helper()
	if !pkt.Header.Extension {
		t.Fatal("packet has no header extension")
	}
	if pkt.Header.ExtensionProfile != rtp.ExtensionProfileOneByte {
		t.Fatalf("ext profile = 0x%04X, want 0xBEDE", pkt.Header.ExtensionProfile)
	}
	ids := pkt.Header.GetExtensionIDs()
	if len(ids) != 1 {
		t.Fatalf("ext ids = %v, want exactly 1", ids)
	}
	ext := pkt.Header.GetExtension(ids[0])
	if len(ext) != 1 {
		t.Fatalf("ext payload len = %d, want 1", len(ext))
	}
	if ext[0] != level {
		t.Fatalf("level byte = 0x%02X, want 0x%02X", ext[0], level)
	}
}
