import type { AudioSettings } from '../api/types'

// withAudioDefaults 补齐 omitempty 后可能缺省的字段，避免设置页滑杆/开关拿到
// undefined。Go 侧 GetAudioSettings 已回落大部分默认值，这里只兜旧版本配置与
// 浏览器 mock；DRED 缺省为开（#44）。
export function withAudioDefaults(s: AudioSettings): AudioSettings {
  return {
    ...s,
    input_mode: s.input_mode ?? 'auto',
    pre_gain_db: s.pre_gain_db ?? 0,
    enable_apm: s.enable_apm ?? false,
    dred: s.dred ?? true,
  }
}
