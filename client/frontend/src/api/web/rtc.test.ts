// rtc.ts 采集生命周期单测（P2-6 退房释放麦克风）：teardown 区分「重建 PC」与
// 「退房」、退房后重入房重新采集、采集在途退房丢弃迟到流、重采失败保持
// recvonly 降级不硬失败、换新轨后回放静音意图。navigator/RTCPeerConnection
// 全部打桩——vitest 跑在 node 环境，无 DOM 与 WebRTC 实现。
import { afterEach, describe, expect, it, vi } from 'vitest'
import { webEventsOn } from './events'
import { VoicePeer, type RTCCallbacks } from './rtc'

function callbacks(): RTCCallbacks {
  return {
    onLocalOffer: vi.fn(),
    onLocalAnswer: vi.fn(),
    onLocalICE: vi.fn(),
    onConnectionState: vi.fn(),
  }
}

// fakeStream 最小 MediaStream 替身：一条可控 enabled 的音频轨 + stop 记录。
function fakeStream(enabled = true) {
  const track = { kind: 'audio', enabled, stop: vi.fn() }
  const stream = { getTracks: () => [track], getAudioTracks: () => [track] }
  return { track, stream: stream as unknown as MediaStream }
}

// FakePC 最小 RTCPeerConnection 替身：只记录上行轨/收发器方向（ensurePeer 用）。
class FakePC {
  connectionState = 'new'
  onicecandidate: unknown = null
  ontrack: unknown = null
  onnegotiationneeded: unknown = null
  onconnectionstatechange: unknown = null
  added: unknown[] = []
  transceivers: string[] = []
  addTrack(track: unknown, _stream: unknown): void {
    this.added.push(track)
  }
  addTransceiver(kind: string, _init?: unknown): void {
    this.transceivers.push(kind)
  }
  async createOffer(): Promise<RTCSessionDescriptionInit> {
    return { type: 'offer', sdp: 'v=0' }
  }
  async setLocalDescription(): Promise<void> {}
  close(): void {
    this.connectionState = 'closed'
  }
}

function deniedError(): Error {
  const e = new Error('denied')
  ;(e as Error & { name: string }).name = 'NotAllowedError'
  return e
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('VoicePeer.teardown 采集释放（P2-6）', () => {
  it('默认（重建 PC）保留采集流：startCapture 不再重采', async () => {
    const g = vi.fn().mockResolvedValue(fakeStream().stream)
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: g } })
    const peer = new VoicePeer(callbacks())
    await peer.startCapture()
    peer.teardown()
    expect((peer as any).localStream).not.toBeNull()
    await expect(peer.startCapture()).resolves.toBe(true)
    expect(g).toHaveBeenCalledTimes(1)
  })

  it('退房（releaseCapture=true）停掉全部轨并清空采集态，重入房重新 getUserMedia', async () => {
    const g = vi.fn().mockResolvedValue(fakeStream().stream)
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: g } })
    const peer = new VoicePeer(callbacks())
    await peer.startCapture()
    peer.teardown(true)
    expect(g).toHaveBeenCalledTimes(1)
    expect((peer as any).localStream).toBeNull()
    // 重入房：重新采集（第二次 getUserMedia），流与轨均可用。
    const ok = await peer.startCapture()
    expect(ok).toBe(true)
    expect(g).toHaveBeenCalledTimes(2)
    expect((peer as any).localStream).not.toBeNull()
  })

  it('采集在途（权限弹窗挂起）退房：迟到的流被停轨丢弃，不挂回已退房的 peer', async () => {
    let resolveGet!: (s: MediaStream) => void
    const g = vi.fn().mockImplementation(() => new Promise<MediaStream>((res) => (resolveGet = res)))
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: g } })
    const peer = new VoicePeer(callbacks())
    const pending = peer.startCapture()
    const { track, stream } = fakeStream()
    peer.teardown(true) // 用户在弹窗期间退房
    resolveGet(stream)
    await expect(pending).resolves.toBe(false)
    expect(track.stop).toHaveBeenCalledTimes(1)
    expect((peer as any).localStream).toBeNull()
    // 重入房重新采集：新代数生效，流正常挂回。
    const again = fakeStream()
    g.mockResolvedValue(again.stream)
    await expect(peer.startCapture()).resolves.toBe(true)
    expect((peer as any).localStream).not.toBeNull()
  })

  it('退房后重采失败保持降级语义：resolve false、不抛错、上报 voice:unavailable', async () => {
    const g = vi.fn().mockRejectedValue(deniedError())
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: g } })
    const peer = new VoicePeer(callbacks())
    await expect(peer.startCapture()).resolves.toBe(false)
    expect(peer.available).toBe(false)
    peer.teardown(true)
    // 重入房：权限仍被拒——重采一次失败，依旧不硬失败（c42d7d6 语义保留）。
    const seen: unknown[] = []
    const off = webEventsOn('voice:unavailable', (d) => seen.push(d))
    await expect(peer.startCapture()).resolves.toBe(false)
    off()
    expect(peer.available).toBe(false)
    expect(seen.length).toBe(1)
  })
})

describe('VoicePeer.ensurePeer 上行轨形态', () => {
  it('有流 sendrecv 挂轨；换新轨后回放静音意图（闭麦重入房不推流）', async () => {
    const g = vi.fn().mockResolvedValue(fakeStream(true).stream)
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: g } })
    vi.stubGlobal('RTCPeerConnection', FakePC)
    const peer = new VoicePeer(callbacks())
    peer.applyMute(true) // 闭麦意图先于采集（localStream 尚不存在，仅记录）
    const { track } = fakeStream()
    g.mockResolvedValue(({ getTracks: () => [track], getAudioTracks: () => [track] }) as unknown as MediaStream)
    await peer.startCapture()
    expect(track.enabled).toBe(true) // 新轨默认开启
    await peer.ensurePeer([])
    expect(track.enabled).toBe(false) // ensurePeer 回放静音意图
  })

  it('无流降级 recvonly（只收不发），不因采集缺失硬失败', async () => {
    const g = vi.fn().mockRejectedValue(deniedError())
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: g } })
    vi.stubGlobal('RTCPeerConnection', FakePC)
    const peer = new VoicePeer(callbacks())
    await peer.startCapture()
    await peer.ensurePeer([])
    const pc = (peer as any).pc as FakePC
    expect(pc.added).toHaveLength(0)
    expect(pc.transceivers).toEqual(['audio'])
  })
})
