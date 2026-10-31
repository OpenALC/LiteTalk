// Web 客户端版本号：构建期由 vite define 注入（vite.config.ts 读 package.json
// 的 version 字段），发版时只递增 package.json，不存在第二处要同步的常量。
// 服务端升级门（server/internal/api/mux.go clientVersionAccepted）按主/次版本
// 解析，值必须以数字开头——"web/1.9.0" 这种形态会被当成非法值整体拒绝，所以
// 这里不能带 "web/" 前缀。
//
// __LITETALK_WEB_VERSION__ 由 define 字面替换；typeof 兜底只在 define 未生效的
// 异常环境（直接跑 TS 的脚本）触发，正常构建/测试都走注入值。
declare const __LITETALK_WEB_VERSION__: string

export const WEB_CLIENT_VERSION: string =
  typeof __LITETALK_WEB_VERSION__ === 'string' ? __LITETALK_WEB_VERSION__ : '0.0.0'
