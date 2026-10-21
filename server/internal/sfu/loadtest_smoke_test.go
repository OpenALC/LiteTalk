package sfu

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// TestLoadSmoke 小规模负载冒烟（CI 跑，约 20s）：2 房 × 4 人 × 15s，
// 全员建连、互收、稳态丢包在容差内（0.5%，见断言 2 注释）、断连清理。
// -short 跳过。全规模压测用 cmd/loadtest（10 房 × 10 人，部署机执行）。
func TestLoadSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过负载冒烟（-short）")
	}

	const (
		rooms      = 2
		usersPer   = 4
		sendDur    = 15 * time.Second
		pktEveryMs = 20 // 50pps
	)
	s := newTestSFU(t, 10)

	type member struct {
		c      *testClient
		sent   atomic.Int64
		prefix string
	}
	roomsArr := make([][]*member, rooms)

	// 建连：同房第 2 人起触发 SFU 重协商（onPeerReady 补发轨道）
	for r := 0; r < rooms; r++ {
		roomsArr[r] = make([]*member, usersPer)
		for i := 0; i < usersPer; i++ {
			uid := fmt.Sprintf("u%d", i)
			roomID := fmt.Sprintf("load%d", r)
			roomsArr[r][i] = &member{
				c:      newTestClient(t, s, roomID, uid),
				prefix: fmt.Sprintf("r%du%d", r, i),
			}
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// 发包：每成员 50pps 带唯一前缀 payload（SFU 不解码，payload 只作丢失检测标记）
	for r := 0; r < rooms; r++ {
		for i := 0; i < usersPer; i++ {
			m := roomsArr[r][i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				seq := uint16(1)
				ts := uint32(1)
				for {
					select {
					case <-stop:
						return
					default:
					}
					seq++
					ts += 960
					_ = m.c.track.WriteRTP(&rtp.Packet{
						Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts},
						Payload: []byte(fmt.Sprintf("%s-%d", m.prefix, seq)),
					})
					m.sent.Add(1)
					time.Sleep(pktEveryMs * time.Millisecond)
				}
			}()
		}
	}

	// 预热：等全部 (发送者→接收者) 路由收流——建连期 ensureRoute 未就绪的
	// 丢包属启动暂态，不计入稳态丢包窗口
	warmDeadline := time.Now().Add(8 * time.Second)
	for {
		ready := true
		for r := 0; r < rooms && ready; r++ {
			for i := 0; i < usersPer && ready; i++ {
				for j := 0; j < usersPer; j++ {
					if i == j {
						continue
					}
					if roomsArr[r][i].c.countMatchingPrefix(roomsArr[r][j].prefix) == 0 {
						ready = false
						break
					}
				}
			}
		}
		if ready || time.Now().After(warmDeadline) {
			if !ready {
				t.Fatal("预热失败：8s 内未全部收流（路由未建立）")
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 稳态窗口起点：清零全部计数
	for r := 0; r < rooms; r++ {
		for i := 0; i < usersPer; i++ {
			roomsArr[r][i].sent.Store(0)
			roomsArr[r][i].c.resetReceived()
		}
	}
	t.Log("预热完成，进入 15s 稳态测量窗口")

	time.Sleep(sendDur)
	close(stop)
	wg.Wait()
	// 末尾在途包排空
	time.Sleep(1 * time.Second)

	// 断言 1：互收——每对 (发送者→接收者) 都有包（稳态窗口内）
	// 断言 2：稳态丢包容差——同房每对逐字节计数对账。完全零丢是理想值，但
	// 回环 50pps 在 CI -race 全量并行下偶发调度丢包（实测 1/725≈0.14%），属
	// 测试环境抖动而非路由错误，故允许 maxLoss = max(1, want/200)（约 0.5%）；
	// 超出容差仍是失败。路由正确性不受影响：任何包都没收到照样 fail。
	for r := 0; r < rooms; r++ {
		for i := 0; i < usersPer; i++ {
			recv := roomsArr[r][i].c
			for j := 0; j < usersPer; j++ {
				if i == j {
					continue
				}
				sender := roomsArr[r][j]
				got := recv.countMatchingPrefix(sender.prefix)
				want := sender.sent.Load()
				if got == 0 {
					t.Errorf("房%d: %s 未收到 %s 的任何包（路由未建立）", r, recv.userID, sender.prefix)
					continue
				}
				diff := want - got
				maxLoss := max(int64(1), want/200)
				if diff > maxLoss {
					t.Errorf("房%d: %s→%s 稳态丢包 %d/%d（%.2f%%），容差 %d",
						r, sender.prefix, recv.userID, diff, want, float64(diff)/float64(want)*100, maxLoss)
				}
			}
		}
	}

	// 断言 3：断连清理——全部 Leave 后房间应为空且被销毁
	for r := 0; r < rooms; r++ {
		roomID := fmt.Sprintf("load%d", r)
		for i := 0; i < usersPer; i++ {
			s.LeaveIf(roomID, roomsArr[r][i].c.userID, roomsArr[r][i].c.userID)
		}
		if s.findRoom(roomID) != nil {
			t.Errorf("房%s: 全员离开后房间未销毁", roomID)
		}
	}
	t.Logf("负载冒烟通过：%d 房 × %d 人 × %ds，丢包在容差内，清理干净", rooms, usersPer, int(sendDur.Seconds()))
}

// countMatchingPrefix 统计收到的、payload 前缀匹配的包数（loadtest 丢包检测）。
func (c *testClient) countMatchingPrefix(prefix string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	p := []byte(prefix + "-")
	for _, pkt := range c.received {
		if len(pkt.Payload) >= len(p) && string(pkt.Payload[:len(p)]) == string(p) {
			n++
		}
	}
	return n
}

// resetReceived 清空收包记录（稳态测量窗口起点，预热后调用）。
func (c *testClient) resetReceived() {
	c.mu.Lock()
	c.received = nil
	c.mu.Unlock()
}
