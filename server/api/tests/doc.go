// Package tests 契约测试（#30）：契约面（api/openapi.yaml、api/signal/）与两侧实现的系统化比对。
//
// 两条轴：
//   - REST：openapi.yaml 的 (method, path) 全集 ↔ 实际注册的路由全集，双向都要相等。
//     实现侧走 AST + 运行时注册表双提取（禁止 grep 数路由——pattern 是拼接表达式，文本匹配会数错）。
//   - 信令：api/signal/ 的消息/错误码/close code/协议版本 ↔ 两侧 protocol_gen.go 的常量与类型，
//     防止有人绕过 Schema 直接改生成物。
//
// 全部 schema/spec 驱动，不硬编码消息全集与端点全集：契约增量不会打爆本包。
package tests
