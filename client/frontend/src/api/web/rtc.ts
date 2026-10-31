// Web 版语音媒体层：浏览器原生 WebRTC 对接 SFU（spike.md 已验证通路）。
// 与 Go 音频链路的差异（有意为之，见批次任务书 §0.6）：
//   - 采集/编码/播放全走浏览器原生栈（getUserMedia + Opus + <audio>），不搬
//     RNNoise/APM/DRED；浏览器自带的回声消除/降噪/AGC 由 getUserMedia 约束开启。
//   - 说话指示不依赖客户端上报：服务端按 RFC 6464 电平判定并下发 voice.speaking
//     （协议 ≥2），浏览器 offer 默认携带 ssrc-audio-level 扩展。
// 协商方向语义照抄 Go 客户端 rtc/peer.go：初始 offer 客户端只发一次，此后
// SFU 主导重协商（收到 rtc.offer → 应答 rtc.answer），本地绝不再发 rtc.offer。
import { webEmit } from './events'

// 服务端约定（server/internal/sfu room.go ensureRoute + Go remote.go 对账注释）：
// TrackID = "audio-<user_id>"、StreamID = "litetalk-<room>"。
const TRACK_ID_PREFIX = 'audio-'

// ICEServer 与 RTCIceServerInit 的窄接口（避免依赖 DOM lib 之外的定义）。
export interface ICEServerLike {
  urls: string[]
  username?: string
  credential?: string
}

// rtcCallbacks rtc 层 → 信令层的回调（signal.ts 接线发送）。
export interface RTCCallbacks {
  onLocalOffer(sdp: string): void
  onLocalAnswer(sdp: string): void
  onLocalICE(c: RTCIceCandidateInit): void
  onConnectionState(state: string): void
}

interface RemoteAudio {
  el: HTMLAudioElement
  gain: number
}

export class VoicePeer {
  private pc: RTCPeerConnection | null = null
  private localStream: MediaStream | null = null
  private callbacks: RTCCallbacks
  // initialOffered 初始 offer 只发一次的闸（SFU 拒绝重复初始 offer）。
  private initialOffered = false
  // pendingRemoteICE 远端描述就绪前到达的候选（初始 answer/重协商 offer 前缓存）。
  private pendingRemoteICE: RTCIceCandidateInit[] = []
  private remoteAudio = new Map<string, RemoteAudio>() // uid → 播放元素
  private muted = false
  private deafened = false
  // available 采集可用性（false = 采集已明确失败；join 不再因此报错，
  // peer 降级 recvonly 只收不发——「能听」与「能说」解耦）。
  available = true
  unavailableError = ''
  // capturePromise 单飞采集：权限弹窗可能长时间挂起，必须可等待、可超时。
  private capturePromise: Promise<boolean> | null = null

  constructor(callbacks: RTCCallbacks) {
    this.callbacks = callbacks
  }

  // startCapture 打开麦克风（幂等单飞）。失败置 available=false 并上报
  // voice:unavailable；**不抛错**——采集失败不阻断入房。
  startCapture(): Promise<boolean> {
    if (this.localStream) return Promise.resolve(true)
    if (!this.capturePromise) this.capturePromise = this.doCapture()
    return this.capturePromise
  }

  private async doCapture(): Promise<boolean> {
    try {
      this.localStream = await navigator.mediaDevices.getUserMedia({
        audio: {
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
        },
        video: false,
      })
      this.available = true
      return true
    } catch (e) {
      this.available = false
      this.unavailableError = e instanceof Error && e.name === 'NotAllowedError'
        ? '麦克风权限被拒绝，请在浏览器设置中允许'
        : '麦克风不可用，请检查设备连接'
      webEmit('voice:unavailable', { error: this.unavailableError })
      return false
    }
  }

  // captureSettled 等采集落定，最多 timeoutMs。getUserMedia 在权限弹窗期间
  // 永不 settle：到点放行，让 ensurePeer 按当前状态建链（有流 sendrecv，
  // 无流 recvonly），不给弹窗挂死建连路径的机会。
  async captureSettled(timeoutMs: number): Promise<void> {
    if (!this.capturePromise) return
    await Promise.race([
      this.capturePromise.catch(() => false),
      new Promise<void>((r) => setTimeout(r, timeoutMs)),
    ])
  }

