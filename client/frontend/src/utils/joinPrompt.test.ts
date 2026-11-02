// #102：入房失败重弹密码框的条件——forbidden 只在频道确有密码时才当作「缺密码」。
import { describe, expect, it } from 'vitest'
import { shouldRepromptPassword } from './joinPrompt'

describe('shouldRepromptPassword', () => {
  it('bad_password 一律重弹（服务端已确认该频道有密码）', () => {
    expect(shouldRepromptPassword('bad_password', true)).toBe(true)
    // 快照过期（本地以为无密码）也要重弹：服务端已经明确说有密码。
    expect(shouldRepromptPassword('bad_password', false)).toBe(true)
  })

  it('forbidden + 有密码 = 缺密码，重弹', () => {
    expect(shouldRepromptPassword('forbidden', true)).toBe(true)
  })

  it('forbidden + 无密码 = 无权限，不弹密码框', () => {
    expect(shouldRepromptPassword('forbidden', false)).toBe(false)
  })

  it('其它错误码不弹密码框', () => {
    for (const code of ['rate_limited', 'not_found', 'internal', 'quota_exceeded', '']) {
      expect(shouldRepromptPassword(code, true)).toBe(false)
    }
    expect(shouldRepromptPassword(undefined, true)).toBe(false)
  })
})
