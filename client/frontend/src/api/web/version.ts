// Web 客户端版本号：随批次发版手动递增。
// 服务端升级门（server/internal/api/mux.go clientVersionAccepted）按
// 主/次版本解析，前缀必须是数字开头——"web/1.9.0" 这种形态会被当成
// 非法值整体拒绝，所以这里不能带 "web/" 前缀。
export const WEB_CLIENT_VERSION = '1.9.0-alpha'