  // ensurePeer 建立对 SFU 的 PeerConnection（无存活实例时）。已有实例时复用，
  // 下行轨由 SFU 重协商补发（与 Go channel.joined 处理器的 alive 判定一致）。
  async ensurePeer(iceServers: ICEServerLike[]): Promise<void> {
    const pc = this.pc
    if (pc && pc.connectionState !== 'closed' && pc.connectionState !== 'failed') return
    if (pc) this.teardown()
    this.initialOffered = false
    this.pendingRemoteICE = []
    // 采集在途时给权限弹窗一个有限窗口（20s），到点按现状建链。
    await this.captureSettled(20_000)
    const conn = new RTCPeerConnection({ iceServers })
    this.pc = conn

    conn.onnegotiationneeded = async () => {
      // 初始协商：客户端 offer 一次。此后重协商由 SFU 主导，这里不再主动 offer。
      if (this.initialOffered || this.pc !== conn) return
      this.initialOffered = true
      try {
        const offer = await conn.createOffer()
        await conn.setLocalDescription(offer)
        if (offer.sdp) this.callbacks.onLocalOffer(offer.sdp)
      } catch (e) {
        console.error('[web] 初始 offer 失败', e)
      }
    }
    conn.onicecandidate = (e) => {
      if (e.candidate) this.callbacks.onLocalICE(e.candidate.toJSON())
    }
    conn.ontrack = (e) => {
      this.attachRemoteAudio(e)
    }
    conn.onconnectionstatechange = () => {
      this.callbacks.onConnectionState(conn.connectionState)
    }
    // 上行：本端麦克风波形（native Opus 编码由浏览器完成）。
    if (this.localStream) {
      for (const track of this.localStream.getAudioTracks()) {
        conn.addTrack(track, this.localStream)
      }
    } else {
      // 采集不可用/仍在弹窗：只收不发（recvonly）。Go 侧无此路径——那边
      // JoinChannel 直接失败；浏览器把「能听」与「能说」解耦是净收益，
      // 用户重入房（届时已有权限/流）即恢复 sendrecv。
      conn.addTransceiver('audio', { direction: 'recvonly' })
    }
  }

  // attachRemoteAudio 远端轨 → <audio> 元素（隐藏、自动播放）。
  // uid 从 TrackID 约定解析（audio-<uid>），旧服务端/异常形态退化 stream id，
  // 再退化空串（SetPeerVolume 对未知 uid 无操作）。
  private attachRemoteAudio(e: RTCTrackEvent): void {
    if (e.track.kind !== 'audio') return
    let uid = ''
    if (e.track.id.startsWith(TRACK_ID_PREFIX)) {
      uid = e.track.id.slice(TRACK_ID_PREFIX.length)
    } else if (e.streams[0]?.id) {
      uid = e.streams[0].id
    }
    const el = document.createElement('audio')
    el.autoplay = true
    // Chromium 怪癖（spike.md #2）：远端流不经 <audio> 元素拉动管线，
    // WebAudio/播放都可能全零；muted 的元素也参与解码，这里统一静音输出、
    // 音量统一走 gain 乘数（deafen/音量调节都改 el.volume/el.muted）。
    el.muted = true
    el.srcObject = new MediaStream([e.track])
    el.dataset.uid = uid
    document.body.appendChild(el)
    const gain = this.remoteAudio.get(uid)?.gain ?? 1
    this.remoteAudio.set(uid, { el, gain })
    this.applyAudioElement(uid)
    // track 结束（离房/对端撤轨）时清元素，防止幽灵音频元素累积。
    e.track.onended = () => {
      el.remove()
      this.remoteAudio.delete(uid)
    }
  }

  private applyAudioElement(uid: string): void {
    const a = this.remoteAudio.get(uid)
    if (!a) return
    a.el.volume = Math.min(1, a.gain)
    // deafen = 总线级静音；underrun 兜底：显式 play() 一次应对 autoplay 策略。
    a.el.muted = this.deafened
    void a.el.play().catch(() => {})
  }

  // handleRemoteOffer SFU 重协商 offer：应答 rtc.answer（返回 sdp 给信令层发送）。
  async handleRemoteOffer(sdp: string): Promise<string | null> {
    const pc = this.pc
    if (!pc) return null
    try {
      await pc.setRemoteDescription({ type: 'offer', sdp })
      await this.flushPendingICE()
      const answer = await pc.createAnswer()
      await pc.setLocalDescription(answer)
      return answer.sdp ?? null
    } catch (e) {
      console.error('[web] 应用 SFU offer 失败', e)
      return null
    }
  }

