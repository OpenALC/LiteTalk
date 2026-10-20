// 错误文案清洗（#15）：Wails 的 RuntimeError 前缀、Go APIError 的「机器码: 」
// 与操作路径包装都要在展示前剥掉；底层原因（网络错误等）必须保留。
import { describe, expect, it } from 'vitest'
import { cleanErrorText } from './errorText'

describe('cleanErrorText', () => {
  it('Wails RuntimeError 实例只取 message，并剥掉 Go 包装与机器码', () => {
    const e = new Error('get /servers/9/channels: forbidden: 不是该服务器成员')
    e.name = 'RuntimeError'
    expect(cleanErrorText(e)).toBe('不是该服务器成员')
  })

  it('字符串里的 RuntimeError: / Error: 前缀逐层剥掉', () => {
    expect(cleanErrorText('RuntimeError: bad_credentials: 邮箱或密码错误')).toBe('邮箱或密码错误')
    expect(cleanErrorText('Error: forbidden: 无权进入该频道')).toBe('无权进入该频道')
    expect(cleanErrorText('RuntimeError: 服务端已关闭')).toBe('服务端已关闭')
  })

  it('纯机器码前缀剥掉，正文中文原样保留', () => {
    expect(cleanErrorText('forbidden: 无权进入该频道')).toBe('无权进入该频道')
    expect(cleanErrorText('rate_limited: 发送过于频繁')).toBe('发送过于频繁')
  })

  it('保留底层有用的网络错误，不为了好看吞掉', () => {
    expect(cleanErrorText('get /servers: dial tcp 127.0.0.1:7845: connectex: No connection could be made'))
      .toBe('dial tcp 127.0.0.1:7845: connectex: No connection could be made')
    expect(cleanErrorText('http://127.0.0.1:8077')).toBe('http://127.0.0.1:8077')
  })

  it('空值与清洗后为空时回落 fallback', () => {
    expect(cleanErrorText(null, '操作失败')).toBe('操作失败')
    expect(cleanErrorText(undefined, '操作失败')).toBe('操作失败')
    expect(cleanErrorText('', '操作失败')).toBe('操作失败')
    expect(cleanErrorText(new Error(), '操作失败')).toBe('操作失败')
    expect(cleanErrorText({ code: 'x' }, '操作失败')).toBe('操作失败')
  })

  it('对象载荷取 string message（跨 realm 的 Error 也能读）', () => {
    expect(cleanErrorText({ message: 'forbidden: 不可访问' })).toBe('不可访问')
    expect(cleanErrorText({ message: '  消息保存失败  ' })).toBe('消息保存失败')
  })
})
