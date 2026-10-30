import { describe, expect, it } from 'vitest'
import type { AudioSettings } from '../api/types'
import { withAudioDefaults } from './audioSettings'

function base(): AudioSettings {
  return {
    bitrate_kbps: 32, vbr: true, dtx: true, fec: true, packet_loss: 10,
    denoise: true, denoise_strength: 2, agc: true,
    mic_volume: 100, playback_volume: 100, vad_sens: 2, opus_complexity: 9,
  }
}

describe('withAudioDefaults', () => {
  it('DRED 缺省为开（旧配置/omitempty 后未出现 false 不应被当成关闭）', () => {
    expect(withAudioDefaults(base()).dred).toBe(true)
  })

  it('显式关闭保留 false', () => {
    expect(withAudioDefaults({ ...base(), dred: false }).dred).toBe(false)
  })

  it('补齐其它 omitempty 字段且不覆盖已有值', () => {
    const s = withAudioDefaults({ ...base(), input_mode: 'left', pre_gain_db: 6, enable_apm: true })
    expect(s.input_mode).toBe('left')
    expect(s.pre_gain_db).toBe(6)
    expect(s.enable_apm).toBe(true)
    const d = withAudioDefaults(base())
    expect(d.input_mode).toBe('auto')
    expect(d.pre_gain_db).toBe(0)
    expect(d.enable_apm).toBe(false)
  })
})
