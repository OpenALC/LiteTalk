package sfu

import (
	"errors"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestTestClientOfferAfterTeardown(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	c := &testClient{t: t, userID: "teardown", pc: pc}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}

	// teardown 后送达的防抖 offer 不应进入已关闭的 PeerConnection。
	c.SendOffer("invalid offer")
}

func TestTestClientOfferErrorsWhileActive(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	c := &testClient{t: t, userID: "active", pc: pc}

	if err := c.answerOffer("invalid offer"); err == nil || errors.Is(err, webrtc.ErrConnectionClosed) {
		t.Fatalf("运行中的无效 offer 应返回协商错误，得到 %v", err)
	}

	// 未经测试桩 teardown 而关闭的 PC 是真正的异常，不得按错误文本吞掉。
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.answerOffer("invalid offer"); !errors.Is(err, webrtc.ErrConnectionClosed) {
		t.Fatalf("意外关闭的 PC 应返回 connection closed，得到 %v", err)
	}
}
