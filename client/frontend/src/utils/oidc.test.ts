import { describe, expect, it } from 'vitest'
import { extractOidcCallbackParams, oidcCallbackErrorText } from './oidc'

// extractOidcCallbackParams（#118 P2-5）：redirect_url 形态由部署者配置，
// code/state 可能落在 hash 段（#/oidc/callback?…）或 search 段（路径形回跳，
// SPA fallback 改写而来），两处都必须接住。
describe('extractOidcCallbackParams', () => {
  it('hash 形 redirect_url：参数在 hash 段', () => {
    expect(extractOidcCallbackParams('', '#/oidc/callback?code=c1&state=s1'))
      .toEqual({ code: 'c1', state: 's1' })
  })

  it('search 形 redirect_url：参数在 search 段', () => {
    expect(extractOidcCallbackParams('?code=c2&state=s2', ''))
      .toEqual({ code: 'c2', state: 's2' })
  })

  it('hash 段无 query 时回落 search', () => {
    expect(extractOidcCallbackParams('?code=c3&state=s3', '#/oidc/callback'))
      .toEqual({ code: 'c3', state: 's3' })
  })

  it('两处都有时 hash 优先（同一来源不会同时出现，取确定口径）', () => {
    expect(extractOidcCallbackParams('?code=c&state=s', '#/oidc/callback?code=h&state=h'))
      .toEqual({ code: 'h', state: 'h' })
  })

  it('缺参数返回空串（调用方按缺失分支提示重新发起）', () => {
    expect(extractOidcCallbackParams('', '')).toEqual({ code: '', state: '' })
    expect(extractOidcCallbackParams('?code=c', '')).toEqual({ code: 'c', state: '' })
  })
})

// oidcCallbackErrorText：区分 state 失效与交换失败；桌面错误串（code: detail
// 前缀形态）与 web APIError（code + message）都要归一到同一文案口径。
describe('oidcCallbackErrorText', () => {
  it('bad_state（web APIError 形态）明确指向重新发起', () => {
    expect(oidcCallbackErrorText({ code: 'bad_state', message: '任意 detail' }))
      .toBe('登录状态无效或已过期，请回到登录页重新发起')
  })

  it('其余错误优先展示清洗后的服务端正文（web APIError）', () => {
    expect(oidcCallbackErrorText({ code: 'oidc_upstream', message: '身份提供方暂时不可用，请稍后再试' }))
      .toBe('身份提供方暂时不可用，请稍后再试')
  })

  it('桌面错误串剥掉机器码前缀保留正文', () => {
    expect(oidcCallbackErrorText(new Error('bad_code: 授权码无效或已过期，请重新发起登录')))
      .toBe('授权码无效或已过期，请重新发起登录')
  })

  it('取不到正文回落通用文案（不让用户看到空提示）', () => {
    expect(oidcCallbackErrorText(undefined)).toBe('登录交换失败，请回到登录页重新发起')
  })
})
