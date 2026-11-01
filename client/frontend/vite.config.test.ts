// Web 客户端版本注入（#119）：CI 标签构建用 LITELTALK_VERSION（= 发布标签），
// 其它构建回落 package.json。直接测解析函数，不必为了读一个环境变量跑完整
// vite build；vite.config.ts 不在 tsconfig include 内，本测试由 vitest 转译执行。
import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { resolveWebVersion } from './vite.config'

const pkgVersion = (
  JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf8')) as {
    version: string
  }
).version

describe('resolveWebVersion', () => {
  it('LITELTALK_VERSION 存在时采用注入值', () => {
    expect(resolveWebVersion(pkgVersion, { LITELTALK_VERSION: 'v1.9.8-alpha' })).toBe('v1.9.8-alpha')
  })

  it('LITELTALK_VERSION 缺失或为空时回落 package.json', () => {
    expect(resolveWebVersion(pkgVersion, {})).toBe(pkgVersion)
    expect(resolveWebVersion(pkgVersion, { LITELTALK_VERSION: '' })).toBe(pkgVersion)
    expect(resolveWebVersion(pkgVersion, { LITELTALK_VERSION: '   ' })).toBe(pkgVersion)
  })
})
