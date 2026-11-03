//go:build linux

package config

// defaultHibernateMode Linux 平台的休眠模式缺省值。
//
// v1.10.0 GTK4 迁移（issue #5）按迁移计划书 §3.2 降级 1 调整：destroy 模式在
// Wails v3.0.0-beta.30 的 GTK4 路径上因上游窗口泄漏不可用（实测见
// hibernate_mode.go），Linux 新配置缺省走 hibernate——只卸载前端组件，
// 不销毁窗口。仅作用于缺省值（配置无该字段/非法值）；用户已显式保存的
// 模式（含 off、含 destroy——后者由读路径强降兜底，见 downgradeDestroyForGTK4）
// 不在这里改写。
const defaultHibernateMode = "hibernate"