  // handleRemoteAnswer 应用 SFU 对初始 offer 的应答。
  async handleRemoteAnswer(sdp: string): Promise<void> {
    const pc = this.pc
    if (!pc) return
    try {
      await pc.setRemoteDescription({ type: 'answer', sdp })
      await this.flushPendingICE()
    } catch (e) {
      console.error('[web] 应用 rtc.answer 失败', e)
    }
  }

  async addRemoteICE(c: RTCIceCandidateInit): Promise<void> {
    const pc = this.pc
    if (!pc) return
    if (!pc.remoteDescription) {
      // 初始 answer / 重协商 offer 未到：先缓存（服务端对等语义）。
      this.pendingRemoteICE.push(c)
      return
    }
    try {
      await pc.addIceCandidate(c)
    } catch (e) {
      // 乱序/过期候选（例如重协商间隙）：忽略，不致命。
      console.warn('[web] addIceCandidate 忽略', e)
    }
  }

  private async flushPendingICE(): Promise<void> {
    const queued = this.pendingRemoteICE.splice(0)
    for (const c of queued) await this.addRemoteICE(c)
  }

  // teardown 拆本地链路（不关采集流——重入房复用同一麦克风轨）。
  teardown(): void {
    for (const { el } of this.remoteAudio.values()) el.remove()
    this.remoteAudio.clear()
    if (this.pc) {
      this.pc.onicecandidate = null
      this.pc.ontrack = null
      this.pc.onnegotiationneeded = null
      this.pc.onconnectionstatechange = null
      try {
        this.pc.close()
      } catch {
        // 已关闭
      }
      this.pc = null
    }
  }

  get connectionState(): string {
    return this.pc?.connectionState ?? 'closed'
  }

  isConnected(): boolean {
    const s = this.connectionState
    return s === 'connected'
  }

  // applyMute 本地闭麦：track.enabled=false 让浏览器上行静音帧（RFC 6464 电平
  // 随之归零，服务端说话判定自动熄灭）。与 Go 的「停发上行包」等效，实现更简单。
  applyMute(muted: boolean): void {
    this.muted = muted
    if (!this.localStream) return
    for (const t of this.localStream.getAudioTracks()) t.enabled = !muted
  }

  applyDeafen(deafened: boolean): void {
    this.deafened = deafened
    for (const uid of this.remoteAudio.keys()) this.applyAudioElement(uid)
  }

  setPeerVolume(uid: string, gain: number): void {
    const a = this.remoteAudio.get(uid)
    if (a) {
      a.gain = gain
      this.applyAudioElement(uid)
    }
  }

  // statsSnapshot 一次周期性统计（voice:stats 事件载荷来源）。字段口径见 types.ts
  // 的 VoiceStats：桌面专有字段（DRED/FEC/缓冲深度）在 web 侧恒缺省，UI 按 0/-- 渲染。
  async statsSnapshot(memberCount: number): Promise<{
    latency_ms: number
    members: number
    peer_ids: string[]
    rtp_sent: number
    rtp_received: number
    loss_pct: number
    rtp_jitter_ms: number
  }> {
    const pc = this.pc
    const out = {
      latency_ms: 0,
      members: memberCount,
      peer_ids: [...this.remoteAudio.keys()],
      rtp_sent: 0,
      rtp_received: 0,
      loss_pct: 0,
      rtp_jitter_ms: 0,
    }
    if (!pc) return out
    const stats = await pc.getStats()
    stats.forEach((s) => {
      const r = s as unknown as Record<string, number | string | undefined>
      if (s.type === 'candidate-pair' && r.state === 'succeeded' && r.currentRoundTripTime != null) {
        out.latency_ms = Math.round(Number(r.currentRoundTripTime) * 1000)
      }
      if (s.type === 'outbound-rtp' && s.kind === 'audio') {
        out.rtp_sent = Number(r.packetsSent ?? 0)
      }
      if (s.type === 'inbound-rtp' && s.kind === 'audio') {
        out.rtp_received = Number(r.packetsReceived ?? 0)
        const lost = Number(r.packetsLost ?? 0)
        const total = out.rtp_received + lost
        if (total > 0) out.loss_pct = Math.round((lost / total) * 1000) / 10
        out.rtp_jitter_ms = Math.round(Number(r.jitter ?? 0) * 1000)
      }
    })
    return out
  }
}
