## 改动

<!-- 一两句话：改了什么、为什么。关联 issue：Closes #N -->

## 类型

- [ ] fix（修复段 +1）
- [ ] feat（功能段 +1）
- [ ] perf / refactor / test / ci / chore / docs

## 自查（见 CLAUDE.md §9）

- [ ] 本地验证通过：gofmt 无输出、go vet、go test、pnpm type-check / test / build（按改动范围）
- [ ] 新逻辑有测试；音频热路径基准仍 0 allocs/op
- [ ] 协议 / REST 改动客户端与服务端两侧同步
- [ ] 数据库迁移成对（up/down）、只追加
- [ ] 无 UI 约束违规（头像 / emoji 图标 / 主题色）

## 验证方式

<!-- 怎么证明它工作：测试名、手测步骤、截图 -->
